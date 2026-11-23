// Package qemu runs a UEFI VM as the machine behind a simulated BMC — the
// tier that boots, installs to, and reboots a real disk. qemu-system is
// spawned directly and controlled over QMP on a unix socket: no libvirt, no
// cgo, nothing between the harness and the process.
//
// The disk layout encodes the metal boot story: a blank virtio disk holds
// boot index 1 and an installer CD index 2, so the first boot falls through
// the empty disk to the installer, and every boot after a completed install
// comes from the disk — no boot-order surgery between phases. Media handed
// over by the BMC (sim.MediaMachine) rides as additional CD-ROMs, written
// before each power-on.
//
// Networking is user-mode with 1:1 port forwards onto a caller-chosen
// loopback address: no privileges, no bridges, and — because all of 127/8
// routes to lo on Linux, inside the guest too — the same address:port
// reaches the guest's services from the host and from within the guest, so
// it can serve as the node's address in certificates and endpoints alike.
package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
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
	// default 127.0.0.1. Any 127/8 address works on Linux.
	HostIP string
	// ForwardPorts are guest TCP ports forwarded 1:1 from HostIP.
	ForwardPorts []int
}

// Machine is one VM. Zero value is not usable — New fills the invariants.
type Machine struct {
	o Options

	mu   sync.Mutex
	proc *exec.Cmd
	done chan struct{}
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
	return &Machine{o: o}
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

// SetMedia writes the BMC-presented images as this VM's media CD-ROMs — the
// sim.MediaMachine hook. The set applies at the next cold Start (attachment
// happens at spawn); a running VM keeps its devices, only the content of
// still-present slots refreshes.
func (m *Machine) SetMedia(_ context.Context, images [][]byte) error {
	for i, img := range images {
		if err := os.WriteFile(m.mediaPath(i), img, 0o600); err != nil {
			return err
		}
	}
	stale, err := filepath.Glob(filepath.Join(m.o.Dir, "media-*.iso"))
	if err != nil {
		return err
	}
	for _, p := range stale {
		var idx int
		if _, err := fmt.Sscanf(filepath.Base(p), "media-%d.iso", &idx); err != nil || idx < len(images) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
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
	args, err := m.args()
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
	cmd := exec.Command("qemu-system-x86_64", args...)
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
	m.mu.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-done:
			return fmt.Errorf("qemu: exited during startup: %s", m.logTail())
		case <-ctx.Done():
			_ = m.Stop(context.WithoutCancel(ctx))
			return ctx.Err()
		default:
		}
		if err := m.qmp(ctx, "query-status"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			_ = m.Stop(context.WithoutCancel(ctx))
			return fmt.Errorf("qemu: QMP never answered: %s", m.logTail())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Stop quits the VM — the machine goes off, the process goes away. A VM that
// ignores the polite quit is killed.
func (m *Machine) Stop(ctx context.Context) error {
	m.mu.Lock()
	proc, done := m.proc, m.done
	m.mu.Unlock()
	if proc == nil {
		return nil
	}
	_ = m.qmp(ctx, "quit") // the reply may not outrun the exit — done is the truth
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
// cold-starts a stopped one.
func (m *Machine) Restart(ctx context.Context) error {
	m.mu.Lock()
	running := m.proc != nil
	m.mu.Unlock()
	if !running {
		return m.Start(ctx)
	}
	return m.qmp(ctx, "system_reset")
}

func (m *Machine) Running(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proc != nil, nil
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

// args builds the qemu command line from the options and the media files
// currently on disk. Caller holds mu.
func (m *Machine) args() ([]string, error) {
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
	diskDev := "virtio-blk-pci,drive=disk0,bootindex=1"
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
			"-device", "ide-cd,drive=install,bus=ide.0,bootindex=2")
		port++
	}
	media, err := filepath.Glob(filepath.Join(m.o.Dir, "media-*.iso"))
	if err != nil {
		return nil, err
	}
	sort.Strings(media)
	for i, iso := range media {
		if port > 5 {
			return nil, fmt.Errorf("qemu: %d CDs exceed the q35 AHCI ports", port+1)
		}
		id := "media" + strconv.Itoa(i)
		a = append(a,
			"-drive", "if=none,id="+id+",format=raw,readonly=on,file="+iso,
			"-device", "ide-cd,drive="+id+",bus=ide."+strconv.Itoa(port))
		port++
	}
	netdev := "user,id=net0"
	for _, p := range m.o.ForwardPorts {
		netdev += fmt.Sprintf(",hostfwd=tcp:%s:%d-:%d", m.o.HostIP, p, p)
	}
	a = append(a, "-netdev", netdev, "-device", "virtio-net-pci,netdev=net0")
	return a, nil
}

// qmp dials the monitor socket, performs the capabilities handshake, and runs
// one argument-less command.
func (m *Machine) qmp(ctx context.Context, execute string) error {
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
	for _, cmd := range []string{"qmp_capabilities", execute} {
		if _, err := fmt.Fprintf(conn, "{\"execute\": %q}\n", cmd); err != nil {
			return fmt.Errorf("qmp %s: %w", cmd, err)
		}
		if err := readReturn(br); err != nil {
			return fmt.Errorf("qmp %s: %w", cmd, err)
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
