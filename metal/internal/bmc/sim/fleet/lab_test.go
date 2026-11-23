package fleet_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

// The committed lab fleet is read by the lab and the lab-e2e workflow only,
// both of which need KVM. A typo in it, or a personality that names no
// fixture, fails here instead of half an hour into a lab run.
func TestCommittedLabFleet(t *testing.T) {
	cfg, err := fleet.Load(filepath.Join("..", "..", "..", "..", "..", "lab", "fleet.yaml"))
	require.NoError(t, err)
	require.Len(t, cfg.Machines, 3, "lab/manifests describes three machines")

	personalities := map[string]bool{}
	for _, m := range cfg.Machines {
		_, err := sim.LoadFixture(m.Personality)
		require.NoError(t, err, "machine %s", m.Name)
		personalities[m.Personality] = true
		assert.Equal(t, fleet.BackendQemu, m.Backend, "machine %s", m.Name)
	}
	assert.Greater(t, len(personalities), 1, "the lab runs more than one BMC dialect")
}
