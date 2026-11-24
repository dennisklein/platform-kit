package sim

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// State is the sim's mutable world: a Fixture plus everything an operation can
// change (power, boot override, mounted media, firmware, accounts, injected
// faults). It implements bmc.Client directly — the in-memory backend for unit
// tests — and Server serves the same State over HTTP for the production client
// and curl. The files of this package split the world by concern: power and
// boot here, media in media.go, firmware and BIOS in convergence.go, and so on.
type State struct {
	mu        sync.Mutex
	f         *Fixture
	power     map[string]bmc.PowerState // by system id
	boot      map[string]*bootOverride
	media     map[string]*mountedMedia // by media slot id
	faults    map[Fault]bool
	resets    map[string]int        // boots per system — test observability
	lastReset map[string]time.Time  // advances on reset — only for systems whose fixture serves it
	powerAt   map[string]time.Time  // when the last reset changed the power state (Protocol.PowerTransition)
	machines  map[string]Machine    // real machines behind systems (machine.go); absent = state only
	progress  map[string]string     // boot progress last read from a machine, by system id
	fw        fwState               // manager firmware and staging slot (convergence.go)
	bios      map[string]*biosState // by system id; nil entry = no BIOS resource served
	accounts  []*account            // manager accounts (accounts.go); empty = open sim, no auth
	sessions  map[string]*session   // by token (sessions.go)
	tasks     []*task               // every task ever started (tasks.go)
	ntp       bmc.NTPConfig         // time sources (ntp.go)
	identity  identityState         // HTTPS identity (identity.go)
	cache     *MediaCache           // on-disk store for large public images; nil = pull into memory
}

// bootOverride is one system's Boot object. A Once override is used up by
// the next boot: its enablement falls back to Disabled, the target stays —
// the specification resets BootSourceOverrideEnabled and nothing else.
type bootOverride struct {
	target  string // "" reads as None
	enabled string // Once | Continuous | Disabled; "" reads as Disabled
	once    bool   // the last override set was a one-time one (test observability)
	mode    string // BootSourceOverrideMode; empty = not served
}

// armed reports whether the next boot takes the override.
func (b *bootOverride) armed() bool {
	return b != nil && b.target != "" && (b.enabled == bmc.OverrideOnce || b.enabled == bmc.OverrideContinuous)
}

// Fault injection — the lying-oracle states real hardware won't produce on demand.
type Fault string

const (
	// FaultUnreachable makes every call fail — the dead-BMC path.
	FaultUnreachable Fault = "unreachable"
	// FaultStalePower freezes reported power state regardless of actions.
	FaultStalePower Fault = "stale-power"
	// FaultFlashFails makes a correct firmware upload answer success while
	// nothing ever flashes — no version advance, no BMC reboot. The recorded
	// X12 pattern generalised: completion must be judged by observables.
	FaultFlashFails Fault = "flash-fails"
	// FaultBiosStuck holds staged BIOS settings across every reset — pending
	// never applies, never clears.
	FaultBiosStuck Fault = "bios-stuck"
	// FaultCertStuck answers success to a certificate import/upload while
	// the listener keeps serving the old identity — the lying oracle for the
	// identity lane.
	FaultCertStuck Fault = "cert-stuck"
)

func NewState(f *Fixture) *State {
	s := &State{
		f:         f,
		power:     map[string]bmc.PowerState{},
		boot:      map[string]*bootOverride{},
		media:     map[string]*mountedMedia{},
		faults:    map[Fault]bool{},
		resets:    map[string]int{},
		lastReset: map[string]time.Time{},
		powerAt:   map[string]time.Time{},
		progress:  map[string]string{},
		fw:        fwState{version: f.Manager.Firmware, lastReset: f.Manager.LastResetTime, versions: map[string]string{}, onReset: map[string]string{}},
		bios:      map[string]*biosState{},
		sessions:  map[string]*session{},
		ntp:       bmc.NTPConfig{Enabled: f.NTP.Enabled, Servers: f.NTP.Servers},
	}
	s.seedAccounts()
	for _, sys := range f.Systems {
		s.power[sys.ID] = bmc.PowerState(sys.Power)
		s.boot[sys.ID] = &bootOverride{mode: sys.BootMode}
		if !sys.LastResetTime.IsZero() {
			s.lastReset[sys.ID] = sys.LastResetTime
		}
		s.bios[sys.ID] = nil // present-but-nil = existing system, no BIOS resource
		if f.Bios.Attributes != nil {
			s.bios[sys.ID] = &biosState{current: jsonTypes(f.Bios.Attributes)}
		}
	}
	return s
}

// Fixture is the fixture this state was born from.
func (s *State) Fixture() *Fixture { return s.f }

// SetFault toggles an injected fault.
func (s *State) SetFault(f Fault, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[f] = on
}

// BootOverride reports the last boot override that was set (test
// observability) — still readable after a boot spent a one-time override.
// Empty target means none was ever set.
func (s *State) BootOverride(systemID string) (target string, once bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bo := s.boot[systemID]; bo != nil {
		return bo.target, bo.once
	}
	return "", false
}

