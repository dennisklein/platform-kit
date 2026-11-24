package sim

import (
	"bytes"
	"cmp"
	"embed"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Fixture describes one simulated management endpoint: the facts it serves and
// the dialect it serves them in. Three personalities are embedded. `generic` is
// a BMC that follows the DMTF specification with no OEM extensions — the lab's
// default machine. `huawei-2288h-v5` and `supermicro-x12` mirror captured
// hardware, quirks included.
//
// Where personalities differ, the difference is a field here, never a vendor
// name in the server: a new machine is a new YAML file.
type Fixture struct {
	Name string `yaml:"name"`
	// Vendor is the manufacturer the endpoint reports itself as.
	Vendor string `yaml:"vendor"`
	// Product is ServiceRoot.Product; empty serves none.
	Product string `yaml:"product"`
	// RedfishVersion is ServiceRoot.RedfishVersion (default 1.15.0).
	RedfishVersion string `yaml:"redfish_version"`

	Protocol Protocol `yaml:"protocol"`
	Layout   Layout   `yaml:"layout"`

	Manager struct {
		Firmware string `yaml:"firmware"`
		Hostname string `yaml:"hostname"`
		MAC      string `yaml:"mac"`
		Model    string `yaml:"model"`
		// ActiveSoftwareImage serves Links.ActiveSoftwareImage, pointing at the
		// inventory entry that tracks the manager firmware.
		ActiveSoftwareImage bool `yaml:"active_software_image"`
		// LastResetTime seeds the manager's own reset-time observable — the
		// flash-completion proof. Set only where the endpoint serves it (the
		// iBMC does not; the captures decide).
		LastResetTime time.Time `yaml:"last_reset_time"`
	} `yaml:"manager"`

	// Accounts seeds the AccountService. Empty = OPEN sim: no account service
	// served, no auth enforced — the bench posture every credential-less test
	// relies on; any seeded account turns both on.
	Accounts []FixtureAccount `yaml:"accounts"`
	// AccountSlots is the number of fixed ManagerAccount members. Zero serves a
	// growable collection (accounts are created with POST). A positive count
	// serves exactly that many members, vacant ones with an empty UserName:
	// an account is created by PATCHing a vacant slot and POST answers 405.
	AccountSlots int `yaml:"account_slots"`
	// ReservedAccountSlots are vacant slots that refuse a PATCH (400), the
	// way iDRAC keeps slot 1: empty, disabled and read-only.
	ReservedAccountSlots []string `yaml:"reserved_account_slots"`
	// LockoutThreshold locks an account after that many failed logins in a
	// row; a locked account refuses every login until a PATCH sends
	// Locked: false. A new password does not unlock it. Zero never locks.
	LockoutThreshold int `yaml:"lockout_threshold"`

	// NTP is the time-source surface: the standard NetworkProtocol NTP block
	// (standard), or the iBMC's OEM HwNtpService with three server slots
	// (oem_service). Neither = no NTP surface at all.
	NTP struct {
		Standard   bool     `yaml:"standard"`
		OemService bool     `yaml:"oem_service"`
		Enabled    bool     `yaml:"enabled"`
		Servers    []string `yaml:"servers"`
		// Slots is the number of server slots; a write of more servers is
		// refused. Zero is no limit, except on oem_service (three slots).
		Slots int `yaml:"slots"`
	} `yaml:"ntp"`

	// Identity is the certificate-replacement surface. certificate_service is
	// the standard one: CertificateService.GenerateCSR answers with the CSR and
	// ReplaceCertificate switches the served certificate at once. csr_native is
	// the iBMC's OEM HttpsCert (the CSR is polled, serving switches at the next
	// manager reset); key_upload the X12's OEM SmcSSLCert leaf+key upload.
	Identity struct {
		CertificateService bool `yaml:"certificate_service"`
		CSRNative          bool `yaml:"csr_native"`
		KeyUpload          bool `yaml:"key_upload"`
		// CSRDelay is how long the csr_native (OEM) manager takes to make a
		// new key and request; the standard action answers with the request. Until then HttpsCert keeps serving the previous
		// request, and the pending key stays the previous one. Zero makes
		// the request inside the GenerateCSR call.
		CSRDelay time.Duration `yaml:"csr_delay"`
	} `yaml:"identity"`

	// Update is the UpdateService: which transports are advertised and the
	// FirmwareInventory members served.
	Update struct {
		MultipartPush bool `yaml:"multipart_push"`
		// PushURI is MultipartHttpPushUri (default
		// /redfish/v1/UpdateService/update-multipart).
		PushURI      string `yaml:"push_uri"`
		SimpleUpdate bool   `yaml:"simple_update"`
		// Wedge selects the recorded X12 behaviour over the specified one: an
		// upload without the Oem.Supermicro.BMC preserve block, or with any
		// apply time but Immediate, stages and never installs; the manager then
		// refuses further uploads and a graceful reset; StartUpdate rejects
		// every body. False = the standard semantics: OnStartUpdateRequest
		// stages, StartUpdate installs.
		Wedge bool `yaml:"wedge"`
		// OnReset adds OnReset to the push URI's supported apply times: an
		// upload with that apply time and an inventory entry other than the
		// manager's as its target stages the image, and the next boot of
		// the system installs it (the way BIOS, NIC and RAID updates apply
		// on servers that support it).
		OnReset   bool `yaml:"on_reset"`
		Inventory []struct {
			ID         string `yaml:"id"`
			Version    string `yaml:"version"`
			Updateable bool   `yaml:"updateable"`
			// BMC marks the entry that tracks the live manager firmware;
			// Staging marks the X12 staging slot, which mirrors the current
			// version when nothing is staged.
			BMC     bool `yaml:"bmc"`
			Staging bool `yaml:"staging"`
		} `yaml:"inventory"`
	} `yaml:"update"`

	// Bios seeds every system's BIOS view (the fixtures front one system each).
	Bios struct {
		// SettingsID is the pending-object member id under Systems/{id}/Bios
		// ("Settings" by the standard's convention and on the iBMC, "SD" on
		// the X12).
		SettingsID string `yaml:"settings_id"`
		// Registry is what the Bios resource's AttributeRegistry advertises.
		// Both captured vendors spell the name differently in every place it
		// appears, so the next two fields carry the other spellings and force
		// clients to normalise instead of exact-match.
		Registry string `yaml:"registry"`
		// RegistryID is the Registries collection member id (default: Registry).
		RegistryID string `yaml:"registry_id"`
		// RegistryName is the member document's Registry field (default: Registry).
		RegistryName string `yaml:"registry_name"`
		// RegistryURI is the member's Location Uri. Default:
		// /redfish/v1/Registries/<id>/<id>.json, inside the tree; the X12
		// serves the file outside it.
		RegistryURI string `yaml:"registry_uri"`
		// SupportedApplyTimes is @Redfish.Settings.SupportedApplyTimes
		// (default: OnReset). A PATCH asking for an apply time outside it
		// is refused.
		SupportedApplyTimes []string `yaml:"supported_apply_times"`
		// ApplyTimeRequired plays iDRAC 9: a PATCH without
		// @Redfish.SettingsApplyTime stages its values but creates no BIOS
		// configuration job, so they stay pending across every reset. Only
		// a PATCH asking for OnReset schedules them.
		ApplyTimeRequired bool `yaml:"apply_time_required"`
		// PendingFullSet plays iLO 5: the pending settings object serves
		// every attribute, the staged values merged over the current ones,
		// instead of only the staged ones.
		PendingFullSet bool           `yaml:"pending_full_set"`
		Attributes     map[string]any `yaml:"attributes"`
		Schema         []struct {
			Name          string   `yaml:"name"`
			Type          string   `yaml:"type"`
			AllowedValues []string `yaml:"allowed_values"`
		} `yaml:"schema"`
	} `yaml:"bios"`

	Chassis struct {
		BoardSerial string `yaml:"board_serial"`
		Model       string `yaml:"model"`
		// OemBoardSerial mirrors the X12: the board serial lives in
		// Oem.Supermicro.BoardSerialNumber, the plain SerialNumber is empty.
		OemBoardSerial bool `yaml:"oem_board_serial"`
		Adapters       []struct {
			Model    string   `yaml:"model"`
			PortMACs []string `yaml:"port_macs"`
		} `yaml:"adapters"`
	} `yaml:"chassis"`

	// VirtualMedia lists the slots in collection order. The order is part of
	// the personality: nothing guarantees the optical slot comes first.
	VirtualMedia []struct {
		ID             string   `yaml:"id"`
		MediaTypes     []string `yaml:"media_types"`
		InsertStandard bool     `yaml:"insert_standard"`
		OemActions     []string `yaml:"oem_actions"`
	} `yaml:"virtualmedia"`

	Systems []FixtureSystem `yaml:"systems"`
}

// Protocol is how the endpoint speaks, independent of what it serves.
type Protocol struct {
	// OemVendorKey serves an Oem block keyed by the vendor name on the Manager
	// — where the iBMC hangs its OEM services, and how an endpoint without
	// ServiceRoot.Vendor names its maker.
	OemVendorKey bool `yaml:"oem_vendor_key"`
	// RootVendor serves ServiceRoot.Vendor.
	RootVendor bool `yaml:"root_vendor"`
	// ETags serves @odata.etag and the ETag header on every resource and
	// honours If-Match on PATCH (412 on a stale tag).
	ETags bool `yaml:"etags"`
	// RequireIfMatch answers 428 to a PATCH without If-Match — permitted by
	// the specification for any resource that serves an ETag, and what the
	// strictest services do.
	RequireIfMatch bool `yaml:"require_if_match"`
	// ETagGzipSuffix plays a Redfish service behind Apache mod_deflate: the
	// ETag header gets "-gzip" before its closing quote, while @odata.etag in
	// the body and the If-Match comparison keep the plain tag. A client that
	// echoes the header unchanged gets 412 on every PATCH.
	ETagGzipSuffix bool `yaml:"etag_gzip_suffix"`
	// Sessions serves the SessionService and accepts X-Auth-Token next to
	// Basic authentication.
	Sessions bool `yaml:"sessions"`
	// TaskDuration is how long an asynchronous operation runs before its task
	// completes and its effect lands. Zero completes inside the request.
	TaskDuration time.Duration `yaml:"task_duration"`
	// TaskInBody answers 202 with the Task in the body only, no Location
	// header: the specification asks for the header, some services leave
	// it out. A client that follows only the header takes the operation as
	// done at once.
	TaskInBody bool `yaml:"task_in_body"`
	// Latency delays every HTTP answer by this much: a real BMC answers in
	// 0.3 to 1.5 seconds. Zero answers at once.
	Latency time.Duration `yaml:"latency"`
	// MaxConnections is how many HTTP requests the service handles at the
	// same time; one more is answered 503 with Retry-After, as a BMC with a
	// small connection table does. Zero is no limit.
	MaxConnections int `yaml:"max_connections"`
	// PowerTransition is how long a system reports PoweringOn or
	// PoweringOff after a reset before it reports On or Off. Zero changes
	// the reported state inside the request.
	PowerTransition time.Duration `yaml:"power_transition"`
}

// Layout is where the endpoint puts resources the specification has moved over
// the years.
type Layout struct {
	// VirtualMedia is the resource the VirtualMedia collection hangs off:
	// "manager" (the original location, both captured vendors) or "system"
	// (ComputerSystem.VirtualMedia, which deprecates the former).
	VirtualMedia string `yaml:"virtual_media"`
	// Drives is where Drive resources live: "chassis" (Chassis Links.Drives →
	// Chassis/1/Drives/N) or "storage" (Systems/{id}/Storage/1 → Drives).
	Drives string `yaml:"drives"`
	// AdapterMACs is how a chassis NetworkAdapter exposes its port MACs:
	// "device-functions" (NetworkDeviceFunctions → Ethernet.MACAddress) or
	// "network-ports" (the deprecated NetworkPort.AssociatedNetworkAddresses
	// the iBMC serves).
	AdapterMACs string `yaml:"adapter_macs"`
}

const (
	layoutManager         = "manager"
	layoutSystem          = "system"
	layoutChassis         = "chassis"
	layoutStorage         = "storage"
	layoutDeviceFunctions = "device-functions"
	layoutNetworkPorts    = "network-ports"
)

// FixtureSystem is one ComputerSystem.
type FixtureSystem struct {
	ID           string `yaml:"id"`
	UUID         string `yaml:"uuid"`
	Serial       string `yaml:"serial"` // empty = the Supermicro Systems/1 blind spot
	Manufacturer string `yaml:"manufacturer"`
	Model        string `yaml:"model"`
	SKU          string `yaml:"sku"`
	Power        string `yaml:"power"`
	// LastResetTime seeds the reset-time observable — set only where the
	// endpoint serves it (the iBMC does not).
	LastResetTime time.Time `yaml:"last_reset_time"`
	MemoryGiB     int       `yaml:"memory_gib"`
	CPUModel      string    `yaml:"cpu_model"`
	CPUCount      int       `yaml:"cpu_count"`
	BootTargets   []string  `yaml:"boot_targets"`
	// BootMode seeds Boot.BootSourceOverrideMode ("UEFI" | "Legacy"). The X12
	// reports Legacy on a machine that boots UEFI — a client that leaves it
	// alone gets a legacy one-time boot.
	BootMode string `yaml:"boot_mode"`
	// ResetTypes are ResetType@Redfish.AllowableValues (default: On, ForceOff,
	// GracefulShutdown, ForceRestart).
	ResetTypes []string `yaml:"reset_types"`
	// BootProgress serves BootProgress.LastState.
	BootProgress bool         `yaml:"boot_progress"`
	NICs         []FixtureNIC `yaml:"nics"`
	Drives       []struct {
		Model       string `yaml:"model"`
		Serial      string `yaml:"serial"`
		CapacityGiB int    `yaml:"capacity_gib"`
		Media       string `yaml:"media"`
		Protocol    string `yaml:"protocol"`
	} `yaml:"drives"`
}

// FixtureNIC is one host network interface.
type FixtureNIC struct {
	ID  string `yaml:"id"`
	MAC string `yaml:"mac"`
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

// Fixtures lists the embedded fixture names.
func Fixtures() []string {
	entries, _ := embedded.ReadDir("fixtures")
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			names = append(names, name)
		}
	}
	return names
}

