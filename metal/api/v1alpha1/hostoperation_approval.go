package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Finished reports whether the executor recorded an outcome: success,
// failure or abort. It reads the Executed condition, not the phase: the
// phase is a summary written after the conditions and can lag behind them.
func (op *HostOperation) Finished() bool {
	c := meta.FindStatusCondition(op.Status.Conditions, ConditionExecuted)
	return c != nil && c.Status != metav1.ConditionUnknown
}

// ApprovalRequired reports whether the operation needs a person's approval.
// An unset field is a closed gate: the schema default writes true, but an
// object that was not stored (or built by hand) must land the same way.
func (op *HostOperation) ApprovalRequired() bool {
	return op.Spec.Approval.Required == nil || *op.Spec.Approval.Required
}

// AwaitsApproval reports whether the operation needs a person, nobody has
// approved it, and it has not finished. It does not wait for the status: a
// just-proposed operation has no conditions yet.
func (op *HostOperation) AwaitsApproval() bool {
	return op.ApprovalRequired() && op.Spec.Approval.ApprovedBy == "" && !op.Finished()
}

// GuardAnnotations are the annotations that lift a safety rule for one
// operation, in the order a plan lists them. A person who approves the
// operation must see each one that is set.
var GuardAnnotations = []string{AnnotationOverrideClusterGuard, AnnotationIgnoreWindow}
