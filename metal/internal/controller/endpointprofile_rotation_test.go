package controller

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// authDialer serves a sim through the Dial seam with its accounts enforced:
// a dial with a credential the sim does not accept fails and is counted, the
// way a failed login counts towards a real manager's lockout.
type authDialer struct {
	st           *sim.State
	failedLogins int
	// refuse makes the manager reject every password it is asked to set, as
	// a complexity rule the generated password does not meet would.
	refuse bool
}

func (d *authDialer) dial(_ context.Context, cfg gofishclient.Config) (bmc.Client, error) {
	if !d.st.Authenticate(cfg.Username, cfg.Password) {
		d.failedLogins++
		return nil, errors.New("401: the username or password is not valid")
	}
	return refusingClient{Client: d.st, refuse: d.refuse}, nil
}

type refusingClient struct {
	bmc.Client
	refuse bool
}

func (c refusingClient) EnsureAccount(ctx context.Context, username, password string) error {
	if c.refuse {
		return errors.New("400: the password does not meet the complexity requirements")
	}
	return c.Client.EnsureAccount(ctx, username, password)
}

// rotationRig is one endpoint (generic sim, factory admin/admin-pass) with
// the convergence reconciler wired to it through an authenticating dialer.
type rotationRig struct {
	*simEndpoint
	d   *authDialer
	r   *EndpointConvergenceReconciler
	pol *metalv1alpha1.CredentialsPolicy
}

func newRotationRig(t *testing.T) *rotationRig {
	s := newSimEndpoint(t, "generic")
	d := &authDialer{st: s.state}
	return &rotationRig{
		simEndpoint: s,
		d:           d,
		r:           &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: d.dial},
		pol:         &metalv1alpha1.CredentialsPolicy{Username: "pk-metal", MaxAge: &metav1.Duration{Duration: time.Hour}},
	}
}

func (g *rotationRig) pass(t *testing.T) (metav1.ConditionStatus, string, string) {
	t.Helper()
	status, reason, msg, err := g.r.credentials(t.Context(), g.ep, g.pol)
	require.NoError(t, err)
	return status, reason, msg
}

func (g *rotationRig) secret(t *testing.T) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{}
	require.NoError(t, g.cl.Get(t.Context(), client.ObjectKey{Namespace: "pk-metal", Name: managedCredsName(g.ep.Name)}, sec))
	return sec
}

// age makes the managed password due for rotation.
func (g *rotationRig) age(t *testing.T) {
	t.Helper()
	sec := g.secret(t)
	sec.Annotations[rotatedAtAnnotation] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	require.NoError(t, g.cl.Update(t.Context(), sec))
}

func TestRotationAdoptsThroughFactory(t *testing.T) {
	g := newRotationRig(t)

	status, reason, msg := g.pass(t)
	require.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)

	sec := g.secret(t)
	assert.Equal(t, "pk-metal", string(sec.Data["username"]))
	assert.True(t, g.state.Authenticate("pk-metal", string(sec.Data["password"])), "promoted only once it logs in")
	assert.NotContains(t, sec.Data, keyNextPassword, "nothing stays parked after a promotion")
	assert.NotEmpty(t, sec.Annotations[rotatedAtAnnotation])
	assert.Zero(t, g.d.failedLogins, "adoption tries no credential it does not hold")
}

func TestRotationWithoutFactoryLogin(t *testing.T) {
	// Site policy changed the factory password after adoption: the
	// break-glass no longer opens, and rotation must not depend on it.
	g := newRotationRig(t)
	g.pass(t)
	before := string(g.secret(t).Data["password"])
	require.NoError(t, g.state.SetAccountPassword("admin", "changed-by-site-policy"))
	g.age(t)

	status, reason, msg := g.pass(t)
	require.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)

	after := string(g.secret(t).Data["password"])
	assert.NotEqual(t, before, after)
	assert.True(t, g.state.Authenticate("pk-metal", after), "set through the managed account's own session")
	assert.False(t, g.state.Authenticate("pk-metal", before), "the old password is dead")
	assert.Zero(t, g.d.failedLogins, "the dead factory login was never tried")
}

