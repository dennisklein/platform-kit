package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// Taking a node out of its cluster. Whatever asks for it (a deleted claim, a
// deleted TalosNode, a reinstall, a claim pointed at another machine), the
// steps are the same, and they are fixed into one operation when it is
// proposed, so the approver sees all of them:
//
//   - the node answers its Talos API: cordon and drain it, on a control
//     plane hand etcd leadership on and leave etcd, reset the machine through
//     Talos (wiping its disks, credentials included), and delete its
//     Kubernetes Node;
//   - the node does not answer: on a control plane remove its etcd member by
//     ID through a peer (the ID is read now and frozen in the operation), and
//     delete its Kubernetes Node. Nothing can wipe the disk, so the Host is
//     marked as still holding the install (AnnotationDiskNotWiped) and is not
//     used for anything else until an approved wipe;
//   - a release then powers the machine off and empties its media slots.
//
// Removing a control plane changes etcd, and is decided on etcd's own
// membership read live from the control planes (etcdView), with the rules in
// removalVerdict. A control plane joining a running cluster waits for its
// turn too (joinVerdict).

const (
	// purposeRemove labels the operation that removes a node whose
	// TalosNode was deleted while its claim stays.
	purposeRemove = "remove"

	// labelNode on a lifecycle operation names the TalosNode it acts for.
	labelNode = "os.pk.io/node"

	// annotationReinstallOf on a reinstall operation is the hand-off time
	// of the install it replaces (AnnotationReinstall's value).
	annotationReinstallOf = "metal.pk.io/reinstall-of"

	// drainTimeoutSeconds bounds the drain of a removal.
	drainTimeoutSeconds = int32(900)
)

// supports reports whether the executor runs every one of these actions. An
// operation naming one it does not run is refused as a whole, so a plan is
// built only from actions it runs; the rest fall back to what the BMC alone
// can do.
func (r *HostClaimReconciler) supports(keys ...string) bool {
	f := r.SupportsAction
	if f == nil {
		f = func(k string) bool { return implementedActions[k] }
	}
	for _, k := range keys {
		if !f(k) {
			return false
		}
	}
	return true
}

// nodeAnswers: the node's authenticated Talos API answered at its last
// check-in, so the node can be drained, leave etcd and be reset by Talos.
func nodeAnswers(node *osv1alpha1.TalosNode) bool {
	c := meta.FindStatusCondition(node.Status.Conditions, osv1alpha1.ConditionMachineReachable)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == osv1alpha1.ReasonMachineAPI
}

// exitSteps are the steps that take node out of its cluster, before any
// power action. member is its etcd member (nil when it has none or it is a
// worker). resetMode is the Talos reset's wipe mode. wipes says whether the
// steps reset the machine through Talos; ok is false when the executor runs
// none of the steps this node needs, and the caller falls back to the BMC.
func (r *HostClaimReconciler) exitSteps(node *osv1alpha1.TalosNode, member *talos.EtcdMember, resetMode string) (
	steps []metalv1alpha1.HostOperationAction, wipes, ok bool) {
	ref := metalv1alpha1.Reference{Name: node.Name}
	cp := node.Spec.Role == osv1alpha1.RoleControlPlane && member != nil
	if nodeAnswers(node) {
		keys := []string{"cordon", "drain", "talosReset", "deleteKubernetesNode"}
		if cp {
			keys = append(keys, "etcdForfeitLeadership", "etcdLeave")
		}
		if r.supports(keys...) {
			steps = []metalv1alpha1.HostOperationAction{
				{Cordon: &metalv1alpha1.CordonAction{NodeRef: ref}},
				// The node is wiped next: data in emptyDir volumes is lost
				// either way, and a drain that refused such pods would only
				// stop the removal.
				{Drain: &metalv1alpha1.DrainAction{NodeRef: ref, TimeoutSeconds: drainTimeoutSeconds, DeleteEmptyDirData: true}},
			}
			if cp {
				steps = append(steps,
					metalv1alpha1.HostOperationAction{EtcdForfeitLeadership: &metalv1alpha1.EtcdForfeitLeadershipAction{NodeRef: ref}},
					metalv1alpha1.HostOperationAction{EtcdLeave: &metalv1alpha1.EtcdLeaveAction{NodeRef: ref}},
				)
			}
			// Not graceful: the steps above already drained the node and
			// took it out of etcd, each with its own record, and a graceful
			// reset would try to leave etcd a second time.
			steps = append(steps,
				metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
					NodeRef: ref, Graceful: ptr.To(false), Reboot: false, Mode: resetMode,
				}},
				metalv1alpha1.HostOperationAction{DeleteKubernetesNode: &metalv1alpha1.DeleteKubernetesNodeAction{NodeRef: ref}},
			)
			return steps, true, true
		}
	}
	keys := []string{"deleteKubernetesNode"}
	if cp {
		keys = append(keys, "etcdRemoveMember")
	}
	if !r.supports(keys...) {
		return nil, false, false
	}
	if cp {
		steps = append(steps, metalv1alpha1.HostOperationAction{EtcdRemoveMember: &metalv1alpha1.EtcdRemoveMemberAction{
			NodeRef: ref, MemberID: memberIDHex(member.ID), Hostname: member.Hostname,
		}})
	}
	steps = append(steps, metalv1alpha1.HostOperationAction{DeleteKubernetesNode: &metalv1alpha1.DeleteKubernetesNodeAction{NodeRef: ref}})
	return steps, false, true
}

