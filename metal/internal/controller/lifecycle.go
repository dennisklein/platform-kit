package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
)

// The claim's side of the node lifecycle on day 2: removing a node whose
// TalosNode was deleted while the claim keeps the machine, reinstalling the
// node a claim runs, and the steps a boot carries when it replaces an
// install. The steps themselves are built in removal.go.

// reinstallRequested: the claim asks for its node to be reinstalled on its
// host, and the hand-off record the request names is the one there now.
func reinstallRequested(claim *metalv1alpha1.HostClaim, hostName, nodeName string) bool {
	v, ok := claim.Annotations[metalv1alpha1.AnnotationReinstall]
	h := claim.Status.HandOff
	if !ok || h == nil || h.Host != hostName || h.Node != nodeName {
		return false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	return err == nil && at.Equal(h.At.Truncate(time.Second))
}

// bootPrefix returns the steps a boot of node onto host runs before its
// media go in: when the boot replaces an install (prior names the node the
// machine ran), that node leaves its cluster first: drained, out of etcd,
// reset. A reinstall keeps the user disks; another node's install is wiped
// entirely. A control plane joining a running cluster waits for its turn
// (joinVerdict); one leaving waits by removalVerdict. reason is set to hold.
func (r *HostClaimReconciler) bootPrefix(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host,
	node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster, prior, opName string) (
	[]metalv1alpha1.HostOperationAction, string, string, error) {
	var steps []metalv1alpha1.HostOperationAction
	if prior != "" {
		old := node
		if prior != node.Name {
			old = &osv1alpha1.TalosNode{}
			if err := r.Get(ctx, client.ObjectKey{Name: prior}, old); err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, "", "", err
				}
				// The node it ran is gone: nothing is left to take out of a
				// cluster but a stale etcd member, which the join gate
				// below reports.
				old = nil
			}
		}
		if old != nil {
			member, last, reason, msg, err := r.etcdExitGuard(ctx, claim, old, opName)
			if err != nil || reason != "" {
				return nil, reason, msg, err
			}
			mode := ""
			if old.Name == node.Name {
				mode = "SystemDisk"
			}
			s, _, ok := r.exitSteps(old, member, last, mode)
			if !ok && member != nil {
				return nil, metalv1alpha1.ReasonAwaitingOperation, fmt.Sprintf(
					"host %s runs control plane %s, whose etcd member has to leave before the machine is installed again, "+
						"and this operator's executor cannot run that step yet", host.Name, old.Name), nil
			}
			steps = s
		}
	}
	if node.Spec.Role != osv1alpha1.RoleControlPlane || r.Dial == nil || cluster.Status.BootstrappedAt == nil || prior == node.Name {
		// A reinstall is a rejoin under the same name, whose member the
		// steps above remove; a cluster not bootstrapped yet has every
		// control plane come up together, which its bootstrap waits for.
		return steps, "", "", nil
	}
	msg, err := r.joinGate(ctx, node, cluster, opName)
	if err != nil || msg != "" {
		return nil, metalv1alpha1.ReasonEtcdJoinWaiting, msg, err
	}
	return steps, "", "", nil
}

// joinGate reads the cluster's etcd and the other control planes, and says
// what node's join waits for, "" when it may go ahead.
func (r *HostClaimReconciler) joinGate(ctx context.Context, node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster,
	opName string) (string, error) {
	if busy, err := r.etcdChangeInFlight(ctx, cluster.Name, opName); err != nil {
		return "", err
	} else if busy != "" {
		return fmt.Sprintf("control plane %s waits: %s changes the etcd membership of cluster %s now; control planes join one at a time",
			node.Name, busy, cluster.Name), nil
	}
	v, why, err := r.etcdOf(ctx, cluster)
	if err != nil {
		return "", err
	}
	joining, err := r.joiningControlPlanes(ctx, cluster.Name, node.Name, v)
	if err != nil {
		return "", err
	}
	return joinVerdict(v, why, node.Name, cluster.Name, joining), nil
}

