package gofish_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// A session with pinned keys accepts the BMC whose key is pinned, whatever
// signed its certificate, and fails the handshake with any other: no
// request, and so no credential, reaches a BMC that presents another key.
func TestPinnedKeySession(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	var requests atomic.Int32
	next := sim.NewServer(sim.NewState(f))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		next.ServeHTTP(w, r)
	}))
	defer srv.Close()
	served := gofishclient.PublicKeyPin(srv.Certificate())
	cfg := gofishclient.Config{
		Endpoint: srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
	}

	cfg.PinnedKeys = []string{"sha256:00", served}
	c, err := gofishclient.Connect(t.Context(), cfg)
	require.NoError(t, err, "the pinned key is accepted, self-signed or not")
	_, err = c.Manager(t.Context())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	before := requests.Load()
	cfg.PinnedKeys = []string{"sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	cfg.Insecure = true // the pin wins over the opt-out
	_, err = gofishclient.Connect(t.Context(), cfg)
	require.Error(t, err)
	var mismatch *gofishclient.KeyMismatchError
	require.True(t, errors.As(err, &mismatch), "%v", err)
	assert.Equal(t, served, mismatch.Presented, "the error names the presented key")
	assert.Equal(t, before, requests.Load(), "no request reaches a BMC with another key")
}
