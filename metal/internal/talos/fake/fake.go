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
//
// etcd membership is modelled by EtcdCluster, which the members of one cluster
// share: Bootstrap starts one, JoinEtcd scripts a control plane that joined,
// and the membership calls (remove, leave, reset) change it the way they
// change a real cluster, so a stale member left by a forced reset stays
// listed. A machine scripted with SetEtcdRunning alone, without a cluster,
// lists only itself.
//
// Every call can be scripted to fail with SetFailure, by method name.
package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

	// etcd is the cluster this machine is a member of; nil before
	// bootstrap or join, and after leaving.
	etcd        *EtcdCluster
	memberID    uint64
	etcdErrors  []string
	etcdAlarms  []talos.EtcdAlarm
	snapshot    []byte
	recovered   []byte
	bootstraps  []talos.BootstrapOptions
	applyOpts   []talos.ApplyOptions
	reboots     []talos.RebootOptions
	shutdowns   []talos.ShutdownOptions
	resets      []talos.ResetOptions
	upgrades    []talos.UpgradeOptions
	removedByID []uint64
	failures    map[string]error

	// boots counts the restarts since the machine was created; BootTime
	// derives from it. staged is the version a staged upgrade installs at
	// the next boot.
	boots  int
	staged string
}

// bootEpoch is the first boot of every fake machine. Each restart moves the
// boot time a minute on, so a test can compare two readings exactly.
var bootEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

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

// SetUnreachable makes every call fail with Unavailable (the machine is off or
// unrouted). The dial still succeeds, as the real client's does: it connects
// only when a call is made.
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

// SetFailure makes every later call of method (a talos.Client method name,
// "EtcdSnapshot") fail with err; a nil err clears it. The call is still
// recorded where the fake records calls, as a refused request reaches a node.
func (m *Machine) SetFailure(method string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = map[string]error{}
	}
	if err == nil {
		delete(m.failures, method)
		return
	}
	m.failures[method] = err
}

// JoinEtcd scripts the machine as a member of c with etcd data and etcd
// running: a control plane that joined an existing cluster. A learner
// receives the log but does not vote until c.Promote.
func (m *Machine) JoinEtcd(c *EtcdCluster, learner bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etcd = c
	m.memberID = c.add(m.hostname, learner)
	m.etcdData, m.etcdRunning = true, true
}

// Etcd returns the cluster the machine is a member of, or nil.
func (m *Machine) Etcd() *EtcdCluster {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.etcd
}

// SetEtcdErrors scripts the errors this member's etcd reports in its status.
func (m *Machine) SetEtcdErrors(errs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etcdErrors = errs
}

// SetEtcdAlarms scripts the alarms this node's etcd lists.
func (m *Machine) SetEtcdAlarms(alarms ...talos.EtcdAlarm) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etcdAlarms = alarms
}

// SetSnapshot scripts the bytes EtcdSnapshot streams. Unset, it streams a
// fixed text naming the machine.
func (m *Machine) SetSnapshot(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = slices.Clone(b)
}

// Recovered returns the snapshot an EtcdRecover uploaded, or nil.
func (m *Machine) Recovered() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.recovered)
}

// Boot scripts a restart the machine did not ask Talos for (a BMC power
// cycle, a crash): the boot time moves on, and a staged upgrade takes. The
// Talos reboot, upgrade and reset calls boot the machine themselves.
func (m *Machine) Boot() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bootLocked()
}

// Boots counts the restarts since the machine was created.
func (m *Machine) Boots() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boots
}

func (m *Machine) bootLocked() {
	m.boots++
	if m.staged != "" {
		m.version, m.staged = m.staged, ""
	}
}

// Version returns the version the machine reports now.
func (m *Machine) Version() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version
}

// Bootstraps returns the options of every accepted Bootstrap call.
func (m *Machine) Bootstraps() []talos.BootstrapOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.bootstraps)
}

// ApplyCalls returns the options of every non-dry-run apply, in order with
// Applied (a Configure records none).
func (m *Machine) ApplyCalls() []talos.ApplyOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.applyOpts)
}

// Reboots returns every accepted Reboot request.
func (m *Machine) Reboots() []talos.RebootOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.reboots)
}

// Shutdowns returns every accepted Shutdown request.
func (m *Machine) Shutdowns() []talos.ShutdownOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.shutdowns)
}

// Resets returns every accepted Reset request.
func (m *Machine) Resets() []talos.ResetOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.resets)
}

// Upgrades returns every accepted Upgrade request.
func (m *Machine) Upgrades() []talos.UpgradeOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.upgrades)
}

// RemovedByID returns the member IDs removed through this machine.
func (m *Machine) RemovedByID() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.removedByID)
}

