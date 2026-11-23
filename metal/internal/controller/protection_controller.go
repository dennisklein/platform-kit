package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// claimProtection keeps a claimed machine's Host and every endpoint fronting it
// alive until the claim lets go — the pvc-protection pattern.
const claimProtection = "metal.pk.io/claim-protection"

// fieldOwnerProtection owns the endpoint's DeletionHeld condition. The
// endpoint's observer stops writing once deletion starts, so nothing else
// names it; a distinct owner keeps that true by construction.
const fieldOwnerProtection = "pk-metal-protection"

// ProtectionReconciler holds claimed machines against garbage collection. An
// Asset owns its ManagementEndpoint, which owns the Hosts it fronts, so
// deleting an Asset — a CMDB clean-up, a re-keyed git file — used to collect a
// machine someone holds, mid-install or in production. A finalizer on the Host
// keeps the record; a finalizer on each fronting endpoint keeps the path the
// release has to power the machine off through. Both come off when the claim
// is released, and the collection the delete started then completes.
type ProtectionReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints/status,verbs=patch

// Reconcile keeps the protection finalizer on a claimed Host and on the
// endpoints that front it. A finalizer is metadata, so holding one needs patch
// on the object itself.
func (r *ProtectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	claimed := host.Spec.ClaimRef != nil
	if err := r.ensureFinalizer(ctx, host, claimed); err != nil {
		return ctrl.Result{}, err
	}

	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return ctrl.Result{}, err
	}
	for _, path := range host.Status.ManagedBy {
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: path.EndpointRef.Name}, ep); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			continue
		}
		holder := claimedHostBehind(hosts.Items, ep.Name)
		if err := r.ensureFinalizer(ctx, ep, holder != nil); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.markEndpointHold(ctx, ep, holder); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// claimedHostBehind returns a claimed host the endpoint fronts, if any.
func claimedHostBehind(hosts []metalv1alpha1.Host, endpoint string) *metalv1alpha1.Host {
	for i := range hosts {
		if hosts[i].Spec.ClaimRef == nil {
			continue
		}
		for _, p := range hosts[i].Status.ManagedBy {
			if p.EndpointRef.Name == endpoint {
				return &hosts[i]
			}
		}
	}
	return nil
}

// ensureFinalizer converges the protection finalizer on want with an
// optimistic-lock patch: finalizers are a list, and a blind merge patch would
// drop one someone else added in between.
func (r *ProtectionReconciler) ensureFinalizer(ctx context.Context, obj client.Object, want bool) error {
	if controllerutil.ContainsFinalizer(obj, claimProtection) == want {
		return nil
	}
	base := obj.DeepCopyObject().(client.Object)
	if want {
		controllerutil.AddFinalizer(obj, claimProtection)
	} else {
		controllerutil.RemoveFinalizer(obj, claimProtection)
	}
	if err := r.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return client.IgnoreNotFound(err)
	}
	logf.FromContext(ctx).Info("claim protection", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName(), "held", want)
	return nil
}

// markEndpointHold surfaces the hold on a deleting endpoint.
func (r *ProtectionReconciler) markEndpointHold(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, holder *metalv1alpha1.Host) error {
	if ep.DeletionTimestamp.IsZero() || holder == nil {
		return nil
	}
	ref := holder.Spec.ClaimRef
	cond := metav1.Condition{
		Type: metalv1alpha1.ConditionDeletionHeld, Status: metav1.ConditionTrue,
		Reason: metalv1alpha1.ReasonHostClaimed, ObservedGeneration: ep.Generation,
		Message: fmt.Sprintf("deletion requested, held: host %s is claimed by %s/%s — delete the claim to release it",
			holder.Name, ref.Namespace, ref.Name),
	}
	if c := meta.FindStatusCondition(ep.Status.Conditions, cond.Type); c != nil &&
		c.Status == cond.Status && c.Message == cond.Message {
		return nil
	}
	cond.LastTransitionTime = metav1.Now()
	apply := &metalv1alpha1.ManagementEndpoint{
		TypeMeta:   metav1.TypeMeta{APIVersion: metalv1alpha1.GroupVersion.String(), Kind: "ManagementEndpoint"},
		ObjectMeta: metav1.ObjectMeta{Name: ep.Name},
	}
	apply.Status.Conditions = []metav1.Condition{cond}
	return client.IgnoreNotFound(applyOwnedStatus(ctx, r.Client, apply, fieldOwnerProtection))
}

func (r *ProtectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.Host{}).
		Watches(&metalv1alpha1.ManagementEndpoint{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []ctrl.Request {
				names := hostNames(obj.(*metalv1alpha1.ManagementEndpoint))
				reqs := make([]ctrl.Request, 0, len(names))
				for _, n := range names {
					reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: n}})
				}
				return reqs
			})).
		Named("protection").
		Complete(r)
}
