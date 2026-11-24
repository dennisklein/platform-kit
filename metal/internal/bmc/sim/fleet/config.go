// Package fleet serves several simulated BMCs from one process: one HTTPS
// listener and one fixture per machine, each with its own identity and,
// optionally, a real machine behind it — a QEMU VM or a Talos container. It
// is what `pk-metal sim fleet` runs and what the virtual lab is built on.
package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Config is the fleet file.
//
//	stateDir: .lab/state     # per-machine disks, firmware variables, console logs
//	mediaCache: .lab/cache   # large public images, downloaded once for all machines
//	machines:
//	  - name: node-1
//	    listen: 10.77.0.1:8441
//	    personality: generic
//	    credentials: { username: admin, password: admin-pass }
//	    identity: { serial: PKLAB0001, uuid: …, bmcMAC: "02:…", nicMACs: ["52:54:…"] }
//	    backend: qemu
//	    qemu: { tap: pktap1, memoryMiB: 4096, cpus: 2, diskGiB: 20, diskSerial: pklab-disk-1 }
//
// Relative paths are taken from the working directory.
type Config struct {
	// StateDir holds one directory per machine with a backend that keeps
	// state. Required as soon as one machine has the qemu backend.
	StateDir string `yaml:"stateDir"`
	// MediaCache is the directory large images mounted without credentials
	// are kept in. Empty pulls every image into memory.
	MediaCache string `yaml:"mediaCache"`
	// Media optionally serves a directory of images over HTTPS next to the
	// BMCs — something to mount where no asset server is around.
	Media    *Media    `yaml:"media"`
	Machines []Machine `yaml:"machines"`
}

// Media is a directory served over HTTPS with an ephemeral certificate.
type Media struct {
	Listen string `yaml:"listen"`
	Dir    string `yaml:"dir"`
}

// Machine is one BMC endpoint and what is behind it.
type Machine struct {
	// Name names the machine in logs and, for a backend, its VM or container.
	Name string `yaml:"name"`
	// Listen is the address the BMC's Redfish service answers on, HTTPS with
	// an ephemeral self-signed certificate.
	Listen string `yaml:"listen"`
	// Personality is an embedded fixture name or a path to a fixture file;
	// empty is generic.
	Personality string `yaml:"personality"`
	// Credentials replace the fixture's factory account. Empty keeps it.
	Credentials Credentials `yaml:"credentials"`
	// Identity overrides the fixture's identity facts; empty fields keep the
	// fixture's.
	Identity Identity `yaml:"identity"`
	// Backend is what the BMC controls: none (the sim's own state), qemu or
	// docker. Empty is none.
	Backend string  `yaml:"backend"`
	Qemu    *Qemu   `yaml:"qemu"`
	Docker  *Docker `yaml:"docker"`
}

type Credentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Identity struct {
	Serial  string   `yaml:"serial"`
	UUID    string   `yaml:"uuid"`
	BMCMAC  string   `yaml:"bmcMAC"`
	NICMACs []string `yaml:"nicMACs"`
}

// Qemu shapes the VM of a qemu machine. Zero values take the driver's
// defaults.
type Qemu struct {
	// Tap attaches the VM's NIC to this existing tap device, with the first
	// of the machine's nicMACs as its address. Empty is user-mode networking.
	Tap       string `yaml:"tap"`
	MemoryMiB int    `yaml:"memoryMiB"`
	CPUs      int    `yaml:"cpus"`
	DiskGiB   int    `yaml:"diskGiB"`
	// DiskSerial is the serial of the VM's disk, which the BMC's drive
	// inventory reports as well.
	DiskSerial string `yaml:"diskSerial"`
	// Accel is the accelerator; empty is kvm.
	Accel string `yaml:"accel"`
	// HostIP and ForwardPorts forward guest TCP ports in user-mode
	// networking.
	HostIP       string `yaml:"hostIP"`
	ForwardPorts []int  `yaml:"forwardPorts"`
}

// Docker shapes the container of a docker machine.
type Docker struct {
	Image   string `yaml:"image"`
	Network string `yaml:"network"`
	IP      string `yaml:"ip"`
}

// Backends.
const (
	BackendNone   = "none"
	BackendQemu   = "qemu"
	BackendDocker = "docker"
)

