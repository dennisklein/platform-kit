package gofish_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// stallingSim serves the generic sim, except that a request for a path under
// stallPrefix gets no answer until the test ends: the BMC has accepted the
// connection and then stopped answering.
func stallingSim(t *testing.T, stallPrefix string) (*httptest.Server, sim.FixtureAccount) {
	t.Helper()
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	next := sim.NewServer(sim.NewState(f))
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, stallPrefix) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv, f.Accounts[0]
}

// A BMC that stops answering ends the request with an error within the
// request timeout, not never.
func TestStalledRequestEndsWithinTheTimeout(t *testing.T) {
	srv, acct := stallingSim(t, "/redfish/v1/Managers")
	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: srv.URL, Username: acct.Username, Password: acct.Password,
		RequestTimeout: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	defer c.Close()

	start := time.Now()
	_, err = c.Manager(context.Background())
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Contains(t, err.Error(), "no answer within 200ms")
}

// Cancelling the context the session was opened with (Ctrl-C in the CLI)
// ends the request in flight at once, long before the request timeout.
func TestCancelEndsAStalledRequest(t *testing.T) {
	srv, acct := stallingSim(t, "/redfish/v1/Managers")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := gofishclient.Connect(ctx, gofishclient.Config{
		Endpoint: srv.URL, Username: acct.Username, Password: acct.Password,
	})
	require.NoError(t, err)
	defer c.Close()

	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err = c.Manager(ctx)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.NotContains(t, err.Error(), "no answer within", "the caller ended it, not the timeout")
}

// One session opens one connection and keeps it for all its requests: a
// TLS handshake with a BMC costs up to a second.
func TestSessionReusesItsConnection(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(sim.NewServer(sim.NewState(f)))
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
		Insecure: true,
	})
	require.NoError(t, err)
	ctx := context.Background()
	_, err = c.Manager(ctx)
	require.NoError(t, err)
	_, err = c.Chassis(ctx)
	require.NoError(t, err)
	_, err = c.Systems(ctx)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Equal(t, int32(1), conns.Load(), "one TLS handshake for the whole session")
}
