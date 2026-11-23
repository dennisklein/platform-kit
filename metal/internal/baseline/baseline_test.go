package baseline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func TestFromSystem(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 14, 57, 9, 0, time.UTC)
	got := FromSystem(bmc.System{PowerState: bmc.PowerOn, LastResetTime: t0})
	assert.Equal(t, Observation{PowerState: bmc.PowerOn, LastResetTime: t0}, got)
}
