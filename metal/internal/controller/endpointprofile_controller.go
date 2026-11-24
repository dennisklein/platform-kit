package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
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
// The credentials lane runs the operator's own account. A new password is
// parked in the managed Secret as a candidate beside the working one, set on
// the manager (through the managed session, or the asset's factory
// credentials, which are left untouched as standing break-glass), and
// promoted only once it logs in. Every crash window therefore heals on a later
// pass, a refused password leaves the working one in place, and a lost
// managed Secret is a repair through the factory login, not an incident.
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
	// Dial opens the sessions to the endpoint; nil dials over gofish.
	Dial Dialer

	// logins remembers the credentials a manager refused, so that a probe
	// does not try a known-bad password on every pass.
	logins loginBackoff
}

// loginBackoff spaces the probes of a credential a manager refused: every
// refused login is an audit event and counts towards the account lockout of
// the manager. The wait doubles with each refusal, from loginBackoffMin up
// to loginBackoffMax. A success, or a reset of the account to that
// password, forgets the refusals.
type loginBackoff struct {
	mu      sync.Mutex
	refused map[string]refusedLogin
	now     func() time.Time // nil is time.Now
}

type refusedLogin struct {
	at    time.Time
	times int
}

const (
	loginBackoffMin = time.Minute
	loginBackoffMax = time.Hour
)

// loginKey names one credential at one endpoint; the password only as its
// hash.
func loginKey(ep, user, pass string) string {
	sum := sha256.Sum256([]byte(pass))
	return ep + "\x00" + user + "\x00" + hex.EncodeToString(sum[:])
}

func (b *loginBackoff) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// wait is how long the credential must not be tried yet; zero when it may.
func (b *loginBackoff) wait(key string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.refused[key]
	if !ok {
		return 0
	}
	d := loginBackoffMin << min(r.times-1, 6)
	return max(0, min(d, loginBackoffMax)-b.clock().Sub(r.at))
}

func (b *loginBackoff) refuse(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refused == nil {
		b.refused = map[string]refusedLogin{}
	}
	r := b.refused[key]
	b.refused[key] = refusedLogin{at: b.clock(), times: r.times + 1}
}

func (b *loginBackoff) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.refused, key)
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
	apply := endpointConvergenceShare(ep)
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

// Keys of the managed-credential Secret. username and password are the
// credential every lane dials with, and they only ever hold a password the
// manager has been seen to accept. A rotation parks its candidate under the
// next-* keys and promotes it only after a login with it succeeded.
const (
	keyNextUsername = "next-username"
	keyNextPassword = "next-password"
)

// managedCred is one username and password pair out of the managed Secret.
type managedCred struct{ user, pass string }

func (c managedCred) set() bool { return c.user != "" && c.pass != "" }

// credentials converges the operator's managed account in two phases. The
// working password in the managed Secret is never replaced by one the
// manager has not accepted yet:
//
//  1. a new password is parked in the Secret as a candidate (next-username,
//     next-password) beside the working one, so a crash after the manager took
//     it cannot lose it;
//  2. the candidate is set on the manager, through the managed account's own
//     session first and the factory session second, and promoted to username
//     and password only once a login with it succeeds.
//
// A rotation the manager refuses therefore leaves every lane dialling with the
// password that still works, whether or not the factory login does.
func (r *EndpointConvergenceReconciler) credentials(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.CredentialsPolicy) (metav1.ConditionStatus, string, string, error) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: managedCredsName(ep.Name), Namespace: r.CredentialsNamespace,
	}}
	err := r.Get(ctx, client.ObjectKeyFromObject(sec), sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", "", err
	}
	exists := err == nil
	cur := managedCred{string(sec.Data["username"]), string(sec.Data["password"])}
	next := managedCred{string(sec.Data[keyNextUsername]), string(sec.Data[keyNextPassword])}
	curOK := cur.set() && r.probe(ctx, ep, cur.user, cur.pass) == nil
	// One account holds one password: while the working password still logs
	// in to the candidate's account, the candidate is not on the manager, and
	// trying it would only add a failed login towards the lockout counter.
	sameAccount := next.user == cur.user
	nextProbed := false

	if next.set() && (!curOK || !sameAccount) {
		// A candidate from an earlier pass that may have landed before that
		// pass ended (a crash, a lost Secret write).
		nextProbed = true
		if r.probe(ctx, ep, next.user, next.pass) == nil {
			return r.promote(ctx, ep, sec, next)
		}
	}
	if next.set() && next.user != pol.Username {
		next = managedCred{} // parked for an account the profile no longer names
	}

	if !next.set() {
		due := !cur.set() || cur.user != pol.Username || rotationDue(sec, pol.MaxAge)
		if !due {
			return r.keep(ctx, ep, cur, curOK)
		}
		pw, err := randomPassword()
		if err != nil {
			return "", "", "", err
		}
		next = managedCred{pol.Username, pw}
		if err := r.park(ctx, ep, sec, exists, next); err != nil {
			return "", "", "", err
		}
		sameAccount, nextProbed = next.user == cur.user, false
	}

	// landed decides whether the manager now holds the candidate after one
	// attempt to set it. A refused write costs no failed login at all while
	// the working password still answers for the same account.
	landed := func(setErr error) bool {
		if setErr != nil {
			if curOK && sameAccount && r.probe(ctx, ep, cur.user, cur.pass) == nil {
				return false
			}
			if nextProbed {
				return false
			}
		} else {
			r.logins.forget(loginKey(ep.Name, next.user, next.pass))
		}
		nextProbed = true
		return r.probe(ctx, ep, next.user, next.pass) == nil
	}

	var failures []string
	if curOK {
		cfg, _, err := endpointTLSConfig(ctx, r.Client, r.CredentialsNamespace, ep, false)
		if err != nil {
			return "", "", "", err
		}
		cfg.Username, cfg.Password = cur.user, cur.pass
		setErr := withSession(ctx, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
			return cl.EnsureAccount(ctx, next.user, next.pass)
		})
		if landed(setErr) {
			return r.promote(ctx, ep, sec, next)
		}
		failures = append(failures, "managed session: "+errText(setErr))
	}
	setErr := r.repair(ctx, ep, next.user, next.pass)
	if landed(setErr) {
		return r.promote(ctx, ep, sec, next)
	}
	failures = append(failures, errText(setErr))

	kept := "no managed password works yet"
	if curOK {
		kept = "the working password of " + cur.user + " is kept"
	}
	return metav1.ConditionFalse, metalv1alpha1.ReasonRotationFailed,
		"the new password for " + next.user + " was not accepted (" + strings.Join(failures, "; ") + "); " + kept, nil
}