// BootMode reports a system's BootSourceOverrideMode (test observability).
func (s *State) BootMode(systemID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bo := s.boot[systemID]; bo != nil {
		return bo.mode
	}
	return ""
}

// Resets reports how many boots a system has seen (test observability).
func (s *State) Resets(systemID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets[systemID]
}

func (s *State) reachable() error {
	if s.faults[FaultUnreachable] {
		return errUnreachable
	}
	return nil
}

// system finds a fixture system by id.
func (f *Fixture) system(id string) *FixtureSystem {
	for i := range f.Systems {
		if f.Systems[i].ID == id {
			return &f.Systems[i]
		}
	}
	return nil
}

func (s *State) Manager(context.Context) (bmc.Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.Manager{}, err
	}
	return bmc.Manager{
		Vendor:          s.f.Vendor,
		FirmwareVersion: s.fw.version,
		Hostname:        s.f.Manager.Hostname,
		MAC:             s.f.Manager.MAC,
		LastResetTime:   s.fw.lastReset,
	}, nil
}

func (s *State) Chassis(context.Context) (bmc.Chassis, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.Chassis{}, err
	}
	ch := bmc.Chassis{BoardSerial: s.f.Chassis.BoardSerial, Model: s.f.Chassis.Model}
	for _, a := range s.f.Chassis.Adapters {
		ch.Adapters = append(ch.Adapters, bmc.Adapter{Model: a.Model, PortMACs: slices.Clone(a.PortMACs)})
	}
	return ch, nil
}

func (s *State) Systems(ctx context.Context) ([]bmc.System, error) {
	s.syncMachines(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	var out []bmc.System
	for _, fs := range s.f.Systems {
		sys := bmc.System{
			ID:            fs.ID,
			UUID:          fs.UUID,
			SerialNumber:  fs.Serial,
			Manufacturer:  fs.Manufacturer,
			Model:         fs.Model,
			PowerState:    s.reportedPower(fs.ID),
			LastResetTime: s.lastReset[fs.ID],
			MemoryGiB:     fs.MemoryGiB,
			CPUModel:      fs.CPUModel,
			CPUCount:      fs.CPUCount,
			BootTargets:   fs.BootTargets,
			BootProgress:  s.bootProgress(&fs),
		}
		sys.BootOverride.Enabled, sys.BootOverride.Target, sys.BootOverride.Mode = s.bootViewLocked(fs.ID)
		for _, n := range fs.NICs {
			sys.NICs = append(sys.NICs, bmc.NIC{ID: n.ID, MAC: n.MAC})
		}
		for _, d := range fs.Drives {
			sys.Drives = append(sys.Drives, bmc.Drive{Model: d.Model, Serial: d.Serial, CapacityGiB: d.CapacityGiB, MediaType: d.Media, Protocol: d.Protocol})
		}
		out = append(out, sys)
	}
	return out, nil
}

// bootProgress is BootProgress.LastState for a system whose fixture serves it:
// None while off, whatever the machine behind the system reports while on, and
// OSRunning where no machine exists to say otherwise — a state-only system is
// up the moment it has power.
func (s *State) bootProgress(fs *FixtureSystem) string {
	switch {
	case !fs.BootProgress:
		return ""
	case s.power[fs.ID] != bmc.PowerOn:
		return "None"
	case s.progress[fs.ID] != "":
		return s.progress[fs.ID]
	default:
		return "OSRunning"
	}
}

func (s *State) Capabilities(context.Context) (bmc.Capabilities, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.Capabilities{}, err
	}
	caps := bmc.Capabilities{
		MediaDialect:    map[string]bmc.MediaDialect{},
		UpdateDialect:   s.f.updateDialect(),
		IdentityDialect: s.f.identityDialect(),
	}
	for i, vm := range s.f.VirtualMedia {
		caps.MediaDialect[vm.ID] = s.f.dialect(i)
	}
	return caps, nil
}

// reportedPower honours the stale-power fault: the world moved, the oracle lies.
func (s *State) reportedPower(systemID string) bmc.PowerState {
	if s.faults[FaultStalePower] {
		return bmc.PowerOn
	}
	p := s.power[systemID]
	if d := s.f.Protocol.PowerTransition; d > 0 && time.Since(s.powerAt[systemID]) < d {
		// The state the reset moves to, not reached yet.
		if p == bmc.PowerOn {
			return bmc.PowerPoweringOn
		}
		return bmc.PowerPoweringOff
	}
	return p
}

func (s *State) Power(ctx context.Context, systemID string) (bmc.PowerState, error) {
	s.syncMachines(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return "", err
	}
	if _, ok := s.power[systemID]; !ok {
		return "", notFound("ComputerSystem", systemID)
	}
	return s.reportedPower(systemID), nil
}

