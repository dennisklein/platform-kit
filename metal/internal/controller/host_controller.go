package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// hostNameIndex joins endpoints to the Hosts their discovered systems normalize
// to — a computed index (identity, not a spec field), multi-valued and N-ready.
const hostNameIndex = ".status.systems.hostName"

// HostReconciler projects Hosts from ManagementEndpoint statuses — a pure
// cluster-state join, no BMC connection: the endpoint reconciler observes, this
// one aggregates. A Host is keyed by normalized identity (the multipath
// discipline), so every endpoint fronting the same system converges on one Host;
// managedBy[] carries the paths with their independent health. Hosts are owned
// by all fronting endpoints — GC collects a Host only when its last path's
// endpoint is gone.
type HostReconciler struct {
	client.Client
}

// hostPath is one (endpoint, system) pair fronting a host.
type hostPath struct {
	ep  metalv1alpha1.ManagementEndpoint
	sys metalv1alpha1.DiscoveredSystem
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts/status,verbs=patch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch

// Reconcile projects the systems behind the endpoints into one Host and derives
// its lifecycle conditions from the claim, the operations and the OS node.
func (r *HostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	eps := &metalv1alpha1.ManagementEndpointList{}
	if err := r.List(ctx, eps, client.MatchingFields{hostNameIndex: req.Name}); err != nil {
		return ctrl.Result{}, err
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.MatchingFields{opHostIndex: req.Name}); err != nil {
		return ctrl.Result{}, err
	}
	var paths []hostPath
	for _, ep := range eps.Items {
		for _, sys := range ep.Status.Systems {
			if n, ok := identity.Name(identity.Normalize(ep.Status.Vendor, ep.Status.BoardSerial, sys)); ok && n == req.Name {
				paths = append(paths, hostPath{ep: ep, sys: sys})
			}
		}
	}
	if len(paths) == 0 {
		// The last path stopped fronting this host. Nothing to project — an
		// existing Host is left to ownerRef GC (endpoint deletion) or to humans.
		return ctrl.Result{}, nil
	}
	slices.SortFunc(paths, func(a, b hostPath) int {
		if c := strings.Compare(a.ep.Name, b.ep.Name); c != 0 {
			return c
		}
		return strings.Compare(a.sys.ID, b.sys.ID)
	})

	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: req.Name}}
	if !slices.ContainsFunc(paths, func(p hostPath) bool { return p.ep.DeletionTimestamp.IsZero() }) {
		// Only endpoints on their way out still front this host — a claim held
		// them, and its release is letting them go. Materializing the Host anew
		// from them would resurrect what was just deleted, owned by endpoints
		// about to vanish; an existing Host is still projected so its hold
		// stays visible.
		if err := r.Get(ctx, client.ObjectKeyFromObject(host), host); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, host, func() error {
		host.OwnerReferences = endpointOwnerRefs(host.OwnerReferences, paths)
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("materialize host %s: %w", req.Name, err)
	}
	if op == controllerutil.OperationResultCreated {
		logf.FromContext(ctx).Info("materialized host", "host", req.Name, "paths", len(paths))
	}

	// The bound claim, when its UID still matches the pointer — the source of
	// the intent labels (nil after release, which is what clears them).
	var claim *metalv1alpha1.HostClaim
	if ref := host.Spec.ClaimRef; ref != nil {
		c := &metalv1alpha1.HostClaim{}
		switch err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, c); {
		case err == nil && c.UID == ref.UID:
			claim = c
		case client.IgnoreNotFound(err) != nil:
			return ctrl.Result{}, err
		}
	}
	id := identity.Normalize(paths[0].ep.Status.Vendor, paths[0].ep.Status.BoardSerial, paths[0].sys)
	if stampLabels(host, claim, paths[0].ep.Spec.AssetRef.Name, id.Vendor, paths[0].ep.Status.Model) {
		if err := r.Update(ctx, host); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// The hand-off's TalosNode, when one is named and exists — nil feeds the
	// NodeNotFound observation.
	var node *osv1alpha1.TalosNode
	if host.Spec.OS != nil {
		n := &osv1alpha1.TalosNode{}
		switch err := r.Get(ctx, types.NamespacedName{Name: host.Spec.OS.NodeRef.Name}, n); {
		case err == nil:
			node = n
		case client.IgnoreNotFound(err) != nil:
			return ctrl.Result{}, err
		}
	}

	orig := host.DeepCopy()
	projectHost(host, paths, ops.Items, node)
	projectDeletionHold(host)
	facts := factsFor(host, claim)
	phase.StampHost(host, facts)
	if !equality.Semantic.DeepEqual(orig.Status, host.Status) {
		if err := r.applyStatus(ctx, host, facts); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{}, nil
}

// factsFor derives the spec-side phase inputs: claimed while a pointer is set,
// releasing while the claim holding it is being deleted.
func factsFor(host *metalv1alpha1.Host, claim *metalv1alpha1.HostClaim) phase.HostFacts {
	_, maintenance := inMaintenance(host)
	return phase.HostFacts{
		Claimed:     host.Spec.ClaimRef != nil,
		Releasing:   claim != nil && !claim.DeletionTimestamp.IsZero(),
		Maintenance: maintenance,
	}
}

// inMaintenance reports whether a person put the host in maintenance, and
// their reason.
func inMaintenance(host *metalv1alpha1.Host) (string, bool) {
	why, ok := host.Annotations[metalv1alpha1.AnnotationMaintenance]
	return strings.TrimSpace(why), ok
}

// maintenanceNote shapes a host's maintenance reason for a message:
// "host X is in maintenance (reason)".
func maintenanceNote(host *metalv1alpha1.Host) string {
	why, _ := inMaintenance(host)
	if why == "" {
		return fmt.Sprintf("host %s is in maintenance", host.Name)
	}
	return fmt.Sprintf("host %s is in maintenance (%s)", host.Name, why)
}

// endMaintenanceCommand is what a person runs to give the host back.
func endMaintenanceCommand(host *metalv1alpha1.Host) string {
	return "kubectl annotate host " + host.Name + " " + metalv1alpha1.AnnotationMaintenance + "-"
}

// projectMaintenance mirrors the maintenance annotation into the Maintenance
// condition, so the reason reads next to every other condition.
func projectMaintenance(host *metalv1alpha1.Host, setCond func(string, metav1.ConditionStatus, string, string)) {
	why, ok := inMaintenance(host)
	if !ok {
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionMaintenance)
		return
	}
	if why == "" {
		why = "no reason given"
	}
	setCond(metalv1alpha1.ConditionMaintenance, metav1.ConditionTrue, metalv1alpha1.ReasonHostInMaintenance,
		why+"; the operator proposes and starts nothing on it. To give it back: "+endMaintenanceCommand(host))
}

