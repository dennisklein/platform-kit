package sim

import (
	"context"
	"fmt"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Machine is a real machine behind the simulated BMC: power actions drive it,
// the reported power state reads through from it. The docker backend runs a
// Talos container per system, the qemu backend a UEFI VM ("fake the BMC, boot
// a real machine"). Attach none and the sim's own power map stays the whole
// world (the unit-test mem backend).
type Machine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Running(ctx context.Context) (bool, error)
}

// MediaMachine is a Machine that can present mounted media to the system it
// runs — the qemu backend attaches them as CD-ROMs. SetMedia is called before
// every boot-shaped power action with the full set of readable media in slot
// order; an empty call withdraws them all.
type MediaMachine interface {
	Machine
	SetMedia(ctx context.Context, media []Media) error
}

// Media is one mounted medium as the machine gets it. Exactly one of Data and
// Path is set: Data for an image the BMC holds in memory, Path for one that
// sits in the media cache and must be attached read-only, in place.
type Media struct {
	SlotID string
	Data   []byte
	Path   string
}

// BootMachine is a Machine that honours the BMC's boot source override.
// SetBootSource is called before every boot-shaped power action with the
// override that boot is to take: a Redfish BootSource ("Cd", "Hdd", "Pxe", …)
// or "" for the machine's own order. once says the override covers that one
// boot only — every later boot, including one the guest starts itself, is
// back on the machine's own order.
type BootMachine interface {
	Machine
	SetBootSource(ctx context.Context, target string, once bool) error
}

// ProgressMachine is a Machine that can tell how far its boot got, as a
// Redfish BootProgress LastState value ("OSBootStarted", "OSRunning", …).
type ProgressMachine interface {
	Machine
	BootProgress(ctx context.Context) (string, error)
}

// AttachMachine puts a machine behind a system. Wire before serving.
func (s *State) AttachMachine(systemID string, m Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.machines == nil {
		s.machines = map[string]Machine{}
	}
	s.machines[systemID] = m
}

// syncMachines refreshes the BMC's power and boot-progress view from the
// attached machines — called before reads, outside the lock (a docker/QMP
// probe is I/O). The stale-power fault still lies on top: the fault models
// the ORACLE, the machine is the world.
func (s *State) syncMachines(ctx context.Context) {
	s.mu.Lock()
	machines := make(map[string]Machine, len(s.machines))
	for id, m := range s.machines {
		machines[id] = m
	}
	s.mu.Unlock()

	for id, m := range machines {
		running, err := m.Running(ctx)
		if err != nil {
			continue // an unprobeable machine keeps the last view — like a BMC would
		}
		progress := ""
		if pm, ok := m.(ProgressMachine); ok && running {
			progress, _ = pm.BootProgress(ctx)
		}
		s.mu.Lock()
		if running {
			s.power[id] = bmc.PowerOn
		} else {
			s.power[id] = bmc.PowerOff
		}
		s.progress[id] = progress
		s.mu.Unlock()
	}
}

// presentMedia performs the boot-time media pull and, when the machine can
// take media, hands the pulled images over before power is driven — on real
// hardware the virtual-media device is attached before the host boots. A
// failed pull leaves that slot without an image, exactly as a node that
// couldn't read its medium.
func (s *State) presentMedia(ctx context.Context, systemID string) error {
	type slot struct {
		id string
		mm *mountedMedia
	}
	s.mu.Lock()
	var mounted []slot
	for _, vm := range s.f.VirtualMedia { // fixture order — deterministic
		if mm := s.media[vm.ID]; mm != nil {
			mounted = append(mounted, slot{vm.ID, mm})
		}
	}
	m := s.machines[systemID]
	s.mu.Unlock()

	for _, sl := range mounted {
		s.bootFetch(ctx, sl.mm)
	}
	mp, ok := m.(MediaMachine)
	if !ok {
		return nil
	}
	var media []Media
	s.mu.Lock()
	for _, sl := range mounted {
		if sl.mm.image != nil || sl.mm.path != "" {
			media = append(media, Media{SlotID: sl.id, Data: sl.mm.image, Path: sl.mm.path})
		}
	}
	s.mu.Unlock()
	if err := mp.SetMedia(ctx, media); err != nil {
		return fmt.Errorf("sim: machine behind %q: %w", systemID, err)
	}
	return nil
}

// presentBootSource hands the machine the override the coming boot takes. A
// used-up or disabled override is no override.
func (s *State) presentBootSource(ctx context.Context, systemID string) error {
	s.mu.Lock()
	m := s.machines[systemID]
	target, once := "", false
	if bo := s.boot[systemID]; bo.armed() {
		target, once = bo.target, bo.enabled == bmc.OverrideOnce
	}
	s.mu.Unlock()
	bm, ok := m.(BootMachine)
	if !ok {
		return nil
	}
	if err := bm.SetBootSource(ctx, target, once); err != nil {
		return fmt.Errorf("sim: machine behind %q: %w", systemID, err)
	}
	return nil
}

// driveMachine maps a power verb onto the attached machine, if any. Runs
// outside the lock; a failing machine op fails the action — a BMC whose power
// command fails reports it.
func (s *State) driveMachine(ctx context.Context, systemID string, verb string) error {
	s.mu.Lock()
	m := s.machines[systemID]
	s.mu.Unlock()
	if m == nil {
		return nil
	}
	var err error
	switch verb {
	case "start":
		err = m.Start(ctx)
	case "stop":
		err = m.Stop(ctx)
	case "restart":
		err = m.Restart(ctx)
	case "cycle":
		// A power cycle is power removed and restored — a cold boot, which is
		// what picks up a changed device set.
		if err = m.Stop(ctx); err == nil {
			err = m.Start(ctx)
		}
	}
	if err != nil {
		return fmt.Errorf("sim: machine behind %q: %w", systemID, err)
	}
	return nil
}
