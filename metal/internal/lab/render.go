package lab

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v4"
)

// Node is one machine of the lab with everything derived for it.
type Node struct {
	Index       int    // 1-based; every synthetic identity below derives from it
	Name        string // node-<index>
	Role        string // controlplane or worker, as the TalosNode spells it
	Personality string
	MemoryMiB   int
	CPUs        int

	Serial     string     // PKLAB0001
	CID        string     // PKLAB-0001
	UUID       string     // 9b1c0001-0000-4000-8000-000000000001
	BMCMAC     string     // 02:77:00:00:01:01
	NICMAC     string     // 52:54:00:77:00:11 (0x10 + index)
	DiskSerial string     // pklab-disk-1
	Tap        string     // pktap1
	Listen     string     // 10.77.0.1:8441, the BMC
	Address    netip.Addr // 10.77.0.11, the machine
}

// The roles, as the TalosNode and the HostClaim's labels spell them.
const (
	RoleControlPlane = "controlplane"
	RoleWorker       = "worker"
)

// Constants every machine shares. The credentials open nothing but a
// simulator; the disk is sparse, so its size costs nothing until written.
const (
	bmcUsername = "admin"
	bmcPassword = "admin-pass"
	diskGiB     = 20
	bmcPortBase = 8440
)

func nodeAddress(index int) netip.Addr {
	a := labGateway.As4()
	a[3] = byte(10 + index)
	return netip.AddrFrom4(a)
}

// Plan lists the machines: the control planes first, then the workers, each
// with its role's shape and its own override applied.
func (s *Spec) Plan() []Node {
	n := s.ControlPlanes + s.Workers
	nodes := make([]Node, 0, n)
	for i := 1; i <= n; i++ {
		role, shape := RoleControlPlane, s.ControlPlane
		if i > s.ControlPlanes {
			role, shape = RoleWorker, s.Worker
		}
		name := fmt.Sprintf("node-%d", i)
		if o, ok := s.Nodes[name]; ok {
			if o.Personality != "" {
				shape.Personality = o.Personality
			}
			if o.MemoryMiB != 0 {
				shape.MemoryMiB = o.MemoryMiB
			}
			if o.CPUs != 0 {
				shape.CPUs = o.CPUs
			}
		}
		if shape.Personality == "" {
			shape.Personality = "generic"
		}
		nodes = append(nodes, Node{
			Index:       i,
			Name:        name,
			Role:        role,
			Personality: shape.Personality,
			MemoryMiB:   shape.MemoryMiB,
			CPUs:        shape.CPUs,
			Serial:      fmt.Sprintf("PKLAB%04d", i),
			CID:         fmt.Sprintf("PKLAB-%04d", i),
			UUID:        fmt.Sprintf("9b1c%04x-0000-4000-8000-%012x", i, i),
			BMCMAC:      fmt.Sprintf("02:77:00:00:01:%02x", i),
			NICMAC:      fmt.Sprintf("52:54:00:77:00:%02x", 0x10+i),
			DiskSerial:  fmt.Sprintf("pklab-disk-%d", i),
			Tap:         fmt.Sprintf("pktap%d", i),
			Listen:      fmt.Sprintf("%s:%d", labGateway, bmcPortBase+i),
			Address:     nodeAddress(i),
		})
	}
	return nodes
}

// The files Render produces, relative to the output directory.
const (
	FileFleet    = "fleet.yaml"
	FileMachines = "manifests/10-machines.yaml"
	FileTalos    = "manifests/20-talos.yaml"
	FileClaims   = "manifests/30-claims.yaml"
	FileTaps     = "taps"
	FileLedger   = "ledger.yaml"
	FileEnv      = "lab.env"
)

// Files are rendered file contents by path relative to the output directory.
type Files map[string][]byte

// Render produces every file of the lab from a valid spec. source names the
// spec in the generated headers.
func Render(s *Spec, source string) (Files, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	patches, err := renderPatches(s.Patches)
	if err != nil {
		return nil, err
	}
	nodes := s.Plan()
	memory := 0
	for _, n := range nodes {
		memory += n.MemoryMiB
	}
	data := struct {
		Source        string
		Spec          *Spec
		Nodes         []Node
		Gateway       netip.Addr
		PrefixBits    int
		Patches       string
		Username      string
		Password      string
		DiskGiB       int
		MemoryMiB     int
		ControlPlanes []Node
		Workers       []Node
	}{
		Source:        source,
		Spec:          s,
		Nodes:         nodes,
		Gateway:       labGateway,
		PrefixBits:    labPrefix.Bits(),
		Patches:       patches,
		Username:      bmcUsername,
		Password:      bmcPassword,
		DiskGiB:       diskGiB,
		MemoryMiB:     memory,
		ControlPlanes: nodes[:s.ControlPlanes],
		Workers:       nodes[s.ControlPlanes:],
	}

	files := Files{}
	for path, tmpl := range templates {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		files[path] = buf.Bytes()
	}
	return files, nil
}

