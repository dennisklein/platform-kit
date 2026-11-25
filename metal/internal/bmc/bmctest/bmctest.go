// Package bmctest is the ONE conformance suite every bmc.Client implementation must pass —
// the in-memory sim, the gofish client against the sim's HTTP tree, and the same
// client against real hardware, read-only. Because sim and hardware answer the
// same suite, the sim's fidelity is itself under test.
package bmctest

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/testpki"
)

// Run executes the v0 conformance assertions against c, with expectations derived from
// the fixture the endpoint is supposed to embody: inventory, capability discovery, the
// serial-anchor asymmetry, power round-trip, and media insert dialect dispatch.
// ReadOnly skips every mutating assertion — the mode used against real hardware.
func Run(t *testing.T, c bmc.Client, f *sim.Fixture, readOnly bool) {
	t.Helper()
	ctx := context.Background()

	t.Run("manager", func(t *testing.T) {
		m, err := c.Manager(ctx)
		require.NoError(t, err)
		assert.Equal(t, f.Manager.Firmware, m.FirmwareVersion, "firmware")
		// The vendor has three homes — ServiceRoot.Vendor, the Manager Oem key,
		// the Manager Manufacturer — and every endpoint uses one of them.
		assert.Equal(t, f.Vendor, m.Vendor, "vendor")
		assert.Equal(t, f.Manager.MAC, m.MAC, "manager MAC")
		assert.Equal(t, f.Manager.Hostname, m.Hostname, "manager hostname")
	})

	t.Run("adapters", func(t *testing.T) {
		// Port MACs come from NetworkDeviceFunctions where the endpoint serves
		// them and from the deprecated NetworkPorts where it does not; either
		// way they are the same facts.
		ch, err := c.Chassis(ctx)
		require.NoError(t, err)
		require.Len(t, ch.Adapters, len(f.Chassis.Adapters))
		for i, want := range f.Chassis.Adapters {
			assert.Equal(t, want.Model, ch.Adapters[i].Model, "adapter %d model", i)
			assertList(t, want.PortMACs, ch.Adapters[i].PortMACs, "adapter %d port MACs", i)
		}
	})

	t.Run("serial-anchors", func(t *testing.T) {
		ch, err := c.Chassis(ctx)
		require.NoError(t, err)
		assert.Equal(t, f.Chassis.BoardSerial, ch.BoardSerial, "chassis serial")
		for i, sys := range mustSystems(t, ctx, c) {
			// Covers BOTH vendor shapes: Huawei's ComputerSystem carries the chassis
			// serial; Supermicro's carries none (fixture serial == "").
			assert.Equal(t, f.Systems[i].Serial, sys.SerialNumber, "system %s serial", sys.ID)
		}
	})

	t.Run("inventory", func(t *testing.T) {
		systems := mustSystems(t, ctx, c)
		require.Len(t, systems, len(f.Systems))
		for i, sys := range systems {
			fx := f.Systems[i]
			assert.Equal(t, fx.UUID, sys.UUID, "system %s uuid", sys.ID)
			assert.Len(t, sys.NICs, len(fx.NICs), "system %s nics", sys.ID)
			assert.Len(t, sys.Drives, len(fx.Drives), "system %s drives", sys.ID)
			for j, nic := range fx.NICs {
				if j < len(sys.NICs) {
					assert.Equal(t, nic.MAC, sys.NICs[j].MAC, "system %s nic %s", sys.ID, nic.ID)
				}
			}
			if fx.BootProgress {
				assert.NotEmpty(t, sys.BootProgress, "system %s serves boot progress", sys.ID)
			} else {
				assert.Empty(t, sys.BootProgress, "system %s serves no boot progress", sys.ID)
			}
		}
	})

	t.Run("capability-discovery", func(t *testing.T) {
		caps, err := c.Capabilities(ctx)
		require.NoError(t, err)
		for i, vm := range f.VirtualMedia {
			assert.Equal(t, fixtureDialect(f, i), caps.MediaDialect[vm.ID], "media %q dialect", vm.ID)
		}
		assert.Equal(t, fixtureUpdateDialect(f), caps.UpdateDialect, "update dialect")
	})

	t.Run("firmware-inventory", func(t *testing.T) {
		inv, err := c.FirmwareInventory(ctx)
		require.NoError(t, err)
		byID := map[string]bmc.Firmware{}
		for _, e := range inv {
			byID[e.ID] = e
		}
		require.Len(t, byID, len(f.Update.Inventory))
		for _, e := range f.Update.Inventory {
			want := e.Version
			if e.BMC || e.Staging {
				// The BMC entry tracks the live manager firmware; the staging
				// slot mirrors it while nothing is staged.
				want = f.Manager.Firmware
			}
			got, ok := byID[e.ID]
			require.True(t, ok, "inventory entry %q missing", e.ID)
			assert.Equal(t, want, got.Version, "entry %q version", e.ID)
			assert.Equal(t, e.Updateable, got.Updateable, "entry %q updateable", e.ID)
		}
	})

	t.Run("bios-read", func(t *testing.T) {
		if f.Bios.Attributes == nil {
			t.Skip("fixture serves no BIOS")
		}
		state, err := c.Bios(ctx, f.Systems[0].ID)
		require.NoError(t, err)
		assert.Equal(t, jsonNorm(t, f.Bios.Attributes), state.Attributes, "current attributes")
		assert.Empty(t, state.Pending, "nothing staged on a fresh endpoint")
		assert.Equal(t, f.Bios.Registry, state.Registry, "the advertised registry identity, verbatim")
	})

	t.Run("bios-schema", func(t *testing.T) {
		if f.Bios.Attributes == nil {
			t.Skip("fixture serves no BIOS")
		}
		schema, err := c.BiosSchema(ctx, f.Systems[0].ID)
		require.NoError(t, err)
		if len(f.Bios.Schema) == 0 {
			assert.Empty(t, schema, "no registry served — absence must read as absence")
			return
		}
		require.Len(t, schema, len(f.Bios.Schema))
		for i, want := range f.Bios.Schema {
			assert.Equal(t, want.Name, schema[i].Name)
			assert.Equal(t, want.Type, schema[i].Type)
			assert.Equal(t, want.AllowedValues, schema[i].AllowedValues, "attribute %q values", want.Name)
		}
	})

	if readOnly {
		return
	}

	t.Run("power-round-trip", func(t *testing.T) {
		id := f.Systems[0].ID
		_, err := c.Power(ctx, id)
		require.NoError(t, err)

		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceOff))
		settles(t, ctx, c, id, bmc.PowerOff, "after ForceOff")

		require.NoError(t, c.SetPower(ctx, id, bmc.ActionOn))
		settles(t, ctx, c, id, bmc.PowerOn, "after On")

		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceRestart))
	})

	t.Run("reset-advances-reset-time", func(t *testing.T) {
		// The baseline-and-advance observable — only where the vendor serves it
		// (X12 yes, iBMC no; the fixture decides, mirroring the hardware captures).
		id := f.Systems[0].ID
		systems, err := c.Systems(ctx)
		require.NoError(t, err)
		require.NotEmpty(t, systems)
		base := systems[0].LastResetTime
		if base.IsZero() {
			t.Skip("fixture serves no LastResetTime")
		}
		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceRestart))
		systems, err = c.Systems(ctx)
		require.NoError(t, err)
		assert.True(t, systems[0].LastResetTime.After(base),
			"LastResetTime must advance past %s on reset, got %s", base, systems[0].LastResetTime)
	})

	t.Run("media-slot-choice", func(t *testing.T) {
		// A boot image goes into an optical slot wherever one can take it —
		// chosen by MediaTypes, never by position: a standard tree is free to
		// list a removable-disk slot first.
		media, err := c.Media(ctx)
		require.NoError(t, err)
		require.Len(t, media, len(f.VirtualMedia))
		for i, vm := range f.VirtualMedia {
			assert.Equal(t, vm.ID, media[i].ID, "slots come in the endpoint's order")
		}
		caps, err := c.Capabilities(ctx)
		require.NoError(t, err)
		slots := bmc.InsertableSlots(media, caps)
		want := insertableSlot(f)
		if want == "" {
			assert.Empty(t, slots, "no insertable slot advertised")
			return
		}
		require.NotEmpty(t, slots)
		assert.Equal(t, want, slots[0], "the first choice")
	})

	t.Run("media-dialect-dispatch", func(t *testing.T) {
		slot := insertableSlot(f)
		if slot == "" {
			t.Skip("fixture advertises no insertable slot")
		}
		// The BMC really fetches at mount time now, so the image must exist: a
		// TLS server demanding the media credentials, ranges supported — the
		// asset server's shape. (Write-mode against real hardware would need a
		// URL served on a BMC-reachable interface, not this loopback listener.)
		creds := &bmc.MediaCredentials{Username: "media", Password: "grant-secret"}
		asset := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || user != creds.Username || pass != creds.Password {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			http.ServeContent(w, r, "talos.iso", time.Time{}, bytes.NewReader(bytes.Repeat([]byte{0xCD}, 4096)))
		}))
		defer asset.Close()
		img := asset.URL + "/talos.iso"
		require.NoError(t, c.InsertMedia(ctx, slot, img, creds))

		media, err := c.Media(ctx)
		require.NoError(t, err)
		var seen bool
		for _, m := range media {
			if m.ID == slot {
				seen = true
				assert.True(t, m.Inserted, "slot %s inserted", slot)
				// The vmm dialect stores the URL userinfo-bearing, the standard
				// dialect clean — compare with userinfo stripped.
				assert.Equal(t, img, stripUserinfo(t, m.ImageURL), "slot %s image", slot)
			}
		}
		require.True(t, seen, "slot %s missing from Media()", slot)
		require.NoError(t, c.EjectMedia(ctx, slot))

		wrong := &bmc.MediaCredentials{Username: "media", Password: "not-the-grant"}
		require.Error(t, c.InsertMedia(ctx, slot, img, wrong),
			"a mount the asset server refuses must fail the insert")
	})

	t.Run("boot-override-allowable-values", func(t *testing.T) {
		id := f.Systems[0].ID
		assert.Error(t, c.SetBootOverride(ctx, id, bmc.BootOverride{Target: "NotARealTarget"}),
			"a target outside AllowableValues must be rejected")
		targets := f.Systems[0].BootTargets
		if len(targets) < 2 {
			return
		}
		// The override is state the endpoint keeps and serves back. An unnamed
		// enablement is a one-time override, an unnamed mode stays what it was
		// (the X12 comes up Legacy).
		servedMode := mustSystems(t, ctx, c)[0].BootOverride.Mode
		assert.Equal(t, f.Systems[0].BootMode, servedMode, "the served boot mode")
		require.NoError(t, c.SetBootOverride(ctx, id, bmc.BootOverride{Target: targets[1]}))
		assert.Equal(t, bmc.BootOverride{Target: targets[1], Enabled: bmc.OverrideOnce, Mode: servedMode},
			mustSystems(t, ctx, c)[0].BootOverride, "the override reads back, the mode untouched")

		want := bmc.BootOverride{Target: targets[1], Enabled: bmc.OverrideOnce}
		if servedMode != "" {
			want.Mode = "UEFI"
		}
		require.NoError(t, c.SetBootOverride(ctx, id, want))
		assert.Equal(t, want, mustSystems(t, ctx, c)[0].BootOverride, "the override reads back as set")

		// A one-time override covers exactly one boot: the enablement falls
		// back to Disabled, target and mode stay.
		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceRestart))
		want.Enabled = bmc.OverrideDisabled
		assert.Equal(t, want, mustSystems(t, ctx, c)[0].BootOverride, "used up by the boot")

		want.Enabled = bmc.OverrideContinuous
		require.NoError(t, c.SetBootOverride(ctx, id, want))
		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceRestart))
		assert.Equal(t, want, mustSystems(t, ctx, c)[0].BootOverride, "a continuous override survives the boot")

		want.Enabled = bmc.OverrideDisabled
		require.NoError(t, c.SetBootOverride(ctx, id, want))
		assert.Equal(t, want, mustSystems(t, ctx, c)[0].BootOverride, "and is withdrawn by disabling it")
	})

	t.Run("power-actions", func(t *testing.T) {
		// Every reset type the system advertises is accepted and lands in the
		// power state it stands for; one it does not advertise is refused.
		id := f.Systems[0].ID
		ends := map[bmc.PowerAction]bmc.PowerState{
			bmc.ActionOn: bmc.PowerOn, bmc.ActionForceOff: bmc.PowerOff,
			bmc.ActionGracefulShutdown: bmc.PowerOff, bmc.ActionForceRestart: bmc.PowerOn,
			bmc.ActionGracefulRestart: bmc.PowerOn, bmc.ActionPowerCycle: bmc.PowerOn,
		}
		for _, action := range fixtureResetTypes(f) {
			want, known := ends[bmc.PowerAction(action)]
			if !known {
				continue
			}
			// Restart-shaped actions need a running system to act on.
			require.NoError(t, c.SetPower(ctx, id, bmc.ActionOn))
			settles(t, ctx, c, id, bmc.PowerOn, "before %s", action)
			require.NoError(t, c.SetPower(ctx, id, bmc.PowerAction(action)), "reset type %s", action)
			settles(t, ctx, c, id, want, "after %s", action)
		}
		assert.Error(t, c.SetPower(ctx, id, bmc.PowerAction("Nmi-NotOffered")),
			"a reset type outside AllowableValues must be rejected")
		require.NoError(t, c.SetPower(ctx, id, bmc.ActionOn))
	})

	t.Run("flash-convergence", func(t *testing.T) {
		image := []byte("mock firmware image SIMFW 99.99.99 trailer")
		if fixtureUpdateDialect(f) != bmc.DialectMultipartPush {
			assert.Error(t, c.UpdateFirmware(ctx, image),
				"flash without an advertised push URI must be refused")
			return
		}
		assert.Error(t, c.UpdateFirmware(ctx, []byte("garbage, no marker")),
			"an unverifiable image must be rejected at upload")

		base, err := c.Manager(ctx)
		require.NoError(t, err)
		require.NoError(t, c.UpdateFirmware(ctx, image))

		m, err := c.Manager(ctx)
		require.NoError(t, err)
		assert.Equal(t, "99.99.99", m.FirmwareVersion, "version must read back after the flash")
		if !base.LastResetTime.IsZero() {
			assert.True(t, m.LastResetTime.After(base.LastResetTime),
				"a genuine flash reboots the BMC — LastResetTime must advance past %s, got %s",
				base.LastResetTime, m.LastResetTime)
		}
		inv, err := c.FirmwareInventory(ctx)
		require.NoError(t, err)
		for _, e := range f.Update.Inventory {
			if !e.BMC && !e.Staging {
				continue
			}
			for _, got := range inv {
				if got.ID == e.ID {
					assert.Equal(t, "99.99.99", got.Version, "entry %q after the flash", e.ID)
				}
			}
		}
	})

	t.Run("stage-system-firmware", func(t *testing.T) {
		entry := ""
		for _, e := range f.Update.Inventory {
			if !e.BMC && !e.Staging && e.Updateable && entry == "" {
				entry = e.ID
			}
		}
		image := []byte("mock system image SIMFW 77.7.7 trailer")
		if fixtureUpdateDialect(f) != bmc.DialectMultipartPush || !f.Update.OnReset || entry == "" {
			if entry != "" {
				assert.Error(t, c.StageFirmware(ctx, image, entry),
					"staging without OnReset among the supported apply times must be refused")
			}
			return
		}
		version := func() string {
			inv, err := c.FirmwareInventory(ctx)
			require.NoError(t, err)
			for _, e := range inv {
				if e.ID == entry {
					return e.Version
				}
			}
			return ""
		}
		before := version()
		require.NoError(t, c.StageFirmware(ctx, image, entry))
		assert.Equal(t, before, version(), "a staged image changes nothing before the reset")
		id := f.Systems[0].ID
		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceOff))
		require.NoError(t, c.SetPower(ctx, id, bmc.ActionOn))
		assert.Equal(t, "77.7.7", version(), "the next boot of the system installs the staged image")
	})

	t.Run("manager-reset", func(t *testing.T) {
		base, err := c.Manager(ctx)
		require.NoError(t, err)
		require.NoError(t, c.ResetManager(ctx, bmc.ActionGracefulRestart))
		m, err := c.Manager(ctx)
		require.NoError(t, err)
		if base.LastResetTime.IsZero() {
			// The iBMC serves no Manager LastResetTime — the reset works, the
			// observable stays withheld (never simulated where hardware is silent).
			assert.True(t, m.LastResetTime.IsZero())
			return
		}
		assert.True(t, m.LastResetTime.After(base.LastResetTime),
			"Manager.Reset must advance LastResetTime past %s, got %s", base.LastResetTime, m.LastResetTime)
	})

	t.Run("bios-pending-applied", func(t *testing.T) {
		if f.Bios.Attributes == nil {
			t.Skip("fixture serves no BIOS")
		}
		id := f.Systems[0].ID
		attr, next := flippableAttribute(t, f)

		require.NoError(t, c.SetBiosAttributes(ctx, id, map[string]any{attr: next}))
		state, err := c.Bios(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, next, state.Pending[attr], "staged value visible in the pending object")
		assert.Equal(t, jsonNorm(t, f.Bios.Attributes)[attr], state.Attributes[attr],
			"current value untouched before the reboot")

		require.NoError(t, c.SetPower(ctx, id, bmc.ActionForceRestart))
		state, err = c.Bios(ctx, id)
		require.NoError(t, err)
		assert.Empty(t, state.Pending, "pending cleared by the reboot")
		assert.Equal(t, next, state.Attributes[attr], "value applied by the reboot")
	})

	t.Run("ntp-round-trip", func(t *testing.T) {
		if !f.NTP.Standard && !f.NTP.OemService {
			_, err := c.NTP(ctx)
			assert.Error(t, err, "a fixture without an NTP surface must read as absence")
			assert.Error(t, c.SetNTP(ctx, bmc.NTPConfig{Enabled: true, Servers: []string{"x"}}))
			return
		}
		cfg, err := c.NTP(ctx)
		require.NoError(t, err)
		assert.Equal(t, f.NTP.Enabled, cfg.Enabled, "seeded enablement")
		assertList(t, f.NTP.Servers, cfg.Servers, "seeded servers")

		want := bmc.NTPConfig{Enabled: true, Servers: []string{"ntp1.metal.internal", "ntp2.metal.internal"}}
		require.NoError(t, c.SetNTP(ctx, want))
		cfg, err = c.NTP(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, cfg, "the write must read back — convergence is judged by observables")

		// Converge back to the seeded state so the suite stays order-free.
		require.NoError(t, c.SetNTP(ctx, bmc.NTPConfig{Enabled: f.NTP.Enabled, Servers: f.NTP.Servers}))
	})

	t.Run("account-ensure", func(t *testing.T) {
		if len(f.Accounts) == 0 {
			// An open fixture serves no AccountService — absence must read as
			// absence on every transport, mirroring bios-schema.
			assert.Error(t, c.EnsureAccount(ctx, "pk-metal", "InitialPass0"),
				"a fixture without accounts models no account service")
			return
		}
		require.NoError(t, c.EnsureAccount(ctx, "pk-metal", "InitialPass0"), "create")
		require.NoError(t, c.EnsureAccount(ctx, "pk-metal", "RotatedPass1"),
			"re-ensure is a password reset, not a conflict")
		require.NoError(t, c.EnsureAccount(ctx, f.Accounts[0].Username, f.Accounts[0].Password),
			"ensuring an existing account converges its password")
	})

	t.Run("account-disable", func(t *testing.T) {
		if len(f.Accounts) == 0 {
			assert.Error(t, c.DisableAccount(ctx, "pk-retired"),
				"a fixture without accounts models no account service")
			return
		}
		require.NoError(t, c.EnsureAccount(ctx, "pk-retired", "RetiredPass2"))
		require.NoError(t, c.DisableAccount(ctx, "pk-retired"), "disable")
		require.NoError(t, c.DisableAccount(ctx, "pk-retired"), "disabling a disabled account is a success")
		require.NoError(t, c.DisableAccount(ctx, "pk-absent"), "an absent account is already off")
		if a, ok := c.(interface{ AccountEnabled(string) bool }); ok {
			assert.False(t, a.AccountEnabled("pk-retired"), "the account is off")
		}
	})

	t.Run("identity-dialect-dispatch", func(t *testing.T) {
		caps, err := c.Capabilities(ctx)
		require.NoError(t, err)
		want := bmc.DialectNoIdentity
		switch {
		case f.Identity.CSRNative, f.Identity.CertificateService:
			want = bmc.DialectCSRNative
		case f.Identity.KeyUpload:
			want = bmc.DialectKeyUpload
		}
		require.Equal(t, want, caps.IdentityDialect, "identity dialect")

		// The whole distinguished name: the standard GenerateCSR action
		// requires every part of it, the iBMC only country and common name.
		subject := bmc.CSRSubject{
			CommonName:         "bmc-test.metal.internal",
			DNSNames:           []string{"bmc-test.metal.internal"},
			IPAddresses:        []string{"127.0.0.1"},
			Country:            "XX",
			State:              "Example State",
			Locality:           "Example City",
			Organization:       "bmctest",
			OrganizationalUnit: "conformance",
		}
		switch caps.IdentityDialect {
		case bmc.DialectNoIdentity:
			_, err := c.GenerateCSR(ctx, subject)
			assert.Error(t, err, "no identity surface must read as absence")
			assert.Error(t, c.UploadServerIdentity(ctx, []byte("x"), []byte("y")))
			return
		case bmc.DialectCSRNative:
			csrPEM, err := c.GenerateCSR(ctx, subject)
			require.NoError(t, err)
			csr := parseCSR(t, csrPEM)
			assert.Equal(t, subject.CommonName, csr.Subject.CommonName)
			assert.Contains(t, csr.DNSNames, subject.CommonName, "the DNS SAN must survive the vendor parameter")
			require.Len(t, csr.IPAddresses, 1, "the IP SAN must survive the vendor parameter")

			assert.Error(t, c.UploadServerIdentity(ctx, []byte("x"), []byte("y")),
				"the key-upload route is not this vendor's dialect")
			// A second request is a new key: the client must hand back the
			// new request, not the one the manager served before.
			againPEM, err := c.GenerateCSR(ctx, subject)
			require.NoError(t, err)
			assert.NotEqual(t, string(csrPEM), string(againPEM), "a second GenerateCSR answers with a new request")
			chain, _ := signCSR(t, parseCSR(t, againPEM))
			require.NoError(t, c.ImportServerCertificate(ctx, chain),
				"the signed chain must import against the manager-held key")
		case bmc.DialectKeyUpload:
			_, err := c.GenerateCSR(ctx, subject)
			assert.Error(t, err, "the csr-native route is not this vendor's dialect")
			certPEM, keyPEM := selfIdentity(t, subject)
			require.NoError(t, c.UploadServerIdentity(ctx, certPEM, keyPEM))
		}
	})

	t.Run("bios-unknown-attribute", func(t *testing.T) {
		if f.Bios.Attributes == nil {
			t.Skip("fixture serves no BIOS")
		}
		assert.Error(t, c.SetBiosAttributes(ctx, f.Systems[0].ID, map[string]any{"NotARealAttribute": "x"}),
			"an unknown attribute must be rejected at PATCH time")
	})
}

