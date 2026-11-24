package sim

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The firmware model. An image is pushed as one multipart upload; what happens
// next depends on the apply time it was pushed with.
//
// The specified behaviour: apply time Immediate flashes — an asynchronous
// operation, tracked by a task, at the end of which the new version reads
// back and (for manager firmware) the manager has rebooted. Apply time
// OnStartUpdateRequest stages the image until UpdateService.StartUpdate
// installs it.
//
// The recorded X12 behaviour (fixture update.wedge) departs from it in ways an
// updater must survive:
//
//   - Immediate flashes only with the Oem.Supermicro.BMC preserve block in the
//     parameters. Without it — or with any other apply time — the image stages
//     and the BMC wedges in update mode: further uploads are refused ("already
//     in update mode") and so is a graceful manager reset (the ROT attestation
//     path). ForceRestart discards the staged image.
//   - StartUpdate rejects every body form; the OEM Install answers success
//     without observable effect. Completion is proven ONLY by observables.
//
// Image "verification" is a sim convention: the bytes must carry a
// "SIMFW <version>" marker, standing in for the real signature/board check.

// fwState is the manager-side firmware world.
type fwState struct {
	version   string            // live BMC firmware — what Manager() reports
	lastReset time.Time         // the manager's own reset time; zero = the endpoint doesn't serve it
	staged    string            // staged image version; empty = nothing staged
	stagedFor string            // the inventory entry the staged image is for
	versions  map[string]string // versions flashed onto entries other than the manager's
	onReset   map[string]string // system images staged for the next system boot, by entry
}

// fwUpload is one multipart push as the HTTP tier parsed it.
type fwUpload struct {
	image []byte
	// applyTime is @Redfish.OperationApplyTime; empty takes the service
	// default, Immediate.
	applyTime string
	// preserve says the Oem.Supermicro.BMC block was present.
	preserve bool
	// targets are the Targets URIs; empty lets the service pick the manager.
	targets []string
}

var simfwMarker = regexp.MustCompile(`SIMFW ([0-9A-Za-z._-]+)`)

// imageVersion reads the version marker out of a firmware image — the sim's
// stand-in for the BMC's image verification.
func imageVersion(image []byte) (string, error) {
	m := simfwMarker.FindSubmatch(image)
	if m == nil {
		return "", refuse("image verification failed (no SIMFW version marker)")
	}
	return string(m[1]), nil
}

func (s *State) FirmwareInventory(context.Context) ([]bmc.Firmware, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	var out []bmc.Firmware
	for _, e := range s.f.Update.Inventory {
		v := e.Version
		switch {
		case e.BMC:
			v = s.fw.version
		case e.Staging:
			// The X12 shape: the staging slot mirrors the current version
			// when nothing is staged.
			v = s.fw.version
			if s.fw.staged != "" {
				v = s.fw.staged
			}
		case s.fw.versions[e.ID] != "":
			v = s.fw.versions[e.ID]
		}
		out = append(out, bmc.Firmware{ID: e.ID, Version: v, Updateable: e.Updateable})
	}
	return out, nil
}

// UpdateFirmware is the seam entrance. The in-memory client speaks the correct
// recipe by construction and waits for the flash; the HTTP tier parses the real
// multipart form, which can get the recipe wrong, and tracks the flash as a
// task.
func (s *State) UpdateFirmware(_ context.Context, image []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.planUpload(fwUpload{image: image, applyTime: "Immediate", preserve: true})
	if err != nil {
		return err
	}
	return work()
}

// pushFirmware is the HTTP tier's entrance: the upload is validated in the
// request, the flash runs as a task.
func (s *State) pushFirmware(u fwUpload) (task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.planUpload(u)
	if err != nil {
		return task{}, err
	}
	return *s.startTask("Firmware update", work), nil
}

// planUpload validates an upload and returns the work it stands for. An upload
// that stages, and one the flash-fails fault swallows, is accepted with
// nothing left to do. Caller holds the lock.
func (s *State) planUpload(u fwUpload) (func() error, error) {
	if err := s.reachable(); err != nil {
		return nil, err
	}
	if s.f.updateDialect() != bmc.DialectMultipartPush {
		return nil, refuse("endpoint advertises no multipart push URI")
	}
	if s.fw.staged != "" {
		// A manager holding a staged image takes no second one. On the X12
		// this is the wedge: only a forced reset gets out of it.
		return nil, refuse("already in update mode")
	}
	v, err := imageVersion(u.image)
	if err != nil {
		return nil, err
	}
	entry, err := s.fwTarget(u.targets)
	if err != nil {
		return nil, err
	}
	applyTime := u.applyTime
	if applyTime == "" {
		applyTime = "Immediate"
	}
	nothing := func() error { return nil }
	if applyTime == "OnReset" && s.f.Update.OnReset {
		if s.isManagerEntry(entry) || len(u.targets) == 0 {
			// OnReset is a system reset; the manager's own firmware is not
			// applied by one.
			return nil, refuse("apply time OnReset needs a system firmware target")
		}
		if !s.faults[FaultFlashFails] {
			s.fw.onReset[entry] = v
		}
		return nothing, nil
	}
	if applyTime != "Immediate" && applyTime != "OnStartUpdateRequest" {
		return nil, notInList("PropertyValueNotInList", "@Redfish.OperationApplyTime", applyTime)
	}
	if applyTime != "Immediate" || (s.f.Update.Wedge && !u.preserve) {
		s.fw.staged, s.fw.stagedFor = v, entry
		return nothing, nil
	}
	if s.faults[FaultFlashFails] {
		// The lying oracle: success answered, nothing ever flashes — no
		// version advance, no reboot. Callers must judge by observables.
		return nothing, nil
	}
	return func() error { s.flash(entry, v); return nil }, nil
}

