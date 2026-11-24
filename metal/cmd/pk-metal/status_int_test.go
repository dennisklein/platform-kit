package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// A claim whose install waits for approval explains itself with the command
// from its Provisioned condition: a claim has no ApprovalPending condition.
func TestExplainClaimAwaitingApproval(t *testing.T) {
	conds := []metav1.Condition{
		{Type: metalv1alpha1.ConditionBound, Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonHostBound, Message: "host h"},
		{Type: metalv1alpha1.ConditionProvisioned, Status: metav1.ConditionFalse, Reason: metalv1alpha1.ReasonAwaitingApproval,
			Message: "boot operation pk/op awaits approval. To install: pk-metal approve pk/op"},
	}
	assert.Equal(t, "boot operation pk/op awaits approval. To install: pk-metal approve pk/op",
		explain(conds, "Approval(Provisioning)"))
}
