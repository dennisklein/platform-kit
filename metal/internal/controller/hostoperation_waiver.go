package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The executor's check of an operation that needs no approval. Admission
// lets the operator's own identity create one (and a member of the admin
// group, whom admission names in spec.proposedBy). The operator code does
// so for three kinds of work only, so an operation proposed in the
// operator's name with any other plan was not made by that code: someone
// holds the operator's token. The executor refuses it before it acts.
// The check is a second line behind admission; it does not stop a person
// with the token who copies the shape of a waived kind.

// waiverRefusal returns why the operator may not run op without an
// approval, "" when it may (or op needs an approval, or a person proposed
// it).
func (r *HostOperationReconciler) waiverRefusal(ctx context.Context, op *metalv1alpha1.HostOperation) (string, error) {
	if op.ApprovalRequired() || r.Operator == "" || op.Spec.ProposedBy != r.Operator {
		return "", nil
	}
	switch {
	case op.Spec.RolloutRef != nil:
		// The rollout's approval covers it; gateRollout checks that.
		return "", nil
	case op.Spec.Reason == reasonRelease:
		// A release that needs no person: the machine runs no node the
		// operator could stop (releaseNeedsPerson).
		if p := op.Labels[labelPurpose]; p == purposeRelease || p == purposeRemove {
			return "", nil
		}
		return "a release without approval must carry the purpose label of a release", nil
	case op.Spec.Reason == reasonProvisioning:
		// An install waives approval only while its claim says Automatic.
		claim := &metalv1alpha1.HostClaim{}
		key := client.ObjectKey{Namespace: op.Labels[labelClaimNamespace], Name: op.Labels[labelClaimName]}
		if key.Name == "" {
			return "an install without approval must name its claim", nil
		}
		if err := r.Get(ctx, key, claim); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return "", err
			}
			return "the claim " + key.String() + " of this install does not exist", nil
		}
		if string(claim.UID) != op.Labels[labelClaimUID] || !approvalWaived(claim) {
			return "the claim " + key.String() + " does not set provisioning.approval: Automatic", nil
		}
		return "", nil
	}
	return "the operator runs an operation without approval only for a rollout step, " +
		"the install of a claim set to Automatic, or a release; this one is none of them (reason \"" +
		op.Spec.Reason + "\")", nil
}

// gateWaiver fails an operation that waiverRefusal refuses, before it
// starts. stop says Reconcile returns res, err.
func (r *HostOperationReconciler) gateWaiver(ctx context.Context, op *metalv1alpha1.HostOperation,
	setCond func(string, metav1.ConditionStatus, string, string), flush func() error) (res ctrl.Result, stop bool, err error) {
	if op.Status.Baseline != nil {
		return ctrl.Result{}, false, nil
	}
	why, err := r.waiverRefusal(ctx, op)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if why == "" {
		return ctrl.Result{}, false, nil
	}
	msg := why + "; nothing was done to the machine"
	key := "operation"
	if len(op.Spec.Actions) > 0 {
		key = op.Spec.Actions[0].Key()
	}
	op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{
		Action: key, Phase: "Failed", Message: msg, FinishedAt: ptrTime(metav1.Now()),
	})
	setCond(metalv1alpha1.ConditionApproved, metav1.ConditionFalse, metalv1alpha1.ReasonWaiverRefused, msg)
	setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse, metalv1alpha1.ReasonWaiverRefused, msg)
	if r.Recorder != nil {
		r.Recorder.Eventf(op, nil, corev1.EventTypeWarning, metalv1alpha1.ReasonWaiverRefused, "Execute", "%s", msg)
	}
	return ctrl.Result{}, true, client.IgnoreNotFound(flush())
}