// RunReadOnly checks a BMC whose facts are not known in advance: real
// hardware, read-only. It asserts the shape of every read, not its values:
// each read answers, the identity anchors are there, every list is stable
// across two reads, and the dialects are ones the client knows. Pass a
// client opened with Config.ReadOnly: nothing here writes, and the rail
// refuses a write that a bug would make.
func RunReadOnly(t *testing.T, c bmc.Client) {
	t.Helper()
	ctx := context.Background()

	t.Run("manager", func(t *testing.T) {
		m, err := c.Manager(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, m.FirmwareVersion, "firmware")
		assert.NotEmpty(t, m.Vendor, "vendor: ServiceRoot.Vendor, the Manager Oem key or the Manufacturer")
	})

	t.Run("chassis-is-stable", func(t *testing.T) {
		// A BMC with several chassis must give the same one on every read:
		// its board serial is an identity anchor.
		first, err := c.Chassis(ctx)
		require.NoError(t, err)
		second, err := c.Chassis(ctx)
		require.NoError(t, err)
		assert.Equal(t, first, second)
	})

	t.Run("systems", func(t *testing.T) {
		ch, err := c.Chassis(ctx)
		require.NoError(t, err)
		first := mustSystems(t, ctx, c)
		require.NotEmpty(t, first, "an endpoint fronts at least one system")
		for _, sys := range first {
			assert.NotEmpty(t, sys.ID)
			assert.True(t, sys.SerialNumber != "" || ch.BoardSerial != "",
				"system %s: a system serial or the chassis board serial anchors the machine", sys.ID)
			assert.Contains(t, []bmc.PowerState{bmc.PowerOn, bmc.PowerOff, bmc.PowerPoweringOn, bmc.PowerPoweringOff},
				sys.PowerState, "system %s power state", sys.ID)
			assert.NotEmpty(t, sys.BootTargets, "system %s boot targets", sys.ID)
		}
		second := mustSystems(t, ctx, c)
		require.Len(t, second, len(first))
		for i := range first {
			assert.Equal(t, first[i].ID, second[i].ID)
			assert.Equal(t, first[i].NICs, second[i].NICs, "system %s NICs", first[i].ID)
			assert.Equal(t, first[i].Drives, second[i].Drives, "system %s drives", first[i].ID)
		}
	})

	t.Run("capabilities", func(t *testing.T) {
		caps, err := c.Capabilities(ctx)
		require.NoError(t, err)
		media, err := c.Media(ctx)
		require.NoError(t, err)
		for _, m := range media {
			assert.Contains(t, []bmc.MediaDialect{bmc.DialectStandard, bmc.DialectVmmControl, bmc.DialectNone},
				caps.MediaDialect[m.ID], "media %s dialect", m.ID)
		}
		assert.Contains(t, []bmc.UpdateDialect{bmc.DialectMultipartPush, bmc.DialectSimpleUpdate, bmc.DialectNoUpdate},
			caps.UpdateDialect)
		assert.Contains(t, []bmc.IdentityDialect{bmc.DialectCSRNative, bmc.DialectKeyUpload, bmc.DialectNoIdentity},
			caps.IdentityDialect)
	})

	t.Run("firmware-inventory", func(t *testing.T) {
		inv, err := c.FirmwareInventory(ctx)
		require.NoError(t, err)
		for _, e := range inv {
			assert.NotEmpty(t, e.ID)
		}
	})

	t.Run("bios-read", func(t *testing.T) {
		systems := mustSystems(t, ctx, c)
		require.NotEmpty(t, systems)
		if _, err := c.Bios(ctx, systems[0].ID); err != nil {
			t.Skipf("no BIOS resource read: %v", err)
		}
		_, err := c.BiosSchema(ctx, systems[0].ID)
		assert.NoError(t, err, "a served BIOS resource has a readable schema, or none")
	})
}

