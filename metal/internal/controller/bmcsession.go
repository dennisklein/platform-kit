package controller

import (
	"context"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
)

// Dialer opens a Redfish session to one management endpoint. Production uses
// gofishclient.Connect; unit tests hand in a function that returns the
// simulator, so the endpoint lanes run without an API server or a network.
type Dialer func(ctx context.Context, cfg gofishclient.Config) (bmc.Client, error)

// orDefault returns d, or the gofish dialer when d is nil.
func (d Dialer) orDefault() Dialer {
	if d == nil {
		return gofishclient.Connect
	}
	return d
}

// withSession dials cfg, runs fn with the session and logs out after. The
// whole pass, dial included, is bounded by connectTimeout. The error is the
// dial error when the dial fails, else whatever fn returns; a caller that
// must tell the two apart keeps fn's failures out of the return value.
func withSession(ctx context.Context, dial Dialer, cfg gofishclient.Config, fn func(ctx context.Context, cl bmc.Client) error) error {
	return withSessionFor(ctx, connectTimeout, dial, cfg, fn)
}

// withSessionFor is withSession with a budget of its own, for a pass that
// waits on the BMC longer than a read pass does.
func withSessionFor(ctx context.Context, budget time.Duration, dial Dialer, cfg gofishclient.Config, fn func(ctx context.Context, cl bmc.Client) error) error {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cl, err := dial.orDefault()(cctx, cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck // best-effort logout
	return fn(cctx, cl)
}
