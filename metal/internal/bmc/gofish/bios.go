package gofish

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/stmcginnis/gofish/schemas"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func (c *client) Bios(ctx context.Context, systemID string) (bmc.BiosState, error) {
	b, err := c.biosOf(systemID)
	if err != nil {
		return bmc.BiosState{}, err
	}
	out := bmc.BiosState{Attributes: map[string]any(b.Attributes), Registry: b.AttributeRegistry}
	// The pending set lives in the @Redfish.Settings object, which gofish keeps
	// private — one raw GET, absent Attributes means nothing staged.
	if uri := c.biosSettings(b.ODataID).SettingsObject.ODataID; uri != "" && uri != b.ODataID {
		var doc struct {
			Attributes map[string]any `json:"Attributes"`
		}
		if _, err := c.getJSON(uri, &doc); err != nil {
			return bmc.BiosState{}, fmt.Errorf("bmc: bios settings: %w", err)
		}
		out.Pending = pendingChanges(doc.Attributes, out.Attributes)
	}
	return out, nil
}

// pendingChanges keeps the settings-object values that differ from the
// current ones. iDRAC, the iBMC and the X12 serve only the staged
// attributes there; iLO serves the whole attribute set with the staged
// values merged in, which read as everything pending. A staged value equal
// to the current one changes nothing at the next reset, so dropping it means
// the same on every vendor.
func pendingChanges(settings, current map[string]any) map[string]any {
	var out map[string]any
	for k, v := range settings {
		if cur, ok := current[k]; ok && reflect.DeepEqual(cur, v) {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	return out
}

func (c *client) SetBiosAttributes(ctx context.Context, systemID string, attrs map[string]any) error {
	if c.readOnly {
		return errReadOnly
	}
	b, err := c.biosOf(systemID)
	if err != nil {
		return err
	}
	settings := c.biosSettings(b.ODataID)
	uri := settings.SettingsObject.ODataID
	if uri == "" {
		uri = b.ODataID
	}
	// A direct PATCH, not gofish's UpdateBiosAttributes: that helper drops
	// attributes already at the requested value, and staging must be literal.
	body := map[string]any{"Attributes": attrs}
	// Ask for the values to be applied at the next reset wherever the service
	// says it takes that request. iDRAC 9 stages a PATCH without it but
	// creates no BIOS configuration job, so the reset that follows applies
	// nothing; with it, the job is created. Elsewhere OnReset is what a
	// PATCH without it means anyway.
	if slices.Contains(settings.SupportedApplyTimes, applyOnReset) {
		body["@Redfish.SettingsApplyTime"] = map[string]any{"ApplyTime": applyOnReset}
	}
	if err := c.patch(uri, body); err != nil {
		return fmt.Errorf("bmc: bios attributes: %w", err)
	}
	return nil
}

func (c *client) BiosSchema(ctx context.Context, systemID string) ([]bmc.BiosAttribute, error) {
	b, err := c.biosOf(systemID)
	if err != nil {
		return nil, err
	}
	if b.AttributeRegistry == "" {
		return nil, nil
	}
	files, err := c.svc.Registries()
	if err != nil {
		return nil, nil // no Registries collection — absence, not failure
	}
	for _, f := range files {
		if !registryMatches(b.AttributeRegistry, f.ID, f.Registry) {
			continue
		}
		for _, loc := range f.Location {
			if loc.URI == "" {
				continue
			}
			reg, err := schemas.GetAttributeRegistry(c.api, loc.URI)
			if err != nil {
				return nil, fmt.Errorf("bmc: attribute registry: %w", err)
			}
			var out []bmc.BiosAttribute
			for _, a := range reg.RegistryEntries.Attributes {
				attr := bmc.BiosAttribute{Name: a.AttributeName, Type: string(a.Type)}
				for _, v := range a.Value {
					attr.AllowedValues = append(attr.AllowedValues, v.ValueName)
				}
				out = append(out, attr)
			}
			return out, nil
		}
	}
	return nil, nil // registry named but not served — the observed-absent path
}

// registryMatches decides whether a Registries member is the registry the Bios
// resource advertises. Exact matching is a real-hardware bug: the X12
// advertises BiosAttributeRegistry.v1_0_0 against a bare member id
// BiosAttributeRegistry (Registry field 1.0.0-dotted), and the iBMC v7_6_2
// against 7.6.2 with the Registry field truncated to 7.6. So: normalise the
// version notation, then accept equality or a dot-boundary prefix in either
// direction.
func registryMatches(advertised, memberID, registry string) bool {
	a := normalizeRegistry(advertised)
	if a == "" {
		return false
	}
	for _, cand := range []string{memberID, registry} {
		c := normalizeRegistry(cand)
		if c == "" {
			continue
		}
		if a == c || strings.HasPrefix(a, c+".") || strings.HasPrefix(c, a+".") {
			return true
		}
	}
	return false
}

// normalizeRegistry folds the version notations the vendors mix freely —
// v7_6_2, 7.6.2, 1_0_0 — into lowercase dot form.
func normalizeRegistry(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", ".")
	s = strings.ReplaceAll(s, ".v", ".")
	return s
}

func (c *client) biosOf(systemID string) (*schemas.Bios, error) {
	s, err := c.systemByID(systemID)
	if err != nil {
		return nil, err
	}
	b, err := s.Bios()
	if err != nil {
		return nil, fmt.Errorf("bmc: bios: %w", err)
	}
	if b == nil {
		return nil, fmt.Errorf("bmc: system %q exposes no BIOS resource", systemID)
	}
	return b, nil
}

// applyOnReset is the @Redfish.SettingsApplyTime value for "at the next
// system reset".
const applyOnReset = "OnReset"

// settingsBlock is the @Redfish.Settings block of a Bios resource: where the
// pending object lives, and which apply times a PATCH to it may ask for.
type settingsBlock struct {
	SettingsObject      ref      `json:"SettingsObject"`
	SupportedApplyTimes []string `json:"SupportedApplyTimes"`
}

// biosSettings reads @Redfish.Settings off the raw Bios resource — gofish
// folds it into a private field. A failed read is an empty block: the PATCH
// then goes to the Bios resource itself, without an apply time.
func (c *client) biosSettings(biosURI string) settingsBlock {
	var doc struct {
		Settings settingsBlock `json:"@Redfish.Settings"`
	}
	if _, err := c.getJSON(biosURI, &doc); err != nil {
		return settingsBlock{}
	}
	return doc.Settings
}
