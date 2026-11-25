// Package bmc is the hardware seam: reconcilers and the CLI depend on Client,
// never on a Redfish library or a vendor dialect. Production speaks gofish; tests and
// the dev environment speak the sim — both are exercised by the same bmctest conformance
// suite, so the sim's fidelity is itself under test.
//
// Vendor asymmetries live in DATA (Capabilities, discovered per endpoint), not in types:
// Supermicro X12 exposes the DMTF InsertMedia action; Huawei iBMC exposes only the OEM
// #VirtualMedia.VmmControl and returns 202 + a task URI. The serial anchors differ too —
// Huawei's ComputerSystem carries the chassis serial, Supermicro's carries none (the board
// serial lives on the Chassis resource); identity normalization over these anchors lives in internal/identity.
package bmc

import (
	"context"
	"slices"
	"time"
)

// Client is one connected management endpoint (a Redfish manager).
type Client interface {
	// Manager reports manager-level identity: firmware, vendor, hostname.
	Manager(ctx context.Context) (Manager, error)
	// Chassis reports chassis-level facts (board serial — the Supermicro anchor).
	Chassis(ctx context.Context) (Chassis, error)
	// Systems enumerates the ComputerSystems this endpoint fronts (1 today, N-ready).
	Systems(ctx context.Context) ([]System, error)
	// Capabilities reports what the endpoint can DO — the HOW-is-data rule.
	Capabilities(ctx context.Context) (Capabilities, error)

	Power(ctx context.Context, systemID string) (PowerState, error)
	SetPower(ctx context.Context, systemID string, action PowerAction) error
	// SetBootOverride points the next boot (Once) or every boot (Continuous) at
	// a source. The current override reads back on System.BootOverride, which
	// is what lets a resumed caller check its goal instead of re-arming it.
	SetBootOverride(ctx context.Context, systemID string, o BootOverride) error

	Media(ctx context.Context) ([]VirtualMedia, error)
	// InsertMedia mounts an image URL on a media slot, dispatching the dialect the
	// endpoint advertises (standard InsertMedia vs OEM VmmControl). creds are the
	// grant's media credentials; nil mounts without authentication.
	InsertMedia(ctx context.Context, mediaID, imageURL string, creds *MediaCredentials) error
	EjectMedia(ctx context.Context, mediaID string) error

	// FirmwareInventory enumerates the UpdateService SoftwareInventory members —
	// the convergence observable (desired version vs Version here).
	FirmwareInventory(ctx context.Context) ([]Firmware, error)
	// UpdateFirmware pushes a BMC image over the endpoint's advertised multipart
	// push URI — the proven X12 recipe: one upload, apply-time Immediate, the
	// Oem.Supermicro.BMC preserve block. Refused
	// where the endpoint advertises no push URI (iBMC offers only SimpleUpdate,
	// which inverts the transport direction and stays unproven on our hardware).
	// Acceptance is NOT completion: the flash and BMC reboot follow, and the only
	// reliable proof is Manager LastResetTime advancing plus the version reading
	// back — this firmware answers success to actions it silently declines.
	UpdateFirmware(ctx context.Context, image []byte) error
	// StageFirmware pushes a system firmware image (BIOS/UEFI, NIC, RAID) for
	// one FirmwareInventory entry, to apply at the system's next reset
	// (@Redfish.OperationApplyTime OnReset). Refused where the push URI does
	// not list OnReset among its supported apply times: an image pushed with
	// another apply time either flashes at once under a running OS or stages
	// where nothing ever installs it. Acceptance proves only that it is
	// staged; the inventory version after the reset is the proof it applied.
	StageFirmware(ctx context.Context, image []byte, entryID string) error
	// ResetManager reboots the BMC itself (Manager.Reset) — host power untouched.
	// GracefulRestart or ForceRestart; graceful is refused mid-update, force also
	// discards a stuck staged image (the recorded X12 wedge cure).
	ResetManager(ctx context.Context, action PowerAction) error

	// Bios reads a system's BIOS attributes: current plus whatever is staged in
	// the @Redfish.Settings pending object (applied on next system reset).
	Bios(ctx context.Context, systemID string) (BiosState, error)
	// SetBiosAttributes stages attributes into the pending settings object. The
	// world changes only at the next system reset; convergence is judged by
	// re-reading Bios afterwards, never by this call succeeding.
	SetBiosAttributes(ctx context.Context, systemID string, attrs map[string]any) error
	// BiosSchema reads the attribute registry the Bios resource names. nil with
	// no error when the endpoint serves no registry — absence is data, and the
	// consumer falls back to verify-after-reboot without allowed-values checks.
	BiosSchema(ctx context.Context, systemID string) ([]BiosAttribute, error)

	// EnsureAccount makes the named manager account exist with exactly this
	// password — created when absent, password reset when present, enabled
	// either way. Role is always Administrator (the executor flashes firmware
	// and resets the manager over it). Idempotent: converging to a password
	// already set is a success. The session driving this call may be the
	// account being ensured (a rotation through the managed session): the
	// caller keeps the old password until a login with the new one succeeds,
	// so an error here, even one after the change landed, cannot lock out.
	EnsureAccount(ctx context.Context, username, password string) error
	// DisableAccount turns the named manager account off (Enabled false):
	// it logs in no more, and its password is kept nowhere. An account
	// that does not exist is already off, so that is a success. It is
	// called with a session of another account than the one it disables.
	DisableAccount(ctx context.Context, username string) error

	// NTP reads the manager's time-source configuration. Two vendor shapes
	// behind one view: the X12 serves the standard NetworkProtocol NTP
	// block; the iBMC serves nothing there and carries an OEM NtpService
	// with three discrete server slots instead.
	NTP(ctx context.Context) (NTPConfig, error)
	// SetNTP converges the manager's time sources. Like every write, the
	// call succeeding proves nothing — convergence is judged by re-reading
	// NTP afterwards.
	SetNTP(ctx context.Context, cfg NTPConfig) error

	// GenerateCSR has the MANAGER create a signing request — the csr-native
	// identity dialect (iBMC HttpsCert.GenerateCSR): the private key never
	// leaves the manager, the pending CSR is read back as PEM. The live
	// ActionInfo requires Country and CommonName; SubjectAltName rides in
	// openssl syntax (undocumented on 6.72, verified on hardware).
	GenerateCSR(ctx context.Context, subject CSRSubject) ([]byte, error)
	// ImportServerCertificate hands the signed chain (PEM, leaf first) back
	// to a csr-native manager. Serving switches only after ResetManager —
	// convergence is judged by the certificate the endpoint serves.
	ImportServerCertificate(ctx context.Context, chainPEM []byte) error
	// UploadServerIdentity pushes a locally-generated leaf and key — the
	// key-upload dialect (X12 SmcSSLCert.Upload, multipart cert_file +
	// key_file, leaf-only: both field implementations strip the chain).
	// Serving switches only after ResetManager.
	UploadServerIdentity(ctx context.Context, certPEM, keyPEM []byte) error

	Close() error
}

