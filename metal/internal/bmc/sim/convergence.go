package sim

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The convergence model: firmware flash and BIOS pending→applied, encoding the
// recorded X12 behaviour (tools/bmc-firmware-update.sh, the 2026-07-19 fleet
// flash) rather than idealised DMTF semantics. The load-bearing quirks:
//
//   - One multipart upload with apply-time Immediate AND the Oem.Supermicro.BMC
//     preserve block flashes; the BMC reboots itself (Manager LastResetTime
//     moves) and the new version reads back.
//   - The same upload WITHOUT the preserve block — or with apply-time
//     OnStartUpdateRequest — stages instead: the image lands in the staging
//     slot, nothing flashes, and the BMC wedges in update mode: further uploads
//     are refused ("already in update mode") and GracefulRestart is refused
//     (the ROT attestation path). ForceRestart discards the staged image.
//   - StartUpdate rejects every body form; the OEM Install answers success
//     without observable effect. Completion is proven ONLY by observables.
//
// Image "verification" is a sim convention: the bytes must carry a
// "SIMFW <version>" marker, standing in for the real signature/board check.

// fwState is the manager-side firmware world.
type fwState struct {
	version   string    // live BMC firmware — what Manager() reports
	lastReset time.Time // the manager's own reset time; zero = vendor doesn't serve it
	staged    string    // staged image version; non-empty = wedged in update mode
}

// biosState is one system's BIOS world.
type biosState struct {
	current  map[string]any
	pending  map[string]any
	messages []string // apply-time failures, served in @Redfish.Settings Messages
}

var simfwMarker = regexp.MustCompile(`SIMFW ([0-9A-Za-z._-]+)`)

// imageVersion reads the version marker out of a firmware image — the sim's
// stand-in for the BMC's image verification.
func imageVersion(image []byte) (string, error) {
	m := simfwMarker.FindSubmatch(image)
	if m == nil {
		return "", fmt.Errorf("sim: image verification failed (no SIMFW version marker)")
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
			// The capture shape: the staging slot mirrors the current version
			// when nothing is staged.
			v = s.fw.version
			if s.fw.staged != "" {
				v = s.fw.staged
			}
		}
		out = append(out, bmc.Firmware{ID: e.ID, Version: v, Updateable: e.Updateable})
	}
	return out, nil
}

func (s *State) UpdateFirmware(_ context.Context, image []byte) error {
	// The in-memory client speaks the correct recipe by construction — apply-time
	// Immediate with the preserve block. The HTTP tier parses the real multipart
	// form and can get both wrong; everything funnels into uploadFirmware.
	return s.uploadFirmware(image, true, true)
}

// uploadFirmware is the one flash entrance: immediate = apply-time Immediate,
// preserve = the Oem.Supermicro.BMC block was present.
func (s *State) uploadFirmware(image []byte, immediate, preserve bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if s.f.updateDialect() != bmc.DialectMultipartPush {
		return fmt.Errorf("sim: endpoint advertises no multipart push URI")
	}
	if s.fw.staged != "" {
		// The recorded wedge: a BMC holding a staged image refuses any new
		// update until a reset discards the staging slot.
		return fmt.Errorf("sim: already in update mode")
	}
	v, err := imageVersion(image)
	if err != nil {
		return err
	}
	if !immediate || !preserve {
		// Well-formed upload, wrong invocation: stages and never installs.
		s.fw.staged = v
		return nil
	}
	if s.faults[FaultFlashFails] {
		// The lying oracle: success answered, nothing ever flashes — no version
		// advance, no reboot. Callers must judge by observables.
		return nil
	}
	s.fw.version = v
	s.resetManagerLocked()
	return nil
}

func (s *State) ResetManager(_ context.Context, action bmc.PowerAction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	switch action {
	case bmc.ActionGracefulRestart:
		if s.fw.staged != "" {
			// GracefulRestart runs the ROT attestation path, which a BMC stuck
			// mid-update refuses.
			return fmt.Errorf("sim: failed to generate attestation evidence, FW update in progress")
		}
	case bmc.ActionForceRestart:
		s.fw.staged = "" // the wedge cure: force discards the staging slot
	default:
		return fmt.Errorf("sim: unsupported manager reset type %q", action)
	}
	s.resetManagerLocked()
	return nil
}

