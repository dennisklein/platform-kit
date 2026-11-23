// Package gofish is the production bmc.Client: stmcginnis/gofish for the typed Redfish
// surface, raw HTTP through the same authenticated client where vendors leave the schema
// (the Huawei OEM #VirtualMedia.VmmControl, Supermicro's Oem board serial). Dialects are
// discovered per endpoint and dispatched as data — never assumed from the vendor string.
package gofish

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	gf "github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

const vmmControlAction = "#VirtualMedia.VmmControl"

type Config struct {
	Endpoint string // https://[fd6b:…]:443 or http://127.0.0.1:8000 (sim)
	Username string
	Password string
	Insecure bool // skip TLS verification (lab BMCs with self-signed certs)
	// RootCAs pins the server-certificate trust anchors (internal-ca on the
	// live BMCs — the PoC identity verdict). Wins over Insecure when set.
	RootCAs *x509.CertPool
	// ReadOnly refuses every mutating call at this layer — the read-only rail: pointing the
	// client at real hardware is safe by construction until explicitly unlocked.
	ReadOnly bool
}

type client struct {
	api      *gf.APIClient
	svc      *gf.Service
	readOnly bool
}

var errReadOnly = fmt.Errorf("bmc: mutating call refused (read-only client)")

// Connect dials the endpoint and returns a bmc.Client. Basic auth (the bmc-inventory.sh
// idiom) — no Redfish sessions, so an aborted run leaks nothing to clean up.
func Connect(ctx context.Context, cfg Config) (bmc.Client, error) {
	gcfg := gf.ClientConfig{
		Endpoint:  cfg.Endpoint,
		Username:  cfg.Username,
		Password:  cfg.Password,
		Insecure:  cfg.Insecure,
		BasicAuth: true,
	}
	if cfg.RootCAs != nil {
		gcfg.Insecure = false
		gcfg.HTTPClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
		}}
	}
	api, err := gf.ConnectContext(ctx, gcfg)
	if err != nil {
		return nil, fmt.Errorf("bmc: connect %s: %w", cfg.Endpoint, err)
	}
	return &client{api: api, svc: api.GetService(), readOnly: cfg.ReadOnly}, nil
}

func (c *client) Close() error {
	c.api.Logout()
	return nil
}

func (c *client) manager() (*schemas.Manager, error) {
	managers, err := c.svc.Managers()
	if err != nil {
		return nil, fmt.Errorf("bmc: managers: %w", err)
	}
	if len(managers) == 0 {
		return nil, fmt.Errorf("bmc: endpoint exposes no managers")
	}
	return managers[0], nil
}

func (c *client) Manager(ctx context.Context) (bmc.Manager, error) {
	m, err := c.manager()
	if err != nil {
		return bmc.Manager{}, err
	}
	out := bmc.Manager{
		FirmwareVersion: m.FirmwareVersion,
		Vendor:          oemVendor(m.OEM),
		Hostname:        c.managerHostname(m.ODataID),
		LastResetTime:   parseResetTime(m.LastResetTime),
	}
	if nics, err := m.EthernetInterfaces(); err == nil {
		for _, n := range nics {
			if n.MACAddress != "" {
				out.MAC = strings.ToLower(n.MACAddress)
				break
			}
		}
	}
	return out, nil
}

func (c *client) chassis() (*schemas.Chassis, error) {
	chs, err := c.svc.Chassis()
	if err != nil {
		return nil, fmt.Errorf("bmc: chassis: %w", err)
	}
	if len(chs) == 0 {
		return nil, fmt.Errorf("bmc: endpoint exposes no chassis")
	}
	return chs[0], nil
}

func (c *client) Chassis(ctx context.Context) (bmc.Chassis, error) {
	ch, err := c.chassis()
	if err != nil {
		return bmc.Chassis{}, err
	}
	out := bmc.Chassis{Model: ch.Model, BoardSerial: ch.SerialNumber}
	// The X12 sweep order from bmc-inventory.sh: Oem.Supermicro.BoardSerialNumber wins,
	// plain SerialNumber is the fallback.
	var oem struct {
		Supermicro struct {
			BoardSerialNumber string
		}
	}
	if len(ch.OEM) > 0 && json.Unmarshal(ch.OEM, &oem) == nil && oem.Supermicro.BoardSerialNumber != "" {
		out.BoardSerial = oem.Supermicro.BoardSerialNumber
	}
	if adapters, err := ch.NetworkAdapters(); err == nil {
		for _, a := range adapters {
			out.Adapters = append(out.Adapters, bmc.Adapter{
				Model:    a.Model,
				PortMACs: adapterPortMACs(a),
			})
		}
	}
	return out, nil
}