// LoadFixture resolves name as an embedded fixture (e.g. "generic") or a path.
// Every call returns its own copy, so callers may mutate the result.
func LoadFixture(name string) (*Fixture, error) {
	data, err := embedded.ReadFile("fixtures/" + name + ".yaml")
	if err != nil {
		if data, err = os.ReadFile(name); err != nil {
			return nil, fmt.Errorf("fixture %q: not embedded (%s) and not a readable path",
				name, strings.Join(Fixtures(), ", "))
		}
	}
	f, err := parseFixture(data)
	if err != nil {
		return nil, fmt.Errorf("fixture %q: %w", name, err)
	}
	return f, nil
}

// parseFixture decodes and validates a fixture's content.
func parseFixture(data []byte) (*Fixture, error) {
	// Strict: a fixture can be a user's file, and a misspelled quirk key
	// would otherwise be dropped, so the sim would stop reproducing that
	// quirk without a word.
	var f Fixture
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// validate rejects layout values the server has no tree for — a typo there
// would otherwise serve the default silently.
func (f *Fixture) validate() error {
	for _, c := range []struct {
		field, value string
		allowed      []string
	}{
		{"layout.virtual_media", f.Layout.VirtualMedia, []string{"", layoutManager, layoutSystem}},
		{"layout.drives", f.Layout.Drives, []string{"", layoutChassis, layoutStorage}},
		{"layout.adapter_macs", f.Layout.AdapterMACs, []string{"", layoutDeviceFunctions, layoutNetworkPorts}},
	} {
		if !slices.Contains(c.allowed, c.value) {
			return fmt.Errorf("%s: %q is not one of %v", c.field, c.value, c.allowed[1:])
		}
	}
	if len(f.Systems) == 0 {
		return fmt.Errorf("no systems")
	}
	// Ids and MACs tell resources apart: two systems with one id serve one
	// URL, and two NICs with one MAC are one NIC to every client.
	ids, macs := map[string]bool{}, map[string]string{}
	mac := func(where, m string) error {
		if m == "" {
			return nil
		}
		key := strings.ToLower(m)
		if other, dup := macs[key]; dup {
			return fmt.Errorf("%s: MAC %s is also used by %s", where, m, other)
		}
		macs[key] = where
		return nil
	}
	if err := mac("manager", f.Manager.MAC); err != nil {
		return err
	}
	for _, s := range f.Systems {
		if ids[s.ID] {
			return fmt.Errorf("system id %q is used twice", s.ID)
		}
		ids[s.ID] = true
		for _, n := range s.NICs {
			if err := mac(fmt.Sprintf("system %q nic %q", s.ID, n.ID), n.MAC); err != nil {
				return err
			}
		}
	}
	if f.AccountSlots > 0 && len(f.Accounts) > f.AccountSlots {
		return fmt.Errorf("%d accounts seeded into %d slots", len(f.Accounts), f.AccountSlots)
	}
	for _, a := range f.Accounts {
		if slices.Contains(f.ReservedAccountSlots, a.ID) {
			return fmt.Errorf("account %s is seeded into reserved slot %s", a.Username, a.ID)
		}
	}
	if f.Protocol.Latency < 0 || f.Protocol.PowerTransition < 0 || f.Protocol.MaxConnections < 0 {
		return fmt.Errorf("protocol: latency, power_transition and max_connections must not be negative")
	}
	if f.LockoutThreshold < 0 {
		return fmt.Errorf("lockout_threshold %d is negative", f.LockoutThreshold)
	}
	return f.checkRoutes()
}

// checkRoutes builds the server once. Some fixture values (the registry
// URI, the push URI, the vendor) become routes, and the router panics on a
// route that is malformed or clashes with another; that must be an error
// when the fixture is loaded, not a crash when the sim starts.
func (f *Fixture) checkRoutes() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the sim cannot serve this fixture: %v", r)
		}
	}()
	NewServer(NewState(f))
	return nil
}

