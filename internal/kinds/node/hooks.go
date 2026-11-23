package node

import (
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

// RoleWords names a role in the semantic diff.
func RoleWords(r Role) string {
	switch r {
	case RoleWorker:
		return "compute node"
	case RoleStorage:
		return "storage node"
	case RoleControl:
		return "control node"
	case RoleRouter:
		return "router"
	}
	return "node"
}

// LocationText is "GC5A-6Z U6".
func LocationText(l Location) string {
	if l.Rack == "" {
		return "no location"
	}
	return fmt.Sprintf("%s U%d", l.Rack, l.Unit)
}

// PeerText is "data-a 37".
func PeerText(p PortPeer) string { return fmt.Sprintf("%s %d", p.Switch, p.Port) }

// render emits the Machine of a node for its cluster.
func render(ctx kinds.RenderContext, d *kinds.Doc) ([]kinds.Object, error) {
	n, err := FromDoc(d)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Path, err)
	}
	if n.Spec.Cluster != ctx.Cluster {
		return nil, nil
	}
	m := Machine{
		APIVersion: MachineAPIVersion,
		Kind:       MachineKind,
		Metadata: kinds.Metadata{
			Name:   n.Metadata.Name,
			Labels: map[string]string{"metal.pk.io/cluster": n.Spec.Cluster, "metal.pk.io/role": string(n.Spec.Role), "inventory.pk.io/rack": n.Spec.Location.Rack},
		},
		Spec: MachineSpec{
			Role:     n.Spec.Role,
			Location: LocationText(n.Spec.Location),
			BMC:      MachineBMC{MAC: n.Spec.BMC.MAC},
			Ports:    []MachinePort{},
		},
	}
	for k, v := range n.Metadata.Labels {
		m.Metadata.Labels[k] = v
	}
	for _, p := range n.Spec.Ports {
		m.Spec.Ports = append(m.Spec.Ports, MachinePort{Name: p.Name, Network: p.Network, Peer: PeerText(p.Peer)})
	}
	body, err := toMap(m)
	if err != nil {
		return nil, err
	}
	return []kinds.Object{{
		ObjectRef: kinds.ObjectRef{Cluster: ctx.Cluster, APIVersion: MachineAPIVersion, Kind: MachineKind, Name: n.Metadata.Name},
		Body:      body,
		Source:    kinds.SourceRef{Path: d.Path, Line: d.Line},
	}}, nil
}

func toMap(v any) (map[string]any, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, yaml.Unmarshal(b, &m)
}