// keep holds the managed password when no rotation is due. One that stopped
// working was changed behind the operator's back: it is put back through the
// factory session, and the Secret is not touched.
func (r *EndpointConvergenceReconciler) keep(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, cur managedCred, curOK bool) (metav1.ConditionStatus, string, string, error) {
	if curOK {
		return metav1.ConditionTrue, metalv1alpha1.ReasonInSync, "account " + cur.user + " managed", nil
	}
	if err := r.repair(ctx, ep, cur.user, cur.pass); err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonRotationFailed, err.Error(), nil
	}
	if err := r.probe(ctx, ep, cur.user, cur.pass); err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonRotationFailed,
			"account " + cur.user + " reset but login still fails: " + err.Error(), nil
	}
	logf.FromContext(ctx).Info("repaired managed account", "endpoint", ep.Name, "account", cur.user)
	return metav1.ConditionTrue, metalv1alpha1.ReasonInSync, "account " + cur.user + " managed", nil
}

// errText is an error's text, or what a nil error meant to the rotation: the
// manager answered success but the password does not log in.
func errText(err error) string {
	if err == nil {
		return "accepted, but a login with it fails"
	}
	return err.Error()
}

// park stores a candidate beside the working credential, creating the
// managed Secret when it is missing. The working keys are left as they are.
func (r *EndpointConvergenceReconciler) park(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, sec *corev1.Secret, exists bool, next managedCred) error {
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[keyNextUsername] = []byte(next.user)
	sec.Data[keyNextPassword] = []byte(next.pass)
	if !exists {
		if err := controllerutil.SetControllerReference(ep, sec, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(ctx, sec); err != nil {
			return fmt.Errorf("managed credentials %s: %w", sec.Name, err)
		}
		return nil
	}
	// An update, not a patch: a Secret that changed since the read makes this
	// pass fail and the next one start from what is stored.
	if err := r.Update(ctx, sec); err != nil {
		return fmt.Errorf("managed credentials %s: %w", sec.Name, err)
	}
	return nil
}

// promote makes a candidate the managed credential once a login with it has
// succeeded, and stamps the rotation time.
func (r *EndpointConvergenceReconciler) promote(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, sec *corev1.Secret, next managedCred) (metav1.ConditionStatus, string, string, error) {
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	sec.Annotations[rotatedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	sec.Data = map[string][]byte{
		"username": []byte(next.user),
		"password": []byte(next.pass),
	}
	// A failed write leaves the candidate parked: the next pass finds the
	// old password dead and the candidate alive, and promotes it then.
	if err := r.Update(ctx, sec); err != nil {
		return "", "", "", fmt.Errorf("managed credentials %s: %w", sec.Name, err)
	}
	logf.FromContext(ctx).Info("rotated managed credentials", "endpoint", ep.Name, "account", next.user)
	return metav1.ConditionTrue, metalv1alpha1.ReasonInSync, "account " + next.user + " managed", nil
}

// ntp converges the manager's time sources: read, write on drift, prove by
// re-reading — the write succeeding proves nothing.
func (r *EndpointConvergenceReconciler) ntp(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.NTPPolicy) (metav1.ConditionStatus, string, string, error) {
	cfg, reason, err := endpointClientConfig(ctx, r.Client, r.CredentialsNamespace, ep, false)
	if err != nil {
		return metav1.ConditionFalse, reason, err.Error(), nil
	}
	want := bmc.NTPConfig{Enabled: true, Servers: pol.Servers}
	err = withSession(ctx, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
		got, err := cl.NTP(ctx)
		if err != nil {
			return err
		}
		if ntpEqual(got, want) {
			return nil
		}
		if err := cl.SetNTP(ctx, want); err != nil {
			return err
		}
		if got, err = cl.NTP(ctx); err != nil {
			return err
		}
		if !ntpEqual(got, want) {
			return fmt.Errorf("wrote %v but read back %v", want.Servers, got.Servers)
		}
		logf.FromContext(ctx).Info("converged NTP", "endpoint", ep.Name, "servers", want.Servers)
		return nil
	})
	if err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonNTPFailed, err.Error(), nil
	}
	return metav1.ConditionTrue, metalv1alpha1.ReasonInSync,
		"ntp " + strings.Join(pol.Servers, ","), nil
}

