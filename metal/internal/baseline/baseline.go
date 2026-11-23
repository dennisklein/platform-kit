// Package baseline is the re-derivable "did I already reboot for this request"
// predicate (design doc § State): a HostOperation captures an Observation at issue
// time, and completion is the world moving PAST it — never a remembered step
// counter. The same predicate keeps a resumed executor from re-issuing a reset
// into a box already mid-reboot, and lets the host projection tell an explained
// reset advance from drift.
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

// Advanced reports whether current proves the world moved past base: a strict
// reset-time advance. An unobservable side (either zero) proves nothing —
// vendors without the observable degrade to issue-and-trust, never to guessing.
//
// The comparison runs at microsecond granularity: a baseline round-trips the
// API as metav1.MicroTime, so a fresh nanosecond-precision observation of the
// SAME reset would otherwise read as after its own persisted baseline — and a
// converge-to-goal reboot step would skip the reboot it never issued.
func Advanced(base, current Observation) bool {
	return !base.LastResetTime.IsZero() && !current.LastResetTime.IsZero() &&
		current.LastResetTime.Truncate(time.Microsecond).After(base.LastResetTime.Truncate(time.Microsecond))
}