// settles asserts that the system reports want within powerSettle. A real
// BMC reports PoweringOn or PoweringOff for a while after a reset.
func settles(t *testing.T, ctx context.Context, c bmc.Client, id string, want bmc.PowerState, msgAndArgs ...any) {
	t.Helper()
	var last bmc.PowerState
	ok := assert.Eventually(t, func() bool {
		state, err := c.Power(ctx, id)
		last = state
		return err == nil && state == want
	}, powerSettle, 20*time.Millisecond, msgAndArgs...)
	if !ok {
		t.Logf("last power state read: %q, want %q", last, want)
	}
}

// powerSettle bounds the wait for a power state to show.
const powerSettle = 30 * time.Second

// assertList compares two lists where an empty one and an absent one are the
// same fact — which of the two a transport hands back is its own business.
func assertList(t *testing.T, want, got []string, msgAndArgs ...any) {
	t.Helper()
	if len(want) == 0 {
		assert.Empty(t, got, msgAndArgs...)
		return
	}
	assert.Equal(t, want, got, msgAndArgs...)
}

func parseCSR(t *testing.T, csrPEM []byte) *x509.CertificateRequest {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	require.NotNil(t, block, "CSR must be PEM")
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	return csr
}

// signCSR signs a request with a throwaway CA — the cert-manager stand-in.
// Returns the chain PEM (leaf first) and the CA certificate.
func signCSR(t *testing.T, csr *x509.CertificateRequest) ([]byte, *x509.Certificate) {
	t.Helper()
	ca := testpki.NewCA(t, "bmctest-ca")
	chain, err := ca.Sign(csr, 24*time.Hour)
	require.NoError(t, err)
	return chain, ca.Cert
}

