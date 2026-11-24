package controller

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Self-protection. Once the operator runs on a cluster it built itself (after
// pk-metal pivot), some of the machines it manages carry the operator: the
// node its pod runs on, and the control planes whose etcd holds every object
// it reads. Rebooting one of those from inside the operator can take the
// operator down halfway through the very operation that rebooted it, or take
// etcd below its majority. So the operator does not do it on its own.
//
// How the operator finds where it runs, step by step (locateSelf):
//
//  1. The manager's Deployment passes the pod's node name in through the
//     downward API (NODE_NAME, from spec.nodeName); the manager hands it on
//     with --node-name.
//  2. The guard reads that Kubernetes Node and collects its addresses.
//  3. The TalosNode whose name equals the node name, or else whose address
//     (spec.address, status.address or a static link address) equals one of
//     the Node's addresses, is the TalosNode the manager runs on. Its
//     clusterRef is the operator's own cluster.
//  4. A Host runs a TalosNode when its spec.os names it.
//
// On a cluster no TalosNode describes (kind, a cluster built some other way)
// nothing matches, and the guard allows everything, as before.

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get

// SelfGuard decides whether a disruptive step may touch a host, given where
// the operator itself runs. A nil *SelfGuard allows everything.
type SelfGuard struct {
	// Reader reads the Node, TalosNodes, TalosClusters, Hosts and operations
	// the verdict depends on. The manager wires its uncached API reader: the
	// verdict is asked for rarely (only before a disruptive step), and a
	// stale cache could hide a control plane that went down a moment ago.
	Reader client.Reader
	// NodeName is the Kubernetes node the manager's pod runs on. Empty
	// means unknown: the guard then allows everything.
	NodeName string
}

// selfLocation is where the operator runs, as far as the guard could tell.
type selfLocation struct {
	// node is the TalosNode the manager's pod runs on; nil when none matched.
	node *osv1alpha1.TalosNode
	// how says how it matched, for messages: `node name "x"` or `address y`.
	how string
}

// locateSelf finds the TalosNode a Kubernetes node is, by name first and then
// by address. k8sNode may be nil (not readable); the name still matches then.
// The result is deterministic: candidates are tried in name order.
func locateSelf(nodeName string, k8sNode *corev1.Node, nodes []osv1alpha1.TalosNode) selfLocation {
	if nodeName == "" {
		return selfLocation{}
	}
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(a, b osv1alpha1.TalosNode) int { return strings.Compare(a.Name, b.Name) })
	for i := range sorted {
		if sorted[i].Name == nodeName {
			return selfLocation{node: &sorted[i], how: fmt.Sprintf("node name %q", nodeName)}
		}
	}
	if k8sNode == nil {
		return selfLocation{}
	}
	var mine []netip.Addr
	for _, a := range k8sNode.Status.Addresses {
		if ip, err := netip.ParseAddr(a.Address); err == nil {
			mine = append(mine, ip.Unmap())
		}
	}
	for i := range sorted {
		for _, ip := range talosNodeAddresses(&sorted[i]) {
			if slices.Contains(mine, ip) {
				return selfLocation{node: &sorted[i], how: "address " + ip.String()}
			}
		}
	}
	return selfLocation{}
}

// talosNodeAddresses are the addresses a TalosNode is known by: its spec and
// observed address and its static link addresses, without prefix lengths.
func talosNodeAddresses(n *osv1alpha1.TalosNode) []netip.Addr {
	var out []netip.Addr
	add := func(s string) {
		if s == "" {
			return
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Addr().Unmap())
			return
		}
		if ip, err := netip.ParseAddr(s); err == nil {
			out = append(out, ip.Unmap())
		}
	}
	add(n.Spec.Address)
	add(n.Status.Address)
	for _, l := range n.Spec.Network.Links {
		for _, a := range l.Addresses {
			add(a)
		}
	}
	return out
}

// disruptionInput is everything the verdict depends on, read by Check.
type disruptionInput struct {
	host  *metalv1alpha1.Host
	self  selfLocation
	nodes []osv1alpha1.TalosNode
	// cluster is the operator's own TalosCluster, nil when not found.
	cluster *osv1alpha1.TalosCluster
	hosts   []metalv1alpha1.Host
	ops     []metalv1alpha1.HostOperation
}

