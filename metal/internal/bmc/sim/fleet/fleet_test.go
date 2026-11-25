package fleet_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

// labFile is the fleet file in the shape the virtual lab writes it.
const labFile = `
stateDir: .lab/state
mediaCache: .lab/cache
machines:
  - name: node-1
    listen: 10.77.0.1:8441
    personality: generic
    credentials: { username: admin, password: admin-pass }
    identity: { serial: PKLAB0001, uuid: 9b1c0001-0000-4000-8000-000000000001, bmcMAC: "02:77:00:00:01:01", nicMACs: ["52:54:00:77:00:11"] }
    backend: qemu
    qemu: { tap: pktap1, memoryMiB: 4096, cpus: 2, diskGiB: 20, diskSerial: pklab-disk-1 }
  - name: node-2
    listen: 10.77.0.1:8442
`

func TestParse(t *testing.T) {
	cfg, err := fleet.Parse([]byte(labFile))
	require.NoError(t, err)
	assert.Equal(t, ".lab/state", cfg.StateDir)
	assert.Equal(t, ".lab/cache", cfg.MediaCache)
	require.Len(t, cfg.Machines, 2)

	m := cfg.Machines[0]
	assert.Equal(t, "node-1", m.Name)
	assert.Equal(t, "10.77.0.1:8441", m.Listen)
	assert.Equal(t, fleet.Credentials{Username: "admin", Password: "admin-pass"}, m.Credentials)
	assert.Equal(t, fleet.Identity{
		Serial: "PKLAB0001", UUID: "9b1c0001-0000-4000-8000-000000000001",
		BMCMAC: "02:77:00:00:01:01", NICMACs: []string{"52:54:00:77:00:11"},
	}, m.Identity)
	assert.Equal(t, fleet.BackendQemu, m.Backend)
	assert.Equal(t, &fleet.Qemu{Tap: "pktap1", MemoryMiB: 4096, CPUs: 2, DiskGiB: 20, DiskSerial: "pklab-disk-1"}, m.Qemu)

	// Everything but name and address has a default.
	assert.Equal(t, "generic", cfg.Machines[1].Personality)
	assert.Equal(t, fleet.BackendNone, cfg.Machines[1].Backend)
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ file, want string }{
		"nothing to serve": {`machines: []`, "no machines"},
		"a misspelt key": {`
machines:
  - { name: a, listen: "127.0.0.1:1", personalty: generic }`, "personalty"},
		"an unknown backend": {`
machines:
  - { name: a, listen: "127.0.0.1:1", backend: libvirt }`, `backend "libvirt"`},
		"a name that is no hostname": {`
machines:
  - { name: Node_1, listen: "127.0.0.1:1" }`, "lower-case"},
		"an address without a port": {`
machines:
  - { name: a, listen: "127.0.0.1" }`, "host:port"},
		"two machines on one address": {`
machines:
  - { name: a, listen: "127.0.0.1:1" }
  - { name: b, listen: "127.0.0.1:1" }`, "listen address 127.0.0.1:1 is also used by"},
		"two machines with one serial": {`
machines:
  - { name: a, listen: "127.0.0.1:1", identity: { serial: S1 } }
  - { name: b, listen: "127.0.0.1:2", identity: { serial: S1 } }`, "serial S1 is also used by"},
		"a MAC that is none": {`
machines:
  - { name: a, listen: "127.0.0.1:1", identity: { bmcMAC: "02:77" } }`, "not a MAC address"},
		"half a credential": {`
machines:
  - { name: a, listen: "127.0.0.1:1", credentials: { username: admin } }`, "both a username and a password"},
		"qemu without a state directory": {`
machines:
  - { name: a, listen: "127.0.0.1:1", backend: qemu }`, "needs stateDir"},
		"a tap NIC without a MAC": {`
stateDir: /tmp/x
machines:
  - { name: a, listen: "127.0.0.1:1", backend: qemu, qemu: { tap: tap0 } }`, "identity.nicMACs"},
		"qemu settings without the backend": {`
machines:
  - { name: a, listen: "127.0.0.1:1", qemu: { cpus: 2 } }`, "qemu settings on a none backend"},
		"docker without an image": {`
machines:
  - { name: a, listen: "127.0.0.1:1", backend: docker }`, "docker.image"},
	} {
		_, err := fleet.Parse([]byte(tc.file))
		assert.ErrorContains(t, err, tc.want, name)
	}

	// Every problem is reported, not just the first.
	_, err := fleet.Parse([]byte(`
machines:
  - { name: a, listen: nope, backend: libvirt }`))
	assert.ErrorContains(t, err, "host:port")
	assert.ErrorContains(t, err, "libvirt")
}