func (c *client) Systems(ctx context.Context) ([]bmc.System, error) {
	systems, err := c.svc.Systems()
	if err != nil {
		return nil, fmt.Errorf("bmc: systems: %w", err)
	}
	var out []bmc.System
	for _, s := range systems {
		sys := bmc.System{
			ID:            s.ID,
			UUID:          s.UUID,
			SerialNumber:  s.SerialNumber,
			Manufacturer:  s.Manufacturer,
			Model:         s.Model,
			PowerState:    bmc.PowerState(s.PowerState),
			LastResetTime: parseResetTime(s.LastResetTime),
			MemoryGiB:     int(derefF(s.MemorySummary.TotalSystemMemoryGiB)),
			CPUModel:      s.ProcessorSummary.Model,
			CPUCount:      int(derefU(s.ProcessorSummary.Count)),
		}
		sys.BootOverride = bmc.BootOverride{
			Target:  string(s.Boot.BootSourceOverrideTarget),
			Enabled: string(s.Boot.BootSourceOverrideEnabled),
			Mode:    string(s.Boot.BootSourceOverrideMode),
		}
		for _, t := range s.Boot.AllowableBootSourceOverrideTargetValues {
			sys.BootTargets = append(sys.BootTargets, string(t))
		}
		if nics, err := s.EthernetInterfaces(); err == nil {
			for _, n := range nics {
				sys.NICs = append(sys.NICs, bmc.NIC{ID: n.ID, MAC: strings.ToLower(n.MACAddress)})
			}
		}
		sys.Drives = c.sweepDrives(s)
		out = append(out, sys)
	}
	return out, nil
}

// sweepDrives mirrors bmc-inventory.sh: system storage first, then chassis-attached
// (where direct-attach NVMe lives), deduped by serial.
func (c *client) sweepDrives(s *schemas.ComputerSystem) []bmc.Drive {
	seen := map[string]bool{}
	var out []bmc.Drive
	add := func(d *schemas.Drive) {
		if d == nil || seen[d.SerialNumber] {
			return
		}
		seen[d.SerialNumber] = true
		out = append(out, bmc.Drive{
			Model:       d.Model,
			Serial:      d.SerialNumber,
			CapacityGiB: derefI(d.CapacityBytes) >> 30,
			MediaType:   string(d.MediaType),
			Protocol:    string(d.Protocol),
		})
	}
	if storage, err := s.Storage(); err == nil {
		for _, st := range storage {
			if drives, err := st.Drives(); err == nil {
				for _, d := range drives {
					add(d)
				}
			}
		}
	}
	if ch, err := c.chassis(); err == nil {
		if drives, err := ch.Drives(); err == nil {
			for _, d := range drives {
				add(d)
			}
		}
	}
	return out
}

func (c *client) Capabilities(ctx context.Context) (bmc.Capabilities, error) {
	media, err := c.Media(ctx)
	if err != nil {
		return bmc.Capabilities{}, err
	}
	caps := bmc.Capabilities{
		MediaDialect:    map[string]bmc.MediaDialect{},
		UpdateDialect:   c.updateDialect(),
		IdentityDialect: c.identityDialect(),
	}
	for _, m := range media {
		switch {
		case m.InsertStandard:
			caps.MediaDialect[m.ID] = bmc.DialectStandard
		case hasVmmControl(m.OemActions):
			caps.MediaDialect[m.ID] = bmc.DialectVmmControl
		default:
			caps.MediaDialect[m.ID] = bmc.DialectNone
		}
	}
	return caps, nil
}

// updateDialect probes the UpdateService: the push URI wins, SimpleUpdate is
// noted where it's all there is (advertised ≠ proven — see bmc.DialectSimpleUpdate).
func (c *client) updateDialect() bmc.UpdateDialect {
	us, err := c.svc.UpdateService()
	if err != nil {
		return bmc.DialectNoUpdate
	}
	switch {
	case us.MultipartHTTPPushURI != "":
		return bmc.DialectMultipartPush
	case c.hasSimpleUpdate(us.ODataID):
		return bmc.DialectSimpleUpdate
	default:
		return bmc.DialectNoUpdate
	}
}

