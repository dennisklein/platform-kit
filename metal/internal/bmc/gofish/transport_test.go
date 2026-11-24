package gofish_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// An insecure BMC session must not change the process's default transport.
// gofish builds its transport from http.DefaultTransport and copies its TLS
// config by pointer, then sets InsecureSkipVerify on it. Once anything in the
// process has given the default transport a TLS config (an HTTP/2 setup does),
// one lab BMC would turn off certificate checks for every other client that
// uses it, and concurrent sessions would race on the shared config.
func TestInsecureSessionLeavesTheDefaultTransportAlone(t *testing.T) {
	def, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok)
	saved := def.TLSClientConfig
	shared := &tls.Config{MinVersion: tls.VersionTLS12}
	def.TLSClientConfig = shared
	t.Cleanup(func() { def.TLSClientConfig = saved })

	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	srv := httptest.NewTLSServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()

	cfg := gofishclient.Config{Endpoint: srv.URL, Insecure: true}
	if len(f.Accounts) > 0 {
		cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
	}
	c, err := gofishclient.Connect(context.Background(), cfg)
	require.NoError(t, err, "the insecure session reaches the self-signed BMC")
	_, err = c.Systems(context.Background())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	assert.Same(t, shared, def.TLSClientConfig)
	assert.False(t, shared.InsecureSkipVerify, "the default transport still verifies certificates")
}
