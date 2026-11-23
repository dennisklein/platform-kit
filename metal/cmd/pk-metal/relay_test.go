package main

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelayTunnelDefaultsToLoopback: REGISTER and DIAL on the tunnel port
// are not authenticated, and DIAL dials onward for whoever connects. The
// tunnel must only be reachable from outside the box when someone binds it
// there on purpose, so its default is a loopback address.
func TestRelayTunnelDefaultsToLoopback(t *testing.T) {
	fs, _, _, _ := relayFlags()
	def := fs.Lookup("tunnel").DefValue
	host, _, err := net.SplitHostPort(def)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "default tunnel address %q must be an IP, not a name", def)
	assert.True(t, ip.IsLoopback(), "default tunnel address %q is not loopback", def)
}