// hostFacts is factsFor for writers that have not read the claim yet — every
// writer of the host's phase must derive it from the same facts, or two
// writers publish two phases in turn.
func hostFacts(ctx context.Context, c client.Client, host *metalv1alpha1.Host) phase.HostFacts {
	var claim *metalv1alpha1.HostClaim
	if ref := host.Spec.ClaimRef; ref != nil {
		cl := &metalv1alpha1.HostClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, cl); err == nil && cl.UID == ref.UID {
			claim = cl
		}
	}
	return factsFor(host, claim)
}

// projectDeletionHold makes the claim protection visible: a Host whose
// deletion was requested (its Asset or endpoint went away) but that a claim
// still holds says so, instead of sitting silently in Terminating.
func projectDeletionHold(host *metalv1alpha1.Host) {
	if host.DeletionTimestamp.IsZero() || host.Spec.ClaimRef == nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionDeletionHeld)
		return
	}
	ref := host.Spec.ClaimRef
	setCondition(&host.Status.Conditions, host.Generation, metalv1alpha1.ConditionDeletionHeld, metav1.ConditionTrue,
		metalv1alpha1.ReasonHostClaimed,
		fmt.Sprintf("deletion requested, held by claim %s/%s — delete the claim to release the machine", ref.Namespace, ref.Name))
}

