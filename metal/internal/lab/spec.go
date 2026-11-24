// Package lab turns the virtual lab's one spec (lab/lab.yaml) into every file
// the lab runs from: the fleet file the simulated BMCs serve, the manifests a
// person would commit for a real site (Assets, TalosCluster and TalosNodes,
// HostClaims), the tap list for the lab network and the operator's ledger.
//
// Those files must agree on serials, MACs, addresses and ports. Each machine's
// identity is derived from its index, so they agree by construction, and the
// size of the lab is a number.
package lab

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v4"
)

// Spec is lab/lab.yaml.
//
//	cluster: { name: lab, talosVersion: v1.14.6, kubernetesVersion: v1.36.2, vip: 10.77.0.10 }
//	controlPlanes: 1
//	workers: 2
//	controlPlane: { memoryMiB: 4096, cpus: 2 }
//	worker: { memoryMiB: 2048, cpus: 2 }
//	nodes:
//	  node-3: { personality: supermicro-x12 }
//	patches:
//	  - inline: |
//	      apiVersion: v1alpha1
//	      kind: ResolverConfig
//	      ...
type Spec struct {
	Cluster       Cluster `yaml:"cluster"`
	ControlPlanes int     `yaml:"controlPlanes"`
	Workers       int     `yaml:"workers"`
	// ControlPlane and Worker are what every machine of that role gets.
	ControlPlane Machine `yaml:"controlPlane"`
	Worker       Machine `yaml:"worker"`
	// Nodes override the role's values for single machines, by name
	// (node-1, node-2, ...). An entry for a machine the current size does not
	// have is ignored, so that a smaller lab needs no other spec.
	Nodes map[string]Machine `yaml:"nodes"`
	// Patches are Talos configuration patches for every node, copied into
	// the TalosCluster as they are.
	Patches []Patch `yaml:"patches"`
}

// Cluster is what the TalosCluster says about the cluster as a whole.
type Cluster struct {
	Name              string `yaml:"name"`
	TalosVersion      string `yaml:"talosVersion"`
	KubernetesVersion string `yaml:"kubernetesVersion"`
	// VIP is the address the Kubernetes API answers on. The control-plane
	// machines share it; with three of them it moves to a survivor when the
	// holder goes down.
	VIP string `yaml:"vip"`
}

// Machine is the shape of one machine. In a node override, zero values keep
// the role's.
type Machine struct {
	// Personality is the BMC dialect the simulator speaks (an embedded
	// fixture name). Empty is generic.
	Personality string `yaml:"personality"`
	MemoryMiB   int    `yaml:"memoryMiB"`
	CPUs        int    `yaml:"cpus"`
}

// Patch is one Talos configuration patch, given inline.
type Patch struct {
	Inline string `yaml:"inline"`
}

// The lab network. lab/net.sh (PK_LAB_ADDRESS) and lab/kind.yaml use the same
// gateway address; it is fixed here rather than in the spec so that the three
// cannot drift apart.
var (
	labGateway = netip.MustParseAddr("10.77.0.1")
	labPrefix  = netip.MustParsePrefix("10.77.0.0/24")
)

// MaxNodes bounds the lab. Machine n gets the address .(10+n), and its NIC
// MAC and BMC MAC end in one octet each; 99 machines stay well inside all of
// that, and far above what a workstation runs.
const MaxNodes = 99

var (
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	versionForm = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	nodeName    = regexp.MustCompile(`^node-([1-9][0-9]*)$`)
	sizeForm    = regexp.MustCompile(`^([0-9]+)cp\+([0-9]+)w$`)
)

