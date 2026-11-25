package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
)

// Wiping a machine that was released without a Talos reset. Its disk may
// still hold a node of some cluster, with that cluster's credentials, and it
// must not boot again as that node. A person asks for the wipe (pk-metal
// wipe sets AnnotationWipe), the operator proposes one operation and a
// person approves it: mount a Talos image whose kernel command line wipes
// the system disk at boot (talos.experimental.wipe=system), make it the
// next boot, and power-cycle. The machine then sits in Talos maintenance
// mode, running from the image, with nothing on its disk; no config and no
// credentials. When the operation succeeded, both annotations go.
//
// The image stays mounted: ejecting it while the machine boots could leave
// the old disk to boot instead. The next install empties every slot first.

const (
	purposeWipe = "wipe"
	// wipeKernelArg makes Talos wipe the system disk before it boots.
	wipeKernelArg = "talos.experimental.wipe=system"
)

// hostWipeReconciler proposes the wipe operation of a Host that asks for
// one. It shares the claim reconciler's configuration and its reading of
// endpoints.
type hostWipeReconciler struct {
	*HostClaimReconciler
}

// Reconcile proposes or completes a host's wipe.
func (r *hostWipeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	version, asked := host.Annotations[metalv1alpha1.AnnotationWipe]
	if !asked {
		return ctrl.Result{}, nil
	}
	mark, dirty := host.Annotations[metalv1alpha1.AnnotationDiskNotWiped]
	if !dirty {
		// Nothing to wipe (wiped already, or never marked).
		delete(host.Annotations, metalv1alpha1.AnnotationWipe)
		return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, host))
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.InNamespace(r.OperationsNamespace), client.MatchingLabels{
		labelPurpose: purposeWipe, labelWipeHost: host.Name,
	}); err != nil {
		return ctrl.Result{}, err
	}
	version = strings.TrimSpace(version)
	name := fmt.Sprintf("wipe-%s-%s", host.Name, hash8(planHash([]string{version, mark})))
	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Name != name {
			continue
		}
		if opSucceeded(op) {
			delete(host.Annotations, metalv1alpha1.AnnotationWipe)
			delete(host.Annotations, metalv1alpha1.AnnotationDiskNotWiped)
			if err := r.Update(ctx, host); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			logf.FromContext(ctx).Info("host wiped", "host", host.Name, "operation", op.Namespace+"/"+op.Name)
		}
		return ctrl.Result{}, nil
	}
	if _, maint := inMaintenance(host); maint {
		return ctrl.Result{}, nil
	}
	if r.installedHere(ctx, host.Name) {
		// A claim runs a node on it: that is no leftover, and a wipe would
		// destroy it. The claim's own removal or reinstall resets it.
		logf.FromContext(ctx).Info("wipe not proposed: a claim runs a node on the host", "host", host.Name)
		return ctrl.Result{}, nil
	}
	if msg, err := r.Self.Check(ctx, host); err != nil || msg != "" {
		return ctrl.Result{}, err
	}
	asset, err := r.ensureWipeAsset(ctx, version)
	if err != nil {
		return ctrl.Result{}, err
	}
	if installerPending(asset) != "" {
		// The asset's watch brings the host back when it is ready.
		return ctrl.Result{}, nil
	}
	shape, err := r.endpointShape(ctx, host)
	if err != nil || !shape.known || len(shape.bootSlots) == 0 {
		return ctrl.Result{}, err
	}
	target, err := boot.PickTarget(shape.targets)
	if err != nil {
		logf.FromContext(ctx).Info("wipe not proposed", "host", host.Name, "why", err.Error())
		return ctrl.Result{}, nil
	}
	op := wipeOp(r.OperationsNamespace, r.Proposer, host.Name, name, asset, shape, target)
	if err := r.Create(ctx, op); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("proposed wipe", "operation", op.Namespace+"/"+op.Name, "host", host.Name)
	return ctrl.Result{}, nil
}

// labelWipeHost on a wipe operation names its host, so the host's wipes are
// listed without the claim labels a wipe does not have.
const labelWipeHost = "metal.pk.io/wipe-host"

// wipeOp is the operation that boots the wipe image on host.
func wipeOp(namespace, proposer, host, name string, asset *osv1alpha1.BootAsset, shape endpointView, target string) *metalv1alpha1.HostOperation {
	var actions []metalv1alpha1.HostOperationAction
	for _, s := range shape.insertable {
		actions = append(actions, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: s}})
	}
	actions = append(actions,
		metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
			MediaID: shape.bootSlots[0], BootAssetRef: &metalv1alpha1.Reference{Name: asset.Name},
			Digest: asset.Status.Digest,
		}},
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{
			Target: target, Once: ptr.To(true), Mode: overrideMode(shape.mode),
		}},
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{labelPurpose: purposeWipe, labelWipeHost: host},
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: host},
			ProposedBy: proposer,
			Reason:     metalv1alpha1.OperationReasonWipe,
			Actions:    actions,
			// Erasing a disk is never decided for a person.
			Approval:                metalv1alpha1.OperationApproval{Required: ptr.To(true)},
			TTLSecondsAfterFinished: ptr.To(releaseTTL),
		},
	}
}

// installedHere: a claim's hand-off record names the host.
func (r *hostWipeReconciler) installedHere(ctx context.Context, host string) bool {
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return true
	}
	for _, c := range claims.Items {
		if h := c.Status.HandOff; h != nil && h.Host == host {
			return true
		}
	}
	return false
}

// ensureWipeAsset finds or creates the factory image of this Talos version
// whose kernel command line wipes the system disk. It is shared by every
// wipe of that version, and owned by nobody: it carries no secret.
func (r *hostWipeReconciler) ensureWipeAsset(ctx context.Context, version string) (*osv1alpha1.BootAsset, error) {
	schematic := &osv1alpha1.Schematic{Customization: osv1alpha1.SchematicCustomization{ExtraKernelArgs: []string{wipeKernelArg}}}
	a := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "wipe-" + hash8(planHash([]any{schematic, version})),
			Labels: map[string]string{labelProvisioning: "true"},
		},
		Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: version, Schematic: schematic},
	}
	return a, r.createOrGet(ctx, a)
}

func (r *hostWipeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.Host{}).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(wipeHostOf)).
		Watches(&osv1alpha1.BootAsset{}, handler.EnqueueRequestsFromMapFunc(r.hostsWaitingForWipe)).
		Named("host-wipe").
		Complete(r)
}

// wipeHostOf maps a wipe operation's event to its host.
func wipeHostOf(_ context.Context, obj client.Object) []ctrl.Request {
	if h := obj.GetLabels()[labelWipeHost]; h != "" {
		return []ctrl.Request{requestFor(h)}
	}
	return nil
}

// hostsWaitingForWipe re-runs the hosts that asked for a wipe when a wipe
// image changes.
func (r *hostWipeReconciler) hostsWaitingForWipe(ctx context.Context, obj client.Object) []ctrl.Request {
	if !strings.HasPrefix(obj.GetName(), "wipe-") {
		return nil
	}
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range hosts.Items {
		if _, ok := hosts.Items[i].Annotations[metalv1alpha1.AnnotationWipe]; ok {
			reqs = append(reqs, requestFor(hosts.Items[i].Name))
		}
	}
	return reqs
}
