package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// The audit trail (internal/audit) gets one line per decision. Each line is
// written where the reconciler sees the decision as a change of the status
// it just wrote: the status before the write and after it are compared, and
// only a write that succeeded counts. A pass that the API server refuses
// (a conflict with a newer version, read from a stale cache) writes no
// line, and the pass that follows sees the transition once more and writes
// it then. So a transition gets its line once, however often the object is
// reconciled. A delete gets its line before the delete request.

func opObject(op *metalv1alpha1.HostOperation) audit.Object {
	return audit.Object{Kind: audit.KindHostOperation, Namespace: op.Namespace, Name: op.Name, UID: op.UID}
}

func endpointObject(ep *metalv1alpha1.ManagementEndpoint) audit.Object {
	return audit.Object{Kind: audit.KindManagementEndpoint, Name: ep.Name, UID: ep.UID}
}

func rolloutObject(ro *osv1alpha1.TalosRollout) audit.Object {
	return audit.Object{Kind: audit.KindTalosRollout, Name: ro.Name, UID: ro.UID}
}

// opPlan is what an operation does, for a reader of the trail: its reason
// and its action keys, in order.
func opPlan(op *metalv1alpha1.HostOperation) string {
	keys := make([]string, len(op.Spec.Actions))
	for i, a := range op.Spec.Actions {
		keys[i] = actionName(a)
	}
	plan := strings.Join(keys, ", ")
	if op.Spec.Reason != "" {
		plan = op.Spec.Reason + ": " + plan
	}
	return plan
}

// rolloutPlan is what a rollout does: its kind, its target and its steps.
func rolloutPlan(ro *osv1alpha1.TalosRollout) string {
	what := string(ro.Spec.Kind)
	if t := ro.Spec.Target; t != nil {
		if v := t.TalosVersion + t.KubernetesVersion; v != "" {
			what += " to " + v
		}
	}
	nodes := make([]string, len(ro.Status.Plan))
	for i, st := range ro.Status.Plan {
		nodes[i] = stepName(st)
	}
	return fmt.Sprintf("%s, %d steps: %s", what, len(nodes), strings.Join(nodes, ", "))
}

// approvedByPerson: the Approved condition says a person approved.
func approvedByPerson(conds []metav1.Condition, reason string) bool {
	c := meta.FindStatusCondition(conds, metalv1alpha1.ConditionApproved)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason
}

