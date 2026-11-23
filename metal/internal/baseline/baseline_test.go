package baseline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func TestAdvanced(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 14, 57, 9, 0, time.UTC)
	obs := func(t time.Time) Observation { return Observation{LastResetTime: t} }

	assert.True(t, Advanced(obs(t0), obs(t0.Add(time.Microsecond))), "any strict advance proves the reset")
	assert.False(t, Advanced(obs(t0), obs(t0)), "equal is not an advance")
	assert.False(t, Advanced(obs(t0), obs(t0.Add(300*time.Nanosecond))),
		"a persisted baseline survives only at MicroTime granularity — the same reset re-observed with nanoseconds must not read as an advance")
	assert.False(t, Advanced(obs(t0), obs(t0.Add(-time.Second))), "a regression proves nothing")
	assert.False(t, Advanced(obs(time.Time{}), obs(t0)), "no baseline observable, nothing provable")
	assert.False(t, Advanced(obs(t0), obs(time.Time{})), "observable lost, nothing provable")
	assert.False(t, Advanced(obs(time.Time{}), obs(time.Time{})), "vendor without the observable")
}

func TestFromSystem(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 14, 57, 9, 0, time.UTC)
	got := FromSystem(bmc.System{PowerState: bmc.PowerOn, LastResetTime: t0})
	assert.Equal(t, Observation{PowerState: bmc.PowerOn, LastResetTime: t0}, got)
}
