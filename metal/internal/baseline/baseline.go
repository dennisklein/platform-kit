// Package baseline is the point-in-time view of a machine's reset observables
// (the reset observables). A HostOperation captures one when it starts, into
// status.baseline, and its steps judge their own progress against it: the
// world moving past the baseline is what completes a step, never a remembered
// step counter. Each consumer does its own comparison, because each treats a
// missing observable differently.
package baseline

import (
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Observation is one point-in-time view of the reset observables.
type Observation struct {
	PowerState bmc.PowerState
	// LastResetTime is zero where the vendor serves no such observable (iBMC) —
	// then nothing is ever provable from it.
	LastResetTime time.Time
}

// FromSystem projects the observables out of a discovered system.
func FromSystem(sys bmc.System) Observation {
	return Observation{PowerState: sys.PowerState, LastResetTime: sys.LastResetTime}
}