// hasSimpleUpdate reads the raw UpdateService Actions — gofish keeps the
// SimpleUpdate target private.
func (c *client) hasSimpleUpdate(uri string) bool {
	resp, err := c.api.Get(uri)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var doc struct {
		Actions struct {
			SimpleUpdate struct {
				Target string `json:"target"`
			} `json:"#UpdateService.SimpleUpdate"`
		} `json:"Actions"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return false
	}
	return doc.Actions.SimpleUpdate.Target != ""
}

func (c *client) Media(ctx context.Context) ([]bmc.VirtualMedia, error) {
	m, err := c.manager()
	if err != nil {
		return nil, err
	}
	vms, err := m.VirtualMedia()
	if err != nil {
		return nil, fmt.Errorf("bmc: virtual media: %w", err)
	}
	var out []bmc.VirtualMedia
	for _, vm := range vms {
		entry := bmc.VirtualMedia{
			ID:             vm.ID,
			InsertStandard: vm.SupportsMediaInsert,
			Inserted:       vm.Inserted != nil && *vm.Inserted,
			ImageURL:       vm.Image,
		}
		for _, mt := range vm.MediaTypes {
			entry.MediaTypes = append(entry.MediaTypes, string(mt))
		}
		entry.OemActions, _ = c.oemMediaActions(vm.ODataID)
		out = append(out, entry)
	}
	return out, nil
}

// oemMediaActions reads the raw VirtualMedia resource for Actions.Oem — the part of the
// tree gofish does not type (the Huawei VmmControl lives here). Returns action names and
// caches nothing: the probe is one GET, as in bmc-inventory.sh.
func (c *client) oemMediaActions(uri string) ([]string, map[string]string) {
	resp, err := c.api.Get(uri)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	var doc struct {
		Actions struct {
			Oem map[string]struct {
				Target string `json:"target"`
			} `json:"Oem"`
		} `json:"Actions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, nil
	}
	var names []string
	targets := map[string]string{}
	for name, a := range doc.Actions.Oem {
		names = append(names, name)
		targets[name] = a.Target
	}
	return names, targets
}

