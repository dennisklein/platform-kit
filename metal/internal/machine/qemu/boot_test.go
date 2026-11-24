package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func TestBootOrder(t *testing.T) {
	devices := []string{devDisk, devInstaller, "cd0", "cd1", devNIC}
	for source, want := range map[string][]string{
		"":          {devDisk, devInstaller, "cd0", "cd1"},
		"Hdd":       {devDisk, devInstaller, "cd0", "cd1"},
		"None":      {devDisk, devInstaller, "cd0", "cd1"},
		"BiosSetup": {devDisk, devInstaller, "cd0", "cd1"},
		"Cd":        {devInstaller, "cd0", "cd1", devDisk},
		"UsbCd":     {devInstaller, "cd0", "cd1", devDisk},
		"Usb":       {devInstaller, "cd0", "cd1", devDisk},
		"Pxe":       {devNIC, devDisk, devInstaller, "cd0", "cd1"},
		"UefiHttp":  {devNIC, devDisk, devInstaller, "cd0", "cd1"},
	} {
		assert.Equal(t, want, bootOrder(source, devices), "boot source %q", source)
	}
}

func TestArgsBootSourceAndTap(t *testing.T) {
	ctx := context.Background()
	m := New(Options{
		Name: "node-1", Dir: t.TempDir(), OVMFCode: "/fw/CODE.fd",
		MemoryMiB: 8192, CPUs: 4, DiskSerial: "pklab-disk-1",
		Tap: "pktap1", MAC: "52:54:00:77:00:11",
	})
	require.NoError(t, m.SetMedia(ctx, []sim.Media{{SlotID: "2", Data: []byte("installer")}, {SlotID: "1", Data: []byte("config")}}))

	// The machine's own order: the disk, then the media.
	args, devices, err := m.args()
	require.NoError(t, err)
	line := strings.Join(args, " ")
	assert.Equal(t, []string{devDisk, "cd0", "cd1", devNIC}, devices)
	assert.Contains(t, line, "-m 8192")
	assert.Contains(t, line, "-smp 4")
	assert.Contains(t, line, "id=hd0,bootindex=1,serial=pklab-disk-1")
	assert.Contains(t, line, "id=cd0,bus=ide.0,bootindex=2")
	assert.Contains(t, line, "id=cd1,bus=ide.1,bootindex=3")
	assert.Contains(t, line, "-netdev tap,id=net0,ifname=pktap1,script=no,downscript=no")
	assert.Contains(t, line, "-device virtio-net-pci,netdev=net0,id=nic0,mac=52:54:00:77:00:11")
	assert.NotContains(t, line, "mac=52:54:00:77:00:11,bootindex", "the NIC does not boot unless asked to")

	// A Cd override puts the media in front of the disk.
	require.NoError(t, m.SetBootSource(ctx, "Cd", true))
	args, _, err = m.args()
	require.NoError(t, err)
	line = strings.Join(args, " ")
	assert.Contains(t, line, "id=cd0,bus=ide.0,bootindex=1")
	assert.Contains(t, line, "id=cd1,bus=ide.1,bootindex=2")
	assert.Contains(t, line, "id=hd0,bootindex=3")

	// A network override boots the NIC.
	require.NoError(t, m.SetBootSource(ctx, "Pxe", false))
	args, _, err = m.args()
	require.NoError(t, err)
	assert.Contains(t, strings.Join(args, " "), "id=nic0,mac=52:54:00:77:00:11,bootindex=1")

	// A tap NIC without a MAC, or with user-mode forwards, is a mistake.
	_, _, err = New(Options{Dir: t.TempDir(), Tap: "pktap1"}).args()
	assert.ErrorContains(t, err, "MAC")
	_, _, err = New(Options{Dir: t.TempDir(), Tap: "pktap1", MAC: "52:54:00:77:00:11", ForwardPorts: []int{50000}}).args()
	assert.ErrorContains(t, err, "user-mode")
}

func TestSetMediaLinksCachedImages(t *testing.T) {
	ctx := context.Background()
	cached := filepath.Join(t.TempDir(), "installer.iso")
	require.NoError(t, os.WriteFile(cached, []byte("a large public image"), 0o600))
	m := New(Options{Dir: t.TempDir()})

	require.NoError(t, m.SetMedia(ctx, []sim.Media{{SlotID: "2", Path: cached}}))
	target, err := os.Readlink(m.mediaPath(0))
	require.NoError(t, err, "a cached image is linked, not copied")
	assert.Equal(t, cached, target)

	// The same slot taking an in-memory image must not write through the link.
	require.NoError(t, m.SetMedia(ctx, []sim.Media{{SlotID: "2", Data: []byte("config")}}))
	raw, err := os.ReadFile(cached)
	require.NoError(t, err)
	assert.Equal(t, "a large public image", string(raw), "the cache entry is untouched")
	raw, err = os.ReadFile(m.mediaPath(0))
	require.NoError(t, err)
	assert.Equal(t, "config", string(raw))

	// The set is what decides between a reset in place and a cold restart.
	assert.NotEqual(t, m.media, m.spawned, "a set the VM was not spawned with")
	m.spawned = append([]string(nil), m.media...)
	require.NoError(t, m.SetMedia(ctx, []sim.Media{{SlotID: "2", Data: []byte("config")}}))
	assert.Equal(t, m.media, m.spawned, "the same content is the same set")
	require.NoError(t, m.SetMedia(ctx, nil))
	assert.NotEqual(t, m.media, m.spawned, "an ejected medium changes it")
}

