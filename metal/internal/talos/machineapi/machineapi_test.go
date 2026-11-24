package machineapi

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// A machine that accepts the connection and then says nothing must not hold
// a call past CallTimeout: the caller's context carries no deadline of its own.
func TestCallsEndOnASilentMachine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	saved := CallTimeout
	CallTimeout = 500 * time.Millisecond
	t.Cleanup(func() { CallTimeout = saved })

	c, err := Dial(context.Background(), talos.Target{Endpoint: ln.Addr().String(), Insecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	start := time.Now()
	_, err = c.Version(context.Background())
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the call ended near CallTimeout")
}

// Every mode the seam names maps to a machine-API mode and back, so the
// result's Mode reads in the seam's own words.
func TestApplyModesRoundTrip(t *testing.T) {
	for _, mode := range []talos.ApplyMode{talos.ApplyAuto, talos.ApplyNoReboot, talos.ApplyReboot, talos.ApplyStaged, talos.ApplyTry} {
		wire, ok := applyModes[mode]
		require.True(t, ok, "mode %q has a wire value", mode)
		assert.Equal(t, mode, applyModeOf(wire))
	}
}