// CSRSubject is the identity a csr-native manager builds its CSR around.
type CSRSubject struct {
	CommonName  string
	DNSNames    []string
	IPAddresses []string
	// Organizational attributes; the iBMC requires Country, the rest are
	// optional cosmetics.
	Country            string
	State              string
	Locality           string
	Organization       string
	OrganizationalUnit string
}

// NTPConfig is the manager's time-synchronization view. Servers in
// preference order; the iBMC caps at three (preferred/alternate/extra).
type NTPConfig struct {
	Enabled bool
	Servers []string
}

// Firmware is one UpdateService SoftwareInventory member. IDs are vendor-shaped
// (X12: BMC/Backup_BMC/Staging_BMC/BIOS…, iBMC: ActiveBMC/BackupBMC/Bios…) and
// kept opaque; consumers match by id from their profile, never by position.
type Firmware struct {
	ID         string
	Version    string
	Updateable bool
}

// BiosState is a system's BIOS view: current attributes and the staged pending
// set. Values are JSON-typed as served (string, bool, float64) — both vendors
// mix enum strings, booleans, and numbers in one map.
type BiosState struct {
	Attributes map[string]any
	Pending    map[string]any
	// Registry is the attribute-registry identity the Bios resource advertises,
	// verbatim (the spelling differs from the Registries member and the file on
	// both vendors — consumers record it, BiosSchema resolves it).
	Registry string
}

