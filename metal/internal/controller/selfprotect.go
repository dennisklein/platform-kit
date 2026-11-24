package controller

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
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
//
// Which control planes count as up is asked of the machines themselves, at
// the moment of the verdict (SelfGuard.Dial). The TalosNode's Ready
// condition is polled only every few minutes once a node is ready, and a
// power cycle is over as soon as the BMC reports the machine on, long before
// Talos and etcd run again. Deciding from either would let a second control
// plane go down while the first one is still in POST.

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostclaims,verbs=get

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
	// Dial reaches the Talos API of the other control planes, to ask each
	// whether it runs and is ready now (see controlPlaneUp). Namespace is
	// where the cluster's secrets bundle lives, from which the client
	// certificate for that question is minted. A nil Dial falls back on the
	// TalosNodes' Ready conditions, which can be minutes old: a test seam,
	// the manager always wires it.
	Dial      talos.Dialer
	Namespace string
}

// probeTimeout bounds the question to one control plane. A node that does
// not answer in time counts as down.
var probeTimeout = 5 * time.Second // a variable for tests only

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
	// handedOff: the claim holding the host records that the host was handed
	// its node (status.handOff). It stays true while the node does not
	// answer, so a control plane the operator cannot reach for a moment is
	// still protected.
	handedOff bool
	// cluster is the operator's own TalosCluster, nil when not found.
	cluster *osv1alpha1.TalosCluster
	hosts   []metalv1alpha1.Host
	ops     []metalv1alpha1.HostOperation
	// up says whether a control plane runs and is ready now; nil reads the
	// TalosNode's Ready condition. Check asks the machine (controlPlaneUp).
	up func(*osv1alpha1.TalosNode) bool
}

// disruptionVerdict is the pure decision: "" when the step may go ahead,
// otherwise the message that says why not and how to allow it. A host is
// protected when the operator's pod runs on its node, or when it runs a
// control plane of the operator's own cluster: one its claim records as
// handed off, or whose node is checked in now. A control plane never
// installed (one joining the cluster) is not protected; one installed stays
// protected while it does not answer. The annotation lifts the first rule;
// for a control plane two rules stay: no other control plane of the cluster
// may be in the middle of a power action, and the other control planes that
// run and are ready right now must be a majority of the cluster.
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
		(in.handedOff || nodeCheckedIn(node) || meta.IsStatusConditionTrue(in.host.Status.Conditions, metalv1alpha1.ConditionOSReady))
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

	// etcd's majority, counted over the other control planes that answer
	// now. One whose operation finished a moment ago is still booting, and
	// does not count until Talos says it runs and is ready again.
	if in.cluster == nil {
		return fmt.Sprintf("%s. It is allowed by annotation, but TalosCluster %s is not found, so nothing says etcd keeps its majority without it",
			what, own)
	}
	up := in.up
	if up == nil {
		up = nodeReady
	}
	members, left := 1, 0
	var down []string
	for i := range in.nodes {
		n := &in.nodes[i]
		if n.Name == node.Name || n.Spec.ClusterRef.Name != own || n.Spec.Role != osv1alpha1.RoleControlPlane {
			continue
		}
		members++
		if up(n) {
			left++
		} else {
			down = append(down, n.Name)
		}
	}
	total := max(int(in.cluster.Status.ControlPlanes), members)
	if need := total/2 + 1; left < need {
		msg := fmt.Sprintf("%s. It is allowed by annotation, but without it %d of the %d control planes of cluster %s run and are ready now, "+
			"and etcd needs %d. The operator does not take etcd below its majority.", what, left, total, own, need)
		if len(down) > 0 {
			msg += " Not running or not ready: " + strings.Join(down, ", ") + "."
		}
		return msg + " Wait until the other control planes are ready, or move the operator to another cluster first (pk-metal pivot)"
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

// disrupts: the operation can reboot, power off, wipe or take etcd away
// from the machine.
func disrupts(op *metalv1alpha1.HostOperation) bool {
	return slices.ContainsFunc(op.Spec.Actions, disruptsAction)
}

// disruptsAction: the action takes the node down, or its etcd member away.
// A config applied without a reboot, staged for the next one, or tried with
// a rollback does not; Auto may reboot, so it counts. Handing etcd
// leadership on keeps every member (etcd elects among the healthy ones), and
// deleting the Kubernetes Node or staging firmware leaves the machine
// running, so none of them is held by a guard.
func disruptsAction(a metalv1alpha1.HostOperationAction) bool {
	switch {
	case a.PowerCycle != nil, a.PowerOff != nil, a.TalosReboot != nil, a.TalosShutdown != nil,
		a.TalosUpgrade != nil, a.TalosReset != nil, a.EtcdLeave != nil:
		return true
	case a.TalosApplyConfig != nil:
		return a.TalosApplyConfig.Mode == "Reboot" || a.TalosApplyConfig.Mode == "Auto"
	}
	return false
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
	var err error
	if in.handedOff, err = g.handedOff(ctx, host); err != nil {
		return "", err
	}
	cluster := &osv1alpha1.TalosCluster{}
	switch err := g.Reader.Get(ctx, client.ObjectKey{Name: in.self.node.Spec.ClusterRef.Name}, cluster); {
	case err == nil:
		in.cluster = cluster
		if g.Dial != nil {
			in.up = g.controlPlaneUp(ctx, cluster)
		}
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

// handedOff reads whether the claim holding host records the hand-off of the
// node the host runs.
func (g *SelfGuard) handedOff(ctx context.Context, host *metalv1alpha1.Host) (bool, error) {
	ref := host.Spec.ClaimRef
	if ref == nil || host.Spec.OS == nil {
		return false, nil
	}
	claim := &metalv1alpha1.HostClaim{}
	if err := g.Reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, claim); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	h := claim.Status.HandOff
	return h != nil && h.Host == host.Name && h.Node == host.Spec.OS.NodeRef.Name, nil
}

// controlPlaneUp asks a control plane over its Talos API whether it runs and
// is ready: stage running, every readiness condition met, which on a control
// plane includes a healthy etcd service. The answer is the machine's, now. A
// node that cannot be asked (no address, no credentials, no answer within
// probeTimeout) counts as down, so what is not known never adds to the
// majority. Each node is asked at most once per verdict.
func (g *SelfGuard) controlPlaneUp(ctx context.Context, cluster *osv1alpha1.TalosCluster) func(*osv1alpha1.TalosNode) bool {
	bundle, bundleErr := loadBundle(ctx, g.Reader, g.Namespace, cluster)
	asked := map[string]bool{}
	return func(n *osv1alpha1.TalosNode) bool {
		if up, ok := asked[n.Name]; ok {
			return up
		}
		up := false
		if endpoint := render.NodeAddress(n); bundleErr == nil && endpoint != "" {
			if tcfg, err := render.Talosconfig(bundle, cluster.Name, []string{endpoint}, checkinCertTTL); err == nil {
				pctx, cancel := context.WithTimeout(ctx, probeTimeout)
				_ = talos.With(pctx, g.Dial, talos.Target{Endpoint: endpoint, Talosconfig: tcfg}, func(c talos.Client) error {
					st, err := c.Status(pctx)
					up = err == nil && st.Stage == "running" && st.Ready
					return err
				})
				cancel()
			}
		}
		asked[n.Name] = up
		return up
	}
}
