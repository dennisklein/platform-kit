package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	talosfake "github.com/dennisklein/platform-kit/metal/internal/talos/fake"
)

// guardCheck asks the lab's guard about a reboot of node.
func guardCheck(t *testing.T, lab *talosLab, node string, mut ...func(*metalv1alpha1.HostOperation)) string {
	t.Helper()
	host := &metalv1alpha1.Host{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "host-" + node}, host))
	op := talosOp(node, rebootPlan(node)...)
	op.UID = "this-op"
	for _, m := range mut {
		m(op)
	}
	msg, err := lab.r.Guard.Check(t.Context(), host, op)
	require.NoError(t, err)
	return msg
}

func TestClusterGuardAllowsAHealthyMajority(t *testing.T) {
	lab := newTalosLab(t)
	assert.Empty(t, guardCheck(t, lab, "cp-2"), "two healthy members of three are left")
	assert.Empty(t, guardCheck(t, lab, "w-1"), "a worker holds no etcd member")
}

// TestClusterGuardOneControlPlaneAtATime: approving operations on two
// control planes at once must not take both down.
func TestClusterGuardOneControlPlaneAtATime(t *testing.T) {
	running := talosOp("cp-1", rebootPlan("cp-1")...)
	running.Name, running.UID = "reboot-cp-1", "other-op"
	running.Status = resumedStatus(0)
	lab := newTalosLab(t, running)

	msg := guardCheck(t, lab, "cp-2")
	assert.Contains(t, msg, "operation pk-metal/reboot-cp-1 is taking down control plane cp-1")
	assert.Contains(t, msg, "one at a time")

	override := func(op *metalv1alpha1.HostOperation) {
		op.Annotations = map[string]string{metalv1alpha1.AnnotationOverrideClusterGuard: "repair"}
	}
	assert.NotEmpty(t, guardCheck(t, lab, "cp-2", override), "the override does not lift one-at-a-time")
	assert.Empty(t, guardCheck(t, lab, "w-1"), "workers are not held by a control plane's operation")
}

func TestClusterGuardCountsFromEtcd(t *testing.T) {
	t.Run("a member that does not answer", func(t *testing.T) {
		lab := newTalosLab(t)
		lab.machines["cp-3"].SetUnreachable(true) // in POST after a crash; its TalosNode still looks fine
		msg := guardCheck(t, lab, "cp-2")
		assert.Contains(t, msg, "1 healthy voting member(s) of 3, and needs 2")
		assert.Contains(t, msg, "member cp-3")
		assert.Contains(t, msg, "does not answer")
		assert.Empty(t, guardCheck(t, lab, "cp-3"), "the sick member itself may be rebooted: the other two hold the majority")
	})

	t.Run("a stale member no node runs", func(t *testing.T) {
		lab := newTalosLab(t)
		lab.etcd.AddStale("cp-old")
		// Four members: without cp-2, two healthy of four is not a majority.
		msg := guardCheck(t, lab, "cp-2")
		assert.Contains(t, msg, "2 healthy voting member(s) of 4, and needs 3")
		assert.Contains(t, msg, "member cp-old")
	})

	t.Run("a learner", func(t *testing.T) {
		lab := newTalosLab(t)
		lab.etcd = talosfake.NewEtcdCluster()
		lab.machines["cp-1"].JoinEtcd(lab.etcd, false)
		lab.machines["cp-2"].JoinEtcd(lab.etcd, false)
		lab.machines["cp-3"].JoinEtcd(lab.etcd, true)
		assert.Contains(t, guardCheck(t, lab, "cp-1"), "cp-3 (")
		assert.Contains(t, guardCheck(t, lab, "cp-1"), "learner")
	})

	t.Run("an alarm or an error", func(t *testing.T) {
		lab := newTalosLab(t)
		lab.machines["cp-1"].SetEtcdAlarms(talos.EtcdAlarm{MemberID: 7, Alarm: "NOSPACE"})
		assert.Contains(t, guardCheck(t, lab, "cp-2"), "alarm NOSPACE")
		lab.machines["cp-1"].SetEtcdAlarms()
		lab.machines["cp-3"].SetEtcdErrors("slow disk")
		assert.Contains(t, guardCheck(t, lab, "cp-2"), "reports slow disk")
	})

	t.Run("no control plane answers", func(t *testing.T) {
		lab := newTalosLab(t)
		for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
			lab.machines[n].SetUnreachable(true)
		}
		msg := guardCheck(t, lab, "cp-2")
		assert.Contains(t, msg, "etcd cannot be asked")
		assert.Contains(t, msg, metalv1alpha1.AnnotationOverrideClusterGuard, "the way out is named")
	})

	t.Run("the override lifts the etcd rule", func(t *testing.T) {
		lab := newTalosLab(t)
		lab.machines["cp-3"].SetUnreachable(true)
		assert.Empty(t, guardCheck(t, lab, "cp-2", func(op *metalv1alpha1.HostOperation) {
			op.Annotations = map[string]string{metalv1alpha1.AnnotationOverrideClusterGuard: "etcd is down already"}
		}))
	})
}