func TestStart(t *testing.T) {
	ctx := context.Background()
	media := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(media, "boot.iso"), make([]byte, 4096), 0o600))
	cfg, err := fleet.Parse([]byte(`
media: { listen: "127.0.0.1:0", dir: ` + media + ` }
machines:
  - name: node-1
    listen: 127.0.0.1:0
    credentials: { username: lab, password: lab-password-1 }
    identity: { serial: PKLAB0001, uuid: 9b1c0001-0000-4000-8000-000000000001, bmcMAC: "02:77:00:00:01:01", nicMACs: ["52:54:00:77:00:11"] }
  - name: node-2
    listen: 127.0.0.1:0
    identity: { serial: PKLAB0002, uuid: 9b1c0002-0000-4000-8000-000000000002, bmcMAC: "02:77:00:00:02:01", nicMACs: ["52:54:00:77:00:21"] }
  - name: node-3
    listen: 127.0.0.1:0
    personality: supermicro-x12
`))
	require.NoError(t, err)
	f, err := fleet.Start(ctx, cfg, fleet.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close(context.Background()) })

	eps := f.Endpoints()
	require.Len(t, eps, 3)
	assert.Equal(t, "lab", eps[0].Username, "the configured account")
	assert.Equal(t, "admin", eps[1].Username, "the fixture's factory account")
	assert.Empty(t, eps[2].Username, "a fixture without accounts stays open")

	// Each endpoint is its own BMC: its own identity, its own state.
	wantSerial := []string{"PKLAB0001", "PKLAB0002", ""}
	wantMAC := []string{"02:77:00:00:01:01", "02:77:00:00:02:01"}
	var clients []bmc.Client
	for i, ep := range eps {
		c, err := gofishclient.Connect(ctx, gofishclient.Config{
			Endpoint: ep.URL, Username: ep.Username, Password: ep.Password, Insecure: true,
		})
		require.NoError(t, err, ep.Name)
		t.Cleanup(func() { _ = c.Close() })
		clients = append(clients, c)

		systems, err := c.Systems(ctx)
		require.NoError(t, err, ep.Name)
		assert.Equal(t, wantSerial[i], systems[0].SerialNumber, ep.Name)
		if i < len(wantMAC) {
			m, err := c.Manager(ctx)
			require.NoError(t, err)
			assert.Equal(t, wantMAC[i], m.MAC, ep.Name)
			assert.Equal(t, "Generic", m.Vendor)
		}
	}

	// A wrong credential is refused by the machine it is wrong for.
	bad, err := gofishclient.Connect(ctx, gofishclient.Config{
		Endpoint: eps[0].URL, Username: "admin", Password: "admin-pass", Insecure: true,
	})
	if err == nil {
		// A service without sessions takes the credential at the first
		// authenticated read; one with sessions refuses the login itself.
		t.Cleanup(func() { _ = bad.Close() })
		_, err = bad.Systems(ctx)
	}
	assert.Error(t, err, "node-1 no longer has the factory account")

	// The media directory is mountable, and a mount on one machine is not a
	// mount on another.
	require.NotEmpty(t, f.MediaURL())
	require.NoError(t, clients[0].InsertMedia(ctx, "2", f.MediaURL()+"/boot.iso", nil))
	require.NoError(t, clients[0].SetPower(ctx, "1", bmc.ActionOn))
	for i, want := range []bool{true, false} {
		slots, err := clients[i].Media(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, slots[1].Inserted, eps[i].Name)
	}
	p, err := clients[1].Power(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, p, "node-2 was not powered on")
	assert.NotNil(t, eps[0].State.BootImage("2"), "node-1 read its medium at boot")

	// An address in use fails the start as a whole.
	_, err = fleet.Start(ctx, &fleet.Config{Machines: []fleet.Machine{
		{Name: "clash", Listen: eps[0].URL[len("https://"):], Personality: "generic", Backend: fleet.BackendNone},
	}}, fleet.Options{})
	assert.Error(t, err)
}
