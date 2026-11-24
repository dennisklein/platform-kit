package metrics

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// RolloutState is where a TalosRollout stands, read from its conditions: the
// one reason that says what it waits for, and the controller's sentence for
// it. The metrics and pk-metal status read it the same way, so that an alert
// and the ROLLOUT table never disagree.
type RolloutState struct {
	// Reason is the reason of the condition that decides, for example
	// AwaitingApproval, PlanBlocked, WaitingForHealth, PausedAfterFirstNode,
	// NodeRunning, RolloutFailed. "NotStarted" before the controller wrote
	// anything.
	Reason  string
	Message string
	// Succeeded is true when the rollout ended well (every step succeeded,
	// or no node needed the change).
	Succeeded bool
	// Ended is true when nothing more runs: succeeded, failed, aborted, or
	// its approval does not cover the plan.
	Ended bool
}

// RolloutStateOf picks the deciding condition. The order is the order in
// which they hold a rollout up: an ended rollout reports how it ended; a plan
// that cannot be made, then a missing approval, come before any progress; a
// pause comes before the progress it stops.
func RolloutStateOf(ro *osv1alpha1.TalosRollout) RolloutState {
	conds := ro.Status.Conditions
	is := func(t string, s metav1.ConditionStatus) *metav1.Condition {
		if c := meta.FindStatusCondition(conds, t); c != nil && c.Status == s {
			return c
		}
		return nil
	}
	if c := is(osv1alpha1.ConditionComplete, metav1.ConditionTrue); c != nil {
		return RolloutState{Reason: c.Reason, Message: c.Message, Succeeded: true, Ended: true}
	}
	if c := is(osv1alpha1.ConditionComplete, metav1.ConditionFalse); c != nil {
		return RolloutState{Reason: c.Reason, Message: c.Message, Ended: true}
	}
	for _, t := range []string{osv1alpha1.ConditionPlanned, osv1alpha1.ConditionApproved} {
		if c := is(t, metav1.ConditionFalse); c != nil {
			return RolloutState{Reason: c.Reason, Message: c.Message}
		}
	}
	if c := is(osv1alpha1.ConditionPaused, metav1.ConditionTrue); c != nil {
		return RolloutState{Reason: c.Reason, Message: c.Message}
	}
	if c := meta.FindStatusCondition(conds, osv1alpha1.ConditionProgressing); c != nil && c.Reason != "" {
		return RolloutState{Reason: c.Reason, Message: c.Message}
	}
	return RolloutState{Reason: osv1alpha1.ReasonRolloutNotStarted}
}
