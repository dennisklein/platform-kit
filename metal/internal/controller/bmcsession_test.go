package controller

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// simEndpoint is one endpoint with its credentials Secret, served by the
// named sim fixture through the Dial seam. The dial records every config it
// was handed, so a test can check which rail a lane opened.
type simEndpoint struct {
	ep      *metalv1alpha1.ManagementEndpoint
	cl      client.Client
	state   *sim.State
	fixture *sim.Fixture
	dialed  []gofishclient.Config
	// leaf is the certificate the BMC presents; its key is pinned.
	leaf *x509.Certificate
}

func newSimEndpoint(t *testing.T, fixture string) *simEndpoint {
	t.Helper()
	f, err := sim.LoadFixture(fixture)
	require.NoError(t, err)
	served, err := asset.EphemeralCert()
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(served.Certificate[0])
	require.NoError(t, err)
	ep := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-a", Generation: 3},
		Spec: metalv1alpha1.ManagementEndpointSpec{
			Address:        "https://bmc-a.example.invalid",
			AssetRef:       metalv1alpha1.Reference{Name: "asset-a"},
			CredentialsRef: &corev1.LocalObjectReference{Name: "bmc-a-factory"},
		},
		// The BMC's key is pinned, as after the first observation.
		Status: metalv1alpha1.ManagementEndpointStatus{TLSPin: &metalv1alpha1.TLSPin{
			PublicKey: gofishclient.PublicKeyPin(leaf), Source: metalv1alpha1.TLSPinFirstUse,
		}},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "bmc-a-factory"},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("admin-pass")},
	}
	return &simEndpoint{
		ep:      ep,
		cl:      fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ep, creds).Build(),
		state:   sim.NewState(f),
		fixture: f,
		leaf:    leaf,
	}
}

// probe presents the endpoint's certificate, as the BMC's TLS listener would.
func (s *simEndpoint) probe(context.Context, string) (*x509.Certificate, error) {
	return s.leaf, nil
}

func (s *simEndpoint) dial(_ context.Context, cfg gofishclient.Config) (bmc.Client, error) {
	s.dialed = append(s.dialed, cfg)
	return s.state, nil
}

func TestObserveThroughDialSeam(t *testing.T) {
	s := newSimEndpoint(t, "generic")
	s.ep.Status.Asset = &metalv1alpha1.AssetSnapshot{
		Serial: s.fixture.Chassis.BoardSerial,
		BMCMac: s.fixture.Manager.MAC,
	}
	r := &ManagementEndpointReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial, Probe: s.probe}

	r.observe(t.Context(), s.ep)

	require.Len(t, s.dialed, 1)
	assert.Equal(t, []string{s.ep.Status.TLSPin.PublicKey}, s.dialed[0].PinnedKeys, "the session accepts the pinned key alone")
	assert.True(t, s.dialed[0].ReadOnly, "the observing lane must dial read-only")
	assert.Equal(t, "admin", s.dialed[0].Username)
	for _, ct := range []string{
		metalv1alpha1.ConditionReachable, metalv1alpha1.ConditionInventoried, metalv1alpha1.ConditionVerified,
	} {
		c := meta.FindStatusCondition(s.ep.Status.Conditions, ct)
		require.NotNil(t, c, ct)
		assert.Equal(t, metav1.ConditionTrue, c.Status, "%s: %s %s", ct, c.Reason, c.Message)
		assert.Equal(t, int64(3), c.ObservedGeneration)
	}
	assert.Equal(t, s.fixture.Vendor, s.ep.Status.Vendor)
	assert.Len(t, s.ep.Status.Systems, len(s.fixture.Systems))
}

func TestObserveDialErrorHolds(t *testing.T) {
	s := newSimEndpoint(t, "generic")
	r := &ManagementEndpointReconciler{
		Client: s.cl, CredentialsNamespace: "pk-metal", Probe: s.probe,
		Dial: func(context.Context, gofishclient.Config) (bmc.Client, error) {
			return nil, errors.New("connection refused")
		},
	}

	r.observe(t.Context(), s.ep)

	c := meta.FindStatusCondition(s.ep.Status.Conditions, metalv1alpha1.ConditionReachable)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "DialError", c.Reason)
	assert.Nil(t, meta.FindStatusCondition(s.ep.Status.Conditions, metalv1alpha1.ConditionInventoried),
		"no session, no inventory verdict")
}

func TestConvergeNTPThroughDialSeam(t *testing.T) {
	s := newSimEndpoint(t, "generic")
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial}
	pol := &metalv1alpha1.NTPPolicy{Servers: []string{"192.0.2.10", "192.0.2.11"}}

	status, reason, msg, err := r.ntp(t.Context(), s.ep, pol)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)

	require.Len(t, s.dialed, 1)
	assert.False(t, s.dialed[0].ReadOnly, "converging NTP writes the manager")
	got, err := s.state.NTP(t.Context())
	require.NoError(t, err)
	assert.Equal(t, bmc.NTPConfig{Enabled: true, Servers: pol.Servers}, got)

	// A second pass reads the converged state back and writes nothing.
	status, _, _, err = r.ntp(t.Context(), s.ep, pol)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, status)
}

func TestConvergeNTPRefusalHolds(t *testing.T) {
	// The iBMC has three NTP slots; a fourth server is refused by the
	// manager and the lane must report it, not claim success.
	s := newSimEndpoint(t, "huawei-2288h-v5")
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial}
	pol := &metalv1alpha1.NTPPolicy{Servers: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"}}

	status, reason, _, err := r.ntp(t.Context(), s.ep, pol)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, metalv1alpha1.ReasonNTPFailed, reason)
}
