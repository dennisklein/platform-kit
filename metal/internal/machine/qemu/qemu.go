// Package qemu runs a UEFI VM as the machine behind a simulated BMC — the
// tier that boots, installs to, and reboots a real disk. qemu-system is
// spawned directly and controlled over QMP on a unix socket: no libvirt, no
// cgo, nothing between the harness and the process.
//
// The machine's own boot order encodes the metal boot story: the virtio disk
// first, then the installer CD, then the media the BMC presents. A blank disk
// falls through to the first bootable CD, and every boot after a completed
// install comes from the disk — no boot-order surgery between phases. Media
// handed over by the BMC (sim.MediaMachine) ride as CD-ROMs, attached at
// spawn; a changed set therefore turns the next reset into a cold restart.
//
// A boot source override from the BMC (sim.BootMachine) reorders the devices
// for the boot it covers. A one-time override is put back to the machine's own
// order right after that boot has started, so that the reboot the guest does
// itself at the end of an install lands on the disk — what a real BMC does
// when it resets BootSourceOverrideEnabled.
//
// Networking comes in two shapes. User-mode with 1:1 port forwards onto a
// caller-chosen loopback address needs no privileges and no bridge, and —
// because all of 127/8 routes to lo on Linux, inside the guest too — the same
// address:port reaches the guest's services from the host and from within
// the guest, so it can serve as the node's address in certificates and
// endpoints alike. A tap device puts the VM on a real L2 segment with a fixed
// MAC: the lab's shape, where machines reach each other and a DHCP server.
package qemu

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
)

// Options shapes one VM. Zero values take the documented defaults.
type Options struct {
	// Name names the VM (qemu -name) and shows up in process listings.
	Name string
	// Dir is the VM's working directory: disk image, firmware vars, media
	// ISOs, QMP socket, and logs all live here. Created by Ensure.
	Dir string
	// InstallerISO is attached as the CD behind the disk in boot order;
	// empty attaches none. DetachInstaller withdraws it for later boots.
	InstallerISO string
	// OVMFCode/OVMFVars point at the UEFI firmware pair; empty probes the
	// well-known distro locations (PK_METAL_OVMF_CODE/PK_METAL_OVMF_VARS override).
	OVMFCode string
	OVMFVars string
	// MemoryMiB defaults to 4096, CPUs to 2, DiskGiB to 8.
	MemoryMiB int
	CPUs      int
	DiskGiB   int
	// DiskSerial is the udev-visible serial of the virtio disk — what an
	// installDiskSelector matches on. Empty sets none.
	DiskSerial string
	// Accel is the qemu accelerator; default kvm (Ensure verifies /dev/kvm).
	Accel string
	// HostIP is the loopback address the forwarded guest ports surface on;
	// default 127.0.0.1. Any 127/8 address works on Linux. User-mode only.
	HostIP string
	// ForwardPorts are guest TCP ports forwarded 1:1 from HostIP. User-mode
	// only.
	ForwardPorts []int
	// Tap names an existing tap device to attach the NIC to; empty selects
	// user-mode networking. The device is expected to be set up and owned by
	// the caller's user already: qemu runs no ifup/ifdown script.
	Tap string
	// MAC is the NIC's address — required with Tap, where the DHCP lease and
	// the inventory hang on it; empty leaves it to qemu in user mode.
	MAC string
	// HangTimeout is how long the console may gain nothing readable after a
	// power-on, before the kernel starts, until the VM is power-cycled
	// (see hang.go). Zero is DefaultHangTimeout; negative turns the guard off.
	HangTimeout time.Duration
	// Logf receives what the machine does on its own, such as the guard's
	// power cycle; nil is log.Printf.
	Logf func(format string, args ...any)
}