// SetPower is ComputerSystem.Reset. Every action that boots the system pulls
// the mounted media first and hands the machine behind the system its boot
// source, the way hardware has the virtual drive attached and the override
// latched before the firmware runs.
func (s *State) SetPower(ctx context.Context, systemID string, action bmc.PowerAction) error {
	s.syncMachines(ctx)
	verb, err := s.powerVerb(systemID, action)
	if err != nil {
		return err
	}
	boots := verb != "stop"
	// Outside the lock from here: the pull is network I/O against the asset
	// server, the machine op a process spawn. A failure in either fails the
	// action with the sim's view untouched.
	if boots {
		if err := s.presentMedia(ctx, systemID); err != nil {
			return err
		}
		if err := s.presentBootSource(ctx, systemID); err != nil {
			return err
		}
	}
	if err := s.driveMachine(ctx, systemID, verb); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !boots {
		if s.power[systemID] != bmc.PowerOff {
			s.powerAt[systemID] = time.Now()
		}
		s.power[systemID] = bmc.PowerOff
		return nil
	}
	s.powerAt[systemID] = time.Now()
	s.power[systemID] = bmc.PowerOn
	s.resets[systemID]++
	// The reset-time observable only advances where the fixture serves it —
	// the sim never invents a fact real hardware withholds.
	if _, served := s.lastReset[systemID]; served {
		s.lastReset[systemID] = time.Now()
	}
	if bo := s.boot[systemID]; bo.enabled == bmc.OverrideOnce {
		bo.enabled = bmc.OverrideDisabled
	}
	// A system coming out of reset applies its staged BIOS settings — the
	// @Redfish.Settings pending→applied edge.
	s.applyBios(systemID)
	// And the firmware staged for the next reset (apply time OnReset).
	s.applyStagedFirmware()
	return nil
}

// powerVerb validates a reset request against the system's state and its
// advertised reset types, and names what the machine has to do for it.
func (s *State) powerVerb(systemID string, action bmc.PowerAction) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return "", err
	}
	fs := s.f.system(systemID)
	if fs == nil {
		return "", notFound("ComputerSystem", systemID)
	}
	if !slices.Contains(fs.resetTypes(), string(action)) {
		return "", notInList("ActionParameterValueNotInList", "ResetType", string(action))
	}
	on := s.power[systemID] == bmc.PowerOn
	switch action {
	case bmc.ActionOn:
		return "start", nil
	case bmc.ActionForceOff, bmc.ActionGracefulShutdown:
		return "stop", nil
	case bmc.ActionForceRestart, bmc.ActionGracefulRestart:
		if !on {
			return "", refuse("%s on powered-off system %q", action, systemID)
		}
		return "restart", nil
	case bmc.ActionPowerCycle:
		if !on {
			return "start", nil
		}
		return "cycle", nil
	default:
		return "", refuse("reset type %q is advertised but not modelled", action)
	}
}

// SetBootOverride is the seam's view of the Boot object PATCH.
func (s *State) SetBootOverride(_ context.Context, systemID string, o bmc.BootOverride) error {
	// An unnamed enablement is a one-time override, an unnamed mode stays as
	// it is — the seam's reading of empty fields.
	enabled := o.Enabled
	if enabled == "" {
		enabled = bmc.OverrideOnce
	}
	var mode *string
	if o.Mode != "" {
		mode = &o.Mode
	}
	return s.patchBoot(systemID, &enabled, &o.Target, mode)
}

// patchBoot applies a partial update of the Boot object. Absent properties keep
// their value.
func (s *State) patchBoot(systemID string, enabled, target, mode *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	fs := s.f.system(systemID)
	if fs == nil {
		return notFound("ComputerSystem", systemID)
	}
	next := *s.boot[systemID]
	if target != nil {
		if !slices.Contains(fs.BootTargets, *target) {
			return notInList("PropertyValueNotInList", "BootSourceOverrideTarget", *target)
		}
		next.target = *target
	}
	if enabled != nil {
		switch *enabled {
		case bmc.OverrideOnce:
			next.once = true
		case bmc.OverrideContinuous:
			next.once = false
		case bmc.OverrideDisabled:
		default:
			return notInList("PropertyValueNotInList", "BootSourceOverrideEnabled", *enabled)
		}
		next.enabled = *enabled
	}
	if mode != nil {
		if fs.BootMode == "" {
			return refuse("the property BootSourceOverrideMode is not supported by this system")
		}
		if *mode != "UEFI" && *mode != "Legacy" {
			return notInList("PropertyValueNotInList", "BootSourceOverrideMode", *mode)
		}
		next.mode = *mode
	}
	if next.target == "None" {
		next.target = ""
	}
	*s.boot[systemID] = next
	return nil
}

// bootView is the Boot object as served: enabled, target and mode.
func (s *State) bootView(systemID string) (enabled, target, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootViewLocked(systemID)
}

func (s *State) bootViewLocked(systemID string) (enabled, target, mode string) {
	enabled, target = bmc.OverrideDisabled, "None"
	bo := s.boot[systemID]
	if bo == nil {
		return enabled, target, ""
	}
	if bo.enabled != "" {
		enabled = bo.enabled
	}
	if bo.target != "" {
		target = bo.target
	}
	return enabled, target, bo.mode
}

func (s *State) Close() error { return nil }

var _ bmc.Client = (*State)(nil)
