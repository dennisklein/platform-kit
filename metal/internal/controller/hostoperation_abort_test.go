package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// askAbort sets the abort annotation on the operation, as pk-metal abort does.
func askAbort(t *testing.T, c client.Client) {
	t.Helper()
	op := &metalv1alpha1.HostOperation{}
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	require.NoError(t, c.Get(t.Context(), key, op))
	if op.Annotations == nil {
		op.Annotations = map[string]string{}
	}
	op.Annotations[metalv1alpha1.AnnotationAbort] = "alice: wrong host"
	require.NoError(t, c.Update(t.Context(), op))
}

func TestAbortBeforeTheStartDoesNothing(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Annotations = map[string]string{metalv1alpha1.AnnotationAbort: "alice: wrong host"}
	r, _, rec := executorFor(t, "huawei-2288h-v5", op)

	got := runOp(t, r)
	ex := executed(got)
	assert.Equal(t, metav1.ConditionFalse, ex.Status)
	assert.Equal(t, metalv1alpha1.ReasonAborted, ex.Reason)
	assert.Contains(t, ex.Message, "before it started")
	assert.Contains(t, ex.Message, "alice: wrong host")
	assert.Equal(t, metalv1alpha1.OperationAborted, got.Status.Phase)
	assert.Empty(t, rec.powers)
	assert.Nil(t, got.Status.Baseline)
}

// TestAbortUndoesTheMountAndTheOverride: a boot aborted while it waits for
// the machine leaves no medium mounted and no override set, so the next
// start of the machine boots what it booted before.
func TestAbortUndoesTheMountAndTheOverride(t *testing.T) {
	srv := isoServer(t)
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd"}},
		insertURL(srv.URL+"/installer.iso"),
		metalv1alpha1.HostOperationAction{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{NodeRef: metalv1alpha1.Reference{Name: "node-a"}}},
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	r, st, rec := executorFor(t, "supermicro-x12", op)
	withImages(t, r, srv)
	downloaded(t, r, srv.URL+"/installer.iso", isoDigest)
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	require.Len(t, got.Status.Steps, 2, "override and mount done; waiting for the check-in")
	require.NotEmpty(t, mounted(t.Context(), st, mediaSlot))

	askAbort(t, r.Client)
	got = runOp(t, r)
	ex := executed(got)
	assert.Equal(t, metalv1alpha1.ReasonAborted, ex.Reason)
	assert.Contains(t, ex.Message, "before step 3/4 awaitCheckIn")
	assert.Contains(t, ex.Message, "ejected virtual media "+mediaSlot)
	assert.Contains(t, ex.Message, "cleared the boot override to Cd")
	assert.Empty(t, mounted(t.Context(), st, mediaSlot))
	systems, err := st.Systems(t.Context())
	require.NoError(t, err)
	assert.Equal(t, bmc.OverrideDisabled, systems[0].BootOverride.Enabled)
	assert.Empty(t, rec.powers, "the power cycle after the abort never ran")
	last := got.Status.Steps[len(got.Status.Steps)-1]
	assert.Equal(t, "abort", last.Action)
	assert.Equal(t, "Aborted", last.Phase)
}

// TestAbortHandsBackTheCordon: a drain that waits is aborted, and the node
// the operation cordoned is schedulable again; the reboot never happens.
func TestAbortHandsBackTheCordon(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", rebootPlan("w-1")...))
	require.NoError(t, lab.kube.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "stuck", Namespace: "apps", Finalizers: []string{"test/hold"}},
		Spec:       corev1.PodSpec{NodeName: "w-1"},
	}))

	op := lab.pass(t)
	require.Contains(t, executed(op).Message, "draining node w-1")
	require.True(t, lab.unschedulable(t, "w-1"))
	require.Equal(t, "w-1", op.Status.CordonedNode)

	askAbort(t, lab.r.Client)
	op = lab.pass(t)
	ex := executed(op)
	assert.Equal(t, metalv1alpha1.ReasonAborted, ex.Reason, ex.Message)
	assert.Contains(t, ex.Message, "before step 2/5 drain")
	assert.Contains(t, ex.Message, "made Node w-1 schedulable again")
	assert.False(t, lab.unschedulable(t, "w-1"))
	assert.Empty(t, lab.machines["w-1"].Reboots())
	assert.Zero(t, lab.bmcDials, "nothing to undo on the BMC, so it is not dialed")
}

// TestAbortLeavesAPersonsCordon: the node was cordoned before the operation,
// so the operation did not cordon it, and the abort does not uncordon it.
func TestAbortLeavesAPersonsCordon(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", rebootPlan("w-1")...))
	n := &corev1.Node{}
	require.NoError(t, lab.kube.Get(t.Context(), client.ObjectKey{Name: "w-1"}, n))
	n.Spec.Unschedulable = true
	require.NoError(t, lab.kube.Update(t.Context(), n))
	require.NoError(t, lab.kube.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
		Spec:       corev1.PodSpec{NodeName: "w-1"},
	}))

	op := lab.pass(t) // cordon (already), drain evicts and waits
	assert.Empty(t, op.Status.CordonedNode)
	askAbort(t, lab.r.Client)
	op = lab.pass(t)
	assert.Equal(t, metalv1alpha1.ReasonAborted, executed(op).Reason)
	assert.True(t, lab.unschedulable(t, "w-1"), "the person's cordon stays")
}

// TestAbortFinishesAStepThatActed: a reboot already asked for is seen
// through; the abort takes effect before the next step.
func TestAbortFinishesAStepThatActed(t *testing.T) {
	lab := newTalosLab(t, talosOp("w-1", rebootPlan("w-1")...))
	m := lab.machines["w-1"]
	m.SetStage("booting", false)
	// Cordon, drain (nothing to evict) and the reboot request, then wait.
	op := lab.pass(t)
	require.Len(t, m.Reboots(), 1, "%+v", executed(op))

	askAbort(t, lab.r.Client)
	op = lab.pass(t)
	assert.Equal(t, metav1.ConditionUnknown, executed(op).Status, "the reboot in flight is finished first")
	assert.Contains(t, executed(op).Message, "talosReboot")

	m.SetStage("running", true)
	op = lab.pass(t)
	ex := executed(op)
	assert.Equal(t, metalv1alpha1.ReasonAborted, ex.Reason, ex.Message)
	assert.Contains(t, ex.Message, "before step 4/5 waitNodeReady")
	assert.False(t, lab.unschedulable(t, "w-1"))
	assert.Len(t, m.Reboots(), 1)
}
