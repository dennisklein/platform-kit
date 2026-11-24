package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

// TestRelayTunnelDefaultsToLoopback: REGISTER and DIAL on the tunnel port
// need the shared secret, but the secret does not encrypt the tunnel. The
// tunnel must only be reachable from outside the box when someone binds it
// there on purpose, so its default is a loopback address.
func TestRelayTunnelDefaultsToLoopback(t *testing.T) {
	fs, _, _, _, _, _ := relayFlags()
	def := fs.Lookup("tunnel").DefValue
	host, _, err := net.SplitHostPort(def)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "default tunnel address %q must be an IP, not a name", def)
	assert.True(t, ip.IsLoopback(), "default tunnel address %q is not loopback", def)
}

// TestRelayAllowDefault: the onward dials are limited by default, to
// Redfish's port, so the relay is no general TCP pivot.
func TestRelayAllowDefault(t *testing.T) {
	fs, _, _, _, _, _ := relayFlags()
	def := fs.Lookup("allow").DefValue
	assert.Equal(t, "*:443", def)
	_, err := relay.ParseAllow(def)
	require.NoError(t, err)
}

// TestRelaySecret: the relay may create its secret file, boot never does;
// the environment and a file are not both accepted.
func TestRelaySecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.secret")
	t.Setenv(relay.SecretEnv, "")
	require.NoError(t, os.Unsetenv(relay.SecretEnv))

	_, err := relaySecret(path, false)
	assert.ErrorIs(t, err, os.ErrNotExist, "boot must hold the relay's secret, not make one up")
	_, err = relaySecret("", false)
	assert.ErrorContains(t, err, "needs a shared secret")

	made, err := relaySecret(path, true)
	require.NoError(t, err)
	read, err := relaySecret(path, false)
	require.NoError(t, err)
	assert.Equal(t, made, read, "boot reads the secret the relay wrote")

	t.Setenv(relay.SecretEnv, string(made))
	fromEnv, err := relaySecret("", false)
	require.NoError(t, err)
	assert.Equal(t, made, fromEnv)
	_, err = relaySecret(path, false)
	assert.ErrorContains(t, err, "not both")
}