// disruptionVerdict is the pure decision: "" when the step may go ahead,
// otherwise the message that says why not and how to allow it. A host is
// protected when the operator's pod runs on its node, or when it runs a
// control plane of the operator's own cluster (its node checked in, or the
// host reports its OS ready). The annotation lifts the first rule; for a
// control plane two rules stay: no other control plane of the cluster may be
// in the middle of a power action, and the cluster's observed ready control
// planes must keep a majority without this one.
func disruptionVerdict(in disruptionInput) string {
	self := in.self.node
	if self == nil || in.host.Spec.OS == nil {
		return ""
	}
	own := self.Spec.ClusterRef.Name
	node := findNode(in.nodes, in.host.Spec.OS.NodeRef.Name)
	if node == nil {
		return ""
	}
	runsManager := node.Name == self.Name
	ownControlPlane := node.Spec.ClusterRef.Name == own && node.Spec.Role == osv1alpha1.RoleControlPlane &&
		(nodeCheckedIn(node) || meta.IsStatusConditionTrue(in.host.Status.Conditions, metalv1alpha1.ConditionOSReady))
	if !runsManager && !ownControlPlane {
		return ""
	}

	var what string
	switch {
	case runsManager && ownControlPlane:
		what = fmt.Sprintf("host %s runs TalosNode %s, a control plane of cluster %s, and this operator runs on it (%s)",
			in.host.Name, node.Name, own, in.self.how)
	case runsManager:
		what = fmt.Sprintf("host %s runs TalosNode %s, and this operator runs on it (%s)", in.host.Name, node.Name, in.self.how)
	default:
		what = fmt.Sprintf("host %s runs TalosNode %s, a control plane of cluster %s, the cluster this operator runs in",
			in.host.Name, node.Name, own)
	}
	if _, allowed := in.host.Annotations[metalv1alpha1.AnnotationAllowSelfDisruption]; !allowed {
		return what + ". Rebooting or powering it off could stop the operator halfway, so the operator does not do it on its own. " +
			"Safer: move the operator to another cluster first (pk-metal pivot). To allow it for this host: " +
			"kubectl annotate host " + in.host.Name + " " + metalv1alpha1.AnnotationAllowSelfDisruption + "=true"
	}
	if !ownControlPlane {
		return ""
	}

	// One control plane at a time: another one in a power action now is
	// already one fewer etcd member.
	for i := range in.hosts {
		h := &in.hosts[i]
		if h.Name == in.host.Name || h.Spec.OS == nil {
			continue
		}
		other := findNode(in.nodes, h.Spec.OS.NodeRef.Name)
		if other == nil || other.Spec.ClusterRef.Name != own || other.Spec.Role != osv1alpha1.RoleControlPlane {
			continue
		}
		for j := range in.ops {
			op := &in.ops[j]
			if op.Spec.HostRef.Name == h.Name && opStarted(op) && !opFinished(op) && disrupts(op) {
				return fmt.Sprintf("%s. It is allowed by annotation, but operation %s/%s is taking down control plane %s right now; "+
					"control planes of the operator's own cluster go one at a time. This step starts when that one has finished",
					what, op.Namespace, op.Name, h.Name)
			}
		}
	}

	// etcd's majority, from the cluster's own observation.
	if in.cluster == nil {
		return fmt.Sprintf("%s. It is allowed by annotation, but TalosCluster %s is not found, so nothing says etcd keeps its majority without it",
			what, own)
	}
	total, ready := int(in.cluster.Status.ControlPlanes), int(in.cluster.Status.ReadyControlPlanes)
	left := ready
	if nodeReady(node) {
		left--
	}
	if need := total/2 + 1; total == 0 || left < need {
		return fmt.Sprintf("%s. It is allowed by annotation, but cluster %s has %d of %d control planes ready; without this one %d are left "+
			"and etcd needs %d. The operator does not take etcd below its majority. Wait until the other control planes are ready, "+
			"or move the operator to another cluster first (pk-metal pivot)",
			what, own, ready, total, max(left, 0), need)
	}
	return ""
}

func findNode(nodes []osv1alpha1.TalosNode, name string) *osv1alpha1.TalosNode {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

func nodeReady(n *osv1alpha1.TalosNode) bool {
	return meta.IsStatusConditionTrue(n.Status.Conditions, osv1alpha1.ConditionReady)
}

// disrupts: the operation can reboot or power off the machine.
func disrupts(op *metalv1alpha1.HostOperation) bool {
	return slices.ContainsFunc(op.Spec.Actions, func(a metalv1alpha1.HostOperationAction) bool {
		return a.PowerCycle != nil || a.PowerOff != nil
	})
}

// Check reads what the verdict needs and returns "" when a step that reboots
// or powers off host may go ahead, or the message that says why it may not.
func (g *SelfGuard) Check(ctx context.Context, host *metalv1alpha1.Host) (string, error) {
	if g == nil || g.NodeName == "" {
		return "", nil
	}
	// The caller's copy may come from a cache that has not seen a person
	// remove the allowance a moment ago.
	live := &metalv1alpha1.Host{}
	switch err := g.Reader.Get(ctx, client.ObjectKey{Name: host.Name}, live); {
	case err == nil:
		host = live
	case client.IgnoreNotFound(err) != nil:
		return "", err
	}
	if host.Spec.OS == nil {
		return "", nil
	}
	nodes := &osv1alpha1.TalosNodeList{}
	if err := g.Reader.List(ctx, nodes); err != nil {
		return "", err
	}
	var k8sNode *corev1.Node
	n := &corev1.Node{}
	switch err := g.Reader.Get(ctx, client.ObjectKey{Name: g.NodeName}, n); {
	case err == nil:
		k8sNode = n
	case client.IgnoreNotFound(err) != nil:
		return "", err
	}
	in := disruptionInput{host: host, self: locateSelf(g.NodeName, k8sNode, nodes.Items), nodes: nodes.Items}
	if in.self.node == nil {
		return "", nil
	}
	cluster := &osv1alpha1.TalosCluster{}
	switch err := g.Reader.Get(ctx, client.ObjectKey{Name: in.self.node.Spec.ClusterRef.Name}, cluster); {
	case err == nil:
		in.cluster = cluster
	case client.IgnoreNotFound(err) != nil:
		return "", err
	}
	hosts := &metalv1alpha1.HostList{}
	if err := g.Reader.List(ctx, hosts); err != nil {
		return "", err
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := g.Reader.List(ctx, ops); err != nil {
		return "", err
	}
	in.hosts, in.ops = hosts.Items, ops.Items
	return disruptionVerdict(in), nil
}