// wipesDisk: the operation resets the machine's disks through Talos.
func wipesDisk(op *metalv1alpha1.HostOperation) bool {
	return slices.ContainsFunc(op.Spec.Actions, func(a metalv1alpha1.HostOperationAction) bool {
		return a.TalosReset != nil && a.TalosReset.Mode != "UserDisks"
	})
}

// actionNode is the TalosNode an action names, "" for the BMC actions.
func actionNode(a metalv1alpha1.HostOperationAction) string {
	switch {
	case a.AwaitCheckIn != nil:
		return a.AwaitCheckIn.NodeRef.Name
	case a.Cordon != nil:
		return a.Cordon.NodeRef.Name
	case a.Drain != nil:
		return a.Drain.NodeRef.Name
	case a.Uncordon != nil:
		return a.Uncordon.NodeRef.Name
	case a.TalosReboot != nil:
		return a.TalosReboot.NodeRef.Name
	case a.TalosShutdown != nil:
		return a.TalosShutdown.NodeRef.Name
	case a.TalosUpgrade != nil:
		return a.TalosUpgrade.NodeRef.Name
	case a.TalosApplyConfig != nil:
		return a.TalosApplyConfig.NodeRef.Name
	case a.EtcdLeave != nil:
		return a.EtcdLeave.NodeRef.Name
	case a.EtcdRemoveMember != nil:
		return a.EtcdRemoveMember.NodeRef.Name
	case a.TalosReset != nil:
		return a.TalosReset.NodeRef.Name
	case a.WaitNodeReady != nil:
		return a.WaitNodeReady.NodeRef.Name
	case a.WaitEtcdHealthy != nil:
		return a.WaitEtcdHealthy.NodeRef.Name
	case a.EtcdForfeitLeadership != nil:
		return a.EtcdForfeitLeadership.NodeRef.Name
	case a.DeleteKubernetesNode != nil:
		return a.DeleteKubernetesNode.NodeRef.Name
	}
	return ""
}

// changesEtcd: the operation removes an etcd member, by leaving, by ID, or
// by wiping a control plane's EPHEMERAL partition.
func changesEtcd(op *metalv1alpha1.HostOperation) bool {
	return slices.ContainsFunc(op.Spec.Actions, func(a metalv1alpha1.HostOperationAction) bool {
		return a.EtcdLeave != nil || a.EtcdRemoveMember != nil || a.TalosReset != nil
	})
}

