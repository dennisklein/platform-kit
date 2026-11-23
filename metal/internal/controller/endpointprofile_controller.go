package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// rotatedAtAnnotation on the managed-credential Secret records when the
// password was last generated — the rotation-cadence observable.
const rotatedAtAnnotation = labelDomain + "rotated-at"

// EndpointConvergenceReconciler drives matched EndpointProfiles onto the
// manager itself. Unlike the host lanes there is no operation gate: merging
// the profile is the approval — nothing here disrupts the host — so the
// reconciler writes the BMC directly (with the executor, a deliberate
// opening of the write rail) and reports through the endpoint's Configured
// condition, which it alone maintains; the observing endpoint reconciler
// leaves it untouched.
//
// The credentials lane runs the operator's own account: the desired password
// is persisted to the managed Secret FIRST, then the account is converged to
// it — always through a session on ANOTHER account (the asset's factory
// credentials, deliberately left untouched as standing break-glass). Every
// crash window therefore heals on a later pass instead of locking out, and
// a lost managed Secret is a repair, not an incident.
type EndpointConvergenceReconciler struct {
	client.Client
	// CredentialsNamespace is where credential Secrets live (factory and managed).
	CredentialsNamespace string
	// PollInterval is the re-verification cadence while profiles match.
	PollInterval time.Duration
	// APIReader reads the endpoint straight from the API server, bypassing the
	// informer cache. The identity lane decides whether to hand a certificate
	// to a manager that restarts when it accepts one, and it decides by reading
	// status.identityPushed. A cache that has not caught up yet reports no
	// push, which is the one wrong answer that cannot be taken back — and,
	// because this reconciler applies its fields server-side, a pass that reads
	// no push also stops declaring it and so deletes the record. Defaults to
	// the manager's API reader in SetupWithManager.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints/status,verbs=patch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups=metal.pk.io,resources=endpointprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete,namespace=pk-metal
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificaterequests,verbs=get;create;delete,namespace=pk-metal

// Reconcile converges a management controller's own configuration (accounts,
// time, identity certificate) on the matching EndpointProfiles. The managed
// credentials and the in-flight identity key are Secrets it owns in the
// credentials namespace, and the certificate is requested from cert-manager in
// the same namespace.
func (r *EndpointConvergenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ep := &metalv1alpha1.ManagementEndpoint{}
	if err := r.reader().Get(ctx, req.NamespacedName, ep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	profiles, err := r.matchedEndpointProfiles(ctx, ep)
	if err != nil {
		return ctrl.Result{}, err
	}

	orig := ep.DeepCopy()
	if len(profiles) == 0 {
		// Unmatched is unmanaged, not un-rotated: a managed account already
		// rotated stays as it is (the Secret keeps working credentials), the
		// operator just stops converging it. Only in-flight identity
		// material is withdrawn — a private key must not linger.
		if err := r.withdrawIdentity(ctx, ep); err != nil {
			return ctrl.Result{}, err
		}
		meta.RemoveStatusCondition(&ep.Status.Conditions, metalv1alpha1.ConditionConfigured)
	} else {
		status, reason, msg, err := r.converge(ctx, ep, profiles)
		if err != nil {
			return ctrl.Result{}, err
		}
		setCondition(&ep.Status.Conditions, ep.Generation, metalv1alpha1.ConditionConfigured, status, reason, msg)
	}

	phase.StampEndpoint(ep)
	if !equality.Semantic.DeepEqual(orig.Status, ep.Status) {
		if err := r.applyStatus(ctx, ep); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if len(profiles) > 0 {
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// applyStatus declares only what this reconciler decides: the Configured
// condition it alone maintains, the push memory, and the phase derived from
// them. The discovered facts are the other author's and are left out entirely,
// so a pass here cannot write over them.
//
// The push memory is why this is an apply and not an update. It records
// something PHYSICAL that already happened — the manager accepted a
// certificate and restarted. A full-object update built from a cached read
// could drop it under a concurrent write, and the next pass, reading no push,
// would push again: the in-flight material still matches the desired state, so
// no fresh request is generated, and the manager refuses to import one it has
// already consumed. The lane then wedges on an error no retry can clear.
func (r *EndpointConvergenceReconciler) applyStatus(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) error {
	apply := &metalv1alpha1.ManagementEndpoint{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metalv1alpha1.GroupVersion.String(),
			Kind:       "ManagementEndpoint",
		},
		ObjectMeta: metav1.ObjectMeta{Name: ep.Name},
	}
	if c := meta.FindStatusCondition(ep.Status.Conditions, metalv1alpha1.ConditionConfigured); c != nil {
		apply.Status.Conditions = []metav1.Condition{*c}
	}
	apply.Status.IdentityPushed = ep.Status.IdentityPushed
	phase.StampEndpointOnto(apply, ep)
	return applyOwnedStatus(ctx, r.Client, apply, fieldOwnerConvergence)
}

// converge merges the matched profiles and runs every declared lane in
// order. First non-True wins the condition — later lanes wait for earlier
// ones rather than racing them.
func (r *EndpointConvergenceReconciler) converge(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, profiles []metalv1alpha1.EndpointProfile) (metav1.ConditionStatus, string, string, error) {
	desired, conflict := mergeEndpointProfiles(profiles)
	if conflict != "" {
		return metav1.ConditionFalse, metalv1alpha1.ReasonProfileConflict, conflict, nil
	}
	// Never write to an unproven box: the wrong-box guard that holds every
	// mutating lane is the endpoint verification.
	if !meta.IsStatusConditionTrue(ep.Status.Conditions, metalv1alpha1.ConditionVerified) {
		return metav1.ConditionUnknown, metalv1alpha1.ReasonAwaitingVerification,
			"profile matched but the endpoint is not verified", nil
	}

	var summary []string
	if desired.credentials != nil {
		status, reason, msg, err := r.credentials(ctx, ep, desired.credentials)
		if err != nil || status != metav1.ConditionTrue {
			return status, reason, msg, err
		}
		summary = append(summary, msg)
	}
	if desired.ntp != nil {
		status, reason, msg, err := r.ntp(ctx, ep, desired.ntp)
		if err != nil || status != metav1.ConditionTrue {
			return status, reason, msg, err
		}
		summary = append(summary, msg)
	}
	// Identity last: certificate validity needs the clock the ntp lane just
	// converged (a skewed manager rejects a freshly-issued NotBefore).
	if desired.tls != nil {
		status, reason, msg, err := r.identity(ctx, ep, desired.tls)
		if err != nil || status != metav1.ConditionTrue {
			return status, reason, msg, err
		}
		summary = append(summary, msg)
	} else if err := r.withdrawIdentity(ctx, ep); err != nil {
		return "", "", "", err
	}
	return metav1.ConditionTrue, metalv1alpha1.ReasonInSync, strings.Join(summary, "; "), nil
}

// withdrawIdentity retires the lane's leftovers when no profile declares an
// identity anymore: the in-flight material goes (a key must not linger), the
// push memory goes with the desired state it belonged to. The certificate
// already on the manager stays — unmanaged, not un-pushed.
func (r *EndpointConvergenceReconciler) withdrawIdentity(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) error {
	if err := r.cleanupIdentityInFlight(ctx, ep); err != nil {
		return err
	}
	ep.Status.IdentityPushed = nil
	return nil
}

// credentials converges the operator's managed account: Secret first (the
// desired state), then the account on the manager, then a proving login.
func (r *EndpointConvergenceReconciler) credentials(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.CredentialsPolicy) (metav1.ConditionStatus, string, string, error) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: managedCredsName(ep.Name), Namespace: r.CredentialsNamespace,
	}}
	err := r.Get(ctx, client.ObjectKeyFromObject(sec), sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", "", err
	}
	if apierrors.IsNotFound(err) || string(sec.Data["username"]) != pol.Username || rotationDue(sec, pol.MaxAge) {
		pw, err := randomPassword()
		if err != nil {
			return "", "", "", err
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
			if err := controllerutil.SetControllerReference(ep, sec, r.Scheme()); err != nil {
				return err
			}
			if sec.Annotations == nil {
				sec.Annotations = map[string]string{}
			}
			sec.Annotations[rotatedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
			sec.Data = map[string][]byte{
				"username": []byte(pol.Username),
				"password": []byte(pw),
			}
			return nil
		}); err != nil {
			return "", "", "", fmt.Errorf("managed credentials %s: %w", sec.Name, err)
		}
		logf.FromContext(ctx).Info("rotated managed credentials", "endpoint", ep.Name, "account", pol.Username)
	}

	user, pass := string(sec.Data["username"]), string(sec.Data["password"])
	if r.probe(ctx, ep, user, pass) == nil {
		return metav1.ConditionTrue, metalv1alpha1.ReasonInSync, "account " + user + " managed", nil
	}
	if err := r.repair(ctx, ep, user, pass); err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonRotationFailed, err.Error(), nil
	}
	if err := r.probe(ctx, ep, user, pass); err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonRotationFailed,
			"account " + user + " reset but login still fails: " + err.Error(), nil
	}
	logf.FromContext(ctx).Info("converged managed account", "endpoint", ep.Name, "account", user)
	return metav1.ConditionTrue, metalv1alpha1.ReasonInSync, "account " + user + " managed", nil
}