// renderPatches writes the patch list as the TalosCluster's patches: block,
// through the YAML encoder, so that any inline text (leading spaces, a
// missing final newline) comes out as the same string it went in as.
func renderPatches(patches []Patch) (string, error) {
	if len(patches) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(map[string][]Patch{"patches": patches}); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return indent(2, buf.String()), nil
}

// indent prefixes every non-empty line with n spaces and drops the final
// newline, for a block placed by the template.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// Write puts the files into dir. Each file is written to a temporary name and
// renamed, so that a reader never sees half a manifest.
func Write(dir string, files Files) error {
	for _, path := range sortedKeys(files) {
		target := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, files[path], 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
	}
	return nil
}

var templates = map[string]*template.Template{
	FileFleet:    parse(FileFleet, fleetTemplate),
	FileMachines: parse(FileMachines, machinesTemplate),
	FileTalos:    parse(FileTalos, talosTemplate),
	FileClaims:   parse(FileClaims, claimsTemplate),
	FileTaps:     parse(FileTaps, tapsTemplate),
	FileLedger:   parse(FileLedger, ledgerTemplate),
	FileEnv:      parse(FileEnv, envTemplate),
}

func parse(name, text string) *template.Template {
	return template.Must(template.New(name).Option("missingkey=error").Parse(text))
}

const fleetTemplate = `# Generated from {{.Source}}. Do not edit: change the spec and run mise run lab:up.
#
# The machines of the virtual lab, as ` + "`pk-metal sim fleet --config`" + ` serves them:
# one simulated BMC per entry, each driving one QEMU machine. All values are
# synthetic and derived from the machine's index: the MACs are locally
# administered, the addresses are the lab bridge's, and the credentials open
# nothing but a simulator.
#
# Relative paths are relative to the repository root.
stateDir: .lab/state # per machine: disk, UEFI variables, console log
mediaCache: .lab/cache # downloaded installer images, kept across lab:down
machines:
{{- range .Nodes}}
  - name: {{.Name}} # {{.Role}}
    listen: {{.Listen}}
    personality: {{.Personality}}
    credentials: { username: {{$.Username}}, password: {{$.Password}} }
    identity:
      serial: {{.Serial}}
      uuid: {{.UUID}}
      bmcMAC: "{{.BMCMAC}}"
      nicMACs: ["{{.NICMAC}}"]
    backend: qemu
    qemu: { tap: {{.Tap}}, memoryMiB: {{.MemoryMiB}}, cpus: {{.CPUs}}, diskGiB: {{$.DiskGiB}}, diskSerial: {{.DiskSerial}} }
{{- end}}
`

const machinesTemplate = `# Generated from {{.Source}}. Do not edit: change the spec and run mise run lab:up.
#
# What the organization asserts about the lab machines, and the credentials
# that open their management controllers. In a real site these are the files a
# person commits when a machine is racked.
#
# The operator joins each Asset's bmcMac against its ledger to find the
# controller, logs in with the referenced Secret, and compares the serial the
# machine reports with the one asserted here before it trusts the machine.
apiVersion: v1
kind: Secret
metadata:
  name: lab-bmc
  namespace: pk-metal
type: Opaque
stringData:
  # The simulators' credentials. They open nothing else.
  username: {{.Username}}
  password: {{.Password}}
{{- range .Nodes}}
---
apiVersion: metal.pk.io/v1alpha1
kind: Asset
metadata:
  name: {{.Name}}
spec:
  cid: {{.CID}}
  serial: {{.Serial}}
  bmcMac: "{{.BMCMAC}}"
  credentialsRef:
    name: lab-bmc
{{- end}}
`

