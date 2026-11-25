package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

const (
	// claimAssetIndex joins Hosts back to the claims selecting them (via the asset).
	claimAssetIndex = ".spec.assetRef.name"
	// hostClaimIndex finds the Host(s) a claim currently holds.
	hostClaimIndex = ".spec.claimRef"
)

// HostClaimReconciler binds claims to Hosts PVC-style and provisions what they
// bind. The authoritative pointer is Host.spec.claimRef: the first claim to
// write it wins (apiserver optimistic concurrency — a conflicting writer errors,
// requeues, and observes the winner); losers hold at HostAlreadyClaimed and
// retry level-triggered when the host is released. A bound claim with OS intent
// gets one boot operation proposed (provisioning.go). Claim deletion is held by
// a finalizer until the machine is released: powered off by an operation if it
// was handed an OS, then unbound — delete → Releasing → Available.
type HostClaimReconciler struct {
	client.Client
	// Audit writes the audit trail; nil writes none.
	Audit *audit.Logger
	// OperationsNamespace is where boot and release operations are created
	// (the operator's own).
	OperationsNamespace string
	// Proposer is the operator's own username, stamped as proposedBy.
	Proposer string
	// CheckInTimeoutSeconds bounds a boot operation's wait for the node's
	// check-in; zero defaults to 30 minutes.
	CheckInTimeoutSeconds int32
	// ReleaseShutdownTimeout is how long a release lets the OS shut down
	// before it forces the machine off; zero defaults to 5 minutes.
	ReleaseShutdownTimeout time.Duration
	// APIReader reads straight from the API server, bypassing the informer
	// cache. A release that finds no boot of the claim in the cache unbinds
	// the machine as it is; it confirms that with this reader first, because
	// a cache behind the executor's baseline write would hand back a machine
	// mid-boot. The release reads the hosts the claim holds through it too
	// (see finishRelease). Defaults to the manager's API reader in
	// SetupWithManager.
	APIReader client.Reader
	// Self keeps the operator from proposing a reinstall or a power-off of
	// a machine it runs on (see SelfGuard); nil allows every one.
	Self *SelfGuard
	// Dial opens Talos API connections, to read a cluster's etcd
	// membership before a control plane joins or leaves. nil reads nothing
	// and guards nothing: an operator without a Talos client cannot run the
	// Talos steps either, and the manager always wires one.
	Dial talos.Dialer
	// CredentialsNamespace is where the clusters' secrets bundles are read
	// (the operator's own namespace).
	CredentialsNamespace string
	// SupportsAction says whether the executor runs an action key; nil
	// asks the executor's own list. Plans use only actions it runs.
	SupportsAction func(key string) bool
}

