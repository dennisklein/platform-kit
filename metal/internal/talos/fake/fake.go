// Package fake is the scripted talos.Client for envtest. A Machine models one
// node; tests advance its stage, reconcilers observe it through the seam. A
// Fleet routes dials to machines by address, so a cluster of nodes can be
// scripted.
//
// Reality rules are enforced so tests cannot pass on a call a real node would
// refuse: the INSECURE dial answers only in maintenance mode (after
// configuration the port serves apid, which rejects unauthenticated clients);
// the TALOSCONFIG dial answers only once a config was applied (before that,
// nothing serves the cluster PKI); Bootstrap answers AlreadyExists once the
// node has etcd data; and etcd lists members only where it runs.
package fake

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// Machine is one scripted node.
type Machine struct {
	mu             sync.Mutex
	hostname       string
	stage          string
	ready          bool
	unmet          []string
	version        string
	applied        [][]byte
	etcdData       bool
	etcdRunning    bool
	bootstrapCalls int
	etcdStartsLate bool
	unreachable    bool
}

// SetEtcdStartsLate makes a successful Bootstrap write etcd data without
// starting etcd: it stays not-running until SetEtcdRunning(true), the window
// a real node spends pulling and starting etcd after accepting the call.
func (m *Machine) SetEtcdStartsLate(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etcdStartsLate = v
}

// NewMachine returns a blank machine in maintenance mode: the state a
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

// Configure records a config as delivered by other means (the boot media on
// metal), as if it had been applied.
func (m *Machine) Configure(machineConfig []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applied = append(m.applied, slices.Clone(machineConfig))
}

// Bootstrapped reports whether a Bootstrap call succeeded on this machine.
func (m *Machine) Bootstrapped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.etcdData
}

// SetEtcdRunning scripts etcd stopping (a reboot) or starting again without
// touching its data, so a bootstrapped node can look un-bootstrapped to an
// observer that only asks etcd.
func (m *Machine) SetEtcdRunning(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etcdRunning = v
}

// BootstrapCalls counts every Bootstrap call, including refused ones.
func (m *Machine) BootstrapCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bootstrapCalls
}

// Dialer returns a talos.Dialer that connects every target to this machine.
func (m *Machine) Dialer() talos.Dialer {
	return func(_ context.Context, t talos.Target) (talos.Client, error) {
		return m.dial(t)
	}
}

func (m *Machine) dial(t talos.Target) (talos.Client, error) {
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

// Fleet routes dials to machines by host, the way a network would.
type Fleet struct {
	mu       sync.Mutex
	machines map[string]*Machine
}

// NewFleet returns an empty fleet.
func NewFleet() *Fleet { return &Fleet{machines: map[string]*Machine{}} }

// Add places a machine at address under a hostname (what etcd lists it as).
func (f *Fleet) Add(address, hostname string, m *Machine) {
	m.mu.Lock()
	m.hostname = hostname
	m.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.machines[address] = m
}

// Dialer routes each target to the machine at its host.
func (f *Fleet) Dialer() talos.Dialer {
	return func(_ context.Context, t talos.Target) (talos.Client, error) {
		host := t.Endpoint
		if h, _, err := net.SplitHostPort(t.Endpoint); err == nil {
			host = h
		}
		f.mu.Lock()
		m := f.machines[host]
		f.mu.Unlock()
		if m == nil {
			return nil, fmt.Errorf("fake: dial %s: no route to host", t.Endpoint)
		}
		return m.dial(t)
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
	c.m.bootstrapCalls++
	switch {
	case len(c.m.applied) == 0:
		return fmt.Errorf("fake: bootstrap before a config was applied")
	case c.m.etcdData:
		return fmt.Errorf("fake: %w: etcd data directory is not empty", talos.ErrAlreadyBootstrapped)
	}
	c.m.etcdData, c.m.etcdRunning = true, !c.m.etcdStartsLate
	return nil
}

func (c *conn) EtcdMembers(context.Context) ([]string, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if !c.m.etcdRunning {
		return nil, fmt.Errorf("fake: etcd is not running")
	}
	return []string{c.m.hostname}, nil
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