// ntp converges the manager's time sources: read, write on drift, prove by
// re-reading — the write succeeding proves nothing.
func (r *EndpointConvergenceReconciler) ntp(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.NTPPolicy) (metav1.ConditionStatus, string, string, error) {
	cfg, reason, err := endpointClientConfig(ctx, r.Client, r.CredentialsNamespace, ep, false)
	if err != nil {
		return metav1.ConditionFalse, reason, err.Error(), nil
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cl, err := gofishclient.Connect(cctx, cfg)
	if err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonNTPFailed, err.Error(), nil
	}
	defer cl.Close() //nolint:errcheck // best-effort logout

	want := bmc.NTPConfig{Enabled: true, Servers: pol.Servers}
	got, err := cl.NTP(cctx)
	if err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonNTPFailed, err.Error(), nil
	}
	if !ntpEqual(got, want) {
		if err := cl.SetNTP(cctx, want); err != nil {
			return metav1.ConditionFalse, metalv1alpha1.ReasonNTPFailed, err.Error(), nil
		}
		if got, err = cl.NTP(cctx); err != nil {
			return metav1.ConditionFalse, metalv1alpha1.ReasonNTPFailed, err.Error(), nil
		}
		if !ntpEqual(got, want) {
			return metav1.ConditionFalse, metalv1alpha1.ReasonNTPFailed,
				fmt.Sprintf("wrote %v but read back %v", want.Servers, got.Servers), nil
		}
		logf.FromContext(ctx).Info("converged NTP", "endpoint", ep.Name, "servers", want.Servers)
	}
	return metav1.ConditionTrue, metalv1alpha1.ReasonInSync,
		"ntp " + strings.Join(pol.Servers, ","), nil
}