// auditOperation writes the lines for what changed between the status
// before and after one successful status write of op. host is the
// operation's Host when the pass read it, else nil.
func (r *HostOperationReconciler) auditOperation(ctx context.Context, before, after *metalv1alpha1.HostOperation,
	host *metalv1alpha1.Host) {
	l := r.Audit
	if l == nil {
		return
	}
	o := opObject(after)
	ref := after.Namespace + "/" + after.Name
	if len(before.Status.Conditions) == 0 && len(after.Status.Conditions) > 0 {
		l.Proposed(o, after.Spec.ProposedBy, after.Spec.HostRef.Name, opPlan(after), after.ApprovalRequired())
		r.auditAcks(ctx, after)
	}
	if !approvedByPerson(before.Status.Conditions, metalv1alpha1.ReasonApproved) &&
		approvedByPerson(after.Status.Conditions, metalv1alpha1.ReasonApproved) {
		l.Approved(o, after.Spec.ProposedBy, after.Spec.Approval.ApprovedBy, "")
	}
	actions := after.Spec.Actions
	started := func(i int) {
		if i >= len(actions) || after.Finished() {
			return
		}
		l.StepStarted(o, i, actionName(actions[i]))
		// The cluster guard is asked again before every step that takes
		// the node down: its override is a decision for each of them.
		if i > 0 && disruptsAction(actions[i]) {
			if _, ok := guardOverride(after, metalv1alpha1.AnnotationOverrideClusterGuard); ok {
				l.GuardLiftedAtStep(o, metalv1alpha1.AnnotationOverrideClusterGuard,
					after.Annotations[metalv1alpha1.AnnotationOverrideClusterGuard], i, actionName(actions[i]))
			}
		}
	}
	if before.Status.Baseline == nil && after.Status.Baseline != nil {
		// The start is where the window and the guards were decided.
		for _, key := range metalv1alpha1.GuardAnnotations {
			if _, ok := guardOverride(after, key); ok {
				l.GuardLifted(o, key, after.Annotations[key], "")
			}
		}
		if v, ok := hostAnnotation(host, metalv1alpha1.AnnotationAllowSelfDisruption); ok && disrupts(after) {
			l.GuardLifted(audit.Object{Kind: audit.KindHost, Name: host.Name, UID: host.UID},
				metalv1alpha1.AnnotationAllowSelfDisruption, v, ref)
		}
		started(recordedSteps(after))
	}
	for j := len(before.Status.Steps); j < len(after.Status.Steps); j++ {
		s := after.Status.Steps[j]
		switch s.Phase {
		case "Succeeded":
			l.StepFinished(o, j, s.Action, s.Phase)
			started(j + 1)
		case "Failed":
			reason := ""
			if c := meta.FindStatusCondition(after.Status.Conditions, metalv1alpha1.ConditionExecuted); c != nil {
				reason = c.Reason
			}
			l.StepFailed(o, j, s.Action, reason, s.Message)
		}
	}
	if !before.Finished() && after.Finished() {
		c := meta.FindStatusCondition(after.Status.Conditions, metalv1alpha1.ConditionExecuted)
		var ran time.Duration
		if b := after.Status.Baseline; b != nil && b.CapturedAt != nil {
			ran = c.LastTransitionTime.Sub(b.CapturedAt.Time)
		}
		// The phase is derived from the conditions, as the phase package
		// stamps it; a reconciler does not read status.phase.
		ph, _ := phase.ForOperation(after.Status.Conditions)
		l.Finished(o, string(ph), c.Reason, ran)
	}
}

