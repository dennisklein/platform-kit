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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// ownership.
type AssetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Ledger *ledger.Ledger
	// TLS is the verification policy stamped onto materialized endpoints
	// (live: pin internal-ca; dev: insecureSkipVerify for the sim).
	TLS      *metalv1alpha1.EndpointTLS
	Recorder events.EventRecorder
}

func (r *AssetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	asset := &metalv1alpha1.Asset{}
	if err := r.Get(ctx, req.NamespacedName, asset); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Endpoint cleanup on Asset deletion is ownerRef GC — nothing to do here.
	if !asset.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if asset.Spec.BMCMac == "" {
		r.Recorder.Eventf(asset, nil, corev1.EventTypeWarning, "NoBMCMac", "Materialize",
			"asset asserts no bmcMac — no endpoint materialized")
		return ctrl.Result{}, nil
	}
	addr, ok := r.Ledger.Lookup(asset.Spec.BMCMac)
	if !ok {
		r.Recorder.Eventf(asset, nil, corev1.EventTypeWarning, "NoLedgerEntry", "Materialize",
			"bmcMac %s has no ledger entry — no endpoint materialized", asset.Spec.BMCMac)
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
			r.Recorder.Eventf(asset, nil, corev1.EventTypeWarning, "MaterializeConflict", "Materialize",
				"endpoint %s: %v", asset.Name, err)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("materialize endpoint %s: %w", asset.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("materialized endpoint", "endpoint", ep.Name, "op", op, "address", addr)
	}
	return ctrl.Result{}, nil
}

func (r *AssetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.Asset{}).
		Owns(&metalv1alpha1.ManagementEndpoint{}).
		Named("asset").
		Complete(r)
}
