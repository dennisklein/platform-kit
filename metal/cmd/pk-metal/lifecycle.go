package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The node lifecycle verbs. None of them touches a machine: each writes the
// request onto the object it concerns, the operator proposes the operation,
// and a person approves it with pk-metal approve, as for every other change.

func init() {
	verbHelp["wipe"] = `
A machine released while its node did not answer could not be reset: its
disk may still hold the node, with its cluster's credentials. The operator
marks such a Host (DiskWiped=False) and installs nothing else on it until it
is wiped. wipe asks for that: the operator proposes an operation that boots a
Talos image which erases the system disk, and stops there. Approve it with
pk-metal approve.

The image's Talos version defaults to the version of the cluster the node
on the disk belonged to.`
	verbHelp["reinstall"] = `
reinstall wipes and installs the same node on the same machine again: for a
corrupted install, or after a disk was replaced (edit the TalosNode's
installDiskSelector first). The operation drains the node, takes a control
plane out of etcd, resets the system disk, boots the installer and waits for
the node to check in again. Approve it with pk-metal approve.

A control plane of a three-member cluster is down to two members while it
reinstalls, an even count the operator refuses without --ack-etcd-risk.`
	verbHelp["replace"] = `
replace moves a node to another machine, for hardware that failed: the
claim is pointed at the new machine, the old one is released first (its node
leaves the cluster and its etcd member is removed; a machine that does not
answer is powered off and marked for a wipe), and then the node is
installed on the new machine. The node keeps its name, so its etcd member
must go before it joins again: a control plane of a three-member cluster is
at two members in between, which needs --ack-etcd-risk.

A TalosNode that names its install disk by serial or its network link by
MAC address fits one machine only: edit those to the new machine's values
before replace, or the install finds no disk and the node no link.

For hardware that still works, add the new node first and remove the old
one after (see https://dennisklein.github.io/platform-kit/workflows/add-and-remove-nodes/):
etcd then never runs short.`
	verbHelp["scale"] = `
scale prints the plan that takes a cluster to the given number of control
planes and workers: which TalosNodes get a machine, which free hosts they
claim, which claims are deleted, and in which order the operator works
through them. Nothing changes without --apply.

Growing needs TalosNodes written ahead (their network, disk and role are
yours to say) and free hosts (Available, unclaimed, wiped). Each new node
gets a HostClaim named after it. Control planes join one at a time.

Shrinking deletes the claims of the highest-named nodes: their nodes leave
the cluster, the machines are reset and go back to the pool. The TalosNodes
stay, without a machine, ready for the next scale-up. Control planes leave
one at a time, the etcd leader last. A plan that passes through an even
number of control planes needs --ack-etcd-risk.

Claims written here are not in git; if you keep them in git, write the
plan's claims there instead.`
}

func wipeCmd(args []string) {
	fs := newFlags("wipe")
	version := fs.String("talos-version", "", "Talos version of the wipe image (default: the version of the node's cluster)")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one host")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if err := wipe(ctx, cl, fs.Arg(0), *version, os.Stdout); err != nil {
		die(err)
	}
}

// wipe marks a not-wiped host for a wipe.
func wipe(ctx context.Context, cl client.Client, name, version string, w io.Writer) error {
	host := &metalv1alpha1.Host{}
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, host); err != nil {
		return fmt.Errorf("host %s: %w", name, err)
	}
	mark, ok := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]
	if !ok {
		return fmt.Errorf("host %s is not marked as holding an install (%s); a release that reset it through Talos wiped it already",
			name, metalv1alpha1.AnnotationDiskNotWiped)
	}
	if version == "" {
		clusterName, _, _ := strings.Cut(mark, "/")
		cluster := &osv1alpha1.TalosCluster{}
		if err := cl.Get(ctx, client.ObjectKey{Name: clusterName}, cluster); err != nil {
			return fmt.Errorf("cluster %s of the node on host %s is not readable (%v); give the image's version: --talos-version v1.x.y",
				clusterName, name, err)
		}
		version = cluster.Spec.TalosVersion
	}
	if err := patchAnnotations(ctx, cl, host, map[string]any{metalv1alpha1.AnnotationWipe: version}); err != nil {
		return fmt.Errorf("update host %s: %w", name, err)
	}
	fmt.Fprintf(w, "host %s: wipe asked for (it may still hold node %s).\n", name, mark)
	fmt.Fprintf(w, "The operator proposes an operation that boots a Talos %s image which erases the system disk; approve it with: pk-metal approve\n", version)
	return nil
}

