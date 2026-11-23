package controller

import (
	"context"
	"crypto/x509"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// assetRefIndex lets an Asset event fan out to the endpoints that reference it.
const assetRefIndex = ".spec.assetRef.name"

// connectTimeout bounds one whole observation pass (dial + discovery).
const connectTimeout = 15 * time.Second

// ManagementEndpointReconciler connects to the endpoint (read-only — it writes
// nothing to a BMC), refreshes the Asset snapshot, discovers manager facts, and
// verifies assertion vs discovery. Every pass rewrites conditions as fresh
// observations; phase/blockedOn are stamped by internal/phase; the status update
// is skipped when nothing changed (status-write dedupe).
type ManagementEndpointReconciler struct {
	client.Client
	// CredentialsNamespace is where credential and CA Secrets live (the operator's
	// namespace — endpoint objects are cluster-scoped, Secrets are not).
	CredentialsNamespace string
	// PollInterval is the steady-state re-observation cadence.
	PollInterval time.Duration
}

func (r *ManagementEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ep := &metalv1alpha1.ManagementEndpoint{}
	if err := r.Get(ctx, req.NamespacedName, ep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// No finalizer by design: sessions are basic-auth (nothing leaks) and the
	// reconciler owns no external resources.
	if !ep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	orig := ep.DeepCopy()
	r.snapshotAsset(ctx, ep)
	r.observe(ctx, ep)
	phase.StampEndpoint(ep)

	// Discovered facts double as the EndpointProfile matching surface.
	if stampEndpointLabels(ep) {
		if err := r.Update(ctx, ep); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !equality.Semantic.DeepEqual(orig.Status, ep.Status) {
		if err := r.applyStatus(ctx, ep); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// applyStatus declares the discovered half of the endpoint's status. The
// convergence lane's fields are stripped rather than restated: naming them
// here would claim them, which is the erasure this is meant to end.
func (r *ManagementEndpointReconciler) applyStatus(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) error {
	apply := &metalv1alpha1.ManagementEndpoint{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metalv1alpha1.GroupVersion.String(),
			Kind:       "ManagementEndpoint",
		},
		ObjectMeta: metav1.ObjectMeta{Name: ep.Name},
		Status:     *ep.Status.DeepCopy(),
	}
	apply.Status.IdentityPushed = nil
	apply.Status.Conditions = slices.DeleteFunc(apply.Status.Conditions,
		func(c metav1.Condition) bool { return c.Type == metalv1alpha1.ConditionConfigured })
	phase.StampEndpointOnto(apply, ep)
	return applyOwnedStatus(ctx, r.Client, apply, fieldOwnerEndpoint)
}

// snapshotAsset refreshes status.asset from the referenced Asset. A missing Asset
// keeps the last snapshot — a source outage degrades to the cache (design doc
// § The Asset); actual endpoint deletion is the materializer's ownerRef GC.
func (r *ManagementEndpointReconciler) snapshotAsset(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) {
	asset := &metalv1alpha1.Asset{}
	if err := r.Get(ctx, types.NamespacedName{Name: ep.Spec.AssetRef.Name}, asset); err != nil {
		logf.FromContext(ctx).V(1).Info("asset not resolvable, keeping snapshot",
			"asset", ep.Spec.AssetRef.Name, "err", err)
		return
	}
	ep.Status.Asset = &metalv1alpha1.AssetSnapshot{
		CID:                asset.Spec.CID,
		Serial:             asset.Spec.Serial,
		BMCMac:             asset.Spec.BMCMac,
		VLAN:               asset.Spec.VLAN,
		ObservedGeneration: asset.Generation,
	}
}

// observe performs one read-only pass against the endpoint and rewrites the
// Reachable / Inventoried / Verified conditions from what it saw.
func (r *ManagementEndpointReconciler) observe(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) {
	setCond := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: ep.Generation,
		})
	}

	cfg, reason, err := r.connectConfig(ctx, ep)
	if err != nil {
		setCond(metalv1alpha1.ConditionReachable, metav1.ConditionFalse, reason, err.Error())
		return
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cl, err := gofishclient.Connect(cctx, cfg)
	if err != nil {
		setCond(metalv1alpha1.ConditionReachable, metav1.ConditionFalse, "DialError", err.Error())
		return
	}
	defer cl.Close()
	setCond(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp", "")

	d, err := discover(cctx, cl)
	if err != nil {
		setCond(metalv1alpha1.ConditionInventoried, metav1.ConditionFalse, "DiscoveryError", err.Error())
		return
	}
	ep.Status.Vendor = d.mgr.Vendor
	ep.Status.Model = d.chassis.Model
	ep.Status.FirmwareVersion = d.mgr.FirmwareVersion
	ep.Status.BoardSerial = d.chassis.BoardSerial
	ep.Status.Firmware = firmwareItems(d.firmware)
	ep.Status.Systems = discoveredSystems(d.systems)
	ep.Status.Capabilities = endpointCapabilities(d.caps)
	setCond(metalv1alpha1.ConditionInventoried, metav1.ConditionTrue, "FactsGathered", "")

	status, reason, msg := verifiedCondition(ep.Status.Asset, d.mgr.MAC, serialAnchors(d.chassis, d.systems))
	setCond(metalv1alpha1.ConditionVerified, status, reason, msg)
}

// connectConfig resolves the read-only observation config (the read-only rail — nothing
// in the endpoint reconciler writes to a BMC).
func (r *ManagementEndpointReconciler) connectConfig(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) (gofishclient.Config, string, error) {
	return endpointClientConfig(ctx, r.Client, r.CredentialsNamespace, ep, true)
}

// managedCredsName is the managed-credential Secret an EndpointProfile
// rotation maintains for this endpoint (operator namespace, owned by the
// endpoint — GC'd with it).
func managedCredsName(epName string) string { return "endpoint-creds-" + epName }

// endpointClientConfig resolves credentials and the TLS policy into a client
// config — shared by the observing endpoint reconciler (read-only), the
// operation executor, and the convergence lanes. The managed-credential
// Secret, when present, wins over spec.credentialsRef (the factory
// break-glass). The reason accompanies a non-nil error as a condition reason.
func endpointClientConfig(ctx context.Context, c client.Client, credentialsNamespace string, ep *metalv1alpha1.ManagementEndpoint, readOnly bool) (gofishclient.Config, string, error) {
	cfg, reason, err := endpointTLSConfig(ctx, c, credentialsNamespace, ep, readOnly)
	if err != nil {
		return cfg, reason, err
	}
	if cfg.Username, cfg.Password, err = secretCreds(ctx, c, credentialsNamespace, managedCredsName(ep.Name)); apierrors.IsNotFound(err) {
		if ep.Spec.CredentialsRef == nil {
			return cfg, "CredentialsMissing", fmt.Errorf("spec.credentialsRef is not set")
		}
		cfg.Username, cfg.Password, err = secretCreds(ctx, c, credentialsNamespace, ep.Spec.CredentialsRef.Name)
	}
	if err != nil {
		return cfg, "CredentialsMissing", err
	}
	return cfg, "", nil
}

// endpointTLSConfig resolves everything but the credentials — address and
// TLS policy. The convergence lanes use it directly with explicit
// credentials (probing a specific password, repairing through the factory).
func endpointTLSConfig(ctx context.Context, c client.Client, credentialsNamespace string, ep *metalv1alpha1.ManagementEndpoint, readOnly bool) (gofishclient.Config, string, error) {
	cfg := gofishclient.Config{
		Endpoint: ep.Spec.Address,
		ReadOnly: readOnly,
	}
	switch tlsSpec := ep.Spec.TLS; {
	case tlsSpec == nil:
		// system trust store
	case tlsSpec.CASecretRef != nil:
		ca := &corev1.Secret{}
		key := types.NamespacedName{Namespace: credentialsNamespace, Name: tlsSpec.CASecretRef.Name}
		if err := c.Get(ctx, key, ca); err != nil {
			return cfg, "CAMissing", fmt.Errorf("ca secret %s: %w", key, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca.Data["ca.crt"]) {
			return cfg, "CAMissing", fmt.Errorf("ca secret %s: no certificates in ca.crt", key)
		}
		cfg.RootCAs = pool
	case tlsSpec.InsecureSkipVerify:
		cfg.Insecure = true
	}
	return cfg, "", nil
}

// secretCreds reads username/password out of one credentials Secret. The
// NotFound goes back verbatim so callers can distinguish an absent Secret
// (fall back / adopt) from a malformed one (hold).
func secretCreds(ctx context.Context, c client.Client, namespace, name string) (string, string, error) {
	creds := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := c.Get(ctx, key, creds); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", err
		}
		return "", "", fmt.Errorf("credentials secret %s: %w", key, err)
	}
	user, pass := string(creds.Data["username"]), string(creds.Data["password"])
	if user == "" || pass == "" {
		return "", "", fmt.Errorf("credentials secret %s: needs username and password keys", key)
	}
	return user, pass, nil
}

type discovered struct {
	mgr      bmc.Manager
	chassis  bmc.Chassis
	caps     bmc.Capabilities
	systems  []bmc.System
	firmware []bmc.Firmware
}

func discover(ctx context.Context, cl bmc.Client) (discovered, error) {
	var d discovered
	var err error
	if d.mgr, err = cl.Manager(ctx); err != nil {
		return d, err
	}
	if d.chassis, err = cl.Chassis(ctx); err != nil {
		return d, err
	}
	if d.caps, err = cl.Capabilities(ctx); err != nil {
		return d, err
	}
	if d.systems, err = cl.Systems(ctx); err != nil {
		return d, err
	}
	if d.firmware, err = cl.FirmwareInventory(ctx); err != nil {
		return d, err
	}
	return d, nil
}

// firmwareItems projects the UpdateService inventory into status — the
// firmware-convergence observable. Sorted by id for the write dedupe.
func firmwareItems(fw []bmc.Firmware) []metalv1alpha1.FirmwareInventoryItem {
	out := make([]metalv1alpha1.FirmwareInventoryItem, 0, len(fw))
	for _, f := range fw {
		out = append(out, metalv1alpha1.FirmwareInventoryItem{
			ID: f.ID, Version: f.Version, Updateable: f.Updateable,
		})
	}
	slices.SortFunc(out, func(a, b metalv1alpha1.FirmwareInventoryItem) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// endpointCapabilities projects the client's discovered capabilities into status.
// DualMedia is a derivation: ≥2 insertable slots (X12's concurrent dual-mount is
// PoC-confirmed; iBMC's second slot exposes no actions) — refined once the sim
// models convergence.
func endpointCapabilities(caps bmc.Capabilities) *metalv1alpha1.EndpointCapabilities {
	out := &metalv1alpha1.EndpointCapabilities{
		MediaDialects:   map[string]string{},
		UpdateDialect:   string(caps.UpdateDialect),
		IdentityDialect: string(caps.IdentityDialect),
	}
	insertable := 0
	for id, d := range caps.MediaDialect {
		out.MediaDialects[id] = string(d)
		if d != bmc.DialectNone {
			insertable++
		}
	}
	out.DualMedia = insertable >= 2
	return out
}

// discoveredSystems projects discovery into the status cache the HostReconciler
// joins on — identity anchors and the path coordinate, not the full inventory
// (that shape arrives with its consumer, the claim selection). Sorted by
// id: deterministic status is what the write dedupe compares.
func discoveredSystems(systems []bmc.System) []metalv1alpha1.DiscoveredSystem {
	out := make([]metalv1alpha1.DiscoveredSystem, 0, len(systems))
	for _, s := range systems {
		ds := metalv1alpha1.DiscoveredSystem{
			ID:           s.ID,
			UUID:         s.UUID,
			SerialNumber: s.SerialNumber,
			Manufacturer: s.Manufacturer,
			Model:        s.Model,
			BootTargets:  s.BootTargets,
		}
		if !s.LastResetTime.IsZero() {
			ds.LastResetTime = &metav1.MicroTime{Time: s.LastResetTime}
		}
		out = append(out, ds)
	}
	slices.SortFunc(out, func(a, b metalv1alpha1.DiscoveredSystem) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// serialAnchors collects every serial discovery offers: the chassis board serial
// (the Supermicro anchor — its ComputerSystem carries none) and each system serial
// (the Huawei anchor). Vendor-normalized identity lives in internal/identity;
// verification only needs set membership.
func serialAnchors(chassis bmc.Chassis, systems []bmc.System) []string {
	var anchors []string
	if chassis.BoardSerial != "" {
		anchors = append(anchors, chassis.BoardSerial)
	}
	for _, s := range systems {
		if s.SerialNumber != "" {
			anchors = append(anchors, s.SerialNumber)
		}
	}
	return anchors
}

// verifiedCondition compares the org's assertion (the snapshot) against discovery —
// two independent origins. Mismatch is alert-and-hold: the condition blocks the
// phase ladder, the reconciler keeps observing, nothing destructive follows.
func verifiedCondition(snap *metalv1alpha1.AssetSnapshot, mgrMAC string, anchors []string) (metav1.ConditionStatus, string, string) {
	if snap == nil {
		return metav1.ConditionUnknown, "NoAssertion", "asset has never been resolved"
	}
	if snap.BMCMac != "" && mgrMAC != "" && !strings.EqualFold(snap.BMCMac, mgrMAC) {
		return metav1.ConditionFalse, metalv1alpha1.ReasonMACMismatch,
			fmt.Sprintf("asserted bmcMac %s, endpoint reports %s — wrong box behind this address", snap.BMCMac, mgrMAC)
	}
	if len(anchors) == 0 {
		return metav1.ConditionUnknown, "NoSerialAnchor", "discovery offered no serial to verify against"
	}
	if slices.Contains(anchors, snap.Serial) {
		return metav1.ConditionTrue, "SerialMatch", ""
	}
	return metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch,
		fmt.Sprintf("asserted serial %s not among discovered %v", snap.Serial, anchors)
}

func (r *ManagementEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&metalv1alpha1.ManagementEndpoint{}, assetRefIndex,
		func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.ManagementEndpoint).Spec.AssetRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.ManagementEndpoint{}).
		Watches(&metalv1alpha1.Asset{}, handler.EnqueueRequestsFromMapFunc(r.endpointsForAsset)).
		Named("managementendpoint").
		Complete(r)
}

// endpointsForAsset maps an Asset event to the endpoints referencing it, so an
// assertion edit (e.g. a corrected serial) re-verifies without waiting for the poll.
func (r *ManagementEndpointReconciler) endpointsForAsset(ctx context.Context, obj client.Object) []ctrl.Request {
	eps := &metalv1alpha1.ManagementEndpointList{}
	if err := r.List(ctx, eps, client.MatchingFields{assetRefIndex: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "list endpoints for asset", "asset", obj.GetName())
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(eps.Items))
	for _, ep := range eps.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: ep.Name}})
	}
	return reqs
}
