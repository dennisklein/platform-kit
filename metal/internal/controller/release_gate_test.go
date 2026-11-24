package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// TestReleaseOfAnInstalledNodeWaitsWhileItIsNotReady: a production node is
// briefly not ready (mid Talos upgrade, a kubelet restart, one failed dial)
// when its claim is deleted. The release still waits for a person: what the
// operator decides from is the record that the node was installed, not its
// readiness at that moment. It used to be pre-cleared, and the machine was
// powered off with nobody asked.
func TestReleaseOfAnInstalledNodeWaitsWhileItIsNotReady(t *testing.T) {
	notReady := []metav1.Condition{{Type: metalv1alpha1.ConditionOSReady, Status: metav1.ConditionUnknown,
		Reason: "Converging", Message: "stage upgrading", LastTransitionTime: metav1.Now()}}

	t.Run("the claim's hand-off names the host", func(t *testing.T) {
		claim := testClaim("node-b")
		claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
		host := boundHost(claim)
		host.Status.Conditions = notReady
		r := claimReconcilerFor(t, host)

		require.False(t, releasePass(t, r, claim))
		rel := releaseOps(t, r, claim)
		require.Len(t, rel, 1)
		assert.True(t, *rel[0].Spec.Approval.Required, "an installed node is released only with a person")
		assert.Contains(t, provisionedCond(t, claim).Message, "pk-metal approve")
	})

	t.Run("the boot recorded its check-in, the hand-off is not written yet", func(t *testing.T) {
		claim := testClaim("node-b")
		boot := bootOp("boot", "node-b", false, true)
		for _, a := range boot.Spec.Actions {
			boot.Status.Steps = append(boot.Status.Steps, metalv1alpha1.OperationStep{Action: actionName(a), Phase: "Succeeded"})
		}
		host := boundHost(claim)
		host.Status.Conditions = notReady
		r := claimReconcilerFor(t, host, boot)

		require.False(t, releasePass(t, r, claim))
		rel := releaseOps(t, r, claim)
		require.Len(t, rel, 1)
		assert.True(t, *rel[0].Spec.Approval.Required)
	})

	t.Run("the claim waived approval", func(t *testing.T) {
		claim := testClaim("node-b")
		claim.Spec.Provisioning = &metalv1alpha1.ClaimProvisioning{Approval: metalv1alpha1.ProvisioningApprovalAutomatic}
		claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
		r := claimReconcilerFor(t, boundHost(claim))

		require.False(t, releasePass(t, r, claim))
		rel := releaseOps(t, r, claim)
		require.Len(t, rel, 1)
		assert.False(t, *rel[0].Spec.Approval.Required)
	})
}

// TestReleaseOfARunningNodeWaitsBeforeItsHandOffIsRecorded: the Host
// reports its node running, but the claim was deleted before the claim
// controller wrote the hand-off record of that check-in, and no boot of the
// claim ran (the node was found running). The release still takes the node
// out of its cluster and waits for a person. It used to unbind the machine as
// it was, with the node still running in its cluster.
func TestReleaseOfARunningNodeWaitsBeforeItsHandOffIsRecorded(t *testing.T) {
	claim := testClaim("node-b")
	host := boundHost(claim)
	host.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionOSReady, Status: metav1.ConditionTrue,
		Reason: "NodeReady", LastTransitionTime: metav1.Now()}}
	r := claimReconcilerFor(t, host)

	require.False(t, releasePass(t, r, claim), "the release waits")
	assert.True(t, hostBound(t, r), "the running node's machine stays bound")
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	assert.True(t, *rel[0].Spec.Approval.Required, "a running node is released only with a person")
	assert.Contains(t, provisionedCond(t, claim).Message, "pk-metal approve")
}
