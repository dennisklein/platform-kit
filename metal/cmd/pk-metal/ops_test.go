package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestRetry(t *testing.T) {
	ctx := context.Background()

	op, objs := bootPlan()
	var out bytes.Buffer
	err := retry(ctx, fakeClient(objs...), op.Namespace, op.Name, &out)
	assert.ErrorContains(t, err, "is AwaitingApproval, not Failed", "only failed operations are retried")

	op.Status.Phase = metalv1alpha1.OperationFailed
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