// Machine is one VM. Zero value is not usable — New fills the invariants.
type Machine struct {
	o Options

	mu   sync.Mutex
	proc *exec.Cmd
	done chan struct{}

	// media is the set the BMC presented last, one signature per medium;
	// spawned is the set the running process was started with. They differ
	// when the next reset has to be a cold one.
	media, spawned []string
	// source is the boot source the next boot takes ("" for the machine's
	// own order), once whether it covers that boot only.
	source string
	once   bool
	// devices are the bootable devices of the running process, in the
	// machine's own order; order is the boot source its boot order is
	// currently laid out for.
	devices []string
	order   string
	// console tracks how far the current boot got on the serial console.
	console consoleScan

	// The firmware-hang guard (hang.go): the cancel of the running one, a
	// generation that tells a stale guard from the current one, whether the
	// guard's own power cycle runs, how many it did in all, and how many for
	// the current power-on.
	guardCancel context.CancelFunc
	guardGen    int
	cycling     bool
	hangCycles  int
	guardTries  int
	// intent is the boot source of the last power-on, for the guard's retry.
	intent bootIntent
	// Seams for the guard's tests; New sets the real ones.
	now        func() time.Time
	ticker     func() (<-chan time.Time, func())
	powerCycle func(context.Context) error
	logf       func(format string, args ...any)
}

func New(o Options) *Machine {
	if o.MemoryMiB == 0 {
		o.MemoryMiB = 4096
	}
	if o.CPUs == 0 {
		o.CPUs = 2
	}
	if o.DiskGiB == 0 {
		o.DiskGiB = 8
	}
	if o.Accel == "" {
		o.Accel = "kvm"
	}
	if o.HostIP == "" {
		o.HostIP = "127.0.0.1"
	}
	// Every path handed to qemu is absolute, so neither the process's
	// directory nor a link's directory changes what it names.
	if abs, err := filepath.Abs(o.Dir); err == nil {
		o.Dir = abs
	}
	m := &Machine{o: o, now: time.Now, ticker: realTicker, logf: o.Logf}
	if m.logf == nil {
		m.logf = log.Printf
	}
	m.powerCycle = m.coldCycle
	return m
}

// Ensure prepares the VM's world: the working directory, the firmware pair,
// and a blank disk if none exists. Nothing runs until Start.
func (m *Machine) Ensure(ctx context.Context) error {
	if m.o.Accel == "kvm" {
		if _, err := os.Stat("/dev/kvm"); err != nil {
			return fmt.Errorf("qemu: no usable /dev/kvm (is the kvm module loaded?): %w", err)
		}
	}
	if err := os.MkdirAll(m.o.Dir, 0o700); err != nil {
		return err
	}
	if err := m.ensureFirmware(); err != nil {
		return err
	}
	disk := m.diskPath()
	if _, err := os.Stat(disk); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2",
		disk, strconv.Itoa(m.o.DiskGiB)+"G").CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create: %w: %s", err, out)
	}
	return nil
}

// SetMedia makes the BMC-presented media this VM's media CD-ROMs — the
// sim.MediaMachine hook. An image held in memory is written into the VM's
// directory; one that sits in the media cache is linked, not copied, and
// attached read-only in place. CD-ROMs are attached at spawn, so a set that
// differs from the one the VM runs with makes the next Restart a cold one.
func (m *Machine) SetMedia(_ context.Context, media []sim.Media) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sigs := make([]string, 0, len(media))
	for i, med := range media {
		path := m.mediaPath(i)
		// Remove first, always: writing through a leftover link would
		// overwrite the cache entry it points at.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if med.Path != "" {
			// A relative link target resolves against the link's own
			// directory, not the process's: link to the absolute path.
			target, err := filepath.Abs(med.Path)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, path); err != nil {
				return err
			}
			sigs = append(sigs, "file:"+target)
			continue
		}
		if err := os.WriteFile(path, med.Data, 0o600); err != nil {
			return err
		}
		sum := sha256.Sum256(med.Data)
		sigs = append(sigs, "data:"+hex.EncodeToString(sum[:]))
	}
	stale, err := filepath.Glob(filepath.Join(m.o.Dir, "media-*.iso"))
	if err != nil {
		return err
	}
	for _, p := range stale {
		var idx int
		if _, err := fmt.Sscanf(filepath.Base(p), "media-%d.iso", &idx); err != nil || idx < len(media) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	m.media = sigs
	return nil
}