// applyStatus declares the projected half of the host's status: what the
// fronting endpoints observed, plus status.lastResetTime, the previous
// observation the drift check compares against. The convergence lane's plan
// memory and its two conditions are stripped rather than restated — naming
// them here would claim them.
func (r *HostReconciler) applyStatus(ctx context.Context, host *metalv1alpha1.Host, facts phase.HostFacts) error {
	apply := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: host.Name},
		Status:     *host.Status.DeepCopy(),
	}
	apply.Status.FirmwareProposed = nil
	apply.Status.Conditions = slices.DeleteFunc(apply.Status.Conditions, isConvergenceHostCondition)
	phase.StampHostOnto(apply, host, facts)
	return applyOwnedStatus(ctx, r.Client, apply, fieldOwnerHost)
}

// projectHost rewrites the host's identity, paths, and conditions from what the
// fronting endpoints observed plus the operations acting on it. Derived except
// for one deliberate memory: status.lastResetTime is the previous observation
// the drift check compares against (the same status-as-cache pattern as the
// endpoint's AssetSnapshot).
func projectHost(host *metalv1alpha1.Host, paths []hostPath, ops []metalv1alpha1.HostOperation, node *osv1alpha1.TalosNode) {
	setCond := func(t string, s metav1.ConditionStatus, reason, msg string) {
		setCondition(&host.Status.Conditions, host.Generation, t, s, reason, msg)
	}

	// Identity must byte-agree across paths — divergence means the same serial
	// anchor resolves to different machine facts, so inspection cannot produce a
	// coherent identity: alert-and-hold, keep the first (sorted) projection.
	id := identity.Normalize(paths[0].ep.Status.Vendor, paths[0].ep.Status.BoardSerial, paths[0].sys)
	diverged := ""
	for _, p := range paths[1:] {
		if got := identity.Normalize(p.ep.Status.Vendor, p.ep.Status.BoardSerial, p.sys); got != id {
			diverged = fmt.Sprintf("endpoint %s reports identity %+v, endpoint %s reports %+v",
				paths[0].ep.Name, id, p.ep.Name, got)
			break
		}
	}
	host.Status.Identity = &id

	reachable := 0
	managedBy := make([]metalv1alpha1.ManagementPath, 0, len(paths))
	for _, p := range paths {
		up := meta.IsStatusConditionTrue(p.ep.Status.Conditions, metalv1alpha1.ConditionReachable)
		if up {
			reachable++
		}
		managedBy = append(managedBy, metalv1alpha1.ManagementPath{
			EndpointRef: metalv1alpha1.Reference{Name: p.ep.Name},
			SystemID:    p.sys.ID,
			Reachable:   up,
		})
	}
	host.Status.ManagedBy = managedBy

	pathMsg := fmt.Sprintf("%d/%d paths reachable", reachable, len(paths))
	if reachable > 0 {
		setCond(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "PathUp", pathMsg)
	} else {
		setCond(metalv1alpha1.ConditionReachable, metav1.ConditionFalse, "AllPathsDown", pathMsg)
	}

	if diverged != "" {
		setCond(metalv1alpha1.ConditionInspected, metav1.ConditionFalse, metalv1alpha1.ReasonIdentityDiverged, diverged)
	} else {
		setCond(metalv1alpha1.ConditionInspected, metav1.ConditionTrue, "RedfishFacts", "")
	}

	vstatus, vreason, vmsg := hostVerified(paths)
	setCond(metalv1alpha1.ConditionVerified, vstatus, vreason, vmsg)

	projectMaintenance(host, setCond)
	projectApproval(setCond, ops)
	projectDrift(host, setCond, paths, ops)
	projectOS(host, setCond, node)
}

