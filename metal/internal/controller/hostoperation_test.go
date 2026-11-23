package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// recordingClient counts the mutating calls the executor issues, over the
// in-memory sim.
type recordingClient struct {
	bmc.Client
	overrides int
	powers    []bmc.PowerAction
}

func (c *recordingClient) SetBootOverride(ctx context.Context, id string, o bmc.BootOverride) error {
	c.overrides++
	return c.Client.SetBootOverride(ctx, id, o)
}

func (c *recordingClient) SetPower(ctx context.Context, id string, a bmc.PowerAction) error {
	c.powers = append(c.powers, a)
	return c.Client.SetPower(ctx, id, a)
}

func (c *recordingClient) Close() error { return nil }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, metalv1alpha1.AddToScheme(s))
	require.NoError(t, osv1alpha1.AddToScheme(s))
	return s
}

// executorFor builds an executor over a fake API and the named sim fixture.
func executorFor(t *testing.T, fixture string, objs ...client.Object) (*HostOperationReconciler, *sim.State, *recordingClient) {
	t.Helper()
	f, err := sim.LoadFixture(fixture)
	require.NoError(t, err)
	st := sim.NewState(f)
	rec := &recordingClient{Client: st}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}).
		WithIndex(&metalv1alpha1.HostOperation{}, opHostIndex, func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.HostOperation).Spec.HostRef.Name}
		}).
		WithObjects(append(objs, host)...).Build()
	r := &HostOperationReconciler{
		Client:   cl,
		Recorder: events.NewFakeRecorder(100),
		Connect: func(context.Context, *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
			return rec, f.Systems[0].ID, "", "", nil
		},
		PowerSettle: 50 * time.Millisecond,
	}
	return r, st, rec
}

func preClearedOp(actions ...metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "pk-metal"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: "host-a"},
			ProposedBy: "system:serviceaccount:pk-metal:pk-metal-manager",
			Actions:    actions,
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(false)},
		},
	}
}

// runOp reconciles until the operation finishes (or the budget runs out) and
// returns its final state.
func runOp(t *testing.T, r *HostOperationReconciler) *metalv1alpha1.HostOperation {
	t.Helper()
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	op := &metalv1alpha1.HostOperation{}
	for range 10 {
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, r.Get(t.Context(), key, op))
		if opFinished(op) {
			return op
		}
	}
	t.Fatalf("operation never finished: %+v", op.Status)
	return nil
}

func TestPowerCycleIsOneObservedCycle(t *testing.T) {
	// The iBMC serves no LastResetTime: the only observable is the power state.
	r, st, rec := executorFor(t, "huawei-2288h-v5",
		preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}}))
	before := st.Resets("1")

	op := runOp(t, r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionForceOff, bmc.ActionOn}, rec.powers, "off, observed, on")
	assert.Equal(t, before+1, st.Resets("1"), "exactly one boot")
	require.NotNil(t, op.Status.OffObserved, "the off half is recorded")
}

func TestResumedPowerCycleDoesNotCycleAgain(t *testing.T) {
	// A pass died after the machine was seen off and turned back on, before the
	// step was recorded. The machine is on; the record says it was seen off.
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Status = resumedStatus(0)
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	before := st.Resets("1")

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Empty(t, rec.powers, "a cycle already half-observed and finished is never issued again")
	assert.Equal(t, before, st.Resets("1"))
}

func TestResumedOperationDoesNotRearmBootOverride(t *testing.T) {
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd", Mode: "UEFI"}},
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	// The override was set (step recorded), the cycle ran and the boot used the
	// override up; the pass died before the power step was recorded.
	op.Status = resumedStatus(1)
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	require.NoError(t, st.SetBootOverride(t.Context(), "1", bmc.BootOverride{Target: "Cd", Enabled: bmc.OverrideOnce}))
	require.NoError(t, st.SetPower(t.Context(), "1", bmc.ActionForceRestart)) // consumes the Once override
	systems, err := st.Systems(t.Context())
	require.NoError(t, err)
	require.Equal(t, bmc.OverrideDisabled, systems[0].BootOverride.Enabled, "precondition: override used up")

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Zero(t, rec.overrides, "an override a later step consumed is not re-armed")
	assert.Empty(t, rec.powers)
}

func TestBootOverrideStepChecksItsGoal(t *testing.T) {
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd"}},
	)
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	// The previous pass PATCHed and died before recording the step.
	require.NoError(t, st.SetBootOverride(t.Context(), "1", bmc.BootOverride{Target: "Cd", Enabled: bmc.OverrideOnce}))

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Zero(t, rec.overrides, "an override already set as wanted is not written again")
}

func TestApplyBiosStagesThePinnedAttributes(t *testing.T) {
	// The live BiosSettings says something else — the executor must not read it.
	settings := &metalv1alpha1.BiosSettings{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec: metalv1alpha1.BiosSettingsSpec{Attributes: map[string]apiextensionsv1.JSON{
			"QuietBoot": rawJSON(t, "Disabled"),
		}},
	}
	op := preClearedOp(metalv1alpha1.HostOperationAction{ApplyBios: &metalv1alpha1.ApplyBiosAction{
		SettingsRef: &metalv1alpha1.Reference{Name: "host-a"},
		Attributes:  map[string]apiextensionsv1.JSON{"QuietBoot": rawJSON(t, "Enabled")},
	}})
	r, st, _ := executorFor(t, "huawei-2288h-v5", op, settings)

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	state, err := st.Bios(t.Context(), "1")
	require.NoError(t, err)
	assert.Equal(t, "Enabled", state.Pending["QuietBoot"], "the approved value is staged, not the live one")
}

func TestStaleTimeoutFailsThePowerStep(t *testing.T) {
	// A BMC whose reported power never moves: the step gives up, it does not
	// loop forever or claim success.
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}})
	r, st, _ := executorFor(t, "huawei-2288h-v5", op)
	st.SetFault(sim.FaultStalePower, true)
	r.PowerTimeout = time.Millisecond

	got := runOp(t, r)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metav1.ConditionFalse, ex.Status)
	assert.Contains(t, ex.Message, "gave up")
}

// resumedStatus is an executing operation whose first n steps are recorded,
// whose baseline saw the machine on, and whose power cycle was seen off.
func resumedStatus(n int) metalv1alpha1.HostOperationStatus {
	now := metav1.Now()
	s := metalv1alpha1.HostOperationStatus{
		Baseline:    &metalv1alpha1.OperationBaseline{PowerState: "On", CapturedAt: &now},
		OffObserved: &metav1.MicroTime{Time: now.Time},
		Conditions: []metav1.Condition{
			{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonApprovalNotRequired, LastTransitionTime: now},
			{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting, LastTransitionTime: now},
		},
	}
	for range n {
		s.Steps = append(s.Steps, metalv1alpha1.OperationStep{Action: "setBootOverride", Phase: "Succeeded", FinishedAt: &now})
	}
	return s
}

func rawJSON(t *testing.T, v any) apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return apiextensionsv1.JSON{Raw: raw}
}
