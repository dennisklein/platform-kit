package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The executor's side of a rollout's approval (decision M2). A rollout's
// operations are created by the operator with no approval of their own: the
// person approved the rollout's plan instead. That approval means this
// operation only while it binds to it, so before an operation that names a
// rollout starts, the executor checks that the rollout exists, is approved
// for exactly the planHash the operation carries, is not aborted, and plans
// for this node and host exactly the actions the operation holds. Anything
// else fails the operation before it touches the machine.
//
// An operation that already started is not re-checked: the check is what
// lets it start, and stopping between a drain and its uncordon would leave
// the node worse off than finishing.

// +kubebuilder:rbac:groups=os.pk.io,resources=talosrollouts,verbs=get;list;watch

// rolloutRefusal returns why the rollout an operation names does not cover
// it, "" when it does or the operation names no rollout.
func (r *HostOperationReconciler) rolloutRefusal(ctx context.Context, op *metalv1alpha1.HostOperation) (string, error) {
	ref := op.Spec.RolloutRef
	if ref == nil {
		return "", nil
	}
	ro := &osv1alpha1.TalosRollout{}
	if err := r.Get(ctx, client.ObjectKey{Name: ref.Name}, ro); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return "", err
		}
		return "rollout " + ref.Name + " does not exist", nil
	}
	return rolloutCovers(ro, op), nil
}

// rolloutCovers is the check itself, on objects in hand.
func rolloutCovers(ro *osv1alpha1.TalosRollout, op *metalv1alpha1.HostOperation) string {
	ref := op.Spec.RolloutRef
	if ok, why := rolloutApproved(ro); !ok {
		return "rollout " + ro.Name + " does not cover this operation: " + why
	}
	if ref.PlanHash != ro.Status.PlanHash {
		return fmt.Sprintf("this operation was made for plan %s of rollout %s, which was approved for plan %s",
			shortDigest(ref.PlanHash), ro.Name, shortDigest(ro.Status.PlanHash))
	}
	if ro.Spec.Abort {
		return "rollout " + ro.Name + " was aborted before this operation started"
	}
	node := ""
	for _, a := range op.Spec.Actions {
		if n := actionNode(a); n != "" {
			node = n
			break
		}
	}
	i := rolloutStepFor(ro, node)
	if i < 0 {
		return fmt.Sprintf("the approved plan of rollout %s has no step for node %q", ro.Name, node)
	}
	step := ro.Status.Plan[i]
	if step.Host != op.Spec.HostRef.Name {
		return fmt.Sprintf("the approved plan runs node %s on host %s, this operation targets host %s", node, step.Host, op.Spec.HostRef.Name)
	}
	if !sameActions(op.Spec.Actions, rolloutActions(ro, step)) {
		return fmt.Sprintf("this operation's actions for node %s differ from the ones the approved plan of rollout %s gives it", node, ro.Name)
	}
	return ""
}

// actionNode is the TalosNode a Talos action names, "" for a BMC action.
func actionNode(a metalv1alpha1.HostOperationAction) string {
	switch {
	case a.Drain != nil:
		return a.Drain.NodeRef.Name
	case a.Cordon != nil:
		return a.Cordon.NodeRef.Name
	case a.Uncordon != nil:
		return a.Uncordon.NodeRef.Name
	case a.TalosReboot != nil:
		return a.TalosReboot.NodeRef.Name
	case a.TalosUpgrade != nil:
		return a.TalosUpgrade.NodeRef.Name
	case a.TalosApplyConfig != nil:
		return a.TalosApplyConfig.NodeRef.Name
	case a.WaitNodeReady != nil:
		return a.WaitNodeReady.NodeRef.Name
	case a.WaitEtcdHealthy != nil:
		return a.WaitEtcdHealthy.NodeRef.Name
	case a.TalosShutdown != nil:
		return a.TalosShutdown.NodeRef.Name
	case a.TalosReset != nil:
		return a.TalosReset.NodeRef.Name
	case a.EtcdLeave != nil:
		return a.EtcdLeave.NodeRef.Name
	case a.EtcdRemoveMember != nil:
		return a.EtcdRemoveMember.NodeRef.Name
	}
	return ""
}

// gateRollout is the executor's hook: called from Reconcile once the
// approval gate is passed, it fails an operation the rollout it names does not
// cover, before the operation starts. stop says Reconcile returns res, err.
func (r *HostOperationReconciler) gateRollout(ctx context.Context, op *metalv1alpha1.HostOperation,
	setCond func(string, metav1.ConditionStatus, string, string), flush func() error) (res ctrl.Result, stop bool, err error) {
	if op.Spec.RolloutRef == nil || op.Status.Baseline != nil {
		return ctrl.Result{}, false, nil
	}
	why, err := r.rolloutRefusal(ctx, op)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if why == "" {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue, metalv1alpha1.ReasonApproved,
			"covered by the approval of rollout "+op.Spec.RolloutRef.Name)
		return ctrl.Result{}, false, nil
	}
	msg := why + "; nothing was done to the machine"
	key := "rollout"
	if len(op.Spec.Actions) > 0 {
		key = op.Spec.Actions[0].Key()
	}
	op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{
		Action: key, Phase: "Failed", Message: msg, FinishedAt: ptrTime(metav1.Now()),
	})
	setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse, metalv1alpha1.ReasonRolloutRefused, msg)
	if r.Recorder != nil {
		r.Recorder.Eventf(op, nil, corev1.EventTypeWarning, metalv1alpha1.ReasonRolloutRefused, "Execute", "%s", msg)
	}
	return ctrl.Result{}, true, client.IgnoreNotFound(flush())
}
