package gofish_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// headerSim serves a fixture and records, per request, whether it carried
// the password (an Authorization header) or a session token.
type headerSim struct {
	srv *httptest.Server
	mu  sync.Mutex
	// basic and token count the requests that carried each credential.
	basic, token int
}

func newHeaderSim(t *testing.T, f *sim.Fixture) *headerSim {
	t.Helper()
	next := sim.NewServer(sim.NewState(f))
	h := &headerSim{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if r.Header.Get("Authorization") != "" {
			h.basic++
		}
		if r.Header.Get("X-Auth-Token") != "" {
			h.token++
		}
		h.mu.Unlock()
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *headerSim) counts() (basic, token int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.basic, h.token
}

// openSessions lists the sessions the service holds, read with Basic.
func (h *headerSim) openSessions(t *testing.T, user, pass string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		h.srv.URL+"/redfish/v1/SessionService/Sessions", nil)
	require.NoError(t, err)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var doc struct {
		Members []json.RawMessage `json:"Members"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	return len(doc.Members)
}

// Where the service offers sessions, the password crosses the wire once, in
// the login; every request after it carries the token, and Close deletes
// the session. This holds for each fixture that serves a SessionService.
func TestSessionLoginSendsThePasswordOnce(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			require.True(t, f.Protocol.Sessions, "every captured personality serves sessions")
			f.Accounts = []sim.FixtureAccount{
				{ID: "2", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
			}
			h := newHeaderSim(t, f)

			c, err := gofishclient.Connect(t.Context(),
				gofishclient.Config{Endpoint: h.srv.URL, Username: "admin", Password: "factory"})
			require.NoError(t, err)
			assert.Equal(t, gofishclient.AuthSession, gofishclient.Authentication(c))
			observationPass(t, c)

			basic, token := h.counts()
			assert.Zero(t, basic, "no request carries the password")
			assert.Positive(t, token, "the requests carry the session token")
			assert.Equal(t, 1, h.openSessions(t, "admin", "factory"))

			require.NoError(t, c.Close())
			assert.Zero(t, h.openSessions(t, "admin", "factory"), "Close deletes the session")
		})
	}
}

// A service without a SessionService gets Basic, and the client says so.
func TestNoSessionServiceFallsBackToBasic(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.Protocol.Sessions = false
	h := newHeaderSim(t, f)

	c, err := gofishclient.Connect(t.Context(), gofishclient.Config{
		Endpoint: h.srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
	})
	require.NoError(t, err)
	defer c.Close()
	assert.Equal(t, gofishclient.AuthBasic, gofishclient.Authentication(c))
	_, err = c.Manager(t.Context())
	require.NoError(t, err)
	basic, token := h.counts()
	assert.Positive(t, basic)
	assert.Zero(t, token)
}

// A login the service refuses for the credentials fails the connect as a
// refused login: Basic would only send the same password again, and each
// refusal counts towards the account lockout.
func TestRefusedSessionLoginFailsTheConnect(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	h := newHeaderSim(t, f)

	_, err = gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: h.srv.URL, Username: f.Accounts[0].Username, Password: "wrong",
	})
	require.Error(t, err)
	assert.True(t, gofishclient.LoginRefused(err), "%v", err)
	basic, _ := h.counts()
	assert.Zero(t, basic, "no Basic retry with the refused password")
}
