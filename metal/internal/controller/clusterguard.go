package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/role"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// ClusterGuard keeps etcd's majority in every Talos cluster the operator
// built, not only in the one it runs in (that one SelfGuard protects on top).
// Approving three operations on three control planes at once must not
// reboot all three: the executor runs passes on different hosts side by
// side.
//
// A step that takes a control plane down (power, a Talos reboot, shutdown,
// upgrade or reset, a config apply that reboots, etcd leave) waits while
//
//  1. another control plane of the same cluster is in the middle of such an
//     operation (it started and has not finished), or
//  2. etcd is not fully healthy without this node's member: another member
//     does not answer, reports an error, knows no leader or is a learner
//     still catching up, an alarm is raised, or the members left would not
//     be a majority.
//
// Rule 2 counts from etcd's own member list, asked live of the members, not
// from the TalosNodes: a member a wiped node left behind still counts
// against the majority, and a node that never joined does not count for it.
//
// What the guard does not hold: a node that is not an etcd member (a worker,
// a control plane still joining or already removed), and any cluster that
// was never bootstrapped (day 0 installs every control plane at once).
// An operation whose first such step is the node's own etcd leave counts
// rule 2's majority from the cluster it leaves behind: the member goes
// while the others answer, so two control planes may become one without
// the override, and removal is still decided by the removal verdict.
// AnnotationOverrideClusterGuard on the operation lifts rule 2, for the
// repair of an etcd that is already broken and for a cluster with a single
// control plane; rule 1 stays. Only a non-blank value (the reason) lifts it.
type ClusterGuard struct {
	// Reader reads the TalosNodes, TalosClusters, Hosts and operations the
	// verdict depends on. The manager wires its uncached API reader, for the
	// same reason as SelfGuard's.
	Reader client.Reader
	// Dial reaches the control planes' Talos API to ask etcd; Namespace is
	// where the clusters' secrets bundles live. A nil Dial cannot ask etcd,
	// and then only rule 1 holds.
	Dial      talos.Dialer
	Namespace string
}

// Check returns "" when op may take host's node down now, or why it may not.
func (g *ClusterGuard) Check(ctx context.Context, host *metalv1alpha1.Host, op *metalv1alpha1.HostOperation) (string, error) {
	if g == nil || host.Spec.OS == nil {
		return "", nil
	}
	node := &osv1alpha1.TalosNode{}
	if err := g.Reader.Get(ctx, client.ObjectKey{Name: host.Spec.OS.NodeRef.Name}, node); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if node.Spec.Role != osv1alpha1.RoleControlPlane {
		return "", nil
	}
	cluster := &osv1alpha1.TalosCluster{}
	if err := g.Reader.Get(ctx, client.ObjectKey{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if cluster.Status.BootstrappedAt == nil {
		return "", nil
	}
	what := fmt.Sprintf("host %s runs TalosNode %s, a control plane of cluster %s", host.Name, node.Name, cluster.Name)

	nodes, err := clusterMembers(ctx, g.Reader, cluster.Name)
	if err != nil {
		return "", err
	}
	if other, err := g.otherControlPlaneDown(ctx, host, op, nodes); err != nil || other != "" {
		if other != "" {
			return fmt.Sprintf("%s, and %s; control planes are taken down one at a time", what, other), nil
		}
		return "", err
	}
	if g.Dial == nil {
		return "", nil
	}
	if _, overridden := guardOverride(op, metalv1alpha1.AnnotationOverrideClusterGuard); overridden {
		return "", nil
	}
	override := fmt.Sprintf(" To take it down anyway (etcd is broken already and this is the repair, or the cluster has a single "+
		"control plane): kubectl annotate --overwrite hostoperation -n %s %s %s='<why>' (an empty reason does not count)",
		op.Namespace, op.Name, metalv1alpha1.AnnotationOverrideClusterGuard)

	// Etcd health is a read: os:reader serves it.
	acc, err := newTalosAccessAs(ctx, g.Reader, g.Namespace, g.Dial, cluster, role.Reader)
	if err != nil {
		return fmt.Sprintf("%s, and etcd cannot be asked whether it keeps its majority without it: %v.%s", what, err, override), nil
	}
	rep, err := acc.etcdHealth(ctx, controlPlanes(nodes, node.Name), nodes)
	if err != nil {
		return fmt.Sprintf("%s, and etcd cannot be asked whether it keeps its majority without it: %v.%s", what, err, override), nil
	}
	self := rep.member(node.Name)
	if self == nil {
		return "", nil // not a member: taking it down costs etcd nothing
	}
	healthy := 0
	for _, m := range rep.members {
		if m.ID != self.ID && rep.healthy(m.ID) {
			healthy++
		}
	}
	total := len(rep.members)
	need := total/2 + 1
	if leavesFirst(op, node.Name) {
		// The member asks to leave while every other one answers, so etcd
		// shrinks to total-1 voters before the node goes down: the majority
		// that must stay is that of the smaller cluster. Two control planes
		// become one this way; the last member still needs the override.
		need = (total-1)/2 + 1
	}
	if probs := rep.problems(self.ID); len(probs) > 0 || healthy < need {
		msg := fmt.Sprintf("%s, and without its member etcd would have %d healthy voting member(s) of %d, and needs %d",
			what, healthy, total, need)
		if len(probs) > 0 {
			msg += "; etcd is not fully healthy: " + strings.Join(probs, "; ")
		}
		return msg + "." + override, nil
	}
	return "", nil
}

// leavesFirst: the first step of op that takes the node down is its etcd
// member leaving the cluster, so nothing disrupts it while it is a voter.
func leavesFirst(op *metalv1alpha1.HostOperation, node string) bool {
	i := slices.IndexFunc(op.Spec.Actions, disruptsAction)
	return i >= 0 && op.Spec.Actions[i].EtcdLeave != nil && op.Spec.Actions[i].EtcdLeave.NodeRef.Name == node
}

// otherControlPlaneDown names another operation that is taking down a
// control plane of the cluster right now, or "".
func (g *ClusterGuard) otherControlPlaneDown(ctx context.Context, host *metalv1alpha1.Host,
	op *metalv1alpha1.HostOperation, nodes []osv1alpha1.TalosNode) (string, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := g.Reader.List(ctx, hosts); err != nil {
		return "", err
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := g.Reader.List(ctx, ops); err != nil {
		return "", err
	}
	for i := range hosts.Items {
		h := &hosts.Items[i]
		if h.Name == host.Name || h.Spec.OS == nil {
			continue
		}
		other := findNode(nodes, h.Spec.OS.NodeRef.Name)
		if other == nil || other.Spec.Role != osv1alpha1.RoleControlPlane {
			continue
		}
		for j := range ops.Items {
			o := &ops.Items[j]
			if o.UID != op.UID && o.Spec.HostRef.Name == h.Name && opStarted(o) && !o.Finished() && disrupts(o) {
				return fmt.Sprintf("operation %s/%s is taking down control plane %s (host %s) right now", o.Namespace, o.Name, other.Name, h.Name), nil
			}
		}
	}
	return "", nil
}