// Identity is what tells two machines of one personality apart. Empty fields
// keep the fixture's value.
type Identity struct {
	Serial  string
	UUID    string
	BMCMAC  string
	NICMACs []string
	// DiskSerial replaces the first drive's serial — what an install-disk
	// selector matches on.
	DiskSerial string
}

// Personalize stamps a machine's own identity over the fixture's: serial on
// system and chassis, UUID, the manager's MAC and hostname, and one host NIC
// per given MAC (mirrored on the first chassis adapter, which is the same
// hardware seen from the other side).
func (f *Fixture) Personalize(id Identity) {
	sys := &f.Systems[0]
	if id.Serial != "" {
		if sys.Serial != "" {
			sys.Serial = id.Serial
		}
		f.Chassis.BoardSerial = id.Serial
		f.Manager.Hostname = "bmc-" + strings.ToLower(id.Serial)
	}
	if id.UUID != "" {
		sys.UUID = id.UUID
	}
	if id.BMCMAC != "" {
		f.Manager.MAC = strings.ToLower(id.BMCMAC)
	}
	if len(id.NICMACs) > 0 {
		sys.NICs = nil
		macs := make([]string, 0, len(id.NICMACs))
		for i, mac := range id.NICMACs {
			mac = strings.ToLower(mac)
			macs = append(macs, mac)
			sys.NICs = append(sys.NICs, FixtureNIC{ID: fmt.Sprintf("%d", i+1), MAC: mac})
		}
		if len(f.Chassis.Adapters) > 0 {
			f.Chassis.Adapters[0].PortMACs = macs
		}
	}
	if id.DiskSerial != "" && len(sys.Drives) > 0 {
		sys.Drives[0].Serial = id.DiskSerial
	}
}