// reader is the uncached reader when one is wired, the client otherwise.
func (r *HostClaimReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// defaultReleaseShutdownTimeout mirrors the manager's flag default.
const defaultReleaseShutdownTimeout = 5 * time.Minute

// +kubebuilder:rbac:groups=metal.pk.io,resources=hostclaims,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostclaims/status,verbs=update
// +kubebuilder:rbac:groups=metal.pk.io,resources=assets,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters;talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=pk-metal

// Reconcile binds a claim to the Host its Asset resolves to, proposes the
// operation that boots the claimed machine into its OS, and releases the Host
// again when the claim goes away. Claims live in any namespace, so these rules
// are cluster-wide. Proposing is create and withdrawing is delete; neither
// update nor patch on operations is granted, and approving is a patch.
func (r *HostClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	bound, err := r.boundHosts(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}

	claim := &metalv1alpha1.HostClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			// The claim is gone without passing through release (created
			// before the finalizer existed, or the finalizer was removed by
			// hand) — unbind what it still holds.
			return ctrl.Result{}, r.releaseAll(ctx, bound, "")
		}
		return ctrl.Result{}, err
	}

	if !claim.DeletionTimestamp.IsZero() {
		return r.finishRelease(ctx, claim)
	}
	if controllerutil.AddFinalizer(claim, releaseFinalizer) {
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	target, failReason, failMsg, err := r.resolveTarget(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}

	orig := claim.DeepCopy()
	claim.Status.HostRef = nil
	switch {
	case failReason != "" && len(bound) > 0:
		// The assetRef stopped resolving under a live bind (asset re-keyed, endpoint
		// lost the system). A resolution failure never releases a machine —
		// alert-and-hold: keep the bind, surface the anomaly.
		claim.Status.HostRef = &metalv1alpha1.Reference{Name: bound[0].Name}
		r.setBound(claim, metav1.ConditionTrue, metalv1alpha1.ReasonBindRetained,
			fmt.Sprintf("bound to host %s; %s", bound[0].Name, failMsg))
	case failReason != "":
		r.setBound(claim, metav1.ConditionFalse, failReason, failMsg)
	default:
		// Re-targeting (edited assetRef/systemRef, pk-metal replace): a
		// machine the claim installed is released the way a deleted claim
		// releases it (its node leaves the cluster, the machine is reset or
		// marked), and only then is the new one bound. That keeps two
		// machines from running the same node, and a control plane's etcd
		// member from staying behind for the new install to collide with.
		var old []metalv1alpha1.Host
		for i := range bound {
			if bound[i].Name != target {
				old = append(old, bound[i])
			}
		}
		if len(old) > 0 {
			done, err := r.release(ctx, claim, old)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !done {
				claim.Status.HostRef = &metalv1alpha1.Reference{Name: old[0].Name}
				r.setBound(claim, metav1.ConditionFalse, metalv1alpha1.ReasonReplacing, fmt.Sprintf(
					"the claim now selects host %s; host %s, which it ran on, is released first (see the Provisioned condition)",
					target, old[0].Name))
				return ctrl.Result{}, r.writeStatus(ctx, orig, claim)
			}
		}
		reason, msg, err := r.bind(ctx, claim, target)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reason == "" {
			claim.Status.HostRef = &metalv1alpha1.Reference{Name: target}
			r.setBound(claim, metav1.ConditionTrue, metalv1alpha1.ReasonHostBound, "host "+target)
		} else {
			r.setBound(claim, metav1.ConditionFalse, reason, msg)
		}
	}
	if claim.Status.HostRef != nil {
		if err := r.provision(ctx, claim, claim.Status.HostRef.Name); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		meta.RemoveStatusCondition(&claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	}
	return etcdWaitResult(claim), r.writeStatus(ctx, orig, claim)
}

// etcdWaitPoll is how often a claim that waits for its turn in etcd looks
// again. A variable for tests only.
var etcdWaitPoll = 15 * time.Second

// etcdWaitResult asks for another look at a claim held for etcd's sake. What
// it waits for is mostly not an object this controller watches: another
// control plane's member becoming a voter, its learner catching up, or the
// operation of another claim finishing. Without the poll the claim would
// wait for an unrelated event, and a scale from one control plane to three
// stopped after the second joined.
func etcdWaitResult(claim *metalv1alpha1.HostClaim) ctrl.Result {
	for _, c := range claim.Status.Conditions {
		if c.Status == metav1.ConditionFalse &&
			(c.Reason == metalv1alpha1.ReasonEtcdJoinWaiting || c.Reason == metalv1alpha1.ReasonRemovalWaiting) {
			return ctrl.Result{RequeueAfter: etcdWaitPoll}
		}
	}
	return ctrl.Result{}
}

func (r *HostClaimReconciler) writeStatus(ctx context.Context, orig, claim *metalv1alpha1.HostClaim) error {
	phase.StampClaim(claim)
	return client.IgnoreNotFound(updateStatusIfChanged(ctx, r.Client, claim, &orig.Status, &claim.Status))
}

// finishRelease runs the release while the finalizer holds the claim, and lets
// the claim go once every machine it held is back. It reads the hosts the
// claim holds from the API server: a cache that has not yet seen a host bound
// would let the finalizer go while the host still carries the claim, and the
// host would then be unbound as it is, with no power-off.
func (r *HostClaimReconciler) finishRelease(ctx context.Context, claim *metalv1alpha1.HostClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, releaseFinalizer) {
		return ctrl.Result{}, nil
	}
	bound, err := r.liveBoundHosts(ctx, client.ObjectKeyFromObject(claim))
	if err != nil {
		return ctrl.Result{}, err
	}
	orig := claim.DeepCopy()
	done, err := r.release(ctx, claim, bound)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return etcdWaitResult(claim), r.writeStatus(ctx, orig, claim)
	}
	controllerutil.RemoveFinalizer(claim, releaseFinalizer)
	if err := r.Update(ctx, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logf.FromContext(ctx).Info("released claim", "claim", claim.Namespace+"/"+claim.Name)
	return ctrl.Result{}, nil
}