// auditAcks writes a line for each etcd-risk acknowledgement in effect when
// an operation that changes etcd was proposed: on the claim it serves, and
// on the nodes it acts on. The acknowledgement is what let the operator
// propose a removal that leaves etcd even or without a majority.
func (r *HostOperationReconciler) auditAcks(ctx context.Context, op *metalv1alpha1.HostOperation) {
	if !changesEtcd(op) {
		return
	}
	ref := op.Namespace + "/" + op.Name
	const key = metalv1alpha1.AnnotationAckEtcdRisk
	if name := op.Labels[labelClaimName]; name != "" {
		claim := &metalv1alpha1.HostClaim{}
		err := r.Get(ctx, client.ObjectKey{Namespace: op.Labels[labelClaimNamespace], Name: name}, claim)
		if v, ok := claim.Annotations[key]; err == nil && ok {
			r.Audit.GuardLifted(audit.Object{Kind: audit.KindHostClaim, Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
				key, v, ref)
		}
	}
	for _, name := range opNodes(op) {
		node := &osv1alpha1.TalosNode{}
		err := r.Get(ctx, client.ObjectKey{Name: name}, node)
		if v, ok := node.Annotations[key]; err == nil && ok {
			r.Audit.GuardLifted(audit.Object{Kind: audit.KindTalosNode, Name: node.Name, UID: node.UID}, key, v, ref)
		}
	}
}

// rolloutPhase is how a finished rollout ended, in the words of an
// operation's phase.
func rolloutPhase(c *metav1.Condition) string {
	switch {
	case c.Status == metav1.ConditionTrue:
		return string(metalv1alpha1.OperationSucceeded)
	case c.Reason == osv1alpha1.ReasonRolloutAborted:
		return string(metalv1alpha1.OperationAborted)
	}
	return string(metalv1alpha1.OperationFailed)
}

// auditRollout writes the lines for what changed between the status before
// and after one successful status write of ro.
func auditRollout(l *audit.Logger, before, after *osv1alpha1.TalosRollout) {
	if l == nil {
		return
	}
	o := rolloutObject(after)
	if len(before.Status.Conditions) == 0 && len(after.Status.Conditions) > 0 {
		l.Proposed(o, after.Spec.ProposedBy, after.Spec.ClusterRef.Name, rolloutPlan(after), true)
	}
	if !approvedByPerson(before.Status.Conditions, osv1alpha1.ReasonPlanApproved) &&
		approvedByPerson(after.Status.Conditions, osv1alpha1.ReasonPlanApproved) {
		l.Approved(o, after.Spec.ProposedBy, after.Spec.Approval.ApprovedBy, after.Spec.Approval.PlanHash)
	}
	if after.Spec.Approval.ApprovedBy != "" {
		for i, st := range after.Status.Steps {
			if i >= len(after.Status.Plan) {
				break
			}
			was := osv1alpha1.RolloutNodePending
			if i < len(before.Status.Steps) && before.Status.Steps[i].State != "" {
				was = before.Status.Steps[i].State
			}
			if was == st.State {
				continue
			}
			name := stepName(after.Status.Plan[i])
			if was == osv1alpha1.RolloutNodePending && st.State != osv1alpha1.RolloutNodeSkipped {
				l.StepStarted(o, i, name)
			}
			switch st.State {
			case osv1alpha1.RolloutNodeSucceeded:
				l.StepFinished(o, i, name, string(st.State))
			case osv1alpha1.RolloutNodeFailed:
				l.StepFailed(o, i, name, osv1alpha1.ReasonRolloutFailed, st.Message)
			}
		}
	}
	if !rolloutFinished(before) && rolloutFinished(after) {
		c := meta.FindStatusCondition(after.Status.Conditions, osv1alpha1.ConditionComplete)
		var first *metav1.Time
		for _, st := range after.Status.Steps {
			if st.StartedAt != nil && (first == nil || st.StartedAt.Before(first)) {
				first = st.StartedAt
			}
		}
		var ran time.Duration
		if first != nil {
			ran = c.LastTransitionTime.Sub(first.Time)
		}
		l.Finished(o, rolloutPhase(c), c.Reason, ran)
	}
}

// deleteOp deletes an operation the operator decided to delete, with the
// line in the trail first.
func deleteOp(ctx context.Context, c client.Client, l *audit.Logger, op *metalv1alpha1.HostOperation,
	reason audit.DeleteReason, why string, opts ...client.DeleteOption) error {
	l.Deleted(opObject(op), reason, why)
	return c.Delete(ctx, op, opts...)
}

// hostAnnotation reads an annotation of a host that may be nil.
func hostAnnotation(host *metalv1alpha1.Host, key string) (string, bool) {
	if host == nil {
		return "", false
	}
	v, ok := host.Annotations[key]
	return v, ok
}

// mediaAuditor writes what happens to the media grants into the audit
// trail: who fetched a medium, which requests were refused, and what each
// grant served before it ended. The grants of an operation carry its
// handle (grantHandle), so each line names the operation; one with another
// handle (none today) is written under the handle as its name. Nil writes
// nothing.
func mediaAuditor(l *audit.Logger) func(asset.GrantRecord) {
	if l == nil {
		return nil
	}
	return func(g asset.GrantRecord) {
		o := audit.Object{Kind: audit.KindHostOperation, Name: g.Handle}
		if ref, ok := strings.CutPrefix(g.Handle, grantHandlePrefix); ok {
			if ns, name, ok := strings.Cut(ref, "/"); ok {
				o.Namespace, o.Name = ns, name
			}
		}
		switch g.Event {
		case asset.GrantFetched:
			l.MediaFetched(o, g.Filename, g.Source)
		case asset.GrantRefused:
			l.MediaRefused(o, g.Filename, g.Source, g.Reason)
		case asset.GrantEnded:
			l.MediaGrantEnded(o, g.Filename, g.Source, g.Reason, g.Requests, g.Bytes, g.Refused)
		}
	}
}
