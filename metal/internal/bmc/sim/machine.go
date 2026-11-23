package sim

import (
	"context"
	"fmt"
)

// Machine is a real machine behind the simulated BMC: power actions drive it,
// the reported power state reads through from it. The docker backend runs a
// Talos container per system, the qemu backend a UEFI VM (the e2e tiers'
// "fake the BMC, boot a real machine" trick). Attach none and the sim's
// own power map stays the whole world (the unit-test mem backend).
type Machine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Running(ctx context.Context) (bool, error)
}

// MediaMachine is a Machine that can present mounted media to the system it
// runs — the qemu backend attaches the pulled images as CD-ROMs. SetMedia is
// called before every boot-shaped power action with the full set of pulled
// images in slot order; an empty call withdraws them all.
type MediaMachine interface {
	Machine
	SetMedia(ctx context.Context, images [][]byte) error
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

// syncMachines refreshes the BMC's power view from the attached machines —
// called before reads, outside the lock (a docker/QMP probe is I/O). The
// stale-power fault still lies on top: the fault models the ORACLE, the
// machine is the world.
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
		s.mu.Lock()
		if running {
			s.power[id] = "On"
		} else {
			s.power[id] = "Off"
		}
		s.mu.Unlock()
	}
}

// presentMedia performs the boot-time media pull and, when the machine can
// take media, hands the pulled images over before power is driven — on real
// hardware the virtual-media device is attached before the host boots. A
// failed pull leaves that slot without an image, exactly as a node that
// couldn't read its medium.
func (s *State) presentMedia(ctx context.Context, systemID string) error {
	s.mu.Lock()
	var mounted []*mountedMedia
	for _, vm := range s.f.VirtualMedia { // fixture order — deterministic
		if mm := s.media[vm.ID]; mm != nil {
			mounted = append(mounted, mm)
		}
	}
	m := s.machines[systemID]
	s.mu.Unlock()

	for _, mm := range mounted {
		s.bootFetch(ctx, mm)
	}
	mp, ok := m.(MediaMachine)
	if !ok {
		return nil
	}
	var images [][]byte
	s.mu.Lock()
	for _, mm := range mounted {
		if mm.image != nil {
			images = append(images, mm.image)
		}
	}
	s.mu.Unlock()
	return mp.SetMedia(ctx, images)
}

// driveMachine maps a power action onto the attached machine, if any. Runs
// outside the lock; a failing machine op fails the action — a BMC whose power
// command fails reports it.
func (s *State) driveMachine(ctx context.Context, systemID string, action string) error {
	s.mu.Lock()
	m := s.machines[systemID]
	s.mu.Unlock()
	if m == nil {
		return nil
	}
	var err error
	switch action {
	case "start":
		err = m.Start(ctx)
	case "stop":
		err = m.Stop(ctx)
	case "restart":
		err = m.Restart(ctx)
	}
	if err != nil {
		return fmt.Errorf("sim: machine behind %q: %w", systemID, err)
	}
	return nil
}
