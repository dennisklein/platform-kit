package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stmcginnis/gofish/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
)

func TestNTPEqualIsAnOrderedSet(t *testing.T) {
	want := bmc.NTPConfig{Enabled: true, Servers: []string{"a", "b"}}
	for _, got := range [][]string{{"a", "b"}, {"a", "a", "b"}, {"a", "", "b", ""}, {"a", "b", "b"}} {
		assert.True(t, ntpEqual(bmc.NTPConfig{Enabled: true, Servers: got}, want), "%v", got)
	}
	for _, got := range [][]string{{"b", "a"}, {"a"}, {"a", "b", "c"}} {
		assert.False(t, ntpEqual(bmc.NTPConfig{Enabled: true, Servers: got}, want), "%v", got)
	}
	assert.False(t, ntpEqual(bmc.NTPConfig{Servers: []string{"a", "b"}}, want), "enablement counts")
}

// ntpCountingClient counts the NTP writes.
type ntpCountingClient struct {
	bmc.Client
	writes int
}

func (c *ntpCountingClient) SetNTP(ctx context.Context, cfg bmc.NTPConfig) error {
	c.writes++
	return c.Client.SetNTP(ctx, cfg)
}

// The X12 serves its one site time server in both of its slots: a policy
// that names that server is met, with no write.
func TestNTPConvergesOnADoubledServer(t *testing.T) {
	s := newSimEndpoint(t, "supermicro-x12")
	require.Equal(t, []string{"ntp.example.org", "ntp.example.org"}, s.fixture.NTP.Servers)
	counting := &ntpCountingClient{Client: s.state}
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal",
		Dial: func(context.Context, gofishclient.Config) (bmc.Client, error) { return counting, nil }}

	status, reason, msg, err := r.ntp(t.Context(), s.ep, &metalv1alpha1.NTPPolicy{Servers: []string{"ntp.example.org"}})
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	assert.Zero(t, counting.writes, "the goal was met")

	status, reason, msg, err = r.ntp(t.Context(), s.ep, &metalv1alpha1.NTPPolicy{Servers: []string{"a.example.org", "b.example.org"}})
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)
	assert.Equal(t, 1, counting.writes)
}

// A probe with a password the manager refused is not repeated on the next
// pass: each refused login counts towards the manager's lockout. The wait
// doubles, and a success forgets it.
func TestProbeBacksOffARefusedPassword(t *testing.T) {
	s := newSimEndpoint(t, "generic")
	logins := 0
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal",
		Dial: func(_ context.Context, cfg gofishclient.Config) (bmc.Client, error) {
			logins++
			if cfg.Password != "good" {
				return nil, &schemas.Error{HTTPReturnedStatusCode: 401}
			}
			return s.state, nil
		}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.logins.now = func() time.Time { return now }

	require.Error(t, r.probe(t.Context(), s.ep, "pk-metal", "stale"))
	require.Equal(t, 1, logins)
	err := r.probe(t.Context(), s.ep, "pk-metal", "stale")
	require.ErrorContains(t, err, "refused this password")
	assert.Equal(t, 1, logins, "no second login with a known-bad password")

	now = now.Add(loginBackoffMin)
	require.Error(t, r.probe(t.Context(), s.ep, "pk-metal", "stale"))
	assert.Equal(t, 2, logins, "tried again once the backoff passed")
	now = now.Add(loginBackoffMin)
	require.Error(t, r.probe(t.Context(), s.ep, "pk-metal", "stale"))
	assert.Equal(t, 2, logins, "the second refusal doubles the wait")

	require.NoError(t, r.probe(t.Context(), s.ep, "pk-metal", "good"), "another password is not held back")
	r.logins.forget(loginKey(s.ep.Name, "pk-metal", "stale"))
	require.Error(t, r.probe(t.Context(), s.ep, "pk-metal", "stale"))
	assert.Equal(t, 4, logins)
}