// BiosAttribute is one attribute registry entry — what BiosSchema discovery
// materialises. AllowedValues is empty for non-enumeration types.
type BiosAttribute struct {
	Name          string
	Type          string // "Enumeration" | "Boolean" | "Integer" | "String"
	AllowedValues []string
}

type Manager struct {
	Vendor          string // "Huawei" | "Supermicro" | …
	FirmwareVersion string
	Hostname        string // iBMC reports the chassis serial here — observed, kept opaque
	MAC             string // the BMC's own NIC MAC (dnsmasq-ledger identity)
	// LastResetTime is when the BMC itself last rebooted — the flash-completion
	// observable (a genuine flash reboots the BMC and moves this). Zero where the
	// vendor doesn't serve it (iBMC); never simulated there.
	LastResetTime time.Time
}

type Chassis struct {
	BoardSerial string // Supermicro: the only serial anchor; Huawei: matches the system serial
	Model       string
	Adapters    []Adapter // PCIe NICs (chassis NetworkAdapters view)
}

type Adapter struct {
	Model    string
	PortMACs []string
}

type System struct {
	ID           string // Redfish member id, opaque
	UUID         string
	SerialNumber string // as served by ComputerSystem — EMPTY on Supermicro X12
	Manufacturer string
	Model        string
	PowerState   PowerState
	// LastResetTime is when the system last came out of reset — the
	// baseline-and-advance observable. Zero where the vendor doesn't serve it
	// (iBMC); never simulated where real hardware stays silent.
	LastResetTime time.Time
	MemoryGiB     int
	CPUModel      string
	CPUCount      int
	NICs          []NIC
	Drives        []Drive
	// BootOverride is the override as currently set. A Once override reads
	// Disabled after the boot that consumed it, so a goal check against it is
	// only meaningful before the next power transition.
	BootOverride BootOverride
	// BootTargets are BootSourceOverrideTarget@AllowableValues — learned on hardware:
	// prefer UsbCd when offered (Supermicro boots nothing from Cd), never hardcode.
	BootTargets []string
	// BootProgress is BootProgress.LastState as served ("None", "OSBootStarted",
	// "OSRunning", …). Empty where the endpoint serves no boot progress — absence
	// is data, a consumer must not read it as "not booted".
	BootProgress string
}

// BootOverride is the Redfish Boot override triple. Empty fields read as "not
// served" and, on a write, as "leave as is" (Mode) or Once (Enabled).
type BootOverride struct {
	Target  string // BootSourceOverrideTarget: Cd, UsbCd, Pxe, Hdd, …
	Enabled string // BootSourceOverrideEnabled: Once | Continuous | Disabled
	Mode    string // BootSourceOverrideMode: UEFI | Legacy
}

// Boot override enablement values as Redfish spells them.
const (
	OverrideOnce       = "Once"
	OverrideContinuous = "Continuous"
	OverrideDisabled   = "Disabled"
)

type NIC struct {
	ID  string
	MAC string
}

type Drive struct {
	Model       string
	Serial      string
	CapacityGiB int
	MediaType   string // "SSD"/"HDD" as served — empty where the vendor omits it
	Protocol    string // "PCIe"/"SATA" as served — empty where the vendor omits it
}

// MediaCredentials authenticate the BMC's media fetch — they arrive at the asset
// server as HTTP Basic either way, but each vendor carries them differently:
// Supermicro takes UserName/Password action parameters and rejects URL userinfo
// with 400, Huawei takes userinfo in the image URL and nothing else. The
// implementations dispatch; callers never encode credentials into the URL.
type MediaCredentials struct {
	Username string
	Password string
}

// VirtualMedia is the discovered shape of the endpoint's virtual media slots.
// Media lists slots in the endpoint's collection order.
type VirtualMedia struct {
	ID             string // "CD", "USBStick", "VirtualMedia1"…
	MediaTypes     []string
	InsertStandard bool     // DMTF InsertMedia action present
	OemActions     []string // e.g. "#VirtualMedia.VmmControl"
	Inserted       bool
	ImageURL       string
}