// Load reads and validates a spec file.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse decodes and validates a spec. Unknown keys are errors: a misspelt
// option must not silently become a default.
func Parse(raw []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// ParseSize reads the size knob, <control planes>cp+<workers>w (3cp+2w).
func ParseSize(size string) (controlPlanes, workers int, err error) {
	m := sizeForm.FindStringSubmatch(size)
	if m == nil {
		return 0, 0, fmt.Errorf("size %q is not <control planes>cp+<workers>w, for example 3cp+2w", size)
	}
	controlPlanes, _ = strconv.Atoi(m[1])
	workers, _ = strconv.Atoi(m[2])
	return controlPlanes, workers, nil
}

// Resize sets the machine counts from the size knob and validates the result.
func (s *Spec) Resize(size string) error {
	cp, w, err := ParseSize(size)
	if err != nil {
		return err
	}
	s.ControlPlanes, s.Workers = cp, w
	return s.Validate()
}

// Validate reports everything that is wrong at once, so that a spec is fixed
// in one pass.
func (s *Spec) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !dnsLabel.MatchString(s.Cluster.Name) {
		bad("cluster.name %q must be lower-case letters, digits and dashes", s.Cluster.Name)
	}
	if !versionForm.MatchString(s.Cluster.TalosVersion) {
		bad("cluster.talosVersion %q is not a version like v1.14.6", s.Cluster.TalosVersion)
	}
	if !versionForm.MatchString(s.Cluster.KubernetesVersion) {
		bad("cluster.kubernetesVersion %q is not a version like v1.36.2", s.Cluster.KubernetesVersion)
	}

	switch {
	case s.ControlPlanes < 1:
		bad("controlPlanes is %d: a cluster needs at least one", s.ControlPlanes)
	case s.ControlPlanes%2 == 0:
		// etcd needs a majority of its members. Two members fail when either
		// one does, which is worse than one; four tolerate no more than three.
		bad("controlPlanes is %d: use an odd number (1, 3 or 5), etcd needs a majority", s.ControlPlanes)
	}
	if s.Workers < 0 {
		bad("workers is %d: it cannot be negative", s.Workers)
	}
	if n := s.ControlPlanes + s.Workers; n > MaxNodes {
		bad("%d machines is more than the lab supports (%d)", n, MaxNodes)
	}

	if vip, err := netip.ParseAddr(s.Cluster.VIP); err != nil || !labPrefix.Contains(vip) {
		bad("cluster.vip %q is not an address in the lab network %s", s.Cluster.VIP, labPrefix)
	} else if vip == labGateway {
		bad("cluster.vip %s is the workstation's address on the lab bridge", vip)
	} else if n := s.ControlPlanes + s.Workers; n <= MaxNodes {
		for i := 1; i <= n; i++ {
			if nodeAddress(i) == vip {
				bad("cluster.vip %s is the address of node-%d; machines take .11 upwards, pick one below", vip, i)
			}
		}
	}

	checkMachine := func(where string, m Machine, override bool) {
		// In a node override, zero keeps the role's value.
		keeps := func(v int) bool { return override && v == 0 }
		if !keeps(m.MemoryMiB) && m.MemoryMiB < 1024 {
			bad("%s.memoryMiB is %d: Talos needs at least 1024 (a control plane wants 2048)", where, m.MemoryMiB)
		}
		if !keeps(m.CPUs) && m.CPUs < 1 {
			bad("%s.cpus is %d: at least 1", where, m.CPUs)
		}
		if m.Personality != "" && !dnsLabel.MatchString(m.Personality) {
			bad("%s.personality %q is not a fixture name like generic or supermicro-x12", where, m.Personality)
		}
	}
	checkMachine("controlPlane", s.ControlPlane, false)
	checkMachine("worker", s.Worker, false)
	for _, name := range sortedKeys(s.Nodes) {
		if !nodeName.MatchString(name) {
			bad("nodes: %q is not a machine name; machines are named node-1, node-2, ...", name)
			continue
		}
		checkMachine("nodes."+name, s.Nodes[name], true)
	}
	for i, p := range s.Patches {
		if p.Inline == "" {
			bad("patches[%d]: inline is empty", i)
		}
	}
	return errors.Join(errs...)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