func TestClusterGuardSingleControlPlane(t *testing.T) {
	lab := newTalosLab(t)
	lab.etcd = talosfake.NewEtcdCluster()
	lab.machines["cp-1"].JoinEtcd(lab.etcd, false)
	lab.machines["cp-2"].SetEtcdRunning(false)
	lab.machines["cp-3"].SetEtcdRunning(false)
	msg := guardCheck(t, lab, "cp-1")
	assert.Contains(t, msg, "0 healthy voting member(s) of 1, and needs 1")
	assert.Contains(t, msg, metalv1alpha1.AnnotationOverrideClusterGuard)
}

func TestClusterGuardLeavesDayZeroAlone(t *testing.T) {
	lab := newTalosLab(t)
	cluster := &osv1alpha1.TalosCluster{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "lab"}, cluster))
	cluster.Status.BootstrappedAt = nil
	require.NoError(t, lab.r.Update(t.Context(), cluster))
	for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
		lab.machines[n].SetUnreachable(true)
	}
	assert.Empty(t, guardCheck(t, lab, "cp-2"), "before bootstrap every control plane is installed at once")
}

// TestExecutorHoldsAControlPlaneReboot: the executor asks the guard before
// it starts, and holds the operation as ClusterDisruption without touching
// the node.
func TestExecutorHoldsAControlPlaneReboot(t *testing.T) {
	lab := newTalosLab(t, talosOp("cp-2", rebootPlan("cp-2")...))
	lab.machines["cp-3"].SetUnreachable(true)

	op := lab.pass(t)
	ex := executed(op)
	assert.Equal(t, metav1.ConditionUnknown, ex.Status)
	assert.Equal(t, metalv1alpha1.ReasonClusterDisruption, ex.Reason)
	assert.Equal(t, metalv1alpha1.ReasonClusterDisruption, op.Status.BlockedOn)
	assert.Nil(t, op.Status.Baseline, "not started")
	assert.False(t, lab.unschedulable(t, "cp-2"), "nothing was done, not even the cordon")

	lab.machines["cp-3"].SetUnreachable(false)
	got := runOp(t, lab.r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Len(t, lab.machines["cp-2"].Reboots(), 1)
}

// TestExecutorAsksTheGuardAgainBeforeTheReboot: etcd lost a member while the
// drain ran; the reboot waits for it, and the node stays cordoned meanwhile.
func TestExecutorAsksTheGuardAgainBeforeTheReboot(t *testing.T) {
	lab := newTalosLab(t, talosOp("cp-2", rebootPlan("cp-2")...))
	require.NoError(t, lab.kube.Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps"},
		Spec:       corev1.PodSpec{NodeName: "cp-2"},
	}))

	op := lab.pass(t) // started, cordoned, evicted; the drain waits for the pod to go
	require.NotNil(t, op.Status.Baseline)
	assert.Contains(t, executed(op).Message, "drain")

	lab.machines["cp-3"].SetUnreachable(true)
	op = lab.pass(t)
	ex := executed(op)
	assert.Equal(t, metalv1alpha1.ReasonExecuting, ex.Reason, "a running operation stays Running")
	assert.Contains(t, ex.Message, "step 3/5 talosReboot: held:")
	assert.Contains(t, ex.Message, "member cp-3")
	assert.Empty(t, lab.machines["cp-2"].Reboots())
	assert.True(t, lab.unschedulable(t, "cp-2"))

	lab.machines["cp-3"].SetUnreachable(false)
	got := runOp(t, lab.r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Len(t, lab.machines["cp-2"].Reboots(), 1)
}
