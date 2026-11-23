// Package controller holds the pk-metal reconcilers. The state-model invariant
// (ADR 0012) is enforced here by construction: reconcilers branch on conditions only
// and never touch the derived status fields (phase, blockedOn) — internal/phase.Stamp*
// is the single write site, and `mise run lint:phase` greps this directory to keep it true.
package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
)

// AssetReconciler materializes each Asset into its ManagementEndpoint (design doc
// § object model: "materialize + verify"). The endpoint spec is operator-written in
// full — address joined from the ledger, credentials defaulted from the Asset, TLS
// policy from the operator's defaults; EndpointProfile rotation will narrow this
// ownership. The Asset's Materialized condition says whether that worked, so a
// machine that never shows up says why on the object a person created.
type AssetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Ledger *ledger.Ledger
	// LedgerSource tells a person where the ledger comes from, for the
	// message of an Asset whose bmcMac is missing from it. Empty says "the
	// ledger".
	LedgerSource string
	// LedgerReload tells a person how to make the manager read an edited
	// ledger. Empty leaves it out.
	LedgerReload string
	// TLS is the verification policy stamped onto materialized endpoints
	// (live: pin internal-ca; dev: insecureSkipVerify for the sim).
	TLS      *metalv1alpha1.EndpointTLS
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=assets,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=assets/status,verbs=update
// +kubebuilder:rbac:groups=metal.pk.io,resources=assets/finalizers,verbs=update
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile materializes one Asset into its ManagementEndpoint.
//
// The endpoint carries a controller reference to the Asset, and the API server
// asks for update on the owner's finalizers subresource before it accepts a
// blocking owner reference, hence the finalizers rule here and on every other
// reconciler that owns an object.
func (r *AssetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	asset := &metalv1alpha1.Asset{}
	if err := r.Get(ctx, req.NamespacedName, asset); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Endpoint cleanup on Asset deletion is ownerRef GC — nothing to do here.
	if !asset.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := asset.DeepCopy()
	res, err := r.materialize(ctx, asset)
	if err != nil {
		return res, err
	}
	if !equality.Semantic.DeepEqual(orig.Status, asset.Status) {
		if err := r.Status().Update(ctx, asset); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return res, nil
}

// materialize creates or updates the endpoint and records the outcome in the
// Asset's Materialized condition. A hold is also an event, for whoever watches
// events; the condition is what stays.
func (r *AssetReconciler) materialize(ctx context.Context, asset *metalv1alpha1.Asset) (ctrl.Result, error) {
	set := func(s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&asset.Status.Conditions, metav1.Condition{
			Type: metalv1alpha1.ConditionMaterialized, Status: s, Reason: reason, Message: msg,
			ObservedGeneration: asset.Generation,
		})
	}
	hold := func(reason, msg string) {
		// The event goes out when the verdict changes, not on every pass.
		if c := meta.FindStatusCondition(asset.Status.Conditions, metalv1alpha1.ConditionMaterialized); c == nil ||
			c.Reason != reason || c.Message != msg {
			r.Recorder.Eventf(asset, nil, corev1.EventTypeWarning, reason, "Materialize", "%s", msg)
		}
		set(metav1.ConditionFalse, reason, msg)
	}

	if asset.Spec.BMCMac == "" {
		hold(metalv1alpha1.ReasonNoBMCMac,
			"the asset names no bmcMac, so the operator cannot find its BMC; set spec.bmcMac to the MAC of the BMC's network port")
		return ctrl.Result{}, nil
	}
	addr, ok := r.Ledger.Lookup(asset.Spec.BMCMac)
	if !ok {
		if why, bad := r.Ledger.Invalid(asset.Spec.BMCMac); bad {
			hold(metalv1alpha1.ReasonLedgerEntryInvalid, fmt.Sprintf(
				"the line for bmcMac %s in %s was skipped: %s.%s",
				asset.Spec.BMCMac, r.ledgerSource(), why, r.reloadHint()))
			return ctrl.Result{}, nil
		}
		hold(metalv1alpha1.ReasonNoLedgerEntry, fmt.Sprintf(
			"bmcMac %s is not in %s, so the operator does not know the BMC's address. Add a line %q: https://<BMC address>, or fix the MAC on this asset if it has a typo.%s",
			asset.Spec.BMCMac, r.ledgerSource(), asset.Spec.BMCMac, r.reloadHint()))
		return ctrl.Result{}, nil
	}

	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: asset.Name}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ep, func() error {
		ep.Spec = metalv1alpha1.ManagementEndpointSpec{
			AssetRef:       metalv1alpha1.Reference{Name: asset.Name},
			Address:        addr,
			CredentialsRef: asset.Spec.CredentialsRef.DeepCopy(),
			TLS:            r.TLS.DeepCopy(),
		}
		return controllerutil.SetControllerReference(asset, ep, r.Scheme)
	})
	if err != nil {
		// A same-named endpoint owned by another Asset is an asset collision —
		// surface it, don't fight over the object.
		var owned *controllerutil.AlreadyOwnedError
		if errors.As(err, &owned) || apierrors.IsAlreadyExists(err) {
			hold(metalv1alpha1.ReasonMaterializeConflict, fmt.Sprintf(
				"endpoint %s belongs to another asset: %v", asset.Name, err))
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("materialize endpoint %s: %w", asset.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("materialized endpoint", "endpoint", ep.Name, "op", op, "address", addr)
	}
	set(metav1.ConditionTrue, metalv1alpha1.ReasonEndpointMaterialized,
		fmt.Sprintf("endpoint %s at %s", ep.Name, addr))
	return ctrl.Result{}, nil
}

func (r *AssetReconciler) ledgerSource() string {
	if r.LedgerSource != "" {
		return r.LedgerSource
	}
	return "the ledger"
}

func (r *AssetReconciler) reloadHint() string {
	if r.LedgerReload == "" {
		return ""
	}
	return " " + r.LedgerReload
}

func (r *AssetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.Asset{}).
		Owns(&metalv1alpha1.ManagementEndpoint{}).
		Named("asset").
		Complete(r)
}