const talosTemplate = `# Generated from {{.Source}}. Do not edit: change the spec and run mise run lab:up.
#
# The workload cluster the lab installs, on the lab bridge, the Kubernetes API
# on a virtual IP the control planes hold{{if gt (len .ControlPlanes) 1}} (it moves to another control
# plane when its holder goes down){{end}}.
#
#   VIP     {{.Spec.Cluster.VIP}}
{{- range .Nodes}}
#   {{printf "%-7s" .Name}} {{printf "%-11s" .Address.String}} {{if eq .Role "controlplane"}}control plane{{else}}worker{{end}}
{{- end}}
#
# Links and install disks are selected by what the fleet gives each machine
# (NIC MAC, disk serial), never by a device name that depends on probe order.
# The gateway is the workstation's end of the bridge, which NATs.
#
# The secrets bundle is not here and the operator never makes one. ` + "`mise run\n# lab:up`" + ` generates it once on the workstation with ` + "`talosctl gen secrets`" + ` and
# creates the Secret the cluster names below (key secrets.yaml).
#
# The TalosCluster holds what every node shares (think group_vars, or the
# %include part of a kickstart); each TalosNode holds what is per machine
# (address, install disk, role). The operator renders one Talos machine
# configuration per node from the two. A machine that is already installed does
# not pick a change up by itself: push it with talosctl
# (talosctl -n <address> patch machineconfig --patch @change.yaml).
apiVersion: os.pk.io/v1alpha1
kind: TalosCluster
metadata:
  name: {{.Spec.Cluster.Name}}
spec:
  talosVersion: {{.Spec.Cluster.TalosVersion}}
  kubernetesVersion: {{.Spec.Cluster.KubernetesVersion}}
  endpoint: https://{{.Spec.Cluster.VIP}}:6443
  controlPlane:
    vip: {{.Spec.Cluster.VIP}}
{{- if eq (len .Workers) 0}}
    # No workers: ordinary workloads run on the control planes.
    allowScheduling: true
{{- end}}
  network:
    cni:
      name: flannel
  secretsRef:
    namespace: pk-metal
    name: {{.Spec.Cluster.Name}}-talos-secrets
  schematic:
    customization:
      extraKernelArgs:
        # The simulated BMC records the machine's serial console; without this
        # the boot is invisible in .lab/state/<machine>/console.log.
        - console=ttyS0
{{- if .Patches}}
  # Talos machine configuration documents from the spec, merged into every
  # node's configuration. The operator adds nothing here on its own.
{{.Patches}}
{{- end}}
{{- range .Nodes}}
---
apiVersion: os.pk.io/v1alpha1
kind: TalosNode
metadata:
  name: {{.Name}}
spec:
  clusterRef:
    name: {{$.Spec.Cluster.Name}}
  role: {{.Role}}
  # The disk Talos installs onto; it is repartitioned and overwritten.
  installDiskSelector:
    serial: {{.DiskSerial}}
  network:
    links:
      - name: lan
        hardwareAddr: "{{.NICMAC}}"
        addresses:
          - {{.Address}}/{{$.PrefixBits}}
        routes:
          - gateway: {{$.Gateway}}
{{- end}}
`

const claimsTemplate = `# Generated from {{.Source}}. Do not edit: change the spec and run mise run lab:up.
#
# The decisions: each claim binds one machine, through its Asset, and says what
# it is to run. This is what starts provisioning.
#
#   - A claim WITH spec.os proposes installing that OS onto the machine. Once a
#     person approves (` + "`mise run lab:approve`" + `), the machine boots the
#     installer and its install disk is overwritten.
#   - Deleting a claim POWERS THE MACHINE OFF (if a boot of this claim ran on
#     it). When the machine runs its node, that power-off waits for approval.
#
# The labels select BIOS and firmware profiles; the node's role in the cluster
# comes from its TalosNode. Both come from the same role here.
{{- range $i, $n := .Nodes}}
{{- if $i}}
---
{{- end}}
apiVersion: metal.pk.io/v1alpha1
kind: HostClaim
metadata:
  name: {{.Name}}
  namespace: default
spec:
  assetRef:
    name: {{.Name}}
  labels:
    role: {{.Role}}
  os:
    nodeRef:
      name: {{.Name}}
{{- end}}
`

// One tap per line, for lab/net.sh up.
const tapsTemplate = `{{range .Nodes}}{{.Tap}}
{{end}}`

// The ledger the operator joins Assets against: BMC MAC -> the URL its
// simulator listens on.
const ledgerTemplate = `# Generated from {{.Source}}. Do not edit.
{{- range .Nodes}}
"{{.BMCMAC}}": https://{{.Listen}}
{{- end}}
`

// What lab/lab.sh needs to know, as KEY=value lines. The values are names and
// numbers the spec validates, so a reader may split on the first '='.
const envTemplate = `TALOS_CLUSTER={{.Spec.Cluster.Name}}
BUNDLE_SECRET={{.Spec.Cluster.Name}}-talos-secrets
CONTROL_PLANES={{len .ControlPlanes}}
WORKERS={{len .Workers}}
NODES={{len .Nodes}}
MEMORY_MIB={{.MemoryMiB}}
`
