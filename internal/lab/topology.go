package lab

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

// GenerateTopology builds the topology of a tree from control/inventory,
// control/cabling.yaml and .pk/emulation.yaml. With cluster set, only the
// machines of that cluster are in it (switch uplinks stay).
func GenerateTopology(m *Model, cluster, ref string) *Topology {
	t := &Topology{Name: cluster, Cluster: cluster, Ref: ref, SHA: m.SHA, Substrate: "containerlab", Uplink: "nat",
		Machines: []TopologyMachine{}, Networks: []TopologyNetwork{}, Links: []TopologyLink{}}
	if t.Name == "" {
		t.Name = "site"
	}
	if m.Emulation == nil {
		t.Problems = append(t.Problems, kinds.Problem{Path: ".pk/emulation.yaml", Severity: kinds.SevWarning, Source: kinds.SourceKind,
			Message: "no emulation profile: machines are not emulated"})
	} else {
		if m.Emulation.Spec.Substrate != "" {
			t.Substrate = m.Emulation.Spec.Substrate
		}
		if m.Emulation.Spec.Uplink != "" {
			t.Uplink = m.Emulation.Spec.Uplink
		}
		for _, n := range m.Networks() {
			tn := TopologyNetwork{Name: n.Name, Emulate: n.Emulate}
			spec := m.Emulation.Spec.Networks[n.Name]
			tn.VLAN = spec.VLAN
			if spec.IPv4 != nil {
				tn.CIDR = spec.IPv4.CIDR
			}
			if spec.IPv6 != nil {
				tn.IPv6Prefix = spec.IPv6.Prefix
			}
			t.Networks = append(t.Networks, tn)
		}
	}
	names := make([]string, 0, len(m.Inv.Nodes))
	for name, n := range m.Inv.Nodes {
		if cluster != "" && n.Spec.Cluster != cluster {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n := m.Inv.Nodes[name]
		doc := m.Inv.NodeDocs[name]
		tm := TopologyMachine{Name: name, Cluster: n.Spec.Cluster, Role: string(n.Spec.Role), Model: n.Spec.Model,
			Rack: n.Spec.Location.Rack, Unit: n.Spec.Location.Unit, NICs: []TopologyNIC{}}
		tm.BMC.Network = "mgmt"
		tm.BMC.MAC = n.Spec.BMC.MAC
		if l := m.BMCLink(name); l != nil {
			tm.BMC.Port = fmt.Sprintf("%s %d", l.Switch, l.SwPort)
			if l.Network != "" {
				tm.BMC.Network = l.Network
			}
		}
		r := m.Rule(n)
		if r != nil {
			tm.Emulate = r.Emulate == nil || *r.Emulate
			tm.BMC.Fixture = r.BMC.Fixture
			tm.Machine = TopologyHost{Backend: r.Machine.Backend, CPUs: r.Machine.CPUs, MemoryMiB: r.Machine.MemoryMiB, Firmware: r.Machine.Firmware}
			for _, d := range r.Disks {
				tm.Machine.Disks = append(tm.Machine.Disks, TopologyDisk{Name: d.Name, SizeGiB: d.SizeGiB})
			}
		} else {
			tm.Machine = TopologyHost{Backend: "none"}
			if m.Emulation != nil {
				p := kinds.Problem{Path: docPath(doc), Severity: kinds.SevWarning, Source: kinds.SourceKind, Field: "spec.model",
					Message: fmt.Sprintf("%s: no emulation rule matches model %q", name, n.Spec.Model)}
				if doc != nil {
					p.Line, p.Col = kindutil.Pos(doc, "spec.model")
				}
				t.Problems = append(t.Problems, p)
			}
		}
		if len(tm.Machine.Disks) == 0 && tm.Machine.Backend == "qemu" {
			tm.Machine.Disks = []TopologyDisk{{Name: "system", SizeGiB: 32}}
		}
		if len(tm.Machine.Disks) > 0 {
			tm.Machine.Disks[0].Serial = n.Spec.Serial
		}
		for _, l := range m.Links(name) {
			peer := fmt.Sprintf("%s %d", l.Switch, l.SwPort)
			t.Links = append(t.Links, TopologyLink{A: name + ":" + l.Port, B: fmt.Sprintf("%s:%d", l.Switch, l.SwPort), Cable: l.Cable})
			if l.Port == "bmc" {
				continue
			}
			tm.NICs = append(tm.NICs, TopologyNIC{Name: l.Port, Network: l.Network, MAC: nicMAC(name, l.Port), Peer: peer, Cable: l.Cable})
		}
		for _, pc := range m.Inv.CheckPorts(name) {
			if pc.OK {
				continue
			}
			p := kinds.Problem{Path: docPath(doc), Severity: kinds.SevError, Source: kinds.SourceKind, Field: pc.Field,
				Message: name + ": " + pc.Message}
			if doc != nil {
				p.Line, p.Col = kindutil.Pos(doc, pc.Field)
			}
			t.Problems = append(t.Problems, p)
		}
		t.Machines = append(t.Machines, tm)
	}
	for _, d := range m.Repo.ByKind("CablePlan") {
		ups, _ := kindutil.Get(d.Data, "spec", "uplinks").([]any)
		for _, u := range ups {
			um, _ := u.(map[string]any)
			t.Links = append(t.Links, TopologyLink{
				A:     fmt.Sprintf("%s:%d", kindutil.Str(um, "a", "switch"), kindutil.Int(um, "a", "port")),
				B:     fmt.Sprintf("%s:%d", kindutil.Str(um, "b", "switch"), kindutil.Int(um, "b", "port")),
				Cable: kindutil.Str(um, "id"),
			})
		}
	}
	return t
}

// Summary is a one-line description: "defiant: 13 machines, 3 networks".
func (t *Topology) Summary() string {
	emulated := 0
	var skip []string
	for _, n := range t.Networks {
		if n.Emulate {
			emulated++
		} else {
			skip = append(skip, n.Name)
		}
	}
	s := fmt.Sprintf("%s: %d machines, %d networks", t.Name, len(t.Machines), emulated)
	if len(skip) > 0 {
		s += " (" + strings.Join(skip, ", ") + " not emulated)"
	}
	return s
}

// Scale keeps only the named machines (a scaled twin), their links and the
// links between switches.
func (t *Topology) Scale(machines []string) {
	ms := []TopologyMachine{}
	keep := map[string]bool{}
	for _, tm := range t.Machines {
		if contains(machines, tm.Name) {
			ms = append(ms, tm)
			keep[tm.Name] = true
		}
	}
	all := map[string]bool{}
	for _, tm := range t.Machines {
		all[tm.Name] = true
	}
	links := []TopologyLink{}
	for _, l := range t.Links {
		a, _, _ := strings.Cut(l.A, ":")
		b, _, _ := strings.Cut(l.B, ":")
		if (all[a] && !keep[a]) || (all[b] && !keep[b]) {
			continue
		}
		links = append(links, l)
	}
	t.Machines, t.Links = ms, links
}

// Machine returns a machine of the topology, or nil.
func (t *Topology) Machine(name string) *TopologyMachine {
	for i := range t.Machines {
		if t.Machines[i].Name == name {
			return &t.Machines[i]
		}
	}
	return nil
}

func docPath(d *kinds.Doc) string {
	if d == nil {
		return ""
	}
	return d.Path
}

// nicMAC is a stable, locally administered MAC for an emulated NIC.
func nicMAC(machine, port string) string {
	h := sha256.Sum256([]byte(machine + "/" + port))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}