// StageFirmware is the seam entrance for a staged system update: the image
// waits for the next boot of the system (applyStagedFirmware).
func (s *State) StageFirmware(_ context.Context, image []byte, entryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.f.Update.OnReset {
		return refuse("the push URI does not support apply time OnReset")
	}
	work, err := s.planUpload(fwUpload{image: image, applyTime: "OnReset", targets: []string{firmwareInventoryURI + "/" + entryID}})
	if err != nil {
		return err
	}
	return work()
}

// isManagerEntry: the entry tracks the manager's own firmware (live or
// staging slot). Caller holds the lock.
func (s *State) isManagerEntry(entry string) bool {
	for _, e := range s.f.Update.Inventory {
		if e.ID == entry {
			return e.BMC || e.Staging
		}
	}
	return false
}

// applyStagedFirmware installs what waits for a system boot. The fixtures
// front one system each, so every staged entry belongs to the system that
// boots. Caller holds the lock.
func (s *State) applyStagedFirmware() {
	for entry, v := range s.fw.onReset {
		s.fw.versions[entry] = v
		delete(s.fw.onReset, entry)
	}
}

// startUpdate is UpdateService.StartUpdate where it works: the staged image
// installs, tracked by a task.
func (s *State) startUpdate() (task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return task{}, err
	}
	if s.fw.staged == "" {
		return task{}, refuse("no staged image to install")
	}
	entry, v := s.fw.stagedFor, s.fw.staged
	return *s.startTask("Firmware update", func() error {
		s.fw.staged, s.fw.stagedFor = "", ""
		s.flash(entry, v)
		return nil
	}), nil
}

// fwTarget resolves the Targets of an upload to one inventory entry. The
// manager's own URI names its firmware (the X12 recipe); an empty list leaves
// the choice to the service, which picks the manager.
func (s *State) fwTarget(targets []string) (string, error) {
	managerEntry := ""
	for _, e := range s.f.Update.Inventory {
		if e.BMC {
			managerEntry = e.ID
		}
	}
	entry := managerEntry
	for i, t := range targets {
		resolved := ""
		if t == managerURI {
			resolved = managerEntry
		} else if id, ok := strings.CutPrefix(t, firmwareInventoryURI+"/"); ok {
			for _, e := range s.f.Update.Inventory {
				if e.ID == id && e.Updateable {
					resolved = id
				}
			}
		}
		if resolved == "" {
			return "", notInList("PropertyValueNotInList", "Targets", t)
		}
		if i > 0 && resolved != entry {
			return "", refuse("one image updates one target")
		}
		entry = resolved
	}
	if entry == "" {
		return "", refuse("no updateable manager firmware in the inventory")
	}
	return entry, nil
}

// flash makes an image's version the entry's. Manager firmware reboots the
// manager; anything else just reads back. Caller holds the lock.
func (s *State) flash(entry, version string) {
	for _, e := range s.f.Update.Inventory {
		if e.ID == entry && e.BMC {
			s.fw.version = version
			s.resetManagerLocked()
			return
		}
	}
	s.fw.versions[entry] = version
}

func (s *State) ResetManager(_ context.Context, action bmc.PowerAction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	switch action {
	case bmc.ActionGracefulRestart:
		if s.fw.staged != "" && s.f.Update.Wedge {
			// GracefulRestart runs the ROT attestation path, which a BMC stuck
			// mid-update refuses.
			return refuse("failed to generate attestation evidence, FW update in progress")
		}
	case bmc.ActionForceRestart:
		s.fw.staged, s.fw.stagedFor = "", "" // a forced reset discards the staging slot
	default:
		return notInList("ActionParameterValueNotInList", "ResetType", string(action))
	}
	s.resetManagerLocked()
	return nil
}

// resetManagerLocked moves the manager reset-time observable — only where the
// fixture serves it (the iBMC doesn't; the sim never invents withheld facts) —
// and applies a staged HTTPS identity (a manager reboot is the certificate
// apply mechanism of both OEM identity surfaces).
func (s *State) resetManagerLocked() {
	if !s.fw.lastReset.IsZero() {
		s.fw.lastReset = time.Now()
	}
	s.applyIdentityLocked()
}
