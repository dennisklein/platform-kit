package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestHostOperationApprovalState(t *testing.T) {
	executed := func(s metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: ConditionExecuted, Status: s}}
	}
	for _, tc := range []struct {
		name                     string
		required                 *bool
		approvedBy               string
		conds                    []metav1.Condition
		finished, needed, awaits bool
	}{
		{name: "unset gate is closed", needed: true, awaits: true},
		{name: "required, not approved", required: ptr.To(true), needed: true, awaits: true},
		{name: "approved", required: ptr.To(true), approvedBy: "bob", needed: true},
		{name: "no approval needed", required: ptr.To(false)},
		{name: "running", conds: executed(metav1.ConditionUnknown), needed: true, awaits: true},
		{name: "succeeded", conds: executed(metav1.ConditionTrue), finished: true, needed: true},
		{name: "failed", conds: executed(metav1.ConditionFalse), finished: true, needed: true},
	} {
		op := &HostOperation{
			Spec:   HostOperationSpec{Approval: OperationApproval{Required: tc.required, ApprovedBy: tc.approvedBy}},
			Status: HostOperationStatus{Conditions: tc.conds},
		}
		assert.Equal(t, tc.finished, op.Finished(), tc.name)
		assert.Equal(t, tc.needed, op.ApprovalRequired(), tc.name)
		assert.Equal(t, tc.awaits, op.AwaitsApproval(), tc.name)
	}
}