// recordingQMP is a monitor that accepts everything and keeps what it was
// asked, one entry per command after the handshake.
type recordingQMP struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingQMP) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func serveRecordingQMP(t *testing.T, sock string) *recordingQMP {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	rec := &recordingQMP{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				fmt.Fprintln(conn, `{"QMP": {"version": {}, "capabilities": []}}`)
				lines := bufio.NewScanner(conn)
				for lines.Scan() {
					var cmd struct {
						Execute   string `json:"execute"`
						Arguments struct {
							Path  string `json:"path"`
							Value *int   `json:"value"`
						} `json:"arguments"`
					}
					if json.Unmarshal(lines.Bytes(), &cmd) == nil && cmd.Execute != "qmp_capabilities" {
						entry := cmd.Execute
						if cmd.Arguments.Value != nil {
							entry = fmt.Sprintf("%s=%d", filepath.Base(cmd.Arguments.Path), *cmd.Arguments.Value)
						}
						rec.mu.Lock()
						rec.seen = append(rec.seen, entry)
						rec.mu.Unlock()
					}
					fmt.Fprintln(conn, `{"return": {}}`)
				}
			}()
		}
	}()
	return rec
}

// running makes the machine believe it has a process, which is all the
// in-place paths ask about.
func running(m *Machine, devices ...string) {
	m.proc, m.done = &exec.Cmd{}, make(chan struct{})
	m.devices = devices
}

func TestSetMediaResolvesRelativePaths(t *testing.T) {
	// The lab's fleet file names its state and cache directories relative to
	// the repository root. A link must still name the image from inside the
	// machine's directory, where qemu opens it.
	t.Chdir(t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(".lab", "cache"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(".lab", "cache", "installer.iso"), []byte("image"), 0o600))
	m := New(Options{Dir: filepath.Join(".lab", "state", "node-1")})
	require.NoError(t, os.MkdirAll(m.o.Dir, 0o755))
	assert.True(t, filepath.IsAbs(m.o.Dir), "the machine directory is absolute")

	require.NoError(t, m.SetMedia(context.Background(), []sim.Media{{SlotID: "2", Path: filepath.Join(".lab", "cache", "installer.iso")}}))
	raw, err := os.ReadFile(m.mediaPath(0))
	require.NoError(t, err, "the link resolves")
	assert.Equal(t, "image", string(raw))
}

func TestRestartAppliesAOneTimeBootSource(t *testing.T) {
	ctx := context.Background()
	m := New(Options{Dir: t.TempDir(), HangTimeout: -1}) // no qemu runs to be guarded
	qmp := serveRecordingQMP(t, m.sockPath())
	running(m, devDisk, "cd0", devNIC)

	// No override, the order already the machine's own: a plain reset.
	require.NoError(t, m.Restart(ctx))
	assert.Equal(t, []string{"system_reset"}, qmp.commands())

	// A one-time Cd override: the order is laid out before the reset — the
	// firmware reads it there — and put back right after, so that the reboot
	// the guest does itself comes up from the disk.
	require.NoError(t, m.SetBootSource(ctx, "Cd", true))
	require.NoError(t, m.Restart(ctx))
	assert.Equal(t, []string{
		"system_reset",
		"hd0=-1", "cd0=-1", "nic0=-1", "cd0=1", "hd0=2",
		"system_reset",
		"hd0=-1", "cd0=-1", "nic0=-1", "hd0=1", "cd0=2",
	}, qmp.commands())
	assert.Empty(t, m.source, "the one-time source is used up")

	// A continuous override stays laid out: the next reset changes nothing.
	require.NoError(t, m.SetBootSource(ctx, "Pxe", false))
	require.NoError(t, m.Restart(ctx))
	require.NoError(t, m.Restart(ctx))
	assert.Equal(t, []string{
		"hd0=-1", "cd0=-1", "nic0=-1", "nic0=1", "hd0=2", "cd0=3",
		"system_reset",
		"system_reset",
	}, qmp.commands()[12:])
}

func TestConsoleProgress(t *testing.T) {
	m := New(Options{Dir: t.TempDir()})
	ctx := context.Background()
	write := func(s string) {
		f, err := os.OpenFile(m.ConsoleLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(s)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	progress := func() string {
		p, err := m.BootProgress(ctx)
		require.NoError(t, err)
		return p
	}

	assert.Equal(t, "None", progress(), "a machine that is off")
	running(m)
	assert.Equal(t, "None", progress(), "no console output yet")
	write("BdsDxe: loading Boot0001 \"UEFI QEMU DVD-ROM\"\r\nBdsDxe: starting Boot0001\r\n")
	assert.Equal(t, "None", progress(), "the firmware is still at it")
	write("[    0.000000] Linux vers")
	assert.Equal(t, "None", progress(), "half a line says nothing")
	write("ion 6.12.0-talos (builder@host)\r\n")
	assert.Equal(t, "OSBootStarted", progress())
	write("[    3.141592] [talos] task startSystemServices (1/1): done\r\n")
	assert.Equal(t, "OSRunning", progress())

	// The guest reboots on its own: the firmware line starts a boot over.
	write("[  120.000000] reboot: Restarting system\r\nBdsDxe: starting Boot0002\r\n")
	assert.Equal(t, "None", progress())
	write("[    0.000000] Linux version 6.12.0-talos\r\n")
	assert.Equal(t, "OSBootStarted", progress())

	// A respawn truncates the capture; the scan follows the new one.
	require.NoError(t, os.WriteFile(m.ConsoleLog(), []byte("BdsDxe: starting Boot0001\r\n"), 0o600))
	assert.Equal(t, "None", progress())
}
