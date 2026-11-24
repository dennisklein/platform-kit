// Package poll waits for a condition by asking again, with a pause between
// attempts that ends as soon as the context does. It is standard library
// only on purpose: the BMC client, the boot ladder, the bootstrapper and the
// VM driver import nothing from Kubernetes, and a wait loop is no reason to
// start.
package poll

import (
	"context"
	"time"
)

// Backoff is the pause between attempts. The first pause is Initial; each
// next one is the previous times Factor, capped at Max. A Factor of 1 or less
// keeps the pause constant.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64
}

// Every is a constant pause of d.
func Every(d time.Duration) Backoff { return Backoff{Initial: d} }

func (b Backoff) next(d time.Duration) time.Duration {
	if b.Factor <= 1 {
		return d
	}
	n := time.Duration(float64(d) * b.Factor)
	if b.Max > 0 && n > b.Max {
		n = b.Max
	}
	return n
}

// Until calls fn, then pauses and calls it again, until fn reports done or
// returns an error. The first call is immediate. It returns nil when fn is
// done, fn's error as is, or ctx.Err() when ctx ends during a pause. A
// deadline that belongs to the condition itself (as opposed to the caller's
// patience) is fn's to check, so the error can say what did not happen.
func Until(ctx context.Context, b Backoff, fn func(ctx context.Context) (done bool, err error)) error {
	delay := b.Initial
	for {
		done, err := fn(ctx)
		if err != nil || done {
			return err
		}
		if err := Sleep(ctx, delay); err != nil {
			return err
		}
		delay = b.next(delay)
	}
}

// Sleep pauses for d. It returns ctx.Err() at once when ctx ends first, and
// nil otherwise.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