func (m *Machine) dial(t talos.Target) (talos.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// An unreachable machine is not refused here: the real client connects
	// lazily, so its dial succeeds and every call fails with Unavailable (see
	// failure). A step that waits for a node to go away must see it the way
	// it sees a real one.
	switch {
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

// EtcdCluster is the membership one etcd cluster's members share.
type EtcdCluster struct {
	mu      sync.Mutex
	nextID  uint64
	leader  uint64
	members []talos.EtcdMember
}

// NewEtcdCluster returns a cluster with no members. Bootstrap creates its
// own; tests create one to JoinEtcd machines into.
func NewEtcdCluster() *EtcdCluster { return &EtcdCluster{nextID: 0x1000} }

func (e *EtcdCluster) add(hostname string, learner bool) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	id := e.nextID
	e.members = append(e.members, talos.EtcdMember{
		ID:         id,
		Hostname:   hostname,
		PeerURLs:   []string{"https://" + hostname + ":2380"},
		ClientURLs: []string{"https://" + hostname + ":2379"},
		IsLearner:  learner,
	})
	if e.leader == 0 && !learner {
		e.leader = id
	}
	return id
}

// Members returns the current membership.
func (e *EtcdCluster) Members() []talos.EtcdMember {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.members)
}

// Leader returns the leading member's ID, zero when none votes.
func (e *EtcdCluster) Leader() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leader
}

// Promote turns the learner with this hostname into a voting member.
func (e *EtcdCluster) Promote(hostname string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.members {
		if e.members[i].Hostname == hostname {
			e.members[i].IsLearner = false
		}
	}
	if e.leader == 0 {
		e.electLocked()
	}
}

// AddStale lists a member no machine runs: what a control plane wiped
// without leaving etcd leaves behind. It returns the member's ID.
func (e *EtcdCluster) AddStale(hostname string) uint64 { return e.add(hostname, false) }

func (e *EtcdCluster) has(id uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.ContainsFunc(e.members, func(m talos.EtcdMember) bool { return m.ID == id })
}

func (e *EtcdCluster) remove(id uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := len(e.members)
	e.members = slices.DeleteFunc(e.members, func(m talos.EtcdMember) bool { return m.ID == id })
	if e.leader == id {
		e.electLocked()
	}
	return len(e.members) < n
}

// forfeit moves leadership off id to the first other voting member.
func (e *EtcdCluster) forfeit(id uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.leader != id {
		return
	}
	for _, m := range e.members {
		if m.ID != id && !m.IsLearner {
			e.leader = m.ID
			return
		}
	}
}

func (e *EtcdCluster) electLocked() {
	e.leader = 0
	for _, m := range e.members {
		if !m.IsLearner {
			e.leader = m.ID
			return
		}
	}
}

type conn struct {
	m *Machine
}

// failure returns the scripted error for method. Callers hold m.mu.
func (m *Machine) failure(method string) error {
	if m.unreachable {
		return status.Errorf(codes.Unavailable, "fake: %s: connection refused", method)
	}
	if err := m.failures[method]; err != nil {
		return fmt.Errorf("fake: %s: %w", method, err)
	}
	return nil
}

// soloID is the member ID of a machine scripted to run etcd outside any
// EtcdCluster: stable per hostname, so two calls agree.
func soloID(hostname string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(hostname))
	return h.Sum64()
}

// ownIDLocked is this machine's member ID. Callers hold m.mu.
func (m *Machine) ownIDLocked() uint64 {
	if m.etcd != nil {
		return m.memberID
	}
	return soloID(m.hostname)
}

// etcdLocked fails unless this machine's etcd runs and is still a member.
// Callers hold m.mu.
func (m *Machine) etcdLocked() error {
	switch {
	case !m.etcdRunning:
		return errors.New("fake: etcd is not running")
	case m.etcd != nil && !m.etcd.has(m.memberID):
		// A removed member's etcd stops serving: it learns from its peers
		// that it is no longer part of the cluster.
		return errors.New("fake: etcd: the member has been removed from the cluster")
	}
	return nil
}

func (c *conn) ApplyConfiguration(_ context.Context, machineConfig []byte, opts talos.ApplyOptions) (talos.ApplyResult, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("ApplyConfiguration"); err != nil {
		return talos.ApplyResult{}, err
	}
	mode := opts.Mode
	if mode == talos.ApplyAuto {
		// The container platform, which the fake stands in for, applies
		// without a reboot.
		mode = talos.ApplyNoReboot
	}
	if opts.DryRun {
		var prev string
		if n := len(c.m.applied); n > 0 {
			prev = string(c.m.applied[n-1])
		}
		diff := lineDiff(prev, string(machineConfig))
		return talos.ApplyResult{Mode: mode, Details: "Dry run summary:\nConfig diff:\n\n" + diff}, nil
	}
	c.m.applied = append(c.m.applied, slices.Clone(machineConfig))
	c.m.applyOpts = append(c.m.applyOpts, opts)
	if mode == talos.ApplyReboot {
		c.m.bootLocked()
	}
	return talos.ApplyResult{Mode: mode, Details: "Applied configuration (" + string(mode) + ")"}, nil
}