func (r *HostClaimReconciler) setBound(claim *metalv1alpha1.HostClaim, s metav1.ConditionStatus, reason, msg string) {
	setCondition(&claim.Status.Conditions, claim.Generation, metalv1alpha1.ConditionBound, s, reason, msg)
}

// resolveTarget walks assetRef → materialized endpoint → discovered systems to the
// one Host name this claim selects. failReason follows the 0/1/2+ rule: no systems
// yet is in flight (HostNotDiscovered), several without a systemRef is Ambiguous.
func (r *HostClaimReconciler) resolveTarget(ctx context.Context, claim *metalv1alpha1.HostClaim) (target, failReason, failMsg string, err error) {
	asset := &metalv1alpha1.Asset{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.AssetRef.Name}, asset); err != nil {
		if apierrors.IsNotFound(err) {
			return "", metalv1alpha1.ReasonAssetNotFound,
				fmt.Sprintf("asset %s not found", claim.Spec.AssetRef.Name), nil
		}
		return "", "", "", err
	}
	ep := &metalv1alpha1.ManagementEndpoint{}
	if err := r.Get(ctx, client.ObjectKey{Name: asset.Name}, ep); err != nil {
		if apierrors.IsNotFound(err) {
			// The asset says why it has no endpoint; waiting does not fix a
			// MAC missing from the ledger.
			if c := meta.FindStatusCondition(asset.Status.Conditions, metalv1alpha1.ConditionMaterialized); c != nil &&
				c.Status == metav1.ConditionFalse {
				return "", metalv1alpha1.ReasonAssetNotMaterialized,
					fmt.Sprintf("asset %s has no endpoint (%s): %s", asset.Name, c.Reason, c.Message), nil
			}
			return "", metalv1alpha1.ReasonHostNotDiscovered,
				fmt.Sprintf("asset %s has no endpoint yet", asset.Name), nil
		}
		return "", "", "", err
	}

	names := claimHostNames(ep, claim.Spec.SystemRef)
	switch {
	case len(names) == 1:
		return names[0], "", "", nil
	case len(names) > 1:
		return "", metalv1alpha1.ReasonAmbiguousSystem,
			fmt.Sprintf("endpoint %s fronts %d systems — set systemRef", ep.Name, len(names)), nil
	case claim.Spec.SystemRef != nil && len(ep.Status.Systems) > 0:
		return "", metalv1alpha1.ReasonSystemNotFound,
			fmt.Sprintf("endpoint %s fronts no system %q", ep.Name, claim.Spec.SystemRef.Name), nil
	}
	return "", metalv1alpha1.ReasonHostNotDiscovered,
		fmt.Sprintf("endpoint %s has discovered no system yet", ep.Name), nil
}

