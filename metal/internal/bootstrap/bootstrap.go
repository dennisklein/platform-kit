// Package bootstrap is the day-0 executor's last rung: bring one machine to a
// bootstrapped, Ready Talos node over the machine API. On metal the config
// arrives via the BMC's media channel and this package only bootstraps and
// waits; ApplyInsecure (container/dev tiers — no media) additionally delivers
// the config over the unauthenticated maintenance service, a choice the caller
// makes loudly at the flag.
package bootstrap

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

type Options struct {
	Dial     talos.Dialer
	Endpoint string
	// MachineConfig is the node's rendered config; consumed only with
	// ApplyInsecure (on metal it already rode the media).
	MachineConfig []byte
	// Talosconfig is the cluster's client credentials for the authenticated rungs.
	Talosconfig []byte
	// ApplyInsecure delivers MachineConfig over the maintenance service.
	ApplyInsecure bool
	// Poll is the probe cadence between rungs (default 2s). The overall
	// deadline is the caller's ctx.
	Poll time.Duration
	Logf func(format string, args ...any)
}

// Run climbs: (already Ready? done) → [apply over maintenance] → authenticated
// API answers → etcd bootstrap accepted → stage running + ready. Every wait is
// bounded by ctx; re-runs are safe — a Ready node returns immediately, an
// already-applied config skips the apply.
func Run(ctx context.Context, o Options) error {
	poll := cmp.Or(o.Poll, 2*time.Second)
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	ready, st := o.probeReady(ctx)
	if ready {
		logf("bootstrap: %s already running and ready", o.Endpoint)
		return nil
	}
	// A non-empty stage means the authenticated API answered: the machine is
	// configured, so there is no maintenance service left to apply through.
	if st.Stage != "" {
		logf("bootstrap: machine answers in stage %q", st.Stage)
	} else if o.ApplyInsecure {
		if err := o.applyInsecure(ctx, poll, logf); err != nil {
			return err
		}
	}

	// The authenticated API proves the machine runs a config from our PKI.
	if err := wait(ctx, poll, func() error {
		c, err := o.Dial(ctx, talos.Target{Endpoint: o.Endpoint, Talosconfig: o.Talosconfig})
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		_, err = c.Version(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("bootstrap: machine API never answered at %s: %w", o.Endpoint, err)
	}
	logf("bootstrap: machine API answering at %s", o.Endpoint)

	// Bootstrap needs etcd's pre-state up — retry until the node takes it.
	if err := wait(ctx, poll, func() error {
		c, err := o.Dial(ctx, talos.Target{Endpoint: o.Endpoint, Talosconfig: o.Talosconfig})
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		return c.Bootstrap(ctx)
	}); err != nil {
		return fmt.Errorf("bootstrap: never accepted at %s: %w", o.Endpoint, err)
	}
	logf("bootstrap: etcd bootstrap accepted")

	if err := wait(ctx, poll, func() error {
		c, err := o.Dial(ctx, talos.Target{Endpoint: o.Endpoint, Talosconfig: o.Talosconfig})
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if st.Stage != "running" || !st.Ready {
			return fmt.Errorf("stage %q, unmet %v", st.Stage, st.UnmetConditions)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("bootstrap: node never reached ready at %s: %w", o.Endpoint, err)
	}
	logf("bootstrap: node running and ready")
	return nil
}

// probeReady is the idempotence rung: an authenticated Status that reports
// running+ready means everything below already happened.
func (o *Options) probeReady(ctx context.Context) (bool, talos.Status) {
	c, err := o.Dial(ctx, talos.Target{Endpoint: o.Endpoint, Talosconfig: o.Talosconfig})
	if err != nil {
		return false, talos.Status{}
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(ctx)
	if err != nil {
		return false, talos.Status{}
	}
	return st.Stage == "running" && st.Ready, st
}

// applyInsecure waits for the maintenance service and delivers the config.
func (o *Options) applyInsecure(ctx context.Context, poll time.Duration, logf func(string, ...any)) error {
	if len(o.MachineConfig) == 0 {
		return fmt.Errorf("bootstrap: apply-insecure without a machine config")
	}
	if err := wait(ctx, poll, func() error {
		c, err := o.Dial(ctx, talos.Target{Endpoint: o.Endpoint, Insecure: true})
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Version(ctx); err != nil {
			return err
		}
		return c.ApplyConfiguration(ctx, o.MachineConfig)
	}); err != nil {
		return fmt.Errorf("bootstrap: maintenance service never took the config at %s: %w", o.Endpoint, err)
	}
	logf("bootstrap: config applied over the maintenance service")
	return nil
}

// wait retries fn every poll until it succeeds or ctx ends — the last error
// wins the report.
func wait(ctx context.Context, poll time.Duration, fn func() error) error {
	var last error
	for {
		if last = fn(); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last: %w)", ctx.Err(), last)
		case <-time.After(poll):
		}
	}
}