func reinstallCmd(args []string) {
	fs := newFlags("reinstall")
	ns := fs.String("n", claimNamespace, "namespace of the claim")
	ack := fs.String("ack-etcd-risk", "", "reason to accept an even or short etcd membership while a control plane reinstalls")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one claim or node")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if err := reinstall(ctx, cl, *ns, fs.Arg(0), *ack, os.Stdout); err != nil {
		die(err)
	}
}

// findClaim reads a claim by namespace/name or name, or else the claim whose
// OS intent names the node.
func findClaim(ctx context.Context, cl client.Client, namespace, ref string) (*metalv1alpha1.HostClaim, error) {
	ns, name, err := splitRef(ref, namespace, false)
	if err != nil {
		return nil, err
	}
	c := &metalv1alpha1.HostClaim{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, c); err == nil {
		return c, nil
	}
	claims := &metalv1alpha1.HostClaimList{}
	if err := cl.List(ctx, claims); err != nil {
		return nil, err
	}
	for i := range claims.Items {
		if os := claims.Items[i].Spec.OS; os != nil && os.NodeRef.Name == ref {
			return &claims.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no claim %s/%s, and no claim names a node %s", ns, name, ref)
}

// reinstall asks for the claim's node to be installed again.
func reinstall(ctx context.Context, cl client.Client, namespace, ref, ack string, w io.Writer) error {
	c, err := findClaim(ctx, cl, namespace, ref)
	if err != nil {
		return err
	}
	cref := c.Namespace + "/" + c.Name
	h := c.Status.HandOff
	if h == nil {
		return fmt.Errorf("claim %s records no install to replace; a claim that never handed off installs on its own", cref)
	}
	set := map[string]any{metalv1alpha1.AnnotationReinstall: h.At.UTC().Format(time.RFC3339)}
	if ack != "" {
		set[metalv1alpha1.AnnotationAckEtcdRisk] = ack
	}
	if err := patchAnnotations(ctx, cl, c, set); err != nil {
		return fmt.Errorf("update claim %s: %w", cref, err)
	}
	fmt.Fprintf(w, "claim %s: reinstall of node %s on host %s asked for (the install of %s).\n",
		cref, h.Node, h.Host, h.At.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "The operator proposes one operation: drain, leave etcd (control plane), reset the system disk, install, wait for the check-in.")
	fmt.Fprintln(w, "Approve it with: pk-metal approve. pk-metal status shows what it waits for.")
	return nil
}

func replaceCmd(args []string) {
	fs := newFlags("replace")
	ns := fs.String("n", claimNamespace, "namespace of the claim")
	hostName := fs.String("host", "", "the machine (Host) to move the node to; it must be Available")
	ack := fs.String("ack-etcd-risk", "", "reason to accept an even or short etcd membership while a control plane moves")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 || *hostName == "" {
		badUsage(fs, "give one claim or node, and --host")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if err := replace(ctx, cl, *ns, fs.Arg(0), *hostName, *ack, os.Stdout); err != nil {
		die(err)
	}
}

// hostAsset is the Asset and the system id a claim selects to bind host. A
// management endpoint carries the name of its Asset, and the host's first
// management path names the endpoint and the system behind it.
func hostAsset(host *metalv1alpha1.Host) (asset, system string, err error) {
	if len(host.Status.ManagedBy) == 0 || host.Status.ManagedBy[0].EndpointRef.Name == "" {
		return "", "", fmt.Errorf("host %s has no management path yet; wait until the operator has discovered it", host.Name)
	}
	p := host.Status.ManagedBy[0]
	return p.EndpointRef.Name, p.SystemID, nil
}

// freeHost: the host can be claimed for a new node now.
func freeHost(host *metalv1alpha1.Host) string {
	switch _, dirty := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]; {
	case host.Spec.ClaimRef != nil:
		return "claimed by " + host.Spec.ClaimRef.Namespace + "/" + host.Spec.ClaimRef.Name
	case dirty:
		return "not wiped (pk-metal wipe " + host.Name + ")"
	case host.Status.Phase != metalv1alpha1.HostAvailable:
		return "phase " + string(host.Status.Phase)
	}
	return ""
}

// replace points the claim at another machine.
func replace(ctx context.Context, cl client.Client, namespace, ref, hostName, ack string, w io.Writer) error {
	c, err := findClaim(ctx, cl, namespace, ref)
	if err != nil {
		return err
	}
	cref := c.Namespace + "/" + c.Name
	host := &metalv1alpha1.Host{}
	if err := cl.Get(ctx, client.ObjectKey{Name: hostName}, host); err != nil {
		return fmt.Errorf("host %s: %w", hostName, err)
	}
	if why := freeHost(host); why != "" {
		return fmt.Errorf("host %s cannot take the node: %s", hostName, why)
	}
	asset, system, err := hostAsset(host)
	if err != nil {
		return err
	}
	spec := map[string]any{"assetRef": map[string]any{"name": asset}}
	if system != "" {
		spec["systemRef"] = map[string]any{"name": system}
	}
	patch := map[string]any{"spec": spec}
	if ack != "" {
		patch["metadata"] = map[string]any{"annotations": map[string]any{metalv1alpha1.AnnotationAckEtcdRisk: ack}}
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if err := cl.Patch(ctx, c, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("update claim %s: %w", cref, err)
	}
	old := "(none)"
	if c.Status.HostRef != nil {
		old = c.Status.HostRef.Name
	}
	fmt.Fprintf(w, "claim %s now selects host %s (asset %s); it ran on host %s.\n", cref, hostName, asset, old)
	fmt.Fprintln(w, "The operator first releases the old machine (its node leaves the cluster; a machine that does not answer is powered off and marked for a wipe),")
	fmt.Fprintln(w, "then installs the node on the new one. Each step waits for: pk-metal approve")
	return nil
}

// patchAnnotations sets (or, with a nil value, removes) annotations.
func patchAnnotations(ctx context.Context, cl client.Client, obj client.Object, set map[string]any) error {
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": set}})
	if err != nil {
		return err
	}
	return cl.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
}

func scaleCmd(args []string) {
	fs := newFlags("scale")
	ns := fs.String("n", claimNamespace, "namespace for the claims of new nodes")
	cps := fs.Int("control-planes", -1, "control planes wanted (required)")
	workers := fs.Int("workers", -1, "workers wanted (required)")
	apply := fs.Bool("apply", false, "create and delete the claims of the plan")
	ack := fs.String("ack-etcd-risk", "", "reason to accept passing through an even number of control planes")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 || *cps < 0 || *workers < 0 {
		badUsage(fs, "give one cluster, --control-planes and --workers")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx, stop := signalContext()
	defer stop()
	plan, err := planScale(ctx, cl, fs.Arg(0), *cps, *workers, *ns)
	if err != nil {
		die(err)
	}
	plan.print(os.Stdout)
	if !*apply {
		fmt.Println("\nNothing changed. To do it: add --apply")
		return
	}
	if err := plan.apply(ctx, cl, *ack, os.Stdout); err != nil {
		die(err)
	}
}

// scalePlan is what scale does: claims to create and claims to delete, in
// the order the operator works through them.
type scalePlan struct {
	cluster string
	// now and want are control planes and workers.
	nowCP, nowW, wantCP, wantW int
	add                        []scaleAdd
	remove                     []*metalv1alpha1.HostClaim
	// evenStep: the control-plane count passes through an even number.
	evenStep bool
	problems []string
}

type scaleAdd struct {
	node  string
	role  osv1alpha1.NodeRole
	host  string
	claim *metalv1alpha1.HostClaim
}

// planScale reads the cluster and pairs nodes with hosts.
func planScale(ctx context.Context, cl client.Client, cluster string, wantCP, wantW int, namespace string) (*scalePlan, error) {
	if err := cl.Get(ctx, client.ObjectKey{Name: cluster}, &osv1alpha1.TalosCluster{}); err != nil {
		return nil, fmt.Errorf("cluster %s: %w", cluster, err)
	}
	if wantCP == 0 {
		return nil, fmt.Errorf("a cluster needs at least one control plane; to remove the cluster, delete its claims and its TalosCluster")
	}
	nodes := &osv1alpha1.TalosNodeList{}
	claims := &metalv1alpha1.HostClaimList{}
	hosts := &metalv1alpha1.HostList{}
	for _, l := range []client.ObjectList{nodes, claims, hosts} {
		if err := cl.List(ctx, l); err != nil {
			return nil, err
		}
	}
	claimOf := map[string]*metalv1alpha1.HostClaim{}
	for i := range claims.Items {
		c := &claims.Items[i]
		if c.Spec.OS != nil && c.DeletionTimestamp.IsZero() {
			claimOf[c.Spec.OS.NodeRef.Name] = c
		}
	}
	p := &scalePlan{cluster: cluster, wantCP: wantCP, wantW: wantW}
	var members, spare [2][]string // by role: 0 control plane, 1 worker
	roleIx := func(n *osv1alpha1.TalosNode) int {
		if n.Spec.Role == osv1alpha1.RoleControlPlane {
			return 0
		}
		return 1
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.ClusterRef.Name != cluster || !n.DeletionTimestamp.IsZero() {
			continue
		}
		if claimOf[n.Name] != nil {
			members[roleIx(n)] = append(members[roleIx(n)], n.Name)
		} else {
			spare[roleIx(n)] = append(spare[roleIx(n)], n.Name)
		}
	}
	for i := range members {
		slices.Sort(members[i])
		slices.Sort(spare[i])
	}
	p.nowCP, p.nowW = len(members[0]), len(members[1])
	var free []string
	for i := range hosts.Items {
		if freeHost(&hosts.Items[i]) == "" {
			if _, _, err := hostAsset(&hosts.Items[i]); err == nil {
				free = append(free, hosts.Items[i].Name)
			}
		}
	}
	slices.Sort(free)
	hostByName := map[string]*metalv1alpha1.Host{}
	for i := range hosts.Items {
		hostByName[hosts.Items[i].Name] = &hosts.Items[i]
	}
	// Control planes first: a cluster grows its etcd before its workers,
	// and shrinks its workers before its etcd.
	var removing [2][]*metalv1alpha1.HostClaim
	spareNames := map[string]bool{}
	for _, names := range spare {
		for _, n := range names {
			spareNames[n] = true
		}
	}
	for ix, want := range []int{wantCP, wantW} {
		role := []osv1alpha1.NodeRole{osv1alpha1.RoleControlPlane, osv1alpha1.RoleWorker}[ix]
		have := len(members[ix])
		for k := 0; k < want-have; k++ {
			if k >= len(spare[ix]) {
				p.problems = append(p.problems, fmt.Sprintf("%d more %s TalosNode(s) of cluster %s are needed: write them (address, disk, links) first",
					want-have-k, role, cluster))
				break
			}
			if len(free) == 0 {
				p.problems = append(p.problems, fmt.Sprintf("no free host left for %s: an Available, unclaimed, wiped Host is needed", spare[ix][k]))
				break
			}
			// A TalosNode often fits one machine only (its install disk by
			// serial, its link by MAC): the host of the asset named like the
			// node is taken when it is free.
			// A host written for another spare node is left for that one.
			pick := -1
			for j, h := range free {
				a, _, _ := hostAsset(hostByName[h])
				if a == spare[ix][k] {
					pick = j
					break
				}
				if pick < 0 && !spareNames[a] {
					pick = j
				}
			}
			if pick < 0 {
				pick = 0
			}
			hostName := free[pick]
			free = slices.Delete(free, pick, pick+1)
			asset, system, _ := hostAsset(hostByName[hostName])
			c := &metalv1alpha1.HostClaim{}
			c.Namespace, c.Name = namespace, spare[ix][k]
			c.Spec.AssetRef = metalv1alpha1.Reference{Name: asset}
			if system != "" {
				c.Spec.SystemRef = &metalv1alpha1.Reference{Name: system}
			}
			c.Spec.OS = &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: spare[ix][k]}}
			p.add = append(p.add, scaleAdd{node: spare[ix][k], role: role, host: hostName, claim: c})
		}
		// The highest-named go first; the operator itself keeps the etcd
		// leader for last among control planes leaving together.
		for k := have - 1; k >= want; k-- {
			removing[ix] = append(removing[ix], claimOf[members[ix][k]])
		}
	}
	p.remove = append(removing[1], removing[0]...)
	for n := p.nowCP - 1; n >= wantCP && n > 0; n-- {
		if n%2 == 0 {
			p.evenStep = true
		}
	}
	return p, nil
}

func (p *scalePlan) print(w io.Writer) {
	fmt.Fprintf(w, "cluster %s: %d control plane(s) and %d worker(s) now, %d and %d wanted\n", p.cluster, p.nowCP, p.nowW, p.wantCP, p.wantW)
	if p.wantCP%2 == 0 {
		fmt.Fprintf(w, "warning: %d control planes is an even number: it tolerates no more failures than %d\n", p.wantCP, p.wantCP-1)
	}
	if len(p.add) == 0 && len(p.remove) == 0 {
		fmt.Fprintln(w, "nothing to do")
	}
	step := 1
	for _, a := range p.add {
		how := "installed"
		if a.role == osv1alpha1.RoleControlPlane {
			how = "joins etcd once every member is healthy and the one before has joined"
		}
		fmt.Fprintf(w, "%2d. add %s %s on host %s: claim %s/%s; %s\n", step, a.role, a.node, a.host, a.claim.Namespace, a.claim.Name, how)
		step++
	}
	if len(p.add) > 0 {
		fmt.Fprintln(w, "check: each added TalosNode's install disk and network links must fit the host it is paired with")
	}
	for _, c := range p.remove {
		fmt.Fprintf(w, "%2d. remove %s from host %s: delete claim %s/%s; the node is drained and leaves the cluster, the machine is reset and returns to the pool\n",
			step, c.Spec.OS.NodeRef.Name, hostOf(c), c.Namespace, c.Name)
		step++
	}
	if p.evenStep {
		fmt.Fprintln(w, "note: control planes leave one at a time, the etcd leader last; on the way the cluster has an even number of them,")
		fmt.Fprintln(w, "      which the operator refuses unless the removals carry --ack-etcd-risk")
	}
	for _, s := range p.problems {
		fmt.Fprintln(w, "cannot: "+s)
	}
}

func hostOf(c *metalv1alpha1.HostClaim) string {
	if c.Status.HostRef != nil {
		return c.Status.HostRef.Name
	}
	return "(none)"
}

// apply creates and deletes the plan's claims. Each operation the operator
// proposes from them still waits for approval.
func (p *scalePlan) apply(ctx context.Context, cl client.Client, ack string, w io.Writer) error {
	if len(p.problems) > 0 {
		return fmt.Errorf("the plan cannot be carried out as it is; nothing was changed")
	}
	if p.evenStep && ack == "" {
		return fmt.Errorf("the plan passes through an even number of control planes; give --ack-etcd-risk \"<reason>\" to accept it")
	}
	for _, a := range p.add {
		if err := cl.Create(ctx, a.claim); err != nil {
			return fmt.Errorf("create claim %s/%s: %w", a.claim.Namespace, a.claim.Name, err)
		}
		fmt.Fprintf(w, "created claim %s/%s\n", a.claim.Namespace, a.claim.Name)
	}
	for _, c := range p.remove {
		if ack != "" {
			if err := patchAnnotations(ctx, cl, c, map[string]any{metalv1alpha1.AnnotationAckEtcdRisk: ack}); err != nil {
				return fmt.Errorf("acknowledge on claim %s/%s: %w", c.Namespace, c.Name, err)
			}
		}
		if err := cl.Delete(ctx, c); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete claim %s/%s: %w", c.Namespace, c.Name, err)
		}
		fmt.Fprintf(w, "deleted claim %s/%s\n", c.Namespace, c.Name)
	}
	fmt.Fprintln(w, "The operator proposes the operations; approve each with: pk-metal approve")
	return nil
}