// ntpEqual compares two NTP configurations as ordered sets: a manager with
// fixed server slots serves empty slots, and one that fills every slot
// repeats a server (the X12 serves one server in both of its slots). The
// order stays significant: it is the order of preference.
func ntpEqual(a, b bmc.NTPConfig) bool {
	return a.Enabled == b.Enabled && slices.Equal(ntpServers(a.Servers), ntpServers(b.Servers))
}

// ntpServers drops empty entries and repeats, keeping the first of each.
func ntpServers(servers []string) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// probe proves a credential with a read-only authenticated call. The service
// root is public on every Redfish implementation, so connecting alone proves
// nothing — the Manager read is the authentication oracle.
//
// A credential the manager refused is not tried again until its backoff
// has passed; the probe fails meanwhile without a login.
func (r *EndpointConvergenceReconciler) probe(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, user, pass string) error {
	key := loginKey(ep.Name, user, pass)
	if wait := r.logins.wait(key); wait > 0 {
		return fmt.Errorf("the manager refused this password for %s before; the next login with it is in %s", user, wait.Round(time.Second))
	}
	cfg, _, err := endpointTLSConfig(ctx, r.Client, r.CredentialsNamespace, ep, true)
	if err != nil {
		return err
	}
	cfg.Username, cfg.Password = user, pass
	err = withSession(ctx, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
		_, err := cl.Manager(ctx)
		return err
	})
	switch {
	case err == nil:
		r.logins.forget(key)
	case gofishclient.LoginRefused(err):
		r.logins.refuse(key)
	}
	return err
}

// repair sets the managed account's password through a factory session —
// the break-glass path, and the only way in while no managed password works.
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
	if err := withSession(ctx, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
		return cl.EnsureAccount(ctx, user, pass)
	}); err != nil {
		return fmt.Errorf("factory session: %w", err)
	}
	// The account holds this password now: an earlier refusal of it says
	// nothing any more.
	r.logins.forget(loginKey(ep.Name, user, pass))
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

// passwordSpecials are the punctuation a generated password may hold:
// characters that no vendor rule or Basic authentication treats specially,
// and that a "special character required" rule counts.
const passwordSpecials = "-_."

// randomPassword draws a 16-character password that meets the strictest
// vendor rule met so far: at least one lower-case letter, one upper-case
// letter, one digit and one special character (the iBMC's complexity check
// asks for the last), inside every vendor's length limits.
func randomPassword() (string, error) {
	const (
		lower  = "abcdefghijklmnopqrstuvwxyz"
		upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digits = "0123456789"
		length = 16
	)
	pick := func(n int) (int, error) {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
		if err != nil {
			return 0, err
		}
		return int(v.Int64()), nil
	}
	out := make([]byte, 0, length)
	all := lower + upper + digits + passwordSpecials
	for _, class := range []string{lower, upper, digits, passwordSpecials, all} {
		for len(out) < length {
			i, err := pick(len(class))
			if err != nil {
				return "", err
			}
			out = append(out, class[i])
			if class != all {
				break // one from each required class, the rest from all
			}
		}
	}
	// Shuffle, so the required classes sit at no predictable position.
	for i := len(out) - 1; i > 0; i-- {
		j, err := pick(i + 1)
		if err != nil {
			return "", err
		}
		out[i], out[j] = out[j], out[i]
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