// describe is the semantic diff of a Node.
func describe(old, new *kinds.Doc) []kinds.SemanticLine {
	line := func(d *kinds.Doc, subject, text string, op kinds.Op, field string) kinds.SemanticLine {
		l, _ := kindutil.Pos(d, field)
		return kinds.SemanticLine{Subject: subject, Text: text, Op: op, Path: d.Path, Line: l, Kind: Name}
	}
	switch {
	case old == nil && new == nil:
		return nil
	case old == nil:
		n, err := FromDoc(new)
		if err != nil {
			return nil
		}
		return []kinds.SemanticLine{line(new, new.Name, fmt.Sprintf("new %s in %s, U%d", RoleWords(n.Spec.Role), n.Spec.Location.Rack, n.Spec.Location.Unit), kinds.OpAdd, "")}
	case new == nil:
		n, err := FromDoc(old)
		if err != nil {
			return nil
		}
		return []kinds.SemanticLine{{Subject: old.Name, Text: fmt.Sprintf("%s removed from %s, U%d", RoleWords(n.Spec.Role), n.Spec.Location.Rack, n.Spec.Location.Unit), Op: kinds.OpDelete, Path: old.Path, Kind: Name}}
	}
	a, err1 := FromDoc(old)
	b, err2 := FromDoc(new)
	if err1 != nil || err2 != nil {
		return nil
	}
	name := new.Name
	var out []kinds.SemanticLine
	add := func(subject, text, field string) {
		out = append(out, line(new, subject, text, kinds.OpChange, field))
	}
	if old.APIVersion != new.APIVersion {
		add(name, fmt.Sprintf("stored as %s (was %s)", new.Version(), old.Version()), "apiVersion")
	}
	if a.Spec.Cluster != b.Spec.Cluster {
		add(name, fmt.Sprintf("cluster %s to %s", a.Spec.Cluster, b.Spec.Cluster), "spec.cluster")
	}
	if a.Spec.Role != b.Spec.Role {
		add(name, fmt.Sprintf("role %s to %s", a.Spec.Role, b.Spec.Role), "spec.role")
	}
	if a.Spec.Location != b.Spec.Location {
		if a.Spec.Location.Rack == b.Spec.Location.Rack && a.Spec.Location.Unit == b.Spec.Location.Unit {
			add(name, fmt.Sprintf("height %dU to %dU", a.Spec.Location.Size, b.Spec.Location.Size), "spec.location")
		} else {
			add(name, fmt.Sprintf("moves from %s to %s", LocationText(a.Spec.Location), LocationText(b.Spec.Location)), "spec.location")
		}
	}
	if a.Spec.Model != b.Spec.Model {
		add(name, fmt.Sprintf("model %s to %s", orNone(a.Spec.Model), orNone(b.Spec.Model)), "spec.model")
	}
	if a.Spec.Serial != b.Spec.Serial {
		if a.Spec.Serial == "" {
			add(name, fmt.Sprintf("serial %s from the asset DB", b.Spec.Serial), "spec.serial")
		} else {
			add(name, fmt.Sprintf("new serial %s (was %s)", orNone(b.Spec.Serial), a.Spec.Serial), "spec.serial")
		}
	}
	if a.Spec.BMC.MAC != b.Spec.BMC.MAC {
		add(name, fmt.Sprintf("BMC MAC %s to %s", orNone(a.Spec.BMC.MAC), orNone(b.Spec.BMC.MAC)), "spec.bmc.mac")
	}
	// Ports by name.
	oldPorts := map[string]Port{}
	for _, p := range a.Spec.Ports {
		oldPorts[p.Name] = p
	}
	for i, p := range b.Spec.Ports {
		field := fmt.Sprintf("spec.ports[%d]", i)
		subject := name + " " + p.Name
		o, ok := oldPorts[p.Name]
		delete(oldPorts, p.Name)
		if !ok {
			out = append(out, line(new, subject, fmt.Sprintf("new port on %s, cable %s", PeerText(p.Peer), p.Cable), kinds.OpAdd, field))
			continue
		}
		if o.Peer != p.Peer {
			add(subject, fmt.Sprintf("%s to %s", PeerText(o.Peer), PeerText(p.Peer)), field+".peer")
		}
		if o.Cable != p.Cable {
			add(subject, fmt.Sprintf("cable %s to %s", o.Cable, p.Cable), field+".cable")
		}
		if o.Network != p.Network {
			add(subject, fmt.Sprintf("network %s to %s", o.Network, p.Network), field+".network")
		}
	}
	var gone []string
	for n := range oldPorts {
		gone = append(gone, n)
	}
	sort.Strings(gone)
	for _, n := range gone {
		p := oldPorts[n]
		out = append(out, kinds.SemanticLine{Subject: name + " " + n, Text: fmt.Sprintf("port removed (was %s, cable %s)", PeerText(p.Peer), p.Cable), Op: kinds.OpDelete, Path: new.Path, Kind: Name})
	}
	out = append(out, labelLines(new, a.Metadata.Labels, b.Metadata.Labels)...)
	return out
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// labelLines describes label changes ("label firmware 2.3 to 2.4").
func labelLines(d *kinds.Doc, a, b map[string]string) []kinds.SemanticLine {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var ks []string
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []kinds.SemanticLine
	for _, k := range ks {
		ov, inOld := a[k]
		nv, inNew := b[k]
		l, _ := kindutil.Pos(d, "metadata.labels."+k)
		sl := kinds.SemanticLine{Subject: d.Name, Op: kinds.OpChange, Path: d.Path, Line: l, Kind: d.Kind}
		switch {
		case inOld && !inNew:
			sl.Text = fmt.Sprintf("label %s removed (was %s)", k, ov)
		case !inOld && inNew:
			sl.Text = fmt.Sprintf("label %s=%s", k, nv)
		case ov != nv:
			sl.Text = fmt.Sprintf("label %s %s to %s", k, ov, nv)
		default:
			continue
		}
		out = append(out, sl)
	}
	return out
}

// validate runs the cross-file checks of all nodes.
func validate(r kinds.Repo) []kinds.Problem {
	inv := LoadInventory(r)
	var out []kinds.Problem
	names := make([]string, 0, len(inv.Nodes))
	for n := range inv.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		n, d := inv.Nodes[name], inv.NodeDocs[name]
		alpha := d.Version() == "v1alpha1"
		f := func(beta, old string) string {
			if alpha {
				return old
			}
			return beta
		}
		if n.Spec.Cluster == "" {
			out = append(out, kindutil.Problem(d, "spec.cluster", kinds.SevWarning, "empty", "spec.cluster is empty: the node is in no cluster"))
		} else if r.Find("Cluster", n.Spec.Cluster) == nil {
			out = append(out, kindutil.RefProblem(d, "spec.cluster", kinds.SevError, "unknown cluster %s", n.Spec.Cluster))
		}
		loc := n.Spec.Location
		rackField := f("spec.location.rack", "spec.rack")
		unitField := f("spec.location.unit", "spec.unit")
		if loc.Rack == "" {
			out = append(out, kindutil.Problem(d, rackField, kinds.SevWarning, "empty", "%s is empty: pick a rack", rackField))
		}
		if loc.Rack != "" {
			if _, ok := inv.Racks[strings.ToUpper(loc.Rack)]; !ok {
				out = append(out, kindutil.RefProblem(d, rackField, kinds.SevError, "unknown rack %s", loc.Rack))
			} else {
				units := inv.RackUnits[strings.ToUpper(loc.Rack)]
				top := loc.Unit + max(loc.Size, 1) - 1
				if loc.Unit >= 1 && top > units {
					out = append(out, kindutil.Problem(d, unitField, kinds.SevError, "units", "units %d to %d do not fit into %s (%dU)", loc.Unit, top, loc.Rack, units))
				}
				o := Occupant{Kind: Name, Name: name, Rack: loc.Rack, Unit: loc.Unit, Size: loc.Size, Doc: d}
				if over := inv.Overlaps(o); len(over) > 0 && loc.Unit >= 1 {
					x := over[0]
					u := max(loc.Unit, x.Unit)
					out = append(out, kindutil.Problem(d, unitField, kinds.SevError, "unit-occupied", "unit %d in %s is used by %s", u, loc.Rack, x.Name))
				}
			}
		}
		hasBMC := false
		seenPort := map[string]bool{}
		for i, p := range n.Spec.Ports {
			pf := fmt.Sprintf("spec.ports[%d]", i)
			if p.Name == "bmc" {
				hasBMC = true
			}
			if seenPort[p.Name] {
				out = append(out, kindutil.Problem(d, pf+".name", kinds.SevError, "port-name", "port %s is listed twice", p.Name))
			}
			seenPort[p.Name] = true
		}
		for _, u := range inv.Ports {
			if u.OwnerKind != Name || u.Owner != name {
				continue
			}
			msg, sub := inv.portProblem(u)
			if msg == "" {
				continue
			}
			field := u.Field
			switch sub {
			case "switch", "port":
				field += ".peer." + sub
			case "network", "cable":
				field += "." + sub
			}
			if sub == "switch" {
				out = append(out, kindutil.RefProblem(d, field, kinds.SevError, "%s", msg))
				continue
			}
			sev := kinds.SevError
			if sub == "network" {
				sev = kinds.SevWarning
			}
			out = append(out, kindutil.Problem(d, field, sev, "port-"+sub, "%s", msg))
		}
		if !hasBMC && len(n.Spec.Ports) > 0 {
			out = append(out, kindutil.Problem(d, "spec.ports", kinds.SevWarning, "no-bmc", "%s has no bmc port; the lab cannot power it", name))
		}
	}
	return out
}

// Skeleton is a new Node file with schema defaults.
func Skeleton(name string) ([]byte, error) {
	return []byte(fmt.Sprintf(`apiVersion: %s/v1beta1
kind: Node
metadata:
  name: %s
spec:
  cluster: ""
  role: worker
  location:
    rack: ""
    unit: 1
    size: 2
  bmc:
    mac: ""
  ports: []
`, Group, name)), nil
}