func ntpEqual(a, b bmc.NTPConfig) bool {
	return a.Enabled == b.Enabled && slices.Equal(a.Servers, b.Servers)
}

// probe proves a credential with a read-only authenticated call. The service
// root is public on every Redfish implementation, so connecting alone proves
// nothing — the Manager read is the authentication oracle.
func (r *EndpointConvergenceReconciler) probe(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, user, pass string) error {
	cfg, _, err := endpointTLSConfig(ctx, r.Client, r.CredentialsNamespace, ep, true)
	if err != nil {
		return err
	}
	cfg.Username, cfg.Password = user, pass
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cl, err := gofishclient.Connect(cctx, cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck // read-only session
	_, err = cl.Manager(cctx)
	return err
}

// repair brings the managed account to the Secret's password through a
// factory session — the break-glass path, and the only writer: rotation
// never saws off the branch its own session sits on.
func (r *EndpointConvergenceReconciler) repair(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, user, pass string) error {
	if ep.Spec.CredentialsRef == nil {
		return errors.New("no factory credentialsRef to repair through")
	}
	fuser, fpass, err := secretCreds(ctx, r.Client, r.CredentialsNamespace, ep.Spec.CredentialsRef.Name)
	if err != nil {
		return fmt.Errorf("factory credentials: %w", err)
	}
	cfg, _, err := endpointTLSConfig(ctx, r.Client, r.CredentialsNamespace, ep, false)
	if err != nil {
		return err
	}
	cfg.Username, cfg.Password = fuser, fpass
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cl, err := gofishclient.Connect(cctx, cfg)
	if err != nil {
		return fmt.Errorf("factory session: %w", err)
	}
	defer cl.Close() //nolint:errcheck // best-effort logout
	if err := cl.EnsureAccount(cctx, user, pass); err != nil {
		return fmt.Errorf("factory session: %w", err)
	}
	return nil
}

// matchedEndpointProfiles selects profiles against the endpoint's labels,
// sorted by (layer, name) — merge order.
func (r *EndpointConvergenceReconciler) matchedEndpointProfiles(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) ([]metalv1alpha1.EndpointProfile, error) {
	list := &metalv1alpha1.EndpointProfileList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	var matched []metalv1alpha1.EndpointProfile
	for _, p := range list.Items {
		ok, err := selectorMatches(p.Spec.Match, ep.Labels)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		if ok {
			matched = append(matched, p)
		}
	}
	slices.SortFunc(matched, func(a, b metalv1alpha1.EndpointProfile) int {
		if a.Spec.Layer != b.Spec.Layer {
			return int(a.Spec.Layer - b.Spec.Layer)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return matched, nil
}

// endpointDesired is the merged endpoint configuration — one winner per
// stanza.
type endpointDesired struct {
	credentials *metalv1alpha1.CredentialsPolicy
	ntp         *metalv1alpha1.NTPPolicy
	tls         *metalv1alpha1.IdentityPolicy
}

// mergeEndpointProfiles merges by (layer, name): a later layer overrides a
// whole stanza; two same-layer profiles disagreeing on a stanza is a
// conflict that holds the endpoint whole. Profiles arrive sorted.
func mergeEndpointProfiles(profiles []metalv1alpha1.EndpointProfile) (endpointDesired, string) {
	var d endpointDesired
	var conflict string
	d.credentials, conflict = mergeStanza(profiles, "credentials",
		func(s *metalv1alpha1.EndpointProfileSpec) *metalv1alpha1.CredentialsPolicy { return s.Credentials })
	if conflict != "" {
		return endpointDesired{}, conflict
	}
	d.ntp, conflict = mergeStanza(profiles, "ntp",
		func(s *metalv1alpha1.EndpointProfileSpec) *metalv1alpha1.NTPPolicy { return s.NTP })
	if conflict != "" {
		return endpointDesired{}, conflict
	}
	d.tls, conflict = mergeStanza(profiles, "tls",
		func(s *metalv1alpha1.EndpointProfileSpec) *metalv1alpha1.IdentityPolicy { return s.TLS })
	if conflict != "" {
		return endpointDesired{}, conflict
	}
	return d, ""
}

// mergeStanza runs the per-stanza merge rule: last writer in (layer, name)
// order wins the whole stanza, same-layer disagreement is a conflict.
func mergeStanza[T any](profiles []metalv1alpha1.EndpointProfile, stanza string, get func(*metalv1alpha1.EndpointProfileSpec) *T) (*T, string) {
	var winner *T
	var layer int32
	var name string
	for i := range profiles {
		p := &profiles[i]
		v := get(&p.Spec)
		if v == nil {
			continue
		}
		if winner != nil && layer == p.Spec.Layer && !equality.Semantic.DeepEqual(winner, v) {
			return nil, fmt.Sprintf("%s: profiles %s and %s disagree on layer %d", stanza, name, p.Name, p.Spec.Layer)
		}
		winner, layer, name = v, p.Spec.Layer, p.Name
	}
	return winner, ""
}

// rotationDue reports whether the managed password has outlived the
// profile's cadence. An unparsable or missing stamp rotates — conservative,
// and self-correcting because rotation rewrites the stamp.
func rotationDue(sec *corev1.Secret, maxAge *metav1.Duration) bool {
	if maxAge == nil {
		return false
	}
	t, err := time.Parse(time.RFC3339, sec.Annotations[rotatedAtAnnotation])
	if err != nil {
		return true
	}
	return time.Since(t) > maxAge.Duration
}

// randomPassword draws 16 unbiased alphanumeric characters — inside every
// vendor's password rules (8-20 chars, no special-character requirement met
// by mixed classes).
func randomPassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 16)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// reader is the uncached reader when one is wired, the client otherwise — a
// direct construction in a test stays working, it just reads the cache.
func (r *EndpointConvergenceReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *EndpointConvergenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.ManagementEndpoint{}).
		Owns(&corev1.Secret{}).
		Watches(&metalv1alpha1.EndpointProfile{}, handler.EnqueueRequestsFromMapFunc(r.allEndpoints)).
		Named("endpointconvergence").
		Complete(r)
}

// allEndpoints fans a profile event out to every endpoint — matching is
// per-endpoint.
func (r *EndpointConvergenceReconciler) allEndpoints(ctx context.Context, _ client.Object) []ctrl.Request {
	eps := &metalv1alpha1.ManagementEndpointList{}
	if err := r.List(ctx, eps); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(eps.Items))
	for i := range eps.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: eps.Items[i].Name}})
	}
	return reqs
}
