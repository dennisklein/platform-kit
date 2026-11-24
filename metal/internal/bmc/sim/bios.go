package sim

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The BIOS model: attributes are staged into the @Redfish.Settings pending
// object and become current when the system next comes out of reset. Staging
// never proves convergence — a value the registry does not allow is accepted
// at PATCH time and dropped, with a message, at apply time.

// biosState is one system's BIOS world.
type biosState struct {
	current  map[string]any
	pending  map[string]any
	messages []string // apply-time failures, served in @Redfish.Settings Messages
	// scheduled: a PATCH asked for the pending values to be applied at the
	// next reset. Only an apply_time_required fixture looks at it.
	scheduled bool
}

// supportedApplyTimes is what @Redfish.Settings advertises.
func (s *State) supportedApplyTimes() []string {
	if len(s.f.Bios.SupportedApplyTimes) > 0 {
		return s.f.Bios.SupportedApplyTimes
	}
	return []string{"OnReset"}
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

// SetBiosAttributes is the seam entrance. The in-process client stands for a
// client that asks for OnReset wherever that is needed, so it schedules.
func (s *State) SetBiosAttributes(ctx context.Context, systemID string, attrs map[string]any) error {
	return s.stageBios(ctx, systemID, attrs, "OnReset")
}

// stageBios stages attrs into the pending object. applyTime is the PATCH's
// @Redfish.SettingsApplyTime.ApplyTime, empty when it carried none.
func (s *State) stageBios(_ context.Context, systemID string, attrs map[string]any, applyTime string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if applyTime != "" && !slices.Contains(s.supportedApplyTimes(), applyTime) {
		return notInList("PropertyValueNotInList", "ApplyTime", applyTime)
	}
	b, err := s.biosOf(systemID)
	if err != nil {
		return err
	}
	// Unknown attributes are rejected at PATCH time (the documented iBMC shape);
	// VALUE validation waits for apply — staging never proves convergence.
	for k := range attrs {
		if _, ok := b.current[k]; !ok {
			return &Error{Status: http.StatusBadRequest, MessageID: "PropertyUnknown", Message: fmt.Sprintf("the property %s is not in the list of valid properties for the resource", k), Args: []string{k}}
		}
	}
	if b.pending == nil {
		b.pending = map[string]any{}
	}
	maps.Copy(b.pending, jsonTypes(attrs))
	// A later PATCH that asks for OnReset schedules everything pending, the
	// way the iDRAC job applies the whole pending set.
	b.scheduled = b.scheduled || applyTime == "OnReset"
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
		return nil, notFound("ComputerSystem", systemID)
	}
	if b == nil {
		return nil, notFound("Bios", systemID)
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
	if s.f.Bios.ApplyTimeRequired && !b.scheduled {
		return // staged, but no configuration job: nothing applies it
	}
	b.scheduled = false
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