func (c *conn) Bootstrap(_ context.Context, opts talos.BootstrapOptions) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if c.m.unreachable {
		return c.m.failure("Bootstrap") // never reaches the node, so it is not counted
	}
	c.m.bootstrapCalls++
	switch {
	case c.m.failures["Bootstrap"] != nil:
		return c.m.failure("Bootstrap")
	case len(c.m.applied) == 0:
		return fmt.Errorf("fake: bootstrap before a config was applied")
	case c.m.etcdData:
		return fmt.Errorf("fake: %w: etcd data directory is not empty", talos.ErrAlreadyBootstrapped)
	case opts.RecoverEtcd && c.m.recovered == nil:
		return fmt.Errorf("fake: bootstrap: recover requested, but no snapshot was uploaded")
	}
	c.m.bootstraps = append(c.m.bootstraps, opts)
	c.m.etcd = NewEtcdCluster()
	c.m.memberID = c.m.etcd.add(c.m.hostname, false)
	c.m.etcdData, c.m.etcdRunning = true, !c.m.etcdStartsLate
	return nil
}

func (c *conn) EtcdMembers(context.Context) ([]talos.EtcdMember, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("EtcdMembers"); err != nil {
		return nil, err
	}
	if err := c.m.etcdLocked(); err != nil {
		return nil, err
	}
	if c.m.etcd == nil {
		return []talos.EtcdMember{{ID: soloID(c.m.hostname), Hostname: c.m.hostname}}, nil
	}
	return c.m.etcd.Members(), nil
}

func (c *conn) EtcdStatus(context.Context) (talos.EtcdStatus, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("EtcdStatus"); err != nil {
		return talos.EtcdStatus{}, err
	}
	if err := c.m.etcdLocked(); err != nil {
		return talos.EtcdStatus{}, err
	}
	st := talos.EtcdStatus{
		MemberID:        c.m.ownIDLocked(),
		LeaderID:        c.m.ownIDLocked(),
		ProtocolVersion: "3.6.0",
		StorageVersion:  "3.6.0",
		DBSize:          int64(len(c.m.snapshotLocked())),
		Errors:          slices.Clone(c.m.etcdErrors),
	}
	if c.m.etcd != nil {
		st.LeaderID = c.m.etcd.Leader()
		for _, mem := range c.m.etcd.Members() {
			if mem.ID == c.m.memberID {
				st.IsLearner = mem.IsLearner
			}
		}
	}
	st.DBSizeInUse = st.DBSize
	return st, nil
}

func (c *conn) EtcdAlarms(context.Context) ([]talos.EtcdAlarm, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("EtcdAlarms"); err != nil {
		return nil, err
	}
	if err := c.m.etcdLocked(); err != nil {
		return nil, err
	}
	return slices.Clone(c.m.etcdAlarms), nil
}

func (m *Machine) snapshotLocked() []byte {
	if m.snapshot != nil {
		return m.snapshot
	}
	return []byte("fake etcd snapshot of " + m.hostname + "\n")
}

func (c *conn) EtcdSnapshot(_ context.Context, w io.Writer) (int64, error) {
	c.m.mu.Lock()
	if err := c.m.failure("EtcdSnapshot"); err != nil {
		c.m.mu.Unlock()
		return 0, err
	}
	if err := c.m.etcdLocked(); err != nil {
		c.m.mu.Unlock()
		return 0, err
	}
	snap := slices.Clone(c.m.snapshotLocked())
	c.m.mu.Unlock()
	// Written outside the lock: w may be slow, as a real sink is.
	return io.Copy(w, bytes.NewReader(snap))
}

func (c *conn) EtcdRecover(_ context.Context, snapshot io.Reader) error {
	// Read before locking: the reader may be a pipe fed by the caller.
	b, err := io.ReadAll(snapshot)
	if err != nil {
		return fmt.Errorf("fake: etcd recover: read snapshot: %w", err)
	}
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	switch {
	case c.m.unreachable, c.m.failures["EtcdRecover"] != nil:
		return c.m.failure("EtcdRecover")
	case c.m.etcdRunning:
		return errors.New("fake: etcd recover: etcd is running on this node")
	}
	c.m.recovered = b
	return nil
}

func (c *conn) EtcdRemoveMemberByID(_ context.Context, memberID uint64) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("EtcdRemoveMemberByID"); err != nil {
		return err
	}
	if err := c.m.etcdLocked(); err != nil {
		return err
	}
	if c.m.etcd == nil || !c.m.etcd.remove(memberID) {
		return fmt.Errorf("fake: etcd remove member: member %x not found", memberID)
	}
	c.m.removedByID = append(c.m.removedByID, memberID)
	return nil
}