// opNodes lists the TalosNodes an operation's actions name.
func opNodes(op *metalv1alpha1.HostOperation) []string {
	var out []string
	for _, a := range op.Spec.Actions {
		if n := actionNode(a); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// ackedEtcdRisk: a person acknowledged on the claim or the node that this
// removal may leave etcd even, without a majority, or empty.
func ackedEtcdRisk(claim *metalv1alpha1.HostClaim, node *osv1alpha1.TalosNode) bool {
	if _, ok := claim.Annotations[metalv1alpha1.AnnotationAckEtcdRisk]; ok {
		return true
	}
	_, ok := node.Annotations[metalv1alpha1.AnnotationAckEtcdRisk]
	return ok
}

// removalVerdict decides whether the control plane node may leave etcd now.
// It returns "" to go ahead, or the claim reason and message that say why
// not. leaving names the other control planes that are on their way out
// too. The rules:
//
//   - one change at a time: no member may still be a learner;
//   - the leader leaves last, when several control planes leave;
//   - what stays must have a healthy majority of what stays, must not be an
//     even number of voting members, and must not be empty. Each of these
//     can be acknowledged (AnnotationAckEtcdRisk); an even count tolerates no
//     more failures than the odd count below it, and costs one more vote.
//
// A node etcd does not list leaves nothing behind, and goes ahead.
func removalVerdict(v etcdView, why, node, cluster string, leaving []string, acked bool) (string, string) {
	ackHint := fmt.Sprintf("To remove it anyway, acknowledge it: kubectl annotate hostclaim <claim> %s=\"<reason>\" "+
		"(or the same on TalosNode %s)", metalv1alpha1.AnnotationAckEtcdRisk, node)
	if !v.known() {
		if acked {
			return "", ""
		}
		return metalv1alpha1.ReasonRemovalWaiting, fmt.Sprintf(
			"control plane %s is not removed while nothing says what that does to etcd: %s. %s", node, why, ackHint)
	}
	m := v.member(node)
	if m == nil {
		return "", ""
	}
	if l := slices.DeleteFunc(v.learners(), func(n string) bool { return n == node }); len(l) > 0 {
		return metalv1alpha1.ReasonRemovalWaiting, fmt.Sprintf(
			"control plane %s waits: etcd member %s is still a learner, and etcd changes one member at a time; "+
				"it goes on once that member votes", node, strings.Join(l, ", "))
	}
	if m.ID == v.leader {
		var others []string
		for _, n := range leaving {
			if n != node && v.member(n) != nil {
				others = append(others, n)
			}
		}
		if len(others) > 0 {
			return metalv1alpha1.ReasonRemovalWaiting, fmt.Sprintf(
				"control plane %s leads etcd and leaves last: %s leave first", node, strings.Join(others, ", "))
		}
	}
	voting, healthy := v.voting()
	after, healthyAfter := voting, healthy
	if !m.IsLearner {
		after--
		if v.healthy[m.ID] {
			healthyAfter--
		}
	}
	var risk string
	switch need := after/2 + 1; {
	case after == 0:
		risk = fmt.Sprintf("%s is the last etcd member of cluster %s: removing it deletes the cluster's etcd and everything in it", node, cluster)
	case healthyAfter < need:
		risk = fmt.Sprintf("without %s, %d of the %d etcd members of cluster %s are healthy, and etcd needs %d: the cluster would stop taking writes",
			node, healthyAfter, after, cluster, need)
	case after%2 == 0:
		risk = fmt.Sprintf("without %s, cluster %s has %d etcd members, an even number: it tolerates no more failures than %d members, "+
			"and every write needs one more vote. Remove one more control plane after it, or add one", node, cluster, after, after-1)
	}
	if risk == "" || acked {
		return "", ""
	}
	return metalv1alpha1.ReasonEtcdRiskRefused, risk + ". " + ackHint
}

// joinVerdict decides whether the control plane node may be installed into
// its running cluster now. Control planes join one at a time: a joining
// member makes the quorum bigger before it can vote, so two joins at once,
// or a join next to an unhealthy member, can stop the cluster's writes. It
// returns "" to go ahead, or the message that says what it waits for.
// joining names the other control planes on their way in.
func joinVerdict(v etcdView, why, node, cluster string, joining []string) string {
	if !v.known() {
		return fmt.Sprintf("control plane %s joins cluster %s only when its etcd is known to be healthy: %s", node, cluster, why)
	}
	if m := v.member(node); m != nil {
		return fmt.Sprintf("etcd of cluster %s still lists a member named %s (ID %s), left by an earlier install: "+
			"a second member with that name would never become healthy. Remove the old member first: pk-metal replace does, "+
			"or a removal of the old node", cluster, node, memberIDHex(m.ID))
	}
	if l := v.learners(); len(l) > 0 {
		return fmt.Sprintf("control plane %s waits: etcd member %s of cluster %s is still a learner; control planes join one at a time",
			node, strings.Join(l, ", "), cluster)
	}
	if u := v.unhealthy(); len(u) > 0 {
		return fmt.Sprintf("control plane %s waits: etcd member %s of cluster %s is not healthy; a join next to it could stop the cluster's writes",
			node, strings.Join(u, ", "), cluster)
	}
	if len(joining) > 0 {
		return fmt.Sprintf("control plane %s waits: %s of cluster %s is joining; control planes join one at a time",
			node, strings.Join(joining, ", "), cluster)
	}
	return ""
}

// clusterOf reads the node's TalosCluster; nil when it is gone.
func (r *HostClaimReconciler) clusterOf(ctx context.Context, node *osv1alpha1.TalosNode) (*osv1alpha1.TalosCluster, error) {
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return cluster, nil
}

// etcdOf reads the cluster's etcd membership. The reason is empty when it
// was read.
func (r *HostClaimReconciler) etcdOf(ctx context.Context, cluster *osv1alpha1.TalosCluster) (etcdView, string, error) {
	return clusterEtcd(ctx, r.Client, r.Dial, r.CredentialsNamespace, cluster)
}

// leavingControlPlanes names the control planes of cluster on their way
// out: a deleted TalosNode, or a node whose claim is deleted.
func (r *HostClaimReconciler) leavingControlPlanes(ctx context.Context, cluster string) ([]string, error) {
	nodes, err := clusterMembers(ctx, r.Client, cluster)
	if err != nil {
		return nil, err
	}
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		if n.Spec.Role != osv1alpha1.RoleControlPlane {
			continue
		}
		leaving := !n.DeletionTimestamp.IsZero()
		for _, c := range claims.Items {
			if c.Spec.OS != nil && c.Spec.OS.NodeRef.Name == n.Name && !c.DeletionTimestamp.IsZero() {
				leaving = true
			}
		}
		if leaving {
			out = append(out, n.Name)
		}
	}
	return out, nil
}

// etcdChangeInFlight names an unfinished operation, other than skip, that
// changes the etcd membership of cluster (a removal or reinstall of another
// of its control planes), or a boot of another of its control planes. It
// reads the API server, not the cache: two claims reconciled back to back
// must not both see none.
func (r *HostClaimReconciler) etcdChangeInFlight(ctx context.Context, cluster, skip string) (string, error) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.reader().List(ctx, ops, client.InNamespace(r.OperationsNamespace)); err != nil {
		return "", err
	}
	nodes, err := clusterMembers(ctx, r.Client, cluster)
	if err != nil {
		return "", err
	}
	cp := map[string]bool{}
	for _, n := range nodes {
		if n.Spec.Role == osv1alpha1.RoleControlPlane {
			cp[n.Name] = true
		}
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Name == skip || opFinished(op) || !op.DeletionTimestamp.IsZero() {
			continue
		}
		boots := slices.ContainsFunc(op.Spec.Actions, func(a metalv1alpha1.HostOperationAction) bool { return a.AwaitCheckIn != nil })
		if !boots && !changesEtcd(op) {
			continue
		}
		for _, n := range opNodes(op) {
			if cp[n] {
				return fmt.Sprintf("operation %s/%s on control plane %s", op.Namespace, op.Name, n), nil
			}
		}
	}
	return "", nil
}