// SetBootSource records the boot source override the next boot takes — the
// sim.BootMachine hook. target is a Redfish BootSource; "" (and any source
// this machine has no device for) is the machine's own order.
func (m *Machine) SetBootSource(_ context.Context, target string, once bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.source, m.once = target, once
	return nil
}

// DetachInstaller withdraws the installer CD from every later spawn — how a
// harness proves the disk boots alone.
func (m *Machine) DetachInstaller() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.o.InstallerISO = ""
}

// Start spawns the VM if it is not running and waits until QMP answers — the
// proof the process came up rather than died on its arguments.
func (m *Machine) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.proc != nil {
		m.mu.Unlock()
		return nil
	}
	args, devices, err := m.args()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	logf, err := os.OpenFile(filepath.Join(m.o.Dir, "qemu.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	cmd := exec.Command(qemuBinary(), args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		m.mu.Unlock()
		return fmt.Errorf("qemu: spawn: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = logf.Close()
		close(done)
		m.mu.Lock()
		if m.proc == cmd {
			m.proc, m.done = nil, nil
		}
		m.mu.Unlock()
	}()
	m.proc, m.done = cmd, done
	// What this process was born with: its devices, the media among them, and
	// the boot source its boot order was laid out for. qemu truncates the
	// console capture on spawn, so the scan starts over.
	m.devices, m.spawned, m.order = devices, slices.Clone(m.media), m.source
	m.intent = bootIntent{m.source, m.once}
	m.console = consoleScan{}
	m.mu.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	exited := false
	err = poll.Until(ctx, poll.Every(200*time.Millisecond), func(ctx context.Context) (bool, error) {
		select {
		case <-done:
			exited = true
			return false, fmt.Errorf("qemu: exited during startup: %s", m.logTail())
		default:
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if m.qmp(ctx, "query-status", nil) == nil {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("qemu: QMP never answered: %s", m.logTail())
		}
		return false, nil
	})
	if err == nil {
		err = m.afterBoot(ctx)
	}
	if err == nil {
		// qemu truncated the console capture at spawn: this power-on's
		// output starts at the beginning.
		m.armGuard(0)
	}
	if err != nil && !exited {
		// A process that already exited has nothing left to stop.
		_ = m.Stop(context.WithoutCancel(ctx))
	}
	return err
}

// Stop quits the VM — the machine goes off, the process goes away. A VM that
// ignores the polite quit is killed.
func (m *Machine) Stop(ctx context.Context) error {
	m.mu.Lock()
	m.disarmLocked()
	proc, done := m.proc, m.done
	m.mu.Unlock()
	if proc == nil {
		return nil
	}
	_ = m.qmp(ctx, "quit", nil) // the reply may not outrun the exit — done is the truth
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
	}
	_ = proc.Process.Kill()
	<-done
	return ctx.Err()
}

// Restart resets a running VM in place (its devices stay attached) and
// cold-starts a stopped one. A VM whose media set changed since it was
// spawned is stopped and started instead: its CD-ROMs are what it was born
// with, and the boot it is about to do must see the new set.
func (m *Machine) Restart(ctx context.Context) error {
	m.mu.Lock()
	m.disarmLocked() // this reset is a new power-on, with a guard of its own
	running := m.proc != nil
	cold := running && !slices.Equal(m.media, m.spawned)
	source := m.source
	if running && !cold {
		m.intent = bootIntent{m.source, m.once}
	}
	m.mu.Unlock()
	if !running {
		return m.Start(ctx)
	}
	if cold {
		if err := m.Stop(ctx); err != nil {
			return err
		}
		return m.Start(ctx)
	}
	// The firmware reads the boot order at reset, so it is laid out first.
	if err := m.setBootOrder(ctx, source); err != nil {
		return err
	}
	// The console capture keeps growing across an in-place reset; the new
	// boot's progress is read from here on.
	var offset int64
	if st, err := os.Stat(m.ConsoleLog()); err == nil {
		offset = st.Size()
	}
	m.mu.Lock()
	m.console = consoleScan{offset: offset}
	m.mu.Unlock()
	if err := m.qmp(ctx, "system_reset", nil); err != nil {
		return err
	}
	m.armGuard(offset)
	return m.afterBoot(ctx)
}

// afterBoot runs once a boot is under way. A one-time boot source has done
// its part by then — the firmware read the order at reset — so the machine's
// own order goes back in place, where the next reset finds it: the one a
// freshly installed system does by itself.
func (m *Machine) afterBoot(ctx context.Context) error {
	m.mu.Lock()
	once := m.once
	if once {
		m.source, m.once = "", false
	}
	m.mu.Unlock()
	if !once {
		return nil
	}
	if err := m.setBootOrder(ctx, ""); err != nil {
		return fmt.Errorf("qemu: restoring the boot order after a one-time override: %w", err)
	}
	return nil
}

// setBootOrder lays the running VM's boot order out for a boot source by
// rewriting the devices' bootindex properties over QMP. It takes effect at the
// next reset. A no-op when the order is already the one asked for.
func (m *Machine) setBootOrder(ctx context.Context, source string) error {
	m.mu.Lock()
	devices, current := slices.Clone(m.devices), m.order
	m.mu.Unlock()
	order := bootOrder(source, devices)
	if slices.Equal(order, bootOrder(current, devices)) {
		return nil
	}
	set := func(device string, index int) error {
		return m.qmp(ctx, "qom-set", map[string]any{
			"path": "/machine/peripheral/" + device, "property": "bootindex", "value": index,
		})
	}
	// Two devices cannot hold one index at any moment, so every index is
	// given up before the new ones are handed out.
	for _, d := range devices {
		if err := set(d, -1); err != nil {
			return fmt.Errorf("qemu: boot order: %w", err)
		}
	}
	for i, d := range order {
		if err := set(d, i+1); err != nil {
			return fmt.Errorf("qemu: boot order: %w", err)
		}
	}
	m.mu.Lock()
	m.order = source
	m.mu.Unlock()
	return nil
}

// Device ids, as qemu's QOM tree names them under /machine/peripheral.
const (
	devDisk      = "hd0"
	devInstaller = "cdi"
	devNIC       = "nic0"
)

// bootOrder is the order devices are tried in for a Redfish boot source.
// devices lists what the VM has, in its own order: the disk, then its CD-ROMs.
// Removable-media sources put the CD-ROMs first (every virtual medium is a
// CD-ROM here, whatever its slot calls itself), network sources the NIC;
// everything else — Hdd, None, a source there is no device for — is the
// machine's own order, in which the NIC does not boot at all.
func bootOrder(source string, devices []string) []string {
	var disk, cds []string
	for _, d := range devices {
		switch d {
		case devDisk:
			disk = append(disk, d)
		case devNIC:
		default:
			cds = append(cds, d)
		}
	}
	switch source {
	case "Cd", "UsbCd", "Usb":
		return slices.Concat(cds, disk)
	case "Pxe", "UefiHttp":
		return slices.Concat([]string{devNIC}, disk, cds)
	default:
		return slices.Concat(disk, cds)
	}
}

func (m *Machine) Running(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proc != nil, nil
}

// BootProgress reports how far the current boot got, as a Redfish BootProgress
// LastState, read off the serial console — the sim.ProgressMachine hook:
// "None" until a kernel announces itself, "OSBootStarted" from then on,
// "OSRunning" once the system's own services write to the console.
func (m *Machine) BootProgress(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proc == nil {
		return "None", nil
	}
	if err := m.console.read(m.ConsoleLog()); err != nil {
		return "", err
	}
	return m.console.progress(), nil
}

// Status is what an observer may know about the VM: whether it runs and how
// far its boot got. It names no process id; the VM is reached through its
// BMC, never by signalling the process.
type Status struct {
	Running      bool
	BootProgress string
	ConsoleLog   string
	// HangCycles counts the power cycles the firmware-hang guard did.
	HangCycles int
}

// Status reads the VM's state for a dashboard. It follows the console the
// same way BootProgress does.
func (m *Machine) Status(ctx context.Context) Status {
	m.mu.Lock()
	st := Status{ConsoleLog: m.ConsoleLog(), HangCycles: m.hangCycles}
	m.mu.Unlock()
	st.Running, _ = m.Running(ctx)
	if st.Running {
		st.BootProgress, _ = m.BootProgress(ctx)
	}
	return st
}

// consoleScan follows a serial console capture incrementally: each read picks
// up where the last one stopped.
type consoleScan struct {
	offset int64
	tail   []byte // a last line still without its newline
	state  string
}

// consoleChunk bounds one read; a console that outruns it is caught up on the
// following reads.
const consoleChunk = 1 << 20

func (c *consoleScan) read(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing written yet
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < c.offset {
		*c = consoleScan{} // a new capture replaced the one being followed
	}
	buf := make([]byte, min(st.Size()-c.offset, consoleChunk))
	n, err := f.ReadAt(buf, c.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	c.offset += int64(n)
	c.feed(buf[:n])
	return nil
}

func (c *consoleScan) feed(data []byte) {
	data = append(c.tail, data...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		c.line(data[:i])
		data = data[i+1:]
	}
	// A screenful without a newline is not a line worth keeping whole.
	if len(data) > 4096 {
		data = data[len(data)-4096:]
	}
	c.tail = slices.Clone(data)
}

// line advances the state on the three things a console tells: the firmware
// picking a boot option (a boot starts over — also the reboot a guest does on
// its own), a kernel banner, and the first word from userland.
func (c *consoleScan) line(l []byte) {
	switch {
	case bytes.Contains(l, []byte("BdsDxe:")):
		c.state = ""
	case bytes.Contains(l, []byte("Linux version ")):
		c.state = "OSBootStarted"
	case c.state == "OSBootStarted" && (bytes.Contains(l, []byte("[talos]")) ||
		bytes.Contains(l, []byte("systemd[1]")) || bytes.Contains(l, []byte("login:"))):
		c.state = "OSRunning"
	}
}

func (c *consoleScan) progress() string {
	if c.state == "" {
		return "None"
	}
	return c.state
}

// Remove stops the VM and deletes its world — disk, firmware vars, media,
// logs. The next Ensure births a blank machine.
func (m *Machine) Remove(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return os.RemoveAll(m.o.Dir)
}

// ConsoleLog is the path of the guest's serial console capture — the first
// place to look when a boot goes sideways.
func (m *Machine) ConsoleLog() string { return filepath.Join(m.o.Dir, "console.log") }

// Dir is the VM's working directory — disk, firmware vars, media, and logs.
func (m *Machine) Dir() string { return m.o.Dir }

func (m *Machine) diskPath() string { return filepath.Join(m.o.Dir, "disk.qcow2") }
func (m *Machine) varsPath() string { return filepath.Join(m.o.Dir, "ovmf_vars.fd") }
func (m *Machine) sockPath() string { return filepath.Join(m.o.Dir, "qmp.sock") }
func (m *Machine) mediaPath(i int) string {
	return filepath.Join(m.o.Dir, fmt.Sprintf("media-%d.iso", i))
}

// args builds the qemu command line from the options, the media files
// currently on disk and the boot source the coming boot takes. It also names
// the bootable devices the line creates, in the machine's own order. Caller
// holds mu.
func (m *Machine) args() ([]string, []string, error) {
	cpu := "host"
	if m.o.Accel != "kvm" {
		cpu = "max"
	}
	a := []string{
		"-name", m.o.Name,
		"-machine", "q35",
		"-accel", m.o.Accel,
		"-cpu", cpu,
		"-smp", strconv.Itoa(m.o.CPUs),
		"-m", strconv.Itoa(m.o.MemoryMiB),
		"-display", "none",
		"-serial", "file:" + m.ConsoleLog(),
		"-qmp", "unix:" + m.sockPath() + ",server=on,wait=off",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + m.o.OVMFCode,
		"-drive", "if=pflash,format=raw,file=" + m.varsPath(),
		"-drive", "if=none,id=disk0,format=qcow2,file=" + m.diskPath(),
	}
	media, err := filepath.Glob(filepath.Join(m.o.Dir, "media-*.iso"))
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(media)

	// The devices in the machine's own order, then the order this boot takes.
	devices := []string{devDisk}
	if m.o.InstallerISO != "" {
		devices = append(devices, devInstaller)
	}
	for i := range media {
		devices = append(devices, "cd"+strconv.Itoa(i))
	}
	devices = append(devices, devNIC)
	order := bootOrder(m.source, devices)
	// bootindex renders a device's place in the order; a device outside it
	// gets none and is not booted from.
	bootindex := func(device string) string {
		if i := slices.Index(order, device); i >= 0 {
			return ",bootindex=" + strconv.Itoa(i+1)
		}
		return ""
	}

	diskDev := "virtio-blk-pci,drive=disk0,id=" + devDisk + bootindex(devDisk)
	if m.o.DiskSerial != "" {
		diskDev += ",serial=" + m.o.DiskSerial
	}
	a = append(a, "-device", diskDev)
	// Each CD gets its own AHCI port (q35 exposes six as ide.0–ide.5, one
	// device per port — qemu's auto-placement would try "unit 1" on a shared
	// port and fail the spawn).
	port := 0
	if m.o.InstallerISO != "" {
		a = append(a,
			"-drive", "if=none,id=install,format=raw,readonly=on,file="+m.o.InstallerISO,
			"-device", "ide-cd,drive=install,id="+devInstaller+",bus=ide.0"+bootindex(devInstaller))
		port++
	}
	for i, iso := range media {
		if port > 5 {
			return nil, nil, fmt.Errorf("qemu: %d CDs exceed the q35 AHCI ports", port+1)
		}
		drive, device := "media"+strconv.Itoa(i), "cd"+strconv.Itoa(i)
		a = append(a,
			"-drive", "if=none,id="+drive+",format=raw,readonly=on,file="+iso,
			"-device", "ide-cd,drive="+drive+",id="+device+",bus=ide."+strconv.Itoa(port)+bootindex(device))
		port++
	}

	nic := "virtio-net-pci,netdev=net0,id=" + devNIC
	if m.o.MAC != "" {
		nic += ",mac=" + m.o.MAC
	}
	nic += bootindex(devNIC)
	var netdev string
	switch {
	case m.o.Tap == "":
		netdev = "user,id=net0"
		for _, p := range m.o.ForwardPorts {
			netdev += fmt.Sprintf(",hostfwd=tcp:%s:%d-:%d", m.o.HostIP, p, p)
		}
	case len(m.o.ForwardPorts) > 0:
		return nil, nil, errors.New("qemu: port forwards need user-mode networking, not a tap device")
	case m.o.MAC == "":
		return nil, nil, errors.New("qemu: a NIC on a tap device needs a fixed MAC")
	default:
		// The tap device exists and is up already; qemu only opens it.
		netdev = "tap,id=net0,ifname=" + m.o.Tap + ",script=no,downscript=no"
	}
	a = append(a, "-netdev", netdev, "-device", nic)
	return a, devices, nil
}

// qmp dials the monitor socket, performs the capabilities handshake, and runs
// one command; arguments is nil for a command that takes none.
func (m *Machine) qmp(ctx context.Context, execute string, arguments any) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", m.sockPath())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil { // the greeting banner
		return fmt.Errorf("qmp greeting: %w", err)
	}
	type command struct {
		Execute   string `json:"execute"`
		Arguments any    `json:"arguments,omitempty"`
	}
	for _, cmd := range []command{{Execute: "qmp_capabilities"}, {Execute: execute, Arguments: arguments}} {
		line, err := json.Marshal(cmd)
		if err != nil {
			return fmt.Errorf("qmp %s: %w", cmd.Execute, err)
		}
		if _, err := conn.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("qmp %s: %w", cmd.Execute, err)
		}
		if err := readReturn(br); err != nil {
			return fmt.Errorf("qmp %s: %w", cmd.Execute, err)
		}
	}
	return nil
}