// Load reads and validates a fleet file. Unknown keys are errors: a misspelt
// option must not silently become a default.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a fleet file's content.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	for i := range cfg.Machines {
		m := &cfg.Machines[i]
		if m.Personality == "" {
			m.Personality = "generic"
		}
		if m.Backend == "" {
			m.Backend = BackendNone
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var (
	nameForm = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	macForm  = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)
	uuidForm = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
)

// validate reports everything that is wrong at once, so that a file is fixed
// in one pass.
func (c *Config) validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(c.Machines) == 0 {
		bad("no machines")
	}
	if c.Media != nil {
		if _, _, err := net.SplitHostPort(c.Media.Listen); err != nil {
			bad("media: listen %q is not host:port", c.Media.Listen)
		}
		if c.Media.Dir == "" {
			bad("media: dir is required")
		}
	}
	// Identity facts tell machines apart, so no two machines may share one.
	seen := map[string]string{}
	unique := func(machine, what, value string) {
		if value == "" {
			return
		}
		key := what + " " + value
		if other, dup := seen[key]; dup {
			bad("machine %q: %s is also used by machine %q", machine, key, other)
			return
		}
		seen[key] = machine
	}
	for i, m := range c.Machines {
		name := m.Name
		if !nameForm.MatchString(name) {
			bad("machine %d: name %q must be lower-case letters, digits and dashes", i+1, name)
			name = fmt.Sprintf("#%d", i+1)
		}
		unique(name, "name", m.Name)
		if key, err := listenKey(m.Listen); err != nil {
			bad("machine %q: listen %q is not host:port", name, m.Listen)
		} else if key != "" { // port 0 is "any free port" and never clashes
			unique(name, "listen address", key)
		}
		if (m.Credentials.Username == "") != (m.Credentials.Password == "") {
			bad("machine %q: credentials need both a username and a password", name)
		}
		if m.Identity.UUID != "" && !uuidForm.MatchString(m.Identity.UUID) {
			bad("machine %q: uuid %q is not a UUID", name, m.Identity.UUID)
		}
		unique(name, "serial", m.Identity.Serial)
		unique(name, "uuid", m.Identity.UUID)
		for _, mac := range append([]string{m.Identity.BMCMAC}, m.Identity.NICMACs...) {
			if mac == "" {
				continue
			}
			if !macForm.MatchString(mac) {
				bad("machine %q: %q is not a MAC address", name, mac)
			}
			// Compared in one case: 0A and 0a are the same NIC.
			unique(name, "MAC", strings.ToLower(mac))
		}

		switch m.Backend {
		case BackendNone:
		case BackendQemu:
			if c.StateDir == "" {
				bad("machine %q: the qemu backend needs stateDir", name)
			}
			if q := m.Qemu; q != nil && q.Tap != "" {
				unique(name, "tap device", q.Tap)
				if len(m.Identity.NICMACs) == 0 {
					bad("machine %q: a tap NIC takes its MAC from identity.nicMACs, which is empty", name)
				}
				if len(q.ForwardPorts) > 0 {
					bad("machine %q: forwardPorts need user-mode networking, not a tap device", name)
				}
			}
		case BackendDocker:
			if m.Docker == nil || m.Docker.Image == "" {
				bad("machine %q: the docker backend needs docker.image", name)
			}
		default:
			bad("machine %q: backend %q is not one of none, qemu, docker", name, m.Backend)
		}
		if m.Qemu != nil && m.Backend != BackendQemu {
			bad("machine %q: qemu settings on a %s backend", name, m.Backend)
		}
		if m.Docker != nil && m.Backend != BackendDocker {
			bad("machine %q: docker settings on a %s backend", name, m.Backend)
		}
	}
	return errors.Join(errs...)
}

// listenKey is the address a listener binds, whatever its spelling, so that
// two machines cannot claim one port as 127.0.0.1:1 and 127.0.0.1:01, or as
// [::1]:1 and [0::1]:1: the port as a number and an IP host in its canonical
// form. It is empty for port 0, which never clashes.
func listenKey(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(port); err == nil {
		if n == 0 {
			return "", nil
		}
		port = strconv.Itoa(n)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, port), nil
}