// SetCredentials replaces the first seeded account's username and password —
// the factory credential a lab entry configures per machine.
func (f *Fixture) SetCredentials(username, password string) {
	if len(f.Accounts) == 0 {
		f.Accounts = []FixtureAccount{{ID: "1", Role: "Administrator", Enabled: true}}
	}
	f.Accounts[0].Username, f.Accounts[0].Password = username, password
}

func (f *Fixture) redfishVersion() string { return cmp.Or(f.RedfishVersion, "1.15.0") }

// registryID/registryName/registryURI resolve the registry-identity fields with
// their defaults — a fixture that sets only `registry` serves a coherent tree.
func (f *Fixture) registryID() string {
	return cmp.Or(f.Bios.RegistryID, f.Bios.Registry)
}

func (f *Fixture) registryName() string {
	return cmp.Or(f.Bios.RegistryName, f.Bios.Registry)
}

func (f *Fixture) registryURI() string {
	return cmp.Or(f.Bios.RegistryURI, "/redfish/v1/Registries/"+f.registryID()+"/"+f.registryID()+".json")
}

func (f *Fixture) pushURI() string {
	return cmp.Or(f.Update.PushURI, "/redfish/v1/UpdateService/update-multipart")
}

// mediaBase is the VirtualMedia collection's URI.
func (f *Fixture) mediaBase() string {
	if f.Layout.VirtualMedia == layoutSystem {
		return "/redfish/v1/Systems/" + f.Systems[0].ID + "/VirtualMedia"
	}
	return "/redfish/v1/Managers/1/VirtualMedia"
}

// resetTypes are the system reset types the endpoint advertises.
func (fs *FixtureSystem) resetTypes() []string {
	if len(fs.ResetTypes) > 0 {
		return fs.ResetTypes
	}
	return []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart"}
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

// dialect derives the insert mechanism a slot advertises: the standard action
// wins, the OEM VmmControl is the fallback.
func (f *Fixture) dialect(slot int) bmc.MediaDialect {
	vm := f.VirtualMedia[slot]
	switch {
	case vm.InsertStandard:
		return bmc.DialectStandard
	case slices.Contains(vm.OemActions, vmmControlAction):
		return bmc.DialectVmmControl
	default:
		return bmc.DialectNone
	}
}

const vmmControlAction = "#VirtualMedia.VmmControl"