func TestRotationRefusedKeepsWorkingPassword(t *testing.T) {
	g := newRotationRig(t)
	g.pass(t)
	working := string(g.secret(t).Data["password"])
	g.age(t)
	g.d.refuse = true

	status, reason, msg := g.pass(t)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, metalv1alpha1.ReasonRotationFailed, reason)
	assert.Contains(t, msg, "working password of pk-metal is kept")

	sec := g.secret(t)
	assert.Equal(t, working, string(sec.Data["password"]), "the working password is never overwritten by a refused one")
	assert.True(t, g.state.Authenticate("pk-metal", working), "every lane keeps dialling a password that works")
	parked := string(sec.Data[keyNextPassword])
	require.NotEmpty(t, parked, "the candidate waits beside it")

	cfg, _, err := endpointClientConfig(t.Context(), g.cl, "pk-metal", g.ep, true)
	require.NoError(t, err)
	assert.Equal(t, working, cfg.Password, "dials use the working password, not the candidate")

	// A second refused pass retries the same candidate.
	status, _, _ = g.pass(t)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, parked, string(g.secret(t).Data[keyNextPassword]))
	assert.Zero(t, g.d.failedLogins,
		"a refused password is never tried while the working one still logs in: no step towards a lockout")

	g.d.refuse = false
	status, reason, msg = g.pass(t)
	require.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	assert.Equal(t, parked, string(g.secret(t).Data["password"]), "the parked candidate is the one promoted")
	assert.True(t, g.state.Authenticate("pk-metal", parked))
}

func TestRotationResumesAfterCrash(t *testing.T) {
	// The manager took the candidate, then the operator died before it could
	// promote it: the Secret's working password is now the dead one.
	g := newRotationRig(t)
	g.pass(t)
	sec := g.secret(t)
	sec.Data[keyNextUsername] = []byte("pk-metal")
	sec.Data[keyNextPassword] = []byte("Candidate-0000.x")
	require.NoError(t, g.cl.Update(t.Context(), sec))
	require.NoError(t, g.state.SetAccountPassword("pk-metal", "Candidate-0000.x"))
	require.NoError(t, g.state.SetAccountPassword("admin", "changed-by-site-policy"))

	status, reason, msg := g.pass(t)
	require.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	sec = g.secret(t)
	assert.Equal(t, "Candidate-0000.x", string(sec.Data["password"]), "the candidate the manager holds is promoted")
	assert.NotContains(t, sec.Data, keyNextPassword)
}

func TestClientConfigWhileOnlyACandidateIsParked(t *testing.T) {
	// The first rotation parked its candidate in a new managed Secret, and the
	// manager has not taken it: the factory login is still the way in.
	g := newRotationRig(t)
	require.NoError(t, g.cl.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: managedCredsName(g.ep.Name)},
		Data:       map[string][]byte{keyNextUsername: []byte("pk-metal"), keyNextPassword: []byte("Candidate-0000.x")},
	}))
	cfg, _, err := endpointClientConfig(t.Context(), g.cl, "pk-metal", g.ep, true)
	require.NoError(t, err)
	assert.Equal(t, "admin", cfg.Username)
	assert.Equal(t, "admin-pass", cfg.Password)
}

// A new username replaces the managed account: once the new one logs in,
// the old one is disabled through it, so no admin account stays behind that
// nobody rotates. The factory account is never disabled.
func TestRenamedAccountIsDisabledAfterTheNewOneWorks(t *testing.T) {
	g := newRotationRig(t)
	g.pass(t)
	old := string(g.secret(t).Data["password"])

	var trail bytes.Buffer
	g.r.Audit = audit.New(&trail, testOperator)
	g.pol.Username = "pk-metal-2"
	status, reason, msg := g.pass(t)
	require.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	sec := g.secret(t)
	require.Equal(t, "pk-metal-2", string(sec.Data["username"]))
	assert.Equal(t, "pk-metal", sec.Annotations[retireAccountAnnotation], "the replaced account is remembered")
	assert.True(t, g.state.AccountEnabled("pk-metal"), "not disabled before the new account is promoted")

	status, reason, msg = g.pass(t)
	require.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	assert.Contains(t, msg, "replaced account pk-metal is disabled")
	assert.False(t, g.state.AccountEnabled("pk-metal"))
	assert.False(t, g.state.Authenticate("pk-metal", old), "the replaced account logs in no more")
	assert.True(t, g.state.AccountEnabled("admin"), "the factory account stays")
	assert.NotContains(t, g.secret(t).Annotations, retireAccountAnnotation, "the disable is done")

	// The trail has both changes to the BMC, and no password.
	assert.Contains(t, trail.String(), `"event":"bmc.account.set","kind":"ManagementEndpoint"`)
	assert.Contains(t, trail.String(), `"account":"pk-metal-2"`)
	assert.Contains(t, trail.String(), `"event":"bmc.account.disabled"`)
	assert.Contains(t, trail.String(), `"reason":"Replaced","account":"pk-metal"`)
	assert.NotContains(t, trail.String(), old)

	// The factory account is never named for retirement.
	assert.True(t, g.r.isFactoryAccount(t.Context(), g.ep, "admin"))
	assert.False(t, g.r.isFactoryAccount(t.Context(), g.ep, "pk-metal"))
}