// Capabilities is the endpoint-level summary the reconcilers select strategies from.
type Capabilities struct {
	// MediaDialect is the insert mechanism per media slot id.
	MediaDialect map[string]MediaDialect
	// UpdateDialect is the firmware-update transport the endpoint advertises.
	UpdateDialect UpdateDialect
	// IdentityDialect is how the manager's HTTPS certificate is replaced.
	IdentityDialect IdentityDialect
}

type MediaDialect string

const (
	// DialectStandard is the DMTF #VirtualMedia.InsertMedia action.
	DialectStandard MediaDialect = "insert-media"
	// DialectVmmControl is the Huawei OEM #VirtualMedia.VmmControl action (202 + task).
	DialectVmmControl MediaDialect = "vmm-control"
	// DialectNone means the slot advertises no insert mechanism at all.
	DialectNone MediaDialect = "none"
)

type UpdateDialect string

const (
	// DialectMultipartPush is MultipartHttpPushUri — the operator uploads the
	// image TO the BMC (the X12, and the only transport proven on our hardware:
	// it works even when the BMC cannot route off-link).
	DialectMultipartPush UpdateDialect = "multipart-push"
	// DialectSimpleUpdate is #UpdateService.SimpleUpdate only — the BMC fetches
	// an ImageURI (the iBMC shape). Advertised ≠ proven: no flash over it has
	// been run on our fleet, so UpdateFirmware refuses it for now.
	DialectSimpleUpdate UpdateDialect = "simple-update"
	// DialectNoUpdate means the endpoint advertises no update transport.
	DialectNoUpdate UpdateDialect = "none"
)

type IdentityDialect string

const (
	// DialectCSRNative: the manager generates the CSR, the key never leaves
	// it, the signed chain is imported back (the standard CertificateService
	// GenerateCSR/ReplaceCertificate pair, or the iBMC's OEM HttpsCert).
	DialectCSRNative IdentityDialect = "csr-native"
	// DialectKeyUpload: a locally-generated leaf and key are uploaded (X12
	// SmcSSLCert.Upload).
	DialectKeyUpload IdentityDialect = "key-upload"
	// DialectNoIdentity: no certificate-replacement surface advertised.
	DialectNoIdentity IdentityDialect = "none"
)

type PowerState string

const (
	PowerOn  PowerState = "On"
	PowerOff PowerState = "Off"
	// The transitional states a system reports while a reset takes effect.
	PowerPoweringOn  PowerState = "PoweringOn"
	PowerPoweringOff PowerState = "PoweringOff"
)

// PowerAction values follow Redfish ResetType — discovered from AllowableValues in
// production, never assumed.
type PowerAction string

const (
	ActionOn               PowerAction = "On"
	ActionForceOff         PowerAction = "ForceOff"
	ActionGracefulShutdown PowerAction = "GracefulShutdown"
	ActionForceRestart     PowerAction = "ForceRestart"
	// ActionGracefulRestart is a Manager.Reset type (both vendors advertise it
	// there) and a system reset type where the endpoint offers it. On the X12
	// the graceful manager path runs ROT attestation and is refused mid-update.
	ActionGracefulRestart PowerAction = "GracefulRestart"
	// ActionPowerCycle removes and restores power — a cold boot, where
	// ForceRestart is a warm one.
	ActionPowerCycle PowerAction = "PowerCycle"
)

// InsertableSlots orders the media slots an image can be mounted on, best boot
// slot first: slots whose MediaTypes offer CD or DVD lead (in the endpoint's
// order), the remaining insertable slots follow. Position in the collection says
// nothing — a standard tree may list a USB-stick slot before the optical one —
// and an empty MediaTypes list says nothing either (the iBMC's CD slot serves
// one), so untyped slots stay as fallbacks instead of being dropped.
func InsertableSlots(media []VirtualMedia, caps Capabilities) []string {
	var optical, other []string
	for _, m := range media {
		if d := caps.MediaDialect[m.ID]; d == "" || d == DialectNone {
			continue
		}
		if slices.Contains(m.MediaTypes, "CD") || slices.Contains(m.MediaTypes, "DVD") {
			optical = append(optical, m.ID)
		} else {
			other = append(other, m.ID)
		}
	}
	return append(optical, other...)
}