// projectOS joins the hand-off's TalosNode check-in observations into the OS
// conditions: Booted mirrors MachineReachable (the machine came up — reason
// says how far), OSReady mirrors Ready (NodeReady is what makes Provisioned).
// Without a hand-off both conditions leave — a released machine must not keep
// reporting an OS.
func projectOS(host *metalv1alpha1.Host, setCond func(string, metav1.ConditionStatus, string, string), node *osv1alpha1.TalosNode) {
	if host.Spec.OS == nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionBooted)
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionOSReady)
		return
	}
	if node == nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionBooted)
		setCond(metalv1alpha1.ConditionOSReady, metav1.ConditionUnknown, metalv1alpha1.ReasonNodeNotFound,
			fmt.Sprintf("hand-off names no TalosNode %q", host.Spec.OS.NodeRef.Name))
		return
	}

	if reach := meta.FindStatusCondition(node.Status.Conditions, osv1alpha1.ConditionMachineReachable); reach != nil &&
		reach.Status == metav1.ConditionTrue {
		setCond(metalv1alpha1.ConditionBooted, metav1.ConditionTrue, reach.Reason, reach.Message)
	} else {
		// Not up (yet) — in flight, never a failure verdict from a dial alone.
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionBooted)
	}

	ready := meta.FindStatusCondition(node.Status.Conditions, osv1alpha1.ConditionReady)
	if ready == nil {
		setCond(metalv1alpha1.ConditionOSReady, metav1.ConditionUnknown,
			metalv1alpha1.ReasonAwaitingContact, "no check-in observation yet")
		return
	}
	setCond(metalv1alpha1.ConditionOSReady, ready.Status, ready.Reason, ready.Message)
}

// projectApproval surfaces a proposed operation's human gate on the Host —
// blockedOn renders it as Approval(<domain>) via the phase derivation.
func projectApproval(setCond func(string, metav1.ConditionStatus, string, string), ops []metalv1alpha1.HostOperation) {
	var pending []metalv1alpha1.HostOperation
	for _, op := range ops {
		if opAwaitsApproval(&op) {
			pending = append(pending, op)
		}
	}
	if len(pending) == 0 {
		setCond(metalv1alpha1.ConditionApprovalPending, metav1.ConditionFalse, "NonePending", "")
		return
	}
	slices.SortFunc(pending, func(a, b metalv1alpha1.HostOperation) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	op := pending[0]
	setCond(metalv1alpha1.ConditionApprovalPending, metav1.ConditionTrue, approvalDomain(&op),
		fmt.Sprintf("operation %s/%s awaits approval", op.Namespace, op.Name))
}

// projectDrift advances the reset-time memory and raises Drifted when the world
// moved with no operation explaining it — information, not error (design doc
// § State). An advance is explained by an active operation (in flight over the
// machine) or by a finished one whose closing observation covers it.
func projectDrift(host *metalv1alpha1.Host, setCond func(string, metav1.ConditionStatus, string, string), paths []hostPath, ops []metalv1alpha1.HostOperation) {
	var newest *metav1.MicroTime
	for _, p := range paths {
		if t := p.sys.LastResetTime; t != nil && (newest == nil || t.After(newest.Time)) {
			newest = t
		}
	}
	if newest == nil {
		return // vendor serves no observable — drift detection degrades openly
	}
	prev := host.Status.LastResetTime
	host.Status.LastResetTime = newest
	if prev == nil || !newest.After(prev.Time) {
		return
	}
	explained := false
	for _, op := range ops {
		if opActive(&op) || (op.Status.ObservedResetTime != nil && !newest.After(op.Status.ObservedResetTime.Time)) {
			explained = true
			break
		}
	}
	if explained {
		setCond(metalv1alpha1.ConditionDrifted, metav1.ConditionFalse, metalv1alpha1.ReasonResetExplained,
			fmt.Sprintf("reset at %s covered by an operation", newest.Format(time.RFC3339)))
		return
	}
	setCond(metalv1alpha1.ConditionDrifted, metav1.ConditionTrue, metalv1alpha1.ReasonUnexpectedReset,
		fmt.Sprintf("reset-time advanced to %s with no active operation", newest.Format(time.RFC3339)))
}

// hostVerified aggregates the paths' endpoint verifications: any failed path
// condemns the host (one alarming view is an alarm, however many agree), all
// verified paths clear it, anything else is still in flight.
func hostVerified(paths []hostPath) (metav1.ConditionStatus, string, string) {
	verified := 0
	for _, p := range paths {
		c := meta.FindStatusCondition(p.ep.Status.Conditions, metalv1alpha1.ConditionVerified)
		if c == nil {
			continue
		}
		switch c.Status {
		case metav1.ConditionFalse:
			return metav1.ConditionFalse, c.Reason, fmt.Sprintf("endpoint %s: %s", p.ep.Name, c.Message)
		case metav1.ConditionTrue:
			verified++
		}
	}
	if verified == len(paths) {
		return metav1.ConditionTrue, "AllPathsVerified", ""
	}
	return metav1.ConditionUnknown, "Verifying", fmt.Sprintf("%d/%d paths verified", verified, len(paths))
}

// endpointOwnerRefs rebuilds the endpoint-kind owner references to the current
// fronting set (paths are pre-sorted), preserving refs of any other kind.
func endpointOwnerRefs(existing []metav1.OwnerReference, paths []hostPath) []metav1.OwnerReference {
	gvk := metalv1alpha1.GroupVersion.WithKind("ManagementEndpoint")
	var refs []metav1.OwnerReference
	for _, ref := range existing {
		if ref.Kind != gvk.Kind || ref.APIVersion != gvk.GroupVersion().String() {
			refs = append(refs, ref)
		}
	}
	seen := map[types.UID]bool{}
	for _, p := range paths {
		if seen[p.ep.UID] {
			continue
		}
		seen[p.ep.UID] = true
		refs = append(refs, metav1.OwnerReference{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       p.ep.Name,
			UID:        p.ep.UID,
		})
	}
	return refs
}

// hostNames computes which Hosts an endpoint's discovered systems normalize to —
// shared by the field index and the watch fan-out.
func hostNames(ep *metalv1alpha1.ManagementEndpoint) []string {
	var names []string
	for _, sys := range ep.Status.Systems {
		if n, ok := identity.Name(identity.Normalize(ep.Status.Vendor, ep.Status.BoardSerial, sys)); ok {
			names = append(names, n)
		}
	}
	return names
}

func (r *HostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&metalv1alpha1.ManagementEndpoint{}, hostNameIndex,
		func(o client.Object) []string {
			return hostNames(o.(*metalv1alpha1.ManagementEndpoint))
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.Host{}).
		Watches(&metalv1alpha1.ManagementEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.hostsForEndpoint)).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(hostForOperation)).
		Watches(&metalv1alpha1.HostClaim{}, handler.EnqueueRequestsFromMapFunc(r.hostsForClaim)).
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(r.hostsForNode)).
		Named("host").
		Complete(r)
}