func (c *conn) EtcdLeaveCluster(context.Context) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("EtcdLeaveCluster"); err != nil {
		return err
	}
	if err := c.m.etcdLocked(); err != nil {
		return err
	}
	c.m.leaveLocked()
	return nil
}

// leaveLocked removes this machine's member and stops etcd. The data goes
// too: a node that left must not start etcd again from it.
func (m *Machine) leaveLocked() {
	if m.etcd != nil {
		m.etcd.remove(m.memberID)
	}
	m.etcd, m.memberID = nil, 0
	m.etcdData, m.etcdRunning = false, false
}

func (c *conn) EtcdForfeitLeadership(context.Context) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("EtcdForfeitLeadership"); err != nil {
		return err
	}
	if err := c.m.etcdLocked(); err != nil {
		return err
	}
	if c.m.etcd != nil {
		c.m.etcd.forfeit(c.m.memberID)
	}
	return nil
}

func (c *conn) Reboot(_ context.Context, opts talos.RebootOptions) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("Reboot"); err != nil {
		return err
	}
	// The machine stays reachable: tests script the stages a reboot passes
	// through (SetStage) where they matter.
	c.m.reboots = append(c.m.reboots, opts)
	c.m.bootLocked()
	return nil
}

func (c *conn) Shutdown(_ context.Context, opts talos.ShutdownOptions) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("Shutdown"); err != nil {
		return err
	}
	c.m.shutdowns = append(c.m.shutdowns, opts)
	// A machine that shut down answers nothing until something powers it on.
	c.m.unreachable = true
	return nil
}

func (c *conn) Reset(_ context.Context, opts talos.ResetOptions) error {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("Reset"); err != nil {
		return err
	}
	c.m.resets = append(c.m.resets, opts)
	wipes := func(label string) bool {
		if len(opts.SystemPartitions) > 0 {
			return slices.Contains(opts.SystemPartitions, label)
		}
		return opts.Mode != talos.WipeUserDisks
	}
	if wipes("EPHEMERAL") {
		if opts.Graceful {
			c.m.leaveLocked()
		} else {
			// A forced reset leaves the member listed in its peers'
			// membership: the stale member a later removal by ID clears.
			c.m.etcd, c.m.memberID = nil, 0
			c.m.etcdData, c.m.etcdRunning = false, false
		}
	}
	if wipes("STATE") {
		c.m.applied, c.m.applyOpts = nil, nil
		c.m.stage, c.m.ready, c.m.unmet = "maintenance", false, nil
	}
	if !opts.Reboot {
		// Without a reboot a reset ends powered off.
		c.m.unreachable = true
	} else {
		c.m.bootLocked()
	}
	return nil
}

func (c *conn) Upgrade(_ context.Context, opts talos.UpgradeOptions) (string, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("Upgrade"); err != nil {
		return "", err
	}
	if opts.Image == "" {
		return "", errors.New("fake: upgrade: no image")
	}
	c.m.upgrades = append(c.m.upgrades, opts)
	// The reboot into the new image is instant here; a staged upgrade
	// waits for a reboot the test scripts (Boot).
	c.m.staged = imageTag(opts.Image)
	if !opts.Stage {
		c.m.bootLocked()
	}
	return "Upgrade request received", nil
}

// lineDiff is a crude stand-in for the node's config diff: the lines only the
// old config has, then the lines only the new one has. Enough for a test to
// tell "no changes" from "this changed".
func lineDiff(old, cur string) string {
	if old == cur {
		return "No changes."
	}
	oldLines, curLines := strings.Split(old, "\n"), strings.Split(cur, "\n")
	var b strings.Builder
	for _, l := range oldLines {
		if !slices.Contains(curLines, l) {
			b.WriteString("-" + l + "\n")
		}
	}
	for _, l := range curLines {
		if !slices.Contains(oldLines, l) {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

// imageTag is the tag of an image reference, "" when it has none:
// ghcr.io/siderolabs/installer:v1.14.7@sha256:... is v1.14.7.
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}

func (c *conn) Version(context.Context) (string, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("Version"); err != nil {
		return "", err
	}
	return c.m.version, nil
}

func (c *conn) BootTime(context.Context) (time.Time, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("BootTime"); err != nil {
		return time.Time{}, err
	}
	return bootEpoch.Add(time.Duration(c.m.boots) * time.Minute), nil
}

func (c *conn) Status(context.Context) (talos.Status, error) {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	if err := c.m.failure("Status"); err != nil {
		return talos.Status{}, err
	}
	return talos.Status{
		Stage:           c.m.stage,
		Ready:           c.m.ready,
		UnmetConditions: slices.Clone(c.m.unmet),
	}, nil
}

func (c *conn) Close() error { return nil }
