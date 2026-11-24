package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// tickTalosClock makes every reading of the Talos steps' clock step forward,
// so a test sees time pass between two looks without waiting for it.
func tickTalosClock(t *testing.T, step time.Duration) {
	t.Helper()
	var mu sync.Mutex
	now := time.Now()
	prev := talosClock
	talosClock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(step)
		return now
	}
	t.Cleanup(func() { talosClock = prev })
}

func waiting(t *testing.T, op *metalv1alpha1.HostOperation, contains string) {
	t.Helper()
	assert.Equal(t, metav1.ConditionUnknown, executed(op).Status, "the step still waits: %s", executed(op).Message)
	assert.Contains(t, executed(op).Message, contains)
}

func resetOp(node string) *metalv1alpha1.HostOperation {
	return talosOp(node, metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
		NodeRef: ref(node), Graceful: ptr.To(false), Mode: "SystemDisk",
		// STATE stays, so the fake keeps answering to the cluster's
		// credentials once it is reachable again.
		SystemPartitions: []metalv1alpha1.SystemPartition{"EPHEMERAL"}}})
}

func TestTalosResetIsNotDoneOnOneSilentLook(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, resetOp("w-1"))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Resets(), 1)
	// The node still runs its reset: it answers, but one call is slow.
	m.SetUnreachable(false)
	m.SetFailure("BootTime", context.DeadlineExceeded)
	waiting(t, lab.pass(t), "has not answered since")

	m.SetFailure("BootTime", nil)
	waiting(t, lab.pass(t), "still answers")

	m.SetUnreachable(true)
	waiting(t, lab.pass(t), "has not answered since")
	waiting(t, lab.pass(t), "has not answered since")
	op := lab.pass(t)
	require.True(t, opSucceeded(op), "three silent looks over 40s end the step: %+v", op.Status)
	assert.Len(t, m.Resets(), 1, "a reset the node accepted is not sent again within the resend window")
}

func TestTalosResetLostAnswerIsAskedAgain(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, resetOp("w-1"))
	m := lab.machines["w-1"]
	m.SetFailure("Reset", status.Error(codes.Unavailable, "connection reset by peer"))

	op := lab.pass(t)
	waiting(t, op, "did not answer the request")
	require.NotNil(t, op.Status.TalosStep)
	assert.Nil(t, op.Status.TalosStep.RequestedAt, "a request without an answer is not recorded as accepted")
	assert.NotNil(t, op.Status.TalosStep.BootTime)
	assert.Empty(t, m.Resets())

	m.SetFailure("Reset", nil)
	lab.pass(t)
	assert.Len(t, m.Resets(), 1, "the node answers with its old boot time: the reset is sent again")
	op = runOp(t, lab.r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Len(t, m.Resets(), 1)
}

func TestTalosResetDoneWhenTheNodeServesAnotherCA(t *testing.T) {
	lab := newTalosLab(t, resetOp("w-1"))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Resets(), 1)
	m.SetUnreachable(false)
	m.SetFailure("BootTime", status.Error(codes.Unavailable, `connection error: desc = "transport: authentication `+
		`handshake failed: tls: failed to verify certificate: x509: certificate signed by unknown authority"`))
	op := lab.pass(t)
	assert.True(t, opSucceeded(op), "a node in maintenance mode is wiped: one look is enough: %+v", op.Status)
}

func TestTalosShutdownIsNotDoneOnOneSilentLook(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{TalosShutdown: &metalv1alpha1.TalosShutdownAction{NodeRef: ref("w-1")}}))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Shutdowns(), 1)
	m.SetUnreachable(false)
	m.SetFailure("Version", context.DeadlineExceeded)
	waiting(t, lab.pass(t), "has not answered since")
	m.SetFailure("Version", nil)
	waiting(t, lab.pass(t), "still answers")
	assert.Len(t, m.Shutdowns(), 1, "an accepted shutdown is not sent again: Talos would start it over")

	m.SetUnreachable(true)
	op := runOp(t, lab.r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
}

func TestRestartedNeedsMoreThanAClockStep(t *testing.T) {
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.False(t, restarted(before, before.Add(5*time.Second)), "a clock step on the node is no restart")
	assert.False(t, restarted(before, before.Add(-time.Hour)), "an earlier boot time is no restart")
	assert.True(t, restarted(before, before.Add(10*time.Minute)))
}

func TestTalosApplyConfigConfirmsATry(t *testing.T) {
	cfg := []byte("version: v1alpha1\nmachine: {type: worker}\n")
	secret := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "machineconfig-w-1"},
			Data: map[string][]byte{asset.ConfigFilename: cfg}}
	}
	try := metalv1alpha1.HostOperationAction{TalosApplyConfig: &metalv1alpha1.TalosApplyConfigAction{
		NodeRef: ref("w-1"), ConfigSecretRef: ref("machineconfig-w-1"), Digest: godigest.FromBytes(cfg).String(),
		Mode: "Try", TryTimeoutSeconds: 30}}

	t.Run("a ready node is confirmed with the same bytes", func(t *testing.T) {
		lab := newTalosLab(t, secret(), talosOp("w-1", try))
		op := runOp(t, lab.r)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		calls := lab.machines["w-1"].ApplyCalls()
		require.Len(t, calls, 2, "the try, then its confirmation")
		assert.Equal(t, talos.ApplyTry, calls[0].Mode)
		assert.Equal(t, talos.ApplyNoReboot, calls[1].Mode, "only an apply that is not a try cancels the rollback")
		applied := lab.machines["w-1"].Applied()
		assert.Equal(t, cfg, applied[len(applied)-1], "the confirmation sends the approved bytes")
	})

	t.Run("a node not ready within the window is left to roll back", func(t *testing.T) {
		tickTalosClock(t, 20*time.Second)
		lab := newTalosLab(t, secret(), talosOp("w-1", try))
		lab.machines["w-1"].SetStage("running", false, "nodeReady")
		op := lab.pass(t)
		waiting(t, op, "not ready yet")
		op = runOp(t, lab.r)
		assert.Equal(t, metav1.ConditionFalse, executed(op).Status)
		assert.Contains(t, executed(op).Message, "Try window")
		assert.Len(t, lab.machines["w-1"].ApplyCalls(), 1, "nothing confirmed the try")
	})
}

func TestTalosResetNamesTheUserDisks(t *testing.T) {
	tickTalosClock(t, 20*time.Second)
	lab := newTalosLab(t, talosOp("w-1", metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
		NodeRef: ref("w-1"), Graceful: ptr.To(false), Mode: "UserDisks", UserDisks: []string{"/dev/sdb", "/dev/sdc"}}}))
	m := lab.machines["w-1"]

	lab.pass(t)
	require.Len(t, m.Resets(), 1)
	assert.Equal(t, talos.WipeUserDisks, m.Resets()[0].Mode)
	assert.Equal(t, []string{"/dev/sdb", "/dev/sdc"}, m.Resets()[0].UserDisks, "Talos wipes only the user disks the reset names")
}