// etcdExitGuard decides whether control plane node may leave etcd now, and
// returns its member as read (nil when etcd does not list it). reason is
// empty to go ahead. A cluster that is gone or being deleted has no etcd to
// protect.
func (r *HostClaimReconciler) etcdExitGuard(ctx context.Context, claim *metalv1alpha1.HostClaim, node *osv1alpha1.TalosNode,
	skipOp string) (member *talos.EtcdMember, reason, msg string, err error) {
	if node.Spec.Role != osv1alpha1.RoleControlPlane || r.Dial == nil {
		return nil, "", "", nil
	}
	cluster, err := r.clusterOf(ctx, node)
	if err != nil || cluster == nil || !cluster.DeletionTimestamp.IsZero() {
		return nil, "", "", err
	}
	if busy, err := r.etcdChangeInFlight(ctx, cluster.Name, skipOp); err != nil {
		return nil, "", "", err
	} else if busy != "" {
		return nil, metalv1alpha1.ReasonRemovalWaiting, fmt.Sprintf(
			"control plane %s waits: %s changes the etcd membership of cluster %s now; control planes change one at a time",
			node.Name, busy, cluster.Name), nil
	}
	v, why, err := r.etcdOf(ctx, cluster)
	if err != nil {
		return nil, "", "", err
	}
	leaving, err := r.leavingControlPlanes(ctx, cluster.Name)
	if err != nil {
		return nil, "", "", err
	}
	reason, msg = removalVerdict(v, why, node.Name, cluster.Name, leaving, ackedEtcdRisk(claim, node))
	if m := v.member(node.Name); m != nil {
		member = ptr.To(*m)
	}
	return member, reason, msg, nil
}

// markNotWiped records on the host, in memory, that its disk may still hold
// node's install, and reports whether that changed anything; the caller
// writes the host. A mark already there is kept: it names the oldest
// install, and one wipe clears any.
func markNotWiped(host *metalv1alpha1.Host, cluster, node string) bool {
	if _, ok := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]; ok {
		return false
	}
	if host.Annotations == nil {
		host.Annotations = map[string]string{}
	}
	host.Annotations[metalv1alpha1.AnnotationDiskNotWiped] = cluster + "/" + node
	return true
}

// notWipedHold is the claim message for a host that still holds another
// node's install, or "" when it is wiped or the install is the claim's own
// node (whose install the boot replaces).
func notWipedHold(host *metalv1alpha1.Host, cluster, node string) string {
	v, ok := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]
	if !ok || v == cluster+"/"+node {
		return ""
	}
	return fmt.Sprintf("host %s may still hold an install of node %s, credentials included: it was released without a Talos reset. "+
		"Nothing else is installed on it until it is wiped: pk-metal wipe %s", host.Name, v, host.Name)
}

// notWipedPowerHold is why no BIOS or firmware change is proposed for a
// host that may still hold a released node's install, "" when it is wiped.
func notWipedPowerHold(host *metalv1alpha1.Host) string {
	v, ok := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]
	if !ok {
		return ""
	}
	return fmt.Sprintf("no change that powers host %s on is proposed while it may still hold node %s: wipe it first: pk-metal wipe %s",
		host.Name, v, host.Name)
}
