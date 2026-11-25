package sim

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// fakeBootMachine is the full qemu-backend shape: media, the boot source and
// boot progress.
type fakeBootMachine struct {
	fakeMediaMachine
	sources  []string
	progress string
}

func (m *fakeBootMachine) SetBootSource(_ context.Context, target string, once bool) error {
	if once {
		target += " once"
	}
	m.sources = append(m.sources, target)
	return nil
}

func (m *fakeBootMachine) BootProgress(context.Context) (string, error) { return m.progress, nil }

func TestBootMachineGetsTheOverride(t *testing.T) {
	ctx := context.Background()
	f, err := LoadFixture("generic")
	require.NoError(t, err)
	s := NewState(f)
	sysID := f.Systems[0].ID
	m := &fakeBootMachine{progress: "OSBootStarted"}
	s.AttachMachine(sysID, m)

	// No override: the machine boots its own order.
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionOn))
	// A one-time override covers the next boot and no later one.
	require.NoError(t, s.SetBootOverride(ctx, sysID, bmc.BootOverride{Target: "Cd"}))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionForceRestart))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionForceRestart))
	// A continuous one covers every boot until it is withdrawn.
	require.NoError(t, s.SetBootOverride(ctx, sysID, bmc.BootOverride{Target: "Hdd", Enabled: bmc.OverrideContinuous}))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionPowerCycle))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionGracefulRestart))
	assert.Equal(t, []string{"", "Cd once", "", "Hdd", "Hdd"}, m.sources)
	assert.Equal(t,
		[]string{"media", "start", "media", "restart", "media", "restart", "media", "stop", "start", "media", "restart"},
		m.verbs, "a power cycle is a stop and a start: the cold boot that picks up a changed device set")

	// Boot progress reads through from the machine while it runs.
	systems, err := s.Systems(ctx)
	require.NoError(t, err)
	assert.Equal(t, "OSBootStarted", systems[0].BootProgress)
	m.progress = "OSRunning"
	systems, err = s.Systems(ctx)
	require.NoError(t, err)
	assert.Equal(t, "OSRunning", systems[0].BootProgress)
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionForceOff))
	systems, err = s.Systems(ctx)
	require.NoError(t, err)
	assert.Equal(t, "None", systems[0].BootProgress, "a machine that is off has not booted")
}

func TestMediaCache(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0x5A}, 64<<10)
	gets := 0
	asset := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
			gets++
		}
		http.ServeContent(w, r, "image.iso", time.Time{}, bytes.NewReader(payload))
	}))
	defer asset.Close()

	dir := t.TempDir()
	cache := NewMediaCache(dir)
	cache.MinSize = 32 << 10
	boot := func(t *testing.T, url string, creds *bmc.MediaCredentials) (*State, *fakeMediaMachine) {
		t.Helper()
		f, err := LoadFixture("generic")
		require.NoError(t, err)
		s := NewState(f)
		s.SetMediaCache(cache)
		m := &fakeMediaMachine{}
		s.AttachMachine("1", m)
		require.NoError(t, s.InsertMedia(ctx, "2", url, creds))
		require.NoError(t, s.SetPower(ctx, "1", bmc.ActionOn))
		require.Len(t, m.media, 1)
		return s, m
	}

	// A large public image lands in the cache and reaches the machine as a path.
	s, m := boot(t, asset.URL+"/installer.iso", nil)
	path := m.media[0].Path
	require.NotEmpty(t, path, "the machine gets the cache file")
	assert.Nil(t, m.media[0].Data)
	assert.Equal(t, path, s.BootImagePath("2"))
	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, payload, onDisk)
	assert.Equal(t, 1, gets)

	// A second machine booting the same image downloads nothing.
	_, m2 := boot(t, asset.URL+"/installer.iso", nil)
	assert.Equal(t, path, m2.media[0].Path)
	assert.Equal(t, 1, gets, "the second boot is served from the cache")

	// A different URL is a different entry.
	_, m3 := boot(t, asset.URL+"/other.iso", nil)
	assert.NotEqual(t, path, m3.media[0].Path)
	assert.Equal(t, 2, gets)

	// The same installer under two grants of the media service is one
	// entry: each mount has a new token, and the cache must not grow by one
	// installer per boot.
	_, g1 := boot(t, asset.URL+"/grants/0123456789abcdef0123456789abcdef/installer.iso", nil)
	_, g2 := boot(t, asset.URL+"/grants/fedcba9876543210fedcba9876543210/installer.iso", nil)
	assert.Equal(t, g1.media[0].Path, g2.media[0].Path)
	assert.Equal(t, 3, gets, "the second grant of the installer is served from the cache")

	// A granted image — one mounted with credentials — never touches the disk.
	_, m4 := boot(t, asset.URL+"/config.iso", &bmc.MediaCredentials{Username: "media", Password: "grant"})
	assert.Empty(t, m4.media[0].Path)
	assert.Equal(t, payload, m4.media[0].Data, "a granted image stays in memory")

	// Nor does a small one.
	cache.MinSize = 1 << 20
	_, m5 := boot(t, asset.URL+"/small.iso", nil)
	assert.Empty(t, m5.media[0].Path)
	assert.Equal(t, payload, m5.media[0].Data)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 3, "three cached images, no leftovers")
}

func TestPersonalize(t *testing.T) {
	ctx := context.Background()
	f, err := LoadFixture("generic")
	require.NoError(t, err)
	f.Personalize(Identity{
		Serial: "PKLAB0002", UUID: "9b1c0002-0000-4000-8000-000000000002",
		BMCMAC: "02:77:00:00:02:01", NICMACs: []string{"52:54:00:77:00:21"}, DiskSerial: "pklab-disk-2",
	})
	f.SetCredentials("lab", "lab-password")
	s := NewState(f)

	systems, err := s.Systems(ctx)
	require.NoError(t, err)
	assert.Equal(t, "PKLAB0002", systems[0].SerialNumber)
	assert.Equal(t, "9b1c0002-0000-4000-8000-000000000002", systems[0].UUID)
	assert.Equal(t, []bmc.NIC{{ID: "1", MAC: "52:54:00:77:00:21"}}, systems[0].NICs)
	assert.Equal(t, "pklab-disk-2", systems[0].Drives[0].Serial)
	ch, err := s.Chassis(ctx)
	require.NoError(t, err)
	assert.Equal(t, "PKLAB0002", ch.BoardSerial)
	assert.Equal(t, []string{"52:54:00:77:00:21"}, ch.Adapters[0].PortMACs)
	mgr, err := s.Manager(ctx)
	require.NoError(t, err)
	assert.Equal(t, "02:77:00:00:02:01", mgr.MAC)
	assert.True(t, s.Authenticate("lab", "lab-password"))
	assert.False(t, s.Authenticate("admin", "admin-pass"), "the factory credential is replaced")

	// A fixture whose system carries no serial keeps it that way: the X12's
	// only serial anchor is the chassis.
	x12, err := LoadFixture("supermicro-x12")
	require.NoError(t, err)
	x12.Personalize(Identity{Serial: "PKLAB0003"})
	assert.Empty(t, x12.Systems[0].Serial)
	assert.Equal(t, "PKLAB0003", x12.Chassis.BoardSerial)
}