// hostsForClaim re-stamps the holder's labels when the claim's intent map is
// edited. List-and-filter, same rationale as hostsForNode — and no dependence
// on another reconciler's field index.
func (r *HostReconciler) hostsForClaim(ctx context.Context, obj client.Object) []ctrl.Request {
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range hosts.Items {
		if ref := hosts.Items[i].Spec.ClaimRef; ref != nil &&
			ref.Namespace == obj.GetNamespace() && ref.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: hosts.Items[i].Name}})
		}
	}
	return reqs
}

// hostsForNode re-projects the Hosts whose hand-off names the node — its
// check-in observations feed Booted/OSReady. List-and-filter, same rationale
// as nodeHandedOff.
func (r *HostReconciler) hostsForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range hosts.Items {
		if os := hosts.Items[i].Spec.OS; os != nil && os.NodeRef.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: hosts.Items[i].Name}})
		}
	}
	return reqs
}

// hostForOperation re-projects the host when an operation appears, gets
// approved, or finishes — ApprovalPending and Drifted depend on them.
func hostForOperation(_ context.Context, obj client.Object) []ctrl.Request {
	op := obj.(*metalv1alpha1.HostOperation)
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: op.Spec.HostRef.Name}}}
}

// hostsForEndpoint fans an endpoint event out to the Hosts it fronts; on
// endpoint deletion the last-known status still names them, so the projection
// re-runs against the surviving paths.
func (r *HostReconciler) hostsForEndpoint(_ context.Context, obj client.Object) []ctrl.Request {
	names := hostNames(obj.(*metalv1alpha1.ManagementEndpoint))
	reqs := make([]ctrl.Request, 0, len(names))
	for _, n := range names {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: n}})
	}
	return reqs
}