// claimHostNames narrows an endpoint's discovered systems by the optional systemRef
// (a Redfish member id) and normalizes the rest to distinct Host names.
func claimHostNames(ep *metalv1alpha1.ManagementEndpoint, systemRef *metalv1alpha1.Reference) []string {
	var names []string
	for _, sys := range ep.Status.Systems {
		if systemRef != nil && sys.ID != systemRef.Name {
			continue
		}
		if n, ok := identity.Name(identity.Normalize(ep.Status.Vendor, ep.Status.BoardSerial, sys)); ok &&
			!slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// bind writes the claim onto the host — the one contended write in the model. A nil
// pointer is free (first writer wins; a lost race surfaces as an Update conflict and
// requeues into the loser branch); our UID is idempotent; our namespace/name under a
// foreign UID is a deleted predecessor whose pointer we take over.
func (r *HostClaimReconciler) bind(ctx context.Context, claim *metalv1alpha1.HostClaim, target string) (reason, msg string, err error) {
	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, client.ObjectKey{Name: target}, host); err != nil {
		if apierrors.IsNotFound(err) {
			return metalv1alpha1.ReasonHostNotDiscovered,
				fmt.Sprintf("host %s not yet materialized", target), nil
		}
		return "", "", err
	}
	ref := host.Spec.ClaimRef
	switch {
	case ref != nil && ref.UID == claim.UID:
		// Already ours — but the OS intent is editable, so re-stamp on change.
		if desired := hostOSFrom(claim); !equality.Semantic.DeepEqual(host.Spec.OS, desired) {
			host.Spec.OS = desired
			if err := r.Update(ctx, host); err != nil {
				return "", "", err
			}
		}
		return "", "", nil
	case ref != nil && (ref.Namespace != claim.Namespace || ref.Name != claim.Name):
		return metalv1alpha1.ReasonHostAlreadyClaimed,
			fmt.Sprintf("host %s is bound to claim %s/%s", target, ref.Namespace, ref.Name), nil
	}
	host.Spec.ClaimRef = &metalv1alpha1.ClaimReference{
		Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID,
	}
	host.Spec.OS = hostOSFrom(claim)
	if err := r.Update(ctx, host); err != nil {
		return "", "", err
	}
	logf.FromContext(ctx).Info("bound host", "host", target, "claim", claim.Namespace+"/"+claim.Name)
	return "", "", nil
}

// boundHosts lists the hosts whose claimRef points at the given claim coordinates.
func (r *HostClaimReconciler) boundHosts(ctx context.Context, key types.NamespacedName) ([]metalv1alpha1.Host, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts, client.MatchingFields{hostClaimIndex: key.String()}); err != nil {
		return nil, err
	}
	return hosts.Items, nil
}

// liveBoundHosts is boundHosts read from the API server. The API server
// cannot filter by claimRef, so every host is read and filtered here; only a
// release does this, once per pass of a deleted claim.
func (r *HostClaimReconciler) liveBoundHosts(ctx context.Context, key types.NamespacedName) ([]metalv1alpha1.Host, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := r.reader().List(ctx, hosts); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(hosts.Items, func(h metalv1alpha1.Host) bool {
		ref := h.Spec.ClaimRef
		return ref == nil || ref.Namespace != key.Namespace || ref.Name != key.Name
	}), nil
}

// hostOSFrom maps the claim's OS intent onto the Host's hand-off shape; nil in →
// nil out (a claim without intent only reserves the machine).
func hostOSFrom(claim *metalv1alpha1.HostClaim) *metalv1alpha1.HostOS {
	if claim.Spec.OS == nil {
		return nil
	}
	return &metalv1alpha1.HostOS{NodeRef: claim.Spec.OS.NodeRef}
}

// releaseAll clears the bind pointer, the OS hand-off and the release's
// power-off record from every listed host except keep.
func (r *HostClaimReconciler) releaseAll(ctx context.Context, bound []metalv1alpha1.Host, keep string) error {
	for i := range bound {
		host := &bound[i]
		if host.Name == keep {
			continue
		}
		host.Spec.ClaimRef = nil
		host.Spec.OS = nil
		delete(host.Annotations, annotationReleasePowerOff)
		if err := r.Update(ctx, host); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		logf.FromContext(ctx).Info("released host", "host", host.Name)
	}
	return nil
}