// readReturn scans monitor lines for the command's outcome, skipping
// asynchronous events.
func readReturn(br *bufio.Reader) error {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if e, ok := msg["error"]; ok {
			return errors.New(string(e))
		}
		if _, ok := msg["return"]; ok {
			return nil
		}
	}
}

// ensureFirmware resolves the OVMF pair and gives the VM its private,
// writable variables copy.
func (m *Machine) ensureFirmware() error {
	code, vars, err := findOVMF(m.o.OVMFCode, m.o.OVMFVars)
	if err != nil {
		return err
	}
	m.o.OVMFCode = code
	if _, err := os.Stat(m.varsPath()); err == nil {
		return nil
	}
	raw, err := os.ReadFile(vars)
	if err != nil {
		return fmt.Errorf("qemu: read OVMF vars template: %w", err)
	}
	return os.WriteFile(m.varsPath(), raw, 0o600)
}

// ovmfPairs are the distro locations worth probing, in preference order.
var ovmfPairs = [][2]string{
	{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
	{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
	{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
	{"/usr/share/edk2-ovmf/x64/OVMF_CODE.fd", "/usr/share/edk2-ovmf/x64/OVMF_VARS.fd"},
}

// findOVMF resolves the firmware pair: explicit options win, then the
// PK_METAL_OVMF_CODE/PK_METAL_OVMF_VARS environment, then the probe list.
func findOVMF(code, vars string) (string, string, error) {
	if code == "" {
		code, vars = os.Getenv("PK_METAL_OVMF_CODE"), os.Getenv("PK_METAL_OVMF_VARS")
	}
	if code != "" {
		if vars == "" {
			return "", "", errors.New("qemu: OVMF code given without its vars template")
		}
		return code, vars, nil
	}
	for _, p := range ovmfPairs {
		if fileReadable(p[0]) && fileReadable(p[1]) {
			return p[0], p[1], nil
		}
	}
	return "", "", fmt.Errorf("qemu: no OVMF firmware found (probed %v; set PK_METAL_OVMF_CODE/PK_METAL_OVMF_VARS)", ovmfPairs)
}

func fileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// logTail is the last stretch of the qemu process log — for error messages.
func (m *Machine) logTail() string {
	f, err := os.Open(filepath.Join(m.o.Dir, "qemu.log"))
	if err != nil {
		return "(no qemu log)"
	}
	defer func() { _ = f.Close() }()
	const tail = 2048
	st, err := f.Stat()
	if err != nil {
		return "(no qemu log)"
	}
	if st.Size() > tail {
		_, _ = f.Seek(-tail, io.SeekEnd)
	}
	raw, _ := io.ReadAll(f)
	return string(raw)
}

// qemuBinary names the QEMU system emulator: $PK_METAL_QEMU when set, else
// qemu-system-x86_64 on PATH, else RHEL's /usr/libexec/qemu-kvm, which
// RHEL ships off PATH and under another name.
func qemuBinary() string {
	if b := os.Getenv("PK_METAL_QEMU"); b != "" {
		return b
	}
	if _, err := exec.LookPath("qemu-system-x86_64"); err == nil {
		return "qemu-system-x86_64"
	}
	if _, err := os.Stat("/usr/libexec/qemu-kvm"); err == nil {
		return "/usr/libexec/qemu-kvm"
	}
	return "qemu-system-x86_64"
}
