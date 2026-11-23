// Package fake is the scripted talos.Client for envtest. A Machine models one
// node; tests advance its stage, reconcilers observe it through the seam.
//
// Two reality rules are enforced so tests cannot pass on a dial the real node
// would refuse: the INSECURE dial answers only in maintenance mode (after
// configuration the port serves apid, which rejects unauthenticated clients),
// and the TALOSCONFIG dial answers only once a config was applied (before
// that, nothing serves the cluster PKI).
package fake

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// Machine is one scripted node.
type Machine struct {
	mu           sync.Mutex
	stage        string
	ready        bool
	unmet        []string
	version      string
	applied      [][]byte
	bootstrapped bool
	unreachable  bool
}

// NewMachine returns a blank machine in maintenance mode — the state a
// just-powered-on unprovisioned box presents.
func NewMachine(version string) *Machine {
	return &Machine{stage: "maintenance", version: version}
}

// SetStage scripts the machine's aggregated state.
func (m *Machine) SetStage(stage string, ready bool, unmet ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stage, m.ready, m.unmet = stage, ready, unmet
}

// SetUnreachable makes every dial fail (the machine is off / unrouted).
func (m *Machine) SetUnreachable(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unreachable = v
}

// Applied returns the configs delivered so far.
func (m *Machine) Applied() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.applied)
}

// Bootstrapped reports whether Bootstrap was called.
func (m *Machine) Bootstrapped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bootstrapped
}

// Dialer returns a talos.Dialer that connects every target to this machine.
func (m *Machine) Dialer() talos.Dialer {
	return func(_ context.Context, t talos.Target) (talos.Client, error) {
		m.mu.Lock()
		defer m.mu.Unlock()

		switch {
		case m.unreachable:
			return nil, fmt.Errorf("fake: dial %s: connection refused", t.Endpoint)
		case t.Insecure && t.Talosconfig != nil:
			return nil, fmt.Errorf("fake: dial %s: both Insecure and Talosconfig", t.Endpoint)
		case t.Insecure && m.stage != "maintenance":
			return nil, fmt.Errorf("fake: dial %s: no maintenance service in stage %q", t.Endpoint, m.stage)
		case !t.Insecure && t.Talosconfig == nil:
			return nil, fmt.Errorf("fake: dial %s: neither Insecure nor Talosconfig", t.Endpoint)
		case !t.Insecure && len(m.applied) == 0:
			return nil, fmt.Errorf("fake: dial %s: nothing serves the cluster PKI before a config is applied", t.Endpoint)
		}

		return &conn{m: m}, nil
	}
}

type conn struct {
	m *Machine
}

func (c *conn) ApplyConfiguration(_ context.Context, machineConfig []byte) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	c.m.applied = append(c.m.applied, slices.Clone(machineConfig))
	return nil
}

func (c *conn) Bootstrap(context.Context) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if len(c.m.applied) == 0 {
		return fmt.Errorf("fake: bootstrap before a config was applied")
	}
	c.m.bootstrapped = true
	return nil
}

func (c *conn) Version(context.Context) (string, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	return c.m.version, nil
}

func (c *conn) Status(context.Context) (talos.Status, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	return talos.Status{
		Stage:           c.m.stage,
		Ready:           c.m.ready,
		UnmetConditions: slices.Clone(c.m.unmet),
	}, nil
}

func (c *conn) Close() error { return nil }