func (r *HostClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&metalv1alpha1.HostClaim{}, claimAssetIndex,
		func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.HostClaim).Spec.AssetRef.Name}
		}); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&metalv1alpha1.Host{}, hostClaimIndex,
		func(o client.Object) []string {
			if ref := o.(*metalv1alpha1.Host).Spec.ClaimRef; ref != nil {
				return []string{ref.Namespace + "/" + ref.Name}
			}
			return nil
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.HostClaim{}).
		Watches(&metalv1alpha1.Host{}, handler.EnqueueRequestsFromMapFunc(r.claimsForHost)).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(claimsForOperation)).
		Watches(&metalv1alpha1.ManagementEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.claimsForEndpoint)).
		Watches(&metalv1alpha1.Asset{}, handler.EnqueueRequestsFromMapFunc(r.claimsForAsset)).
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(r.claimsForNode)).
		Watches(&osv1alpha1.BootAsset{}, handler.EnqueueRequestsFromMapFunc(r.claimsForBootAsset)).
		Named("hostclaim").
		Complete(r)
}

// claimsForNode re-runs the claims whose OS intent names the node — its render
// and its check-in both move provisioning.
func (r *HostClaimReconciler) claimsForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.claimsNaming(ctx, obj.GetName())
}

// claimsForBootAsset re-runs the claims of the node a config medium carries;
// an installer image change re-runs every claim with OS intent.
func (r *HostClaimReconciler) claimsForBootAsset(ctx context.Context, obj client.Object) []ctrl.Request {
	if ref := obj.(*osv1alpha1.BootAsset).Spec.NodeRef; ref != nil {
		return r.claimsNaming(ctx, ref.Name)
	}
	return r.claimsNaming(ctx, "")
}

// claimsNaming lists the claims whose OS intent names node ("" = any).
func (r *HostClaimReconciler) claimsNaming(ctx context.Context, node string) []ctrl.Request {
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range claims.Items {
		if os := claims.Items[i].Spec.OS; os != nil && (node == "" || os.NodeRef.Name == node) {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
		}
	}
	return reqs
}

// claimsForEndpoint re-runs the claims selecting the endpoint's asset — its
// discovered media and boot targets shape the boot plan.
func (r *HostClaimReconciler) claimsForEndpoint(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.claimsSelecting(ctx, obj.(*metalv1alpha1.ManagementEndpoint).Spec.AssetRef.Name)
}

// claimsForAsset re-runs the claims selecting the asset — its Materialized
// condition is why a claim waits when the asset has no endpoint.
func (r *HostClaimReconciler) claimsForAsset(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.claimsSelecting(ctx, obj.GetName())
}

// claimsSelecting lists the claims whose assetRef names the asset.
func (r *HostClaimReconciler) claimsSelecting(ctx context.Context, asset string) []ctrl.Request {
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims, client.MatchingFields{claimAssetIndex: asset}); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(claims.Items))
	for i := range claims.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
	}
	return reqs
}

// claimsForHost fans a host event out to its holder (release/re-check) and to every
// claim selecting one of its fronting assets (pending claims retry when the host
// appears or frees up). The asset name is read off the endpoint's spec, not assumed
// from its object name.
func (r *HostClaimReconciler) claimsForHost(ctx context.Context, obj client.Object) []ctrl.Request {
	host := obj.(*metalv1alpha1.Host)
	var reqs []ctrl.Request
	seen := map[types.NamespacedName]bool{}
	add := func(key types.NamespacedName) {
		if !seen[key] {
			seen[key] = true
			reqs = append(reqs, ctrl.Request{NamespacedName: key})
		}
	}
	if ref := host.Spec.ClaimRef; ref != nil {
		add(types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name})
	}
	for _, path := range host.Status.ManagedBy {
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: path.EndpointRef.Name}, ep); err != nil {
			continue
		}
		claims := &metalv1alpha1.HostClaimList{}
		if err := r.List(ctx, claims, client.MatchingFields{claimAssetIndex: ep.Spec.AssetRef.Name}); err != nil {
			continue
		}
		for i := range claims.Items {
			add(types.NamespacedName{Namespace: claims.Items[i].Namespace, Name: claims.Items[i].Name})
		}
	}
	return reqs
}