// joiningControlPlanes names the control planes of cluster, other than node,
// that were installed (a hand-off record, or a check-in) but are not voting
// members of etcd yet.
func (r *HostClaimReconciler) joiningControlPlanes(ctx context.Context, cluster, node string, v etcdView) ([]string, error) {
	nodes, err := clusterMembers(ctx, r.Client, cluster)
	if err != nil {
		return nil, err
	}
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil, err
	}
	installed := map[string]bool{}
	for _, c := range claims.Items {
		if h := c.Status.HandOff; h != nil {
			installed[h.Node] = true
		}
	}
	var out []string
	for i := range nodes {
		n := &nodes[i]
		if n.Name == node || n.Spec.Role != osv1alpha1.RoleControlPlane || !n.DeletionTimestamp.IsZero() {
			continue
		}
		if !installed[n.Name] && !nodeCheckedIn(n) {
			continue
		}
		if m := v.member(n.Name); m == nil || m.IsLearner {
			out = append(out, n.Name)
		}
	}
	return out, nil
}

// removeNode takes the claim's node out of its cluster because its TalosNode
// is being deleted, while the claim keeps the machine: the boots of the node
// stop, and a machine that runs or ran it gets one removal operation, the
// same steps as a release. Once it succeeded the hand-off record and the
// node's boot records go, so that the TalosNode's deletion can finish and a
// node created later under the same name is installed afresh.
func (r *HostClaimReconciler) removeNode(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host,
	node *osv1alpha1.TalosNode, ops []metalv1alpha1.HostOperation) error {
	set := func(reason, msg string) {
		setCondition(&claim.Status.Conditions, claim.Generation, metalv1alpha1.ConditionProvisioned, metav1.ConditionFalse, reason, msg)
	}
	var boots []metalv1alpha1.HostOperation
	for i := range ops {
		if bootsNode(&ops[i], host.Name, node.Name) {
			boots = append(boots, ops[i])
		}
	}
	// No boot of a node on its way out goes on: one waiting for approval is
	// withdrawn, one running stops between steps.
	for i := range boots {
		if boots[i].Finished() || !boots[i].DeletionTimestamp.IsZero() {
			continue
		}
		logf.FromContext(ctx).Info("stopped boot operation of a deleted node", "operation", boots[i].Namespace+"/"+boots[i].Name)
		if err := deleteOp(ctx, r.Client, r.Audit, &boots[i], audit.DeleteStopped,
			"TalosNode "+node.Name+" is being deleted"); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	h := claim.Status.HandOff
	touched := (h != nil && h.Host == host.Name && h.Node == node.Name) || bootRan(boots, host.Name)
	removals, err := r.claimOps(ctx, claim, purposeRemove)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("remove-%s-%s", host.Name, hash8(planHash([]string{string(claim.UID), string(node.UID)})))
	var op *metalv1alpha1.HostOperation
	for i := range removals {
		if removals[i].Name == name {
			op = &removals[i]
		}
	}
	if !touched && op == nil {
		set(metalv1alpha1.ReasonNodeRemoved, fmt.Sprintf(
			"TalosNode %s is being deleted, and host %s never ran it: nothing to remove. The claim keeps the machine for the node it names next",
			node.Name, host.Name))
		return nil
	}
	if op == nil {
		if why, held := heldBy(claim); held {
			set(metalv1alpha1.ReasonHeldByUser, fmt.Sprintf("held%s: TalosNode %s is being deleted, but host %s is not touched while the claim carries the %s annotation. To go on: %s",
				why, node.Name, host.Name, metalv1alpha1.AnnotationHold, releaseHoldCommand(claim)))
			return nil
		}
		if _, maint := inMaintenance(host); maint {
			set(metalv1alpha1.ReasonHostInMaintenance, maintenanceNote(host)+
				": TalosNode "+node.Name+" is removed from its cluster after the machine is given back: "+endMaintenanceCommand(host))
			return nil
		}
		if msg, err := r.Self.Check(ctx, host); err != nil {
			return err
		} else if msg != "" {
			set(metalv1alpha1.ReasonSelfDisruption, "TalosNode "+node.Name+" is being deleted, but its machine is not touched: "+msg)
			return nil
		}
		var reason, msg string
		if op, reason, msg, err = r.removalOp(ctx, claim, host, node, name, ops); err != nil {
			return err
		}
		if op == nil {
			set(reason, msg)
			return nil
		}
		if v := op.Annotations[metalv1alpha1.AnnotationDiskNotWiped]; v != "" {
			cluster, n, _ := strings.Cut(v, "/")
			if markNotWiped(host, cluster, n) {
				if err := r.Update(ctx, host); err != nil {
					return err
				}
			}
		}
		if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		logf.FromContext(ctx).Info("proposed node removal", "operation", op.Namespace+"/"+op.Name, "node", node.Name, "host", host.Name)
	}
	ref := op.Namespace + "/" + op.Name
	ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	switch {
	case op.AwaitsApproval():
		set(metalv1alpha1.ReasonRemovingNode, fmt.Sprintf(
			"TalosNode %s is being deleted: its removal %s. To go ahead: pk-metal approve %s. To keep it for now: pk-metal hold %s/%s",
			node.Name, r.describeRelease(op), ref, claim.Namespace, claim.Name))
	case ex != nil && ex.Status == metav1.ConditionFalse:
		set(metalv1alpha1.ReasonOperationFailed, fmt.Sprintf("removal operation %s of TalosNode %s failed: %s. To try again: pk-metal retry %s",
			ref, node.Name, ex.Message, ref))
	case ex == nil || ex.Status != metav1.ConditionTrue:
		set(metalv1alpha1.ReasonRemovingNode, fmt.Sprintf("removing TalosNode %s from host %s (operation %s)", node.Name, host.Name, ref))
	default:
		if wipesDisk(op) {
			if _, ok := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]; ok {
				delete(host.Annotations, metalv1alpha1.AnnotationDiskNotWiped)
				if err := r.Update(ctx, host); err != nil {
					return err
				}
			}
		}
		// The install is gone: so are its records. A boot record left
		// behind would read as a boot of the next node of that name on
		// this machine, so it is not kept for the site's retention; the
		// audit trail keeps what it decided.
		if h != nil && h.Host == host.Name && h.Node == node.Name {
			claim.Status.HandOff = nil
		}
		for i := range boots {
			if !boots[i].DeletionTimestamp.IsZero() {
				continue
			}
			if err := deleteOp(ctx, r.Client, r.Audit, &boots[i], audit.DeleteRecordOfRemoved,
				"TalosNode "+node.Name+" was removed from host "+host.Name+" by operation "+ref); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		msg := fmt.Sprintf("TalosNode %s was removed from its cluster by operation %s; the claim keeps host %s for the node it names next", node.Name, ref, host.Name)
		if !wipesDisk(op) {
			msg += fmt.Sprintf(". The machine could not be reset, so it is wiped before anything else is installed on it: pk-metal wipe %s", host.Name)
		}
		set(metalv1alpha1.ReasonNodeRemoved, msg)
	}
	return nil
}

// removalOp builds the operation that removes node from host while the claim
// keeps it: the node's exit steps, then a power-off and empty media slots.
// It returns nil with a reason while the removal must wait.
func (r *HostClaimReconciler) removalOp(ctx context.Context, claim *metalv1alpha1.HostClaim, host *metalv1alpha1.Host,
	node *osv1alpha1.TalosNode, name string, ops []metalv1alpha1.HostOperation) (*metalv1alpha1.HostOperation, string, string, error) {
	shape, err := r.endpointShape(ctx, host)
	if err != nil {
		return nil, "", "", err
	}
	if !shape.known {
		return nil, metalv1alpha1.ReasonRemovingNode, fmt.Sprintf("removing TalosNode %s waits until a reachable management path of host %s has read its media slots",
			node.Name, host.Name), nil
	}
	member, last, reason, msg, err := r.etcdExitGuard(ctx, claim, node, name)
	if err != nil || reason != "" {
		return nil, reason, "TalosNode " + node.Name + " is being deleted, but " + msg, err
	}
	steps, wipes, _ := r.exitSteps(node, member, last, "")
	graceful := int32(r.releaseShutdownTimeout().Seconds())
	if wipes {
		graceful = 0
	}
	actions := append(slices.Clone(steps), metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: graceful}})
	for _, s := range shape.insertable {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: s}})
	}
	labels := claimLabels(claim, purposeRemove)
	labels[labelNode] = node.Name
	var annotations map[string]string
	if !wipes {
		annotations = map[string]string{metalv1alpha1.AnnotationDiskNotWiped: node.Spec.ClusterRef.Name + "/" + node.Name}
	}
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperationsNamespace, Labels: labels, Annotations: annotations},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: host.Name},
			ProposedBy: r.Proposer,
			// The node's deletion asked for this removal.
			RequestedBy:             metalv1alpha1.RequestedBy(node.Annotations, claim.Annotations),
			Reason:                  reasonRelease,
			Actions:                 actions,
			Approval:                metalv1alpha1.OperationApproval{Required: ptr.To(releaseNeedsPerson(claim, host, ops))},
			TTLSecondsAfterFinished: ptr.To(releaseTTL),
		},
	}, "", "", nil
}