// resetManagerLocked moves the manager reset-time observable — only where the
// fixture serves it (the iBMC doesn't; the sim never invents withheld facts) —
// and applies a staged HTTPS identity (a manager reboot is the certificate
// apply mechanism on both vendors).
func (s *State) resetManagerLocked() {
	if !s.fw.lastReset.IsZero() {
		s.fw.lastReset = time.Now()
	}
	s.applyIdentityLocked()
}

func (s *State) Bios(_ context.Context, systemID string) (bmc.BiosState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.BiosState{}, err
	}
	b, err := s.biosOf(systemID)
	if err != nil {
		return bmc.BiosState{}, err
	}
	out := bmc.BiosState{Attributes: maps.Clone(b.current), Registry: s.f.Bios.Registry}
	if len(b.pending) > 0 {
		out.Pending = maps.Clone(b.pending)
	}
	return out, nil
}

func (s *State) SetBiosAttributes(_ context.Context, systemID string, attrs map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	b, err := s.biosOf(systemID)
	if err != nil {
		return err
	}
	// Unknown attributes are rejected at PATCH time (the documented iBMC shape);
	// VALUE validation waits for apply — staging never proves convergence.
	for k := range attrs {
		if _, ok := b.current[k]; !ok {
			return fmt.Errorf("sim: unknown BIOS attribute %q", k)
		}
	}
	if b.pending == nil {
		b.pending = map[string]any{}
	}
	maps.Copy(b.pending, jsonTypes(attrs))
	return nil
}

func (s *State) BiosSchema(_ context.Context, systemID string) ([]bmc.BiosAttribute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	if _, err := s.biosOf(systemID); err != nil {
		return nil, err
	}
	var out []bmc.BiosAttribute
	for _, a := range s.f.Bios.Schema {
		out = append(out, bmc.BiosAttribute{Name: a.Name, Type: a.Type, AllowedValues: a.AllowedValues})
	}
	return out, nil
}

func (s *State) biosOf(systemID string) (*biosState, error) {
	b, ok := s.bios[systemID]
	if !ok {
		return nil, fmt.Errorf("sim: no system %q", systemID)
	}
	if b == nil {
		return nil, fmt.Errorf("sim: system %q serves no BIOS resource", systemID)
	}
	return b, nil
}

// biosView is the HTTP tier's locked read: current, pending, and the apply-time
// messages (which the seam-level Bios read doesn't carry — they are served in
// the @Redfish.Settings block, where the iBMC puts them).
func (s *State) biosView(systemID string) (current, pending map[string]any, messages []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, nil, nil, err
	}
	b, err := s.biosOf(systemID)
	if err != nil {
		return nil, nil, nil, err
	}
	return maps.Clone(b.current), maps.Clone(b.pending), slices.Clone(b.messages), nil
}

// applyBios is the reboot hook: a system coming out of reset applies its pending
// settings — unless the stuck fault holds them, the real state a BIOS needing a
// second reboot exhibits. Values outside the fixture schema are dropped with a
// message instead of applied (the @Redfish.Settings Messages shape); attribute
// existence was already enforced at PATCH time.
func (s *State) applyBios(systemID string) {
	b := s.bios[systemID]
	if b == nil || len(b.pending) == 0 || s.faults[FaultBiosStuck] {
		return
	}
	b.messages = nil
	for k, v := range b.pending {
		if !s.valueAllowed(k, v) {
			b.messages = append(b.messages, fmt.Sprintf("The value %v for the property %s is not in the list of acceptable values.", v, k))
			continue
		}
		b.current[k] = v
	}
	b.pending = nil
}

// valueAllowed checks an enumeration value against the fixture schema; types
// without allowed values (and attributes the schema omits) pass — apply-time
// validation can only be as strict as the vendor's registry.
func (s *State) valueAllowed(name string, v any) bool {
	for _, a := range s.f.Bios.Schema {
		if a.Name != name || len(a.AllowedValues) == 0 {
			continue
		}
		sv, ok := v.(string)
		return ok && slices.Contains(a.AllowedValues, sv)
	}
	return true
}

// jsonTypes normalises a value map to what JSON transport produces (numbers as
// float64) so the in-memory client and the HTTP tier serve identical shapes.
func jsonTypes(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = jsonType(v)
	}
	return out
}

func jsonType(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return v
	}
}
