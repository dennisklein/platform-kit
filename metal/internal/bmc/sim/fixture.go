package sim

import (
	"cmp"
	"embed"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Fixture describes one simulated management endpoint. The shape deliberately mirrors
// control/inventory/*.yaml (the discovered fact files) — seeded from those today, refined
// by real Redfish captures as the hardware PoC lands them (fixture-refresh PRs, re-gated by
// bmctest).
type Fixture struct {
	Name    string `yaml:"name"`
	Vendor  string `yaml:"vendor"` // "Huawei" | "Supermicro" — tree shape + dialect defaults
	Manager struct {
		Firmware string `yaml:"firmware"`
		Hostname string `yaml:"hostname"`
		MAC      string `yaml:"mac"`
		// LastResetTime seeds the manager's own reset-time observable — the
		// flash-completion proof. Set only where the vendor serves it (X12 yes,
		// iBMC no; the captures decide).
		LastResetTime time.Time `yaml:"last_reset_time"`
	} `yaml:"manager"`
	// Accounts seeds the AccountService (both capture trees list members from
	// id 2). Empty = OPEN sim: no account service served, no auth enforced —
	// the pre-adoption bench posture every credential-less consumer relies
	// on; any seeded account turns both on.
	Accounts []FixtureAccount `yaml:"accounts"`
	// NTP mirrors the captured time-source surface: the X12 serves the
	// standard NetworkProtocol NTP block (standard), the iBMC an OEM
	// HwNtpService with three server slots (oem_service). Neither = no NTP
	// surface at all.
	NTP struct {
		Standard   bool     `yaml:"standard"`
		OemService bool     `yaml:"oem_service"`
		Enabled    bool     `yaml:"enabled"`
		Servers    []string `yaml:"servers"`
	} `yaml:"ntp"`
	// Identity mirrors the captured certificate-replacement surface: the
	// iBMC generates CSRs on the manager (SecurityService HttpsCert), the
	// X12 takes a leaf+key upload (OEM SmcSSLCert). Neither = no identity
	// surface.
	Identity struct {
		CSRNative bool `yaml:"csr_native"`
		KeyUpload bool `yaml:"key_upload"`
	} `yaml:"identity"`
	// Update mirrors the UpdateService capture: which transports the vendor
	// advertises and the SoftwareInventory members it serves.
	Update struct {
		MultipartPush bool `yaml:"multipart_push"`
		SimpleUpdate  bool `yaml:"simple_update"`
		Inventory     []struct {
			ID         string `yaml:"id"`
			Version    string `yaml:"version"`
			Updateable bool   `yaml:"updateable"`
			// BMC marks the entry that tracks the live manager firmware
			// (X12 "BMC", iBMC "ActiveBMC"); Staging marks the X12 staging
			// slot, which mirrors the current version when nothing is staged.
			BMC     bool `yaml:"bmc"`
			Staging bool `yaml:"staging"`
		} `yaml:"inventory"`
	} `yaml:"update"`
	// Bios seeds every system's BIOS view (our fixtures front one system each).
	Bios struct {
		// SettingsID is the pending-object member id under Systems/{id}/Bios —
		// vendor-shaped: X12 "SD", iBMC "Settings" (both from the captures).
		SettingsID string `yaml:"settings_id"`
		// Registry is what the Bios resource's AttributeRegistry advertises.
		// The 2026-07-21 registry captures pin that BOTH vendors spell the
		// name differently in every place it appears (X12: v1_0_0 on the Bios
		// doc, bare member id, 1.0.0 in the member's Registry field) — the
		// three fields below carry each spelling so clients are forced to
		// normalise instead of exact-match.
		Registry string `yaml:"registry"`
		// RegistryID is the Registries collection member id (default: Registry).
		RegistryID string `yaml:"registry_id"`
		// RegistryName is the member document's Registry field (default: Registry).
		RegistryName string `yaml:"registry_name"`
		// RegistryURI is the member's Location Uri — the X12 serves the file
		// OUTSIDE the Redfish tree (/registries/…), the iBMC under its own
		// RegistryStore. Default: /redfish/v1/Registries/<id>/<id>.json.
		RegistryURI string         `yaml:"registry_uri"`
		Attributes  map[string]any `yaml:"attributes"`
		Schema      []struct {
			Name          string   `yaml:"name"`
			Type          string   `yaml:"type"`
			AllowedValues []string `yaml:"allowed_values"`
		} `yaml:"schema"`
	} `yaml:"bios"`
	Chassis struct {
		BoardSerial string `yaml:"board_serial"`
		Model       string `yaml:"model"`
		// OemBoardSerial mirrors the X12 reality: the board serial lives in
		// Oem.Supermicro.BoardSerialNumber, not in the plain SerialNumber field.
		OemBoardSerial bool `yaml:"oem_board_serial"`
		Adapters       []struct {
			Model    string   `yaml:"model"`
			PortMACs []string `yaml:"port_macs"`
		} `yaml:"adapters"`
	} `yaml:"chassis"`
	VirtualMedia []struct {
		ID             string   `yaml:"id"`
		MediaTypes     []string `yaml:"media_types"`
		InsertStandard bool     `yaml:"insert_standard"`
		OemActions     []string `yaml:"oem_actions"`
	} `yaml:"virtualmedia"`
	Systems []struct {
		ID           string `yaml:"id"`
		UUID         string `yaml:"uuid"`
		Serial       string `yaml:"serial"` // empty = the Supermicro Systems/1 blind spot
		Manufacturer string `yaml:"manufacturer"`
		Model        string `yaml:"model"`
		Power        string `yaml:"power"`
		// LastResetTime seeds the reset-time observable — set only where the real
		// vendor serves it (X12 yes, iBMC no; the PoC captures decide).
		LastResetTime time.Time `yaml:"last_reset_time"`
		MemoryGiB     int       `yaml:"memory_gib"`
		CPUModel      string    `yaml:"cpu_model"`
		CPUCount      int       `yaml:"cpu_count"`
		BootTargets   []string  `yaml:"boot_targets"`
		NICs          []struct {
			ID  string `yaml:"id"`
			MAC string `yaml:"mac"`
		} `yaml:"nics"`
		Drives []struct {
			Model       string `yaml:"model"`
			Serial      string `yaml:"serial"`
			CapacityGiB int    `yaml:"capacity_gib"`
			Media       string `yaml:"media"`
			Protocol    string `yaml:"protocol"`
		} `yaml:"drives"`
	} `yaml:"systems"`
}

// FixtureAccount seeds one ManagerAccount. Named (unlike the other fixture
// sub-shapes) so tests can append accounts to a loaded fixture — the
// enforcement-enabled variant is an in-test mutation, not a second YAML.
type FixtureAccount struct {
	ID       string `yaml:"id"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Role     string `yaml:"role"`
	Enabled  bool   `yaml:"enabled"`
}

//go:embed fixtures/*.yaml
var embedded embed.FS

// LoadFixture resolves name as an embedded fixture (e.g. "huawei-2288h-v5") or a path.
func LoadFixture(name string) (*Fixture, error) {
	data, err := embedded.ReadFile("fixtures/" + name + ".yaml")
	if err != nil {
		if data, err = os.ReadFile(name); err != nil {
			names, _ := embedded.ReadDir("fixtures")
			var known []string
			for _, e := range names {
				known = append(known, strings.TrimSuffix(e.Name(), ".yaml"))
			}
			return nil, fmt.Errorf("fixture %q: not embedded (%s) and not a readable path", name, strings.Join(known, ", "))
		}
	}
	var f Fixture
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("fixture %q: %w", name, err)
	}
	return &f, nil
}

// registryID/registryName/registryURI resolve the registry-identity fields with
// their defaults — a hand-made fixture that sets only `registry` still serves a
// coherent tree.
func (f *Fixture) registryID() string {
	return cmp.Or(f.Bios.RegistryID, f.Bios.Registry)
}

func (f *Fixture) registryName() string {
	return cmp.Or(f.Bios.RegistryName, f.Bios.Registry)
}

func (f *Fixture) registryURI() string {
	return cmp.Or(f.Bios.RegistryURI, "/redfish/v1/Registries/"+f.registryID()+"/"+f.registryID()+".json")
}

// updateDialect derives the firmware-update transport the fixture advertises —
// the push URI wins where both are served (the X12 advertises SimpleUpdate too,
// but push is the proven direction).
func (f *Fixture) updateDialect() bmc.UpdateDialect {
	switch {
	case f.Update.MultipartPush:
		return bmc.DialectMultipartPush
	case f.Update.SimpleUpdate:
		return bmc.DialectSimpleUpdate
	default:
		return bmc.DialectNoUpdate
	}
}

// dialect derives the insert mechanism a slot advertises — the same probe order as
// tools/bmc-inventory.sh: the standard action wins, OEM VmmControl is the fallback.
func (f *Fixture) dialect(slot int) bmc.MediaDialect {
	vm := f.VirtualMedia[slot]
	switch {
	case vm.InsertStandard:
		return bmc.DialectStandard
	case slices.Contains(vm.OemActions, "#VirtualMedia.VmmControl"):
		return bmc.DialectVmmControl
	default:
		return bmc.DialectNone
	}
}