func (c *client) systemByID(id string) (*schemas.ComputerSystem, error) {
	systems, err := c.svc.Systems()
	if err != nil {
		return nil, fmt.Errorf("bmc: systems: %w", err)
	}
	for _, s := range systems {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("bmc: no system %q", id)
}

func (c *client) Power(ctx context.Context, systemID string) (bmc.PowerState, error) {
	s, err := c.systemByID(systemID)
	if err != nil {
		return "", err
	}
	return bmc.PowerState(s.PowerState), nil
}

func (c *client) SetPower(ctx context.Context, systemID string, action bmc.PowerAction) error {
	if c.readOnly {
		return errReadOnly
	}
	s, err := c.systemByID(systemID)
	if err != nil {
		return err
	}
	if _, err := s.Reset(schemas.ResetType(action)); err != nil {
		return fmt.Errorf("bmc: reset %s: %w", action, err)
	}
	return nil
}

func (c *client) SetBootOverride(ctx context.Context, systemID string, o bmc.BootOverride) error {
	if c.readOnly {
		return errReadOnly
	}
	s, err := c.systemByID(systemID)
	if err != nil {
		return err
	}
	enabled := schemas.OnceBootSourceOverrideEnabled
	if o.Enabled != "" {
		enabled = schemas.BootSourceOverrideEnabled(o.Enabled)
	}
	boot := &schemas.Boot{
		BootSourceOverrideTarget:  schemas.BootSource(o.Target),
		BootSourceOverrideEnabled: enabled,
	}
	// Mode travels only when asked for: firmware that serves no mode property
	// rejects a PATCH naming it.
	if o.Mode != "" {
		boot.BootSourceOverrideMode = schemas.BootSourceOverrideMode(o.Mode)
	}
	if err := s.SetBoot(boot); err != nil {
		return fmt.Errorf("bmc: boot override %s: %w", o.Target, err)
	}
	return nil
}

func (c *client) mediaByID(id string) (*schemas.VirtualMedia, error) {
	m, err := c.manager()
	if err != nil {
		return nil, err
	}
	vms, err := m.VirtualMedia()
	if err != nil {
		return nil, fmt.Errorf("bmc: virtual media: %w", err)
	}
	for _, vm := range vms {
		if vm.ID == id {
			return vm, nil
		}
	}
	return nil, fmt.Errorf("bmc: no media slot %q", id)
}

func (c *client) InsertMedia(ctx context.Context, mediaID, imageURL string, creds *bmc.MediaCredentials) error {
	if c.readOnly {
		return errReadOnly
	}
	vm, err := c.mediaByID(mediaID)
	if err != nil {
		return err
	}
	if vm.SupportsMediaInsert {
		t := true
		params := &schemas.VirtualMediaInsertMediaParameters{
			Image: imageURL, Inserted: &t, WriteProtected: &t,
		}
		if creds != nil {
			// Supermicro takes credentials as action parameters — URL userinfo
			// is rejected 400 at the Image schema.
			params.UserName, params.Password = &creds.Username, &creds.Password
		}
		if _, err := vm.InsertMedia(params); err != nil {
			return fmt.Errorf("bmc: InsertMedia: %w", err)
		}
		return nil
	}
	names, targets := c.oemMediaActions(vm.ODataID)
	if hasVmmControl(names) {
		if creds != nil {
			// iBMC's only credential mechanism is userinfo in the image URL.
			if imageURL, err = withUserinfo(imageURL, creds); err != nil {
				return err
			}
		}
		return c.vmmControl(ctx, targets[vmmControlAction], "Connect", imageURL)
	}
	return fmt.Errorf("bmc: media %q advertises no insert mechanism", mediaID)
}

func withUserinfo(imageURL string, creds *bmc.MediaCredentials) (string, error) {
	u, err := url.Parse(imageURL)
	if err != nil {
		return "", fmt.Errorf("bmc: image url: %w", err)
	}
	u.User = url.UserPassword(creds.Username, creds.Password)
	return u.String(), nil
}

func (c *client) EjectMedia(ctx context.Context, mediaID string) error {
	if c.readOnly {
		return errReadOnly
	}
	vm, err := c.mediaByID(mediaID)
	if err != nil {
		return err
	}
	if vm.SupportsMediaEject {
		if _, err := vm.EjectMedia(); err != nil {
			return fmt.Errorf("bmc: EjectMedia: %w", err)
		}
		return nil
	}
	names, targets := c.oemMediaActions(vm.ODataID)
	if hasVmmControl(names) {
		return c.vmmControl(ctx, targets[vmmControlAction], "Disconnect", "")
	}
	return fmt.Errorf("bmc: media %q advertises no eject mechanism", mediaID)
}

func (c *client) FirmwareInventory(ctx context.Context) ([]bmc.Firmware, error) {
	us, err := c.svc.UpdateService()
	if err != nil {
		return nil, fmt.Errorf("bmc: update service: %w", err)
	}
	inv, err := us.FirmwareInventory()
	if err != nil {
		return nil, fmt.Errorf("bmc: firmware inventory: %w", err)
	}
	out := make([]bmc.Firmware, 0, len(inv))
	for _, e := range inv {
		out = append(out, bmc.Firmware{ID: e.ID, Version: e.Version, Updateable: e.Updateable})
	}
	// gofish walks collections concurrently — sort for stable output.
	slices.SortFunc(out, func(a, b bmc.Firmware) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// UpdateFirmware is the proven X12 recipe, verbatim from tools/bmc-firmware-update.sh:
// one multipart POST to the advertised push URI, UpdateParameters typed
// application/json and FIRST (part order and content types match the curl form
// the hardware accepted — gofish's PostMultipart can guarantee neither, so the
// body is built by hand), apply-time Immediate, the Oem.Supermicro.BMC preserve
// block (omitting it stages-but-never-installs). Acceptance ≠ completion:
// callers verify by Manager LastResetTime + version observables.
func (c *client) UpdateFirmware(ctx context.Context, image []byte) error {
	if c.readOnly {
		return errReadOnly
	}
	us, err := c.svc.UpdateService()
	if err != nil {
		return fmt.Errorf("bmc: update service: %w", err)
	}
	if us.MultipartHTTPPushURI == "" {
		return fmt.Errorf("bmc: endpoint advertises no multipart push URI")
	}
	m, err := c.manager()
	if err != nil {
		return err
	}
	params, err := json.Marshal(map[string]any{
		"Targets":                     []string{m.ODataID},
		"@Redfish.OperationApplyTime": "Immediate",
		"Oem": map[string]any{"Supermicro": map[string]any{"BMC": map[string]any{
			"PreserveCfg": true, "PreserveSdr": true, "PreserveSsl": true, "BackupBMC": true,
		}}},
	})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ph := textproto.MIMEHeader{}
	ph.Set("Content-Disposition", `form-data; name="UpdateParameters"`)
	ph.Set("Content-Type", "application/json")
	pw, err := mw.CreatePart(ph)
	if err != nil {
		return err
	}
	if _, err := pw.Write(params); err != nil {
		return err
	}
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", `form-data; name="UpdateFile"; filename="firmware.bin"`)
	fh.Set("Content-Type", "application/octet-stream")
	fw, err := mw.CreatePart(fh)
	if err != nil {
		return err
	}
	if _, err := fw.Write(image); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	resp, err := c.api.RunRawRequestWithHeaders(http.MethodPost, us.MultipartHTTPPushURI,
		bytes.NewReader(buf.Bytes()), mw.FormDataContentType(), nil)
	if err != nil {
		return fmt.Errorf("bmc: firmware upload: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

func (c *client) ResetManager(ctx context.Context, action bmc.PowerAction) error {
	if c.readOnly {
		return errReadOnly
	}
	m, err := c.manager()
	if err != nil {
		return err
	}
	if _, err := m.Reset(schemas.ResetType(action)); err != nil {
		return fmt.Errorf("bmc: manager reset %s: %w", action, err)
	}
	return nil
}

func (c *client) Bios(ctx context.Context, systemID string) (bmc.BiosState, error) {
	b, err := c.biosOf(systemID)
	if err != nil {
		return bmc.BiosState{}, err
	}
	out := bmc.BiosState{Attributes: map[string]any(b.Attributes), Registry: b.AttributeRegistry}
	// The pending set lives in the @Redfish.Settings object, which gofish keeps
	// private — one raw GET, absent-Attributes means nothing staged.
	if uri := c.settingsObject(b.ODataID); uri != "" && uri != b.ODataID {
		resp, err := c.api.Get(uri)
		if err != nil {
			return bmc.BiosState{}, fmt.Errorf("bmc: bios settings: %w", err)
		}
		defer resp.Body.Close()
		var doc struct {
			Attributes map[string]any `json:"Attributes"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			return bmc.BiosState{}, fmt.Errorf("bmc: bios settings: %w", err)
		}
		if len(doc.Attributes) > 0 {
			out.Pending = doc.Attributes
		}
	}
	return out, nil
}

func (c *client) SetBiosAttributes(ctx context.Context, systemID string, attrs map[string]any) error {
	if c.readOnly {
		return errReadOnly
	}
	b, err := c.biosOf(systemID)
	if err != nil {
		return err
	}
	uri := c.settingsObject(b.ODataID)
	if uri == "" {
		uri = b.ODataID
	}
	// A direct PATCH, not gofish's UpdateBiosAttributes: that helper drops
	// attributes already at the requested value, and staging must be literal.
	resp, err := c.api.Patch(uri, map[string]any{"Attributes": attrs})
	if err != nil {
		return fmt.Errorf("bmc: bios attributes: %w", err)
	}
	defer resp.Body.Close()
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
// resource advertises. Exact matching is a real-hardware bug: the 2026-07-21
// captures show the X12 advertising BiosAttributeRegistry.v1_0_0 against a bare
// member id BiosAttributeRegistry (Registry field 1.0.0-dotted), and the iBMC
// v7_6_2 against 7.6.2 with the Registry field truncated to 7.6. So: normalise
// the version notation, then accept equality or a dot-boundary prefix in either
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
	return b, nil
}

// settingsObject reads @Redfish.Settings.SettingsObject off the raw Bios
// resource — gofish folds it into a private field.
func (c *client) settingsObject(biosURI string) string {
	resp, err := c.api.Get(biosURI)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var doc struct {
		Settings struct {
			SettingsObject struct {
				ODataID string `json:"@odata.id"`
			} `json:"SettingsObject"`
		} `json:"@Redfish.Settings"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return ""
	}
	return doc.Settings.SettingsObject.ODataID
}

// EnsureAccount creates or resets the named manager account over the standard
// AccountService: PATCH the member when the username exists, POST to the
// collection when it doesn't. Raw writes, not gofish's typed helpers — the
// payload must be exactly {Password, Enabled} / {UserName, Password, RoleId},
// nothing a schema round-trip might add.
func (c *client) EnsureAccount(ctx context.Context, username, password string) error {
	if c.readOnly {
		return errReadOnly
	}
	as, err := c.svc.AccountService()
	if err != nil {
		return fmt.Errorf("bmc: account service: %w", err)
	}
	if as == nil || as.ODataID == "" {
		// gofish answers a nil service without error when the root has no link.
		return fmt.Errorf("bmc: endpoint exposes no account service")
	}
	accounts, err := as.Accounts()
	if err != nil {
		return fmt.Errorf("bmc: accounts: %w", err)
	}
	for _, a := range accounts {
		if a.UserName != username {
			continue
		}
		resp, err := c.api.Patch(a.ODataID, map[string]any{"Password": password, "Enabled": true})
		if err != nil {
			return fmt.Errorf("bmc: account %s: %w", username, err)
		}
		resp.Body.Close()
		return nil
	}
	uri, err := c.accountsCollection(as.ODataID)
	if err != nil {
		return err
	}
	resp, err := c.api.Post(uri, map[string]any{
		"UserName": username, "Password": password, "RoleId": "Administrator", "Enabled": true,
	})
	if err != nil {
		return fmt.Errorf("bmc: create account %s: %w", username, err)
	}
	resp.Body.Close()
	return nil
}

// accountsCollection reads the Accounts link off the raw AccountService —
// gofish keeps the collection URI private.
func (c *client) accountsCollection(uri string) (string, error) {
	resp, err := c.api.Get(uri)
	if err != nil {
		return "", fmt.Errorf("bmc: account service: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Accounts struct {
			ODataID string `json:"@odata.id"`
		} `json:"Accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || doc.Accounts.ODataID == "" {
		return "", fmt.Errorf("bmc: account service carries no Accounts collection")
	}
	return doc.Accounts.ODataID, nil
}

// smcSSLCertURI is the X12's OEM certificate resource, verbatim from the
// capture — a fixed OEM path, not schema-discoverable.
const smcSSLCertURI = "/redfish/v1/UpdateService/Oem/Supermicro/SSLCert"

// identityDialect probes the certificate-replacement surface: the iBMC's
// SecurityService HttpsCert (linked under Manager Oem) wins as csr-native —
// the key never leaves the manager; the X12's OEM SmcSSLCert.Upload is
// key-upload; neither is none.
func (c *client) identityDialect() bmc.IdentityDialect {
	if uri, _ := c.httpsCertURI(); uri != "" {
		return bmc.DialectCSRNative
	}
	if c.hasSmcSSLCert() {
		return bmc.DialectKeyUpload
	}
	return bmc.DialectNoIdentity
}

// httpsCertURI resolves Manager Oem.<vendor>.SecurityService → Links.HttpsCert
// (the captured iBMC chain).
func (c *client) httpsCertURI() (string, error) {
	m, err := c.manager()
	if err != nil {
		return "", err
	}
	resp, err := c.api.Get(m.ODataID)
	if err != nil {
		return "", fmt.Errorf("bmc: manager: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		Oem map[string]struct {
			SecurityService struct {
				ODataID string `json:"@odata.id"`
			} `json:"SecurityService"`
		} `json:"Oem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("bmc: manager: %w", err)
	}
	for _, oem := range doc.Oem {
		if oem.SecurityService.ODataID == "" {
			continue
		}
		sresp, err := c.api.Get(oem.SecurityService.ODataID)
		if err != nil {
			return "", nil // advertised but unreadable — treat as absent
		}
		var sdoc struct {
			Links struct {
				HttpsCert struct { //nolint:revive // the Redfish property name, verbatim
					ODataID string `json:"@odata.id"`
				} `json:"HttpsCert"`
			} `json:"Links"`
		}
		err = json.NewDecoder(sresp.Body).Decode(&sdoc)
		sresp.Body.Close()
		if err != nil {
			return "", nil
		}
		return sdoc.Links.HttpsCert.ODataID, nil
	}
	return "", nil
}

func (c *client) hasSmcSSLCert() bool {
	resp, err := c.api.Get(smcSSLCertURI)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var doc struct {
		Actions map[string]json.RawMessage `json:"Actions"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return false
	}
	_, ok := doc.Actions["#SmcSSLCert.Upload"]
	return ok
}

// GenerateCSR drives the iBMC's on-manager CSR generation, the PoC-proven
// parameter set: Country and CommonName required, SubjectAltName in openssl
// syntax (real on 6.72, undocumented). The pending CSR then appears in
// HttpsCert.CertificateSigningRequest — polled, the generation is slow.
func (c *client) GenerateCSR(ctx context.Context, subject bmc.CSRSubject) ([]byte, error) {
	if c.readOnly {
		return nil, errReadOnly
	}
	uri, err := c.httpsCertURI()
	if err != nil {
		return nil, err
	}
	if uri == "" {
		return nil, fmt.Errorf("bmc: endpoint exposes no on-manager CSR generation")
	}
	payload := map[string]any{
		"Country":    subject.Country,
		"CommonName": subject.CommonName,
	}
	for k, v := range map[string]string{
		"State":   subject.State,
		"City":    subject.Locality,
		"OrgName": subject.Organization,
		"OrgUnit": subject.OrganizationalUnit,
		"SubjectAltName": func() string {
			var parts []string
			for _, d := range subject.DNSNames {
				parts = append(parts, "DNS:"+d)
			}
			for _, ip := range subject.IPAddresses {
				parts = append(parts, "IP:"+ip)
			}
			return strings.Join(parts, ",")
		}(),
	} {
		if v != "" {
			payload[k] = v
		}
	}
	resp, err := c.api.Post(uri+"/Actions/HttpsCert.GenerateCSR", payload)
	if err != nil {
		return nil, fmt.Errorf("bmc: GenerateCSR: %w", err)
	}
	resp.Body.Close()

	for {
		gresp, err := c.api.Get(uri)
		if err != nil {
			return nil, fmt.Errorf("bmc: HttpsCert: %w", err)
		}
		var doc struct {
			CertificateSigningRequest string
		}
		err = json.NewDecoder(gresp.Body).Decode(&doc)
		gresp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bmc: HttpsCert: %w", err)
		}
		if strings.Contains(doc.CertificateSigningRequest, "BEGIN CERTIFICATE REQUEST") {
			return []byte(doc.CertificateSigningRequest), nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("bmc: CSR never appeared: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// ImportServerCertificate hands the signed chain back as PEM text — the
// parameter is named Certificate (not Content — the PoC's 400), Type text.
func (c *client) ImportServerCertificate(ctx context.Context, chainPEM []byte) error {
	if c.readOnly {
		return errReadOnly
	}
	uri, err := c.httpsCertURI()
	if err != nil {
		return err
	}
	if uri == "" {
		return fmt.Errorf("bmc: endpoint exposes no certificate import")
	}
	resp, err := c.api.Post(uri+"/Actions/HttpsCert.ImportServerCertificate",
		map[string]any{"Type": "text", "Certificate": string(chainPEM)})
	if err != nil {
		return fmt.Errorf("bmc: ImportServerCertificate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		if taskURI := resp.Header.Get("Location"); taskURI != "" {
			return c.pollTask(ctx, taskURI)
		}
	}
	return nil
}

// UploadServerIdentity is the X12 OEM route: one multipart POST, parts
// cert_file and key_file exactly as the action advertises, leaf-only PEM.
func (c *client) UploadServerIdentity(ctx context.Context, certPEM, keyPEM []byte) error {
	if c.readOnly {
		return errReadOnly
	}
	if !c.hasSmcSSLCert() {
		return fmt.Errorf("bmc: endpoint exposes no identity upload")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, part := range []struct {
		name, filename string
		data           []byte
	}{
		{"cert_file", "cert.pem", certPEM},
		{"key_file", "key.pem", keyPEM},
	} {
		fw, err := mw.CreateFormFile(part.name, part.filename)
		if err != nil {
			return err
		}
		if _, err := fw.Write(part.data); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	resp, err := c.api.RunRawRequestWithHeaders(http.MethodPost,
		smcSSLCertURI+"/Actions/SmcSSLCert.Upload",
		bytes.NewReader(buf.Bytes()), mw.FormDataContentType(), nil)
	if err != nil {
		return fmt.Errorf("bmc: SmcSSLCert.Upload: %w", err)
	}
	resp.Body.Close()
	return nil
}

// ntpSurface is the discovered time-source dialect: the OEM NtpService link
// (iBMC — its NetworkProtocol serves no NTP key at all) or the standard
// NetworkProtocol NTP block (X12). Probed per call, one to two GETs.
type ntpSurface struct {
	oemURI      string
	standardURI string
}

func (c *client) discoverNTP() (ntpSurface, error) {
	m, err := c.manager()
	if err != nil {
		return ntpSurface{}, err
	}
	resp, err := c.api.Get(m.ODataID)
	if err != nil {
		return ntpSurface{}, fmt.Errorf("bmc: manager: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		NetworkProtocol struct {
			ODataID string `json:"@odata.id"`
		} `json:"NetworkProtocol"`
		Oem map[string]struct {
			NtpService struct {
				ODataID string `json:"@odata.id"`
			} `json:"NtpService"`
		} `json:"Oem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return ntpSurface{}, fmt.Errorf("bmc: manager: %w", err)
	}
	for _, oem := range doc.Oem {
		if oem.NtpService.ODataID != "" {
			return ntpSurface{oemURI: oem.NtpService.ODataID}, nil
		}
	}
	if doc.NetworkProtocol.ODataID != "" {
		return ntpSurface{standardURI: doc.NetworkProtocol.ODataID}, nil
	}
	return ntpSurface{}, fmt.Errorf("bmc: endpoint exposes no NTP surface")
}

func (c *client) NTP(ctx context.Context) (bmc.NTPConfig, error) {
	surf, err := c.discoverNTP()
	if err != nil {
		return bmc.NTPConfig{}, err
	}
	if surf.oemURI != "" {
		resp, err := c.api.Get(surf.oemURI)
		if err != nil {
			return bmc.NTPConfig{}, fmt.Errorf("bmc: ntp service: %w", err)
		}
		defer resp.Body.Close()
		var doc struct {
			ServiceEnabled     bool
			PreferredNtpServer string
			AlternateNtpServer string
			ExtraNtpServer     string
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			return bmc.NTPConfig{}, fmt.Errorf("bmc: ntp service: %w", err)
		}
		out := bmc.NTPConfig{Enabled: doc.ServiceEnabled}
		for _, s := range []string{doc.PreferredNtpServer, doc.AlternateNtpServer, doc.ExtraNtpServer} {
			if s != "" {
				out.Servers = append(out.Servers, s)
			}
		}
		return out, nil
	}
	resp, err := c.api.Get(surf.standardURI)
	if err != nil {
		return bmc.NTPConfig{}, fmt.Errorf("bmc: network protocol: %w", err)
	}
	defer resp.Body.Close()
	var doc struct {
		NTP *struct {
			ProtocolEnabled bool
			NTPServers      []string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return bmc.NTPConfig{}, fmt.Errorf("bmc: network protocol: %w", err)
	}
	if doc.NTP == nil {
		return bmc.NTPConfig{}, fmt.Errorf("bmc: endpoint exposes no NTP surface")
	}
	out := bmc.NTPConfig{Enabled: doc.NTP.ProtocolEnabled}
	for _, s := range doc.NTP.NTPServers {
		if s != "" {
			out.Servers = append(out.Servers, s)
		}
	}
	return out, nil
}

func (c *client) SetNTP(ctx context.Context, cfg bmc.NTPConfig) error {
	if c.readOnly {
		return errReadOnly
	}
	surf, err := c.discoverNTP()
	if err != nil {
		return err
	}
	if surf.oemURI != "" {
		if len(cfg.Servers) > 3 {
			return fmt.Errorf("bmc: at most 3 NTP servers on this endpoint")
		}
		slot := func(i int) string {
			if i < len(cfg.Servers) {
				return cfg.Servers[i]
			}
			return "" // clears the slot
		}
		resp, err := c.api.Patch(surf.oemURI, map[string]any{
			"ServiceEnabled":     cfg.Enabled,
			"PreferredNtpServer": slot(0),
			"AlternateNtpServer": slot(1),
			"ExtraNtpServer":     slot(2),
		})
		if err != nil {
			return fmt.Errorf("bmc: ntp service: %w", err)
		}
		resp.Body.Close()
		return nil
	}
	resp, err := c.api.Patch(surf.standardURI, map[string]any{
		"NTP": map[string]any{"ProtocolEnabled": cfg.Enabled, "NTPServers": cfg.Servers},
	})
	if err != nil {
		return fmt.Errorf("bmc: network protocol: %w", err)
	}
	resp.Body.Close()
	return nil
}

// vmmControl drives the Huawei OEM action: POST, expect 202 + a task URI, poll the task
// monitor to completion (the capability vmm-boot.sh exercises with curl).
func (c *client) vmmControl(ctx context.Context, target, controlType, image string) error {
	if target == "" {
		return fmt.Errorf("bmc: VmmControl advertised but no target")
	}
	payload := map[string]string{"VmmControlType": controlType}
	if image != "" {
		payload["Image"] = image
	}
	resp, err := c.api.Post(target, payload)
	if err != nil {
		return fmt.Errorf("bmc: VmmControl %s: %w", controlType, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return nil // some firmwares answer synchronously
	}
	taskURI := resp.Header.Get("Location")
	if taskURI == "" {
		return nil
	}
	return c.pollTask(ctx, taskURI)
}

func (c *client) pollTask(ctx context.Context, uri string) error {
	for {
		resp, err := c.api.Get(uri)
		if err != nil {
			return fmt.Errorf("bmc: task poll: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var task struct{ TaskState string }
		_ = json.Unmarshal(body, &task)
		switch task.TaskState {
		case "Completed":
			return nil
		case "Exception", "Killed", "Cancelled":
			return fmt.Errorf("bmc: task %s: %s", uri, task.TaskState)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// parseResetTime reads the LastResetTime gofish leaves as a string. RFC3339 with
// optional fractional seconds is what both the X12 and the sim serve; anything
// else (or absence — the iBMC) is a zero time, i.e. observable not offered.
func parseResetTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func hasVmmControl(actions []string) bool {
	for _, a := range actions {
		if a == vmmControlAction {
			return true
		}
	}
	return false
}

// oemVendor reads the vendor from the Oem key of a resource — the sim and iBMC both
// stamp it there; empty when the endpoint doesn't.
func oemVendor(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for k := range m {
		return k
	}
	return ""
}

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefU(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}

func derefI(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// managerHostname reads HostName from the raw Manager resource — not in gofish's schema
// (iBMC serves it, stamped with the chassis serial; others omit it).
func (c *client) managerHostname(uri string) string {
	resp, err := c.api.Get(uri)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var doc struct{ HostName string }
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return ""
	}
	return doc.HostName
}

func adapterPortMACs(a *schemas.NetworkAdapter) []string {
	// The sim carries them in Oem.PortMACs; real trees use NetworkPorts (the PoC
	// captures settle the exact shape per vendor). Best effort, empty is fine.
	var oem struct {
		PortMACs []string
	}
	if len(a.OEM) > 0 && json.Unmarshal(a.OEM, &oem) == nil {
		return oem.PortMACs
	}
	return nil
}
