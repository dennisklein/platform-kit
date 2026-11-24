package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestRetry(t *testing.T) {
	ctx := context.Background()

	op, objs := bootPlan()
	var out bytes.Buffer
	err := retry(ctx, fakeClient(objs...), op.Namespace, op.Name, &out)
	assert.ErrorContains(t, err, "is AwaitingApproval, not Failed", "only failed operations are retried")

	stale := op.DeepCopy()
	stale.Status.Phase = metalv1alpha1.OperationFailed
	err = retry(ctx, fakeClient(append([]client.Object{stale}, objs[1:]...)...), op.Namespace, op.Name, &out)
	assert.ErrorContains(t, err, "not Failed", "a stale Failed phase without a failed outcome is not retried")

	op.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
		Reason: metalv1alpha1.ReasonStepFailed, Message: "step 7/9 awaitCheckIn: gave up after 30m0s"}}
	claim := &metalv1alpha1.HostClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "node-1", Annotations: map[string]string{holdAnnotation: ""}},
	}
	cl := fakeClient(append(objs, claim)...)
	out.Reset()
	require.NoError(t, retry(ctx, cl, op.Namespace, op.Name, &out))
	assert.Equal(t, `operation pk-metal/provision-node1-1a2b3c4d failed: step 7/9 awaitCheckIn: gave up after 30m0s
deleted pk-metal/provision-node1-1a2b3c4d.
The operator proposes it again for claim default/node-1 in a few seconds; it then waits for approval: pk-metal approve
Claim default/node-1 is on hold, so nothing is proposed until: pk-metal unhold default/node-1
`, out.String())
	err = cl.Get(ctx, client.ObjectKeyFromObject(op), &metalv1alpha1.HostOperation{})
	assert.True(t, apierrors.IsNotFound(err), "the failed operation is gone")

	err = retry(ctx, cl, "default", op.Name, &out)
	assert.ErrorContains(t, err, "no operation provision-node1-1a2b3c4d in namespace default")
}

func TestHold(t *testing.T) {
	ctx := context.Background()
	claim := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "node-1"}}
	cl := fakeClient(claim)
	annotations := func() map[string]string {
		got := &metalv1alpha1.HostClaim{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(claim), got))
		return got.Annotations
	}

	var out bytes.Buffer
	require.NoError(t, hold(ctx, cl, "default", "node-1", "disk swap", true, &out))
	assert.Equal(t, map[string]string{holdAnnotation: "disk swap"}, annotations())
	assert.Contains(t, out.String(), "pk-metal unhold default/node-1")

	out.Reset()
	require.NoError(t, hold(ctx, cl, "default", "node-1", "", false, &out))
	assert.NotContains(t, annotations(), holdAnnotation)
	assert.Contains(t, out.String(), "no longer on hold")

	out.Reset()
	require.NoError(t, hold(ctx, cl, "default", "node-1", "", false, &out))
	assert.Equal(t, "claim default/node-1 is not on hold; nothing changed\n", out.String())

	err := hold(ctx, cl, "pk-metal", "node-1", "", true, &out)
	assert.ErrorContains(t, err, "it is in namespace default: use default/node-1")
}

// A failed BIOS proposal is marked, not deleted: deleting a proposal is how a
// person refuses it, and the operator would never propose it again.
func TestRetryMarksAConvergenceProposal(t *testing.T) {
	ctx := context.Background()
	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "box-1-bios-0123abcd"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef: metalv1alpha1.Reference{Name: "box-1"}, Reason: metalv1alpha1.OperationReasonBiosConvergence,
			Actions: []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
		},
		Status: metalv1alpha1.HostOperationStatus{Conditions: []metav1.Condition{{
			Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
			Reason: metalv1alpha1.ReasonStepFailed, Message: "step 1/2 applyBios: refused",
		}}},
	}
	cl := fakeClient(op)
	var out bytes.Buffer
	require.NoError(t, retry(ctx, cl, op.Namespace, op.Name, &out))

	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(op), got), "the operator, not the CLI, replaces it")
	assert.Contains(t, got.Annotations, metalv1alpha1.AnnotationRetry)
	assert.Contains(t, out.String(), "marked pk-metal/box-1-bios-0123abcd for retry")
	assert.Contains(t, out.String(), "new proposal of the same plan for host box-1")
}

// TestRetryNamesHowItComesBack: an operation that no claim stands behind is
// not proposed again by the operator unless it proposed it. retry says what
// brings the same plan back.
func TestRetryNamesHowItComesBack(t *testing.T) {
	ctx := context.Background()
	failedAt := func(step string) metalv1alpha1.HostOperationStatus {
		return metalv1alpha1.HostOperationStatus{
			Steps: []metalv1alpha1.OperationStep{{Action: "cordon", Phase: "Succeeded"}, {Action: step, Phase: "Failed"}},
			Conditions: []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
				Reason: metalv1alpha1.ReasonStepFailed, Message: "step 2/5 " + step + ": gave up"}},
		}
	}
	reboot := func(step string) *metalv1alpha1.HostOperation {
		return &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "reboot-w-1-x7k2p"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef: metalv1alpha1.Reference{Name: "pk0004"}, Reason: "Reboot", ProposedBy: "alice",
				Actions: rebootPlan("w-1", osv1alpha1.RoleWorker, rebootOptions{DrainTimeout: 15 * time.Minute}),
			},
			Status: failedAt(step),
		}
	}
	for _, tc := range []struct {
		name string
		op   func() *metalv1alpha1.HostOperation
		want string
	}{
		{name: "a reboot whose drain stalled", op: func() *metalv1alpha1.HostOperation { return reboot("drain") },
			want: "Propose it again: pk-metal reboot --drain-timeout 30m w-1 (the drain did not finish"},
		{name: "a reboot that failed elsewhere", op: func() *metalv1alpha1.HostOperation { return reboot("waitNodeReady") },
			want: "Nothing proposes a reboot again. Propose it again: pk-metal reboot w-1\n"},
		{name: "a restore step", op: func() *metalv1alpha1.HostOperation {
			op := reboot("etcdRecover")
			op.Spec.Reason, op.Labels = restoreReason, map[string]string{labelRestoreCluster: "lab"}
			return op
		}, want: "pk-metal restore plan lab --propose"},
		{name: "a rollout step", op: func() *metalv1alpha1.HostOperation {
			op := reboot("drain")
			op.Spec.RolloutRef = &metalv1alpha1.RolloutReference{Name: "lab-reboot-1"}
			return op
		}, want: "It was a step of rollout lab-reboot-1, which does not propose it again"},
		{name: "the operator's own proposal", op: func() *metalv1alpha1.HostOperation {
			op := reboot("drain")
			op.Spec.Reason, op.Spec.ProposedBy = metalv1alpha1.OperationReasonWipe, operator
			return op
		}, want: "The operator proposed it, so it proposes it again in a few seconds"},
		{name: "anything else a person made", op: func() *metalv1alpha1.HostOperation {
			op := reboot("drain")
			op.Spec.Reason = "Custom"
			return op
		}, want: "alice created it, so nothing proposes it again"},
	} {
		op := tc.op()
		cl := fakeClient(op)
		var out bytes.Buffer
		require.NoError(t, retry(ctx, cl, op.Namespace, op.Name, &out), tc.name)
		assert.Contains(t, out.String(), "deleted pk-metal/reboot-w-1-x7k2p.\n", tc.name)
		assert.Contains(t, out.String(), tc.want, tc.name)
	}
}
