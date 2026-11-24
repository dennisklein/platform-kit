package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// slowFixture writes the generic fixture with the timing of a real BMC: a
// reset takes a while to show in the power state, and every answer is late.
func slowFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../bmc/sim/fixtures/generic.yaml")
	require.NoError(t, err)
	slow := strings.Replace(string(raw), "protocol:\n",
		"protocol:\n  power_transition: 700ms\n  latency: 5ms\n", 1)
	require.NotEqual(t, string(raw), slow)
	path := filepath.Join(t.TempDir(), "slow.yaml")
	require.NoError(t, os.WriteFile(path, []byte(slow), 0o600))
	return path
}

// On a BMC that reports PoweringOff and PoweringOn while a reset takes
// effect, a power cycle is still one off and one on: a pass that finds the
// machine on its way does not send the action again.
func TestPowerCycleOnASlowBMC(t *testing.T) {
	r, st, rec := executorFor(t, slowFixture(t),
		preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}}))
	require.NoError(t, st.SetPower(t.Context(), "1", bmc.ActionOn))
	require.Eventually(t, func() bool {
		p, err := st.Power(t.Context(), "1")
		return err == nil && p == bmc.PowerOn
	}, 5*time.Second, 50*time.Millisecond)
	before := st.Resets("1")

	op := runOp(t, r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionForceOff, bmc.ActionOn}, rec.powers, "one off, one on")
	assert.Equal(t, before+1, st.Resets("1"), "exactly one boot")
}