// selfIdentity builds a local key and self-signed leaf — the key-upload
// dialect's input shape.
func selfIdentity(t *testing.T, subject bmc.CSRSubject) (certPEM, keyPEM []byte) {
	t.Helper()
	var ips []net.IP
	for _, raw := range subject.IPAddresses {
		ips = append(ips, net.ParseIP(raw))
	}
	return testpki.SelfSigned(t, subject.CommonName, subject.DNSNames, ips)
}

// flippableAttribute picks a schema enumeration with an allowed value different
// from the current one — the pending→applied probe.
func flippableAttribute(t *testing.T, f *sim.Fixture) (name, next string) {
	t.Helper()
	for _, a := range f.Bios.Schema {
		cur, ok := f.Bios.Attributes[a.Name].(string)
		if !ok {
			continue
		}
		for _, v := range a.AllowedValues {
			if v != cur {
				return a.Name, v
			}
		}
	}
	t.Fatal("fixture offers no flippable enumeration attribute")
	return "", ""
}

// jsonNorm pushes a fixture value map through JSON so its types match what a
// client hands back (numbers as float64) regardless of transport.
func jsonNorm(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(m)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

// fixtureUpdateDialect mirrors the update-transport probe (push wins) —
// duplicated from the sim so the suite stays independent of it.
func fixtureUpdateDialect(f *sim.Fixture) bmc.UpdateDialect {
	switch {
	case f.Update.MultipartPush:
		return bmc.DialectMultipartPush
	case f.Update.SimpleUpdate:
		return bmc.DialectSimpleUpdate
	default:
		return bmc.DialectNoUpdate
	}
}

func mustSystems(t *testing.T, ctx context.Context, c bmc.Client) []bmc.System {
	t.Helper()
	systems, err := c.Systems(ctx)
	require.NoError(t, err)
	return systems
}

// fixtureDialect mirrors the dialect probe order of the clients (standard wins, OEM
// VmmControl second) — duplicated from the sim so the suite stays independent of it.
func fixtureDialect(f *sim.Fixture, slot int) bmc.MediaDialect {
	vm := f.VirtualMedia[slot]
	switch {
	case vm.InsertStandard:
		return bmc.DialectStandard
	default:
		for _, a := range vm.OemActions {
			if a == "#VirtualMedia.VmmControl" {
				return bmc.DialectVmmControl
			}
		}
		return bmc.DialectNone
	}
}

func stripUserinfo(t *testing.T, imageURL string) string {
	t.Helper()
	u, err := url.Parse(imageURL)
	require.NoError(t, err)
	u.User = nil
	return u.String()
}

// insertableSlot is the slot a boot image belongs in: the first insertable one
// that offers CD or DVD, the first insertable one of any kind otherwise —
// duplicated from bmc.InsertableSlots so the suite stays independent of it.
func insertableSlot(f *sim.Fixture) string {
	fallback := ""
	for i, vm := range f.VirtualMedia {
		if fixtureDialect(f, i) == bmc.DialectNone {
			continue
		}
		if slices.Contains(vm.MediaTypes, "CD") || slices.Contains(vm.MediaTypes, "DVD") {
			return vm.ID
		}
		if fallback == "" {
			fallback = vm.ID
		}
	}
	return fallback
}

// fixtureResetTypes are the reset types a system advertises: the fixture's
// list, or the set both captured vendors serve when it names none.
func fixtureResetTypes(f *sim.Fixture) []string {
	if types := f.Systems[0].ResetTypes; len(types) > 0 {
		return types
	}
	return []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart"}
}
