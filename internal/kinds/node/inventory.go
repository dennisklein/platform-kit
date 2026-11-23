package node

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

// FromDoc decodes a Node document of any version into v1beta1.
func FromDoc(d *kinds.Doc) (*Node, error) {
	if d == nil {
		return nil, fmt.Errorf("no document")
	}
	data := d.Data
	if d.Version() == "v1alpha1" {
		var err error
		if data, err = Convert("v1alpha1", d.Data); err != nil {
			return nil, err
		}
	}
	var n Node
	tmp := &kinds.Doc{Data: data}
	if err := tmp.Decode(&n); err != nil {
		return nil, err
	}
	if n.Spec.Location.Size == 0 {
		n.Spec.Location.Size = 2
	}
	return &n, nil
}

// Convert maps a v1alpha1 Node (flat rack, unit, height) to v1beta1
// (location block). Other fields are kept.
func Convert(from string, data map[string]any) (map[string]any, error) {
	if from != "v1alpha1" {
		return nil, fmt.Errorf("Node: cannot convert from %s", from)
	}
	out := deepCopy(data).(map[string]any)
	out["apiVersion"] = Group + "/v1beta1"
	spec, _ := out["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		out["spec"] = spec
	}
	loc := map[string]any{}
	if v, ok := spec["rack"]; ok {
		loc["rack"] = v
		delete(spec, "rack")
	}
	if v, ok := spec["unit"]; ok {
		loc["unit"] = v
		delete(spec, "unit")
	}
	size := any(int64(2))
	if v, ok := spec["height"]; ok {
		size = v
		delete(spec, "height")
	}
	loc["size"] = size
	spec["location"] = loc
	return out, nil
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[k] = deepCopy(vv)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, vv := range x {
			s[i] = deepCopy(vv)
		}
		return s
	}
	return v
}

// Occupant is something that fills units of a rack.
type Occupant struct {
	Kind    string // Node, Switch, Jbod
	Name    string
	Rack    string
	Unit    int
	Size    int
	Role    string
	Cluster string
	Doc     *kinds.Doc
	// Field is the field that holds the unit (for problem positions).
	Field string
}

// Category is the rack view category of an occupant.
func (o Occupant) Category() string {
	switch o.Kind {
	case "Switch":
		return "network"
	case "Jbod":
		return "jbod"
	}
	switch Role(o.Role) {
	case RoleWorker:
		return "compute"
	case RoleStorage:
		return "storage"
	case RoleControl:
		return "control"
	case RoleRouter:
		return "network"
	}
	return "other"
}

// Top is the highest unit the occupant fills.
func (o Occupant) Top() int { return o.Unit + max(o.Size, 1) - 1 }

// PortUse is a cable on a switch port (from a node or the cable plan).
type PortUse struct {
	Switch string
	Port   int
	// Owner is the node name or the cable plan uplink id.
	Owner     string
	OwnerKind string // Node or CablePlan
	PortName  string // p1, bmc, or the uplink end (a, b)
	Cable     string
	Network   string
	Doc       *kinds.Doc
	Field     string
}

// SwitchInfo is a switch with its port count and uplink ports.
type SwitchInfo struct {
	Name    string
	Network string
	Ports   int
	Uplinks []int
	Doc     *kinds.Doc
}

// Inventory is the physical view of a tree: racks, occupants, switches and
// port use. Cross-file checks and the rack view use it.
type Inventory struct {
	Racks     map[string]*kinds.Doc // by upper-case name
	RackUnits map[string]int
	Occupants []Occupant
	Switches  map[string]SwitchInfo
	Spines    map[string]bool
	Ports     []PortUse
	Nodes     map[string]*Node
	NodeDocs  map[string]*kinds.Doc
}

// LoadInventory builds the inventory of a tree.
func LoadInventory(r kinds.Repo) *Inventory {
	inv := &Inventory{
		Racks: map[string]*kinds.Doc{}, RackUnits: map[string]int{}, Switches: map[string]SwitchInfo{},
		Spines: map[string]bool{}, Nodes: map[string]*Node{}, NodeDocs: map[string]*kinds.Doc{},
	}
	for _, d := range r.ByKind("Rack") {
		k := strings.ToUpper(d.Name)
		inv.Racks[k] = d
		u := kindutil.Int(d.Data, "spec", "units")
		if u == 0 {
			u = 47
		}
		inv.RackUnits[k] = u
	}
	for _, d := range r.ByKind("Switch") {
		s := SwitchInfo{Name: d.Name, Network: kindutil.Str(d.Data, "spec", "network"), Ports: kindutil.Int(d.Data, "spec", "ports"), Doc: d}
		if s.Ports == 0 {
			s.Ports = 48
		}
		if l, ok := kindutil.Get(d.Data, "spec", "uplinks").([]any); ok {
			for _, v := range l {
				switch x := v.(type) {
				case int64:
					s.Uplinks = append(s.Uplinks, int(x))
				case float64:
					s.Uplinks = append(s.Uplinks, int(x))
				}
			}
		}
		inv.Switches[d.Name] = s
		size := kindutil.Int(d.Data, "spec", "size")
		if size == 0 {
			size = 1
		}
		inv.Occupants = append(inv.Occupants, Occupant{Kind: "Switch", Name: d.Name, Rack: kindutil.Str(d.Data, "spec", "rack"),
			Unit: kindutil.Int(d.Data, "spec", "unit"), Size: size, Role: "switch", Doc: d, Field: "spec.unit"})
	}
	for _, d := range r.ByKind("Jbod") {
		size := kindutil.Int(d.Data, "spec", "location", "size")
		if size == 0 {
			size = 4
		}
		inv.Occupants = append(inv.Occupants, Occupant{Kind: "Jbod", Name: d.Name, Rack: kindutil.Str(d.Data, "spec", "location", "rack"),
			Unit: kindutil.Int(d.Data, "spec", "location", "unit"), Size: size, Role: "jbod", Doc: d, Field: "spec.location.unit"})
	}
	for _, d := range r.ByKind(Name) {
		if d.Group() != Group {
			continue
		}
		n, err := FromDoc(d)
		if err != nil {
			continue
		}
		inv.Nodes[d.Name] = n
		inv.NodeDocs[d.Name] = d
		field := "spec.location.unit"
		if d.Version() == "v1alpha1" {
			field = "spec.unit"
		}
		inv.Occupants = append(inv.Occupants, Occupant{Kind: Name, Name: d.Name, Rack: n.Spec.Location.Rack, Unit: n.Spec.Location.Unit,
			Size: n.Spec.Location.Size, Role: string(n.Spec.Role), Cluster: n.Spec.Cluster, Doc: d, Field: field})
		for i, p := range n.Spec.Ports {
			inv.Ports = append(inv.Ports, PortUse{Switch: p.Peer.Switch, Port: p.Peer.Port, Owner: d.Name, OwnerKind: Name,
				PortName: p.Name, Cable: p.Cable, Network: p.Network, Doc: d, Field: fmt.Sprintf("spec.ports[%d]", i)})
		}
	}
	for _, d := range r.ByKind("CablePlan") {
		for _, s := range kindutil.Strings(d.Data, "spec", "spines") {
			inv.Spines[s] = true
		}
		ups, _ := kindutil.Get(d.Data, "spec", "uplinks").([]any)
		for i, u := range ups {
			m, _ := u.(map[string]any)
			id := kindutil.Str(m, "id")
			for _, end := range []string{"a", "b"} {
				inv.Ports = append(inv.Ports, PortUse{Switch: kindutil.Str(m, end, "switch"), Port: kindutil.Int(m, end, "port"),
					Owner: id, OwnerKind: "CablePlan", PortName: end, Cable: id, Doc: d, Field: fmt.Sprintf("spec.uplinks[%d].%s", i, end)})
			}
		}
	}
	sort.SliceStable(inv.Occupants, func(i, j int) bool {
		if inv.Occupants[i].Rack != inv.Occupants[j].Rack {
			return inv.Occupants[i].Rack < inv.Occupants[j].Rack
		}
		return inv.Occupants[i].Unit < inv.Occupants[j].Unit
	})
	return inv
}

// InRack returns the occupants of a rack, lowest unit first.
func (inv *Inventory) InRack(rack string) []Occupant {
	var out []Occupant
	for _, o := range inv.Occupants {
		if strings.EqualFold(o.Rack, rack) {
			out = append(out, o)
		}
	}
	return out
}

// Overlaps returns the other occupants that share units with o.
func (inv *Inventory) Overlaps(o Occupant) []Occupant {
	var out []Occupant
	for _, x := range inv.Occupants {
		if x.Doc == o.Doc && x.Name == o.Name {
			continue
		}
		if strings.EqualFold(x.Rack, o.Rack) && x.Unit <= o.Top() && o.Unit <= x.Top() {
			out = append(out, x)
		}
	}
	return out
}

// PortUsers returns the other cables on the same switch port.
func (inv *Inventory) PortUsers(u PortUse) []PortUse {
	var out []PortUse
	for _, x := range inv.Ports {
		if x.Owner == u.Owner && x.PortName == u.PortName && x.OwnerKind == u.OwnerKind {
			continue
		}
		if x.Switch == u.Switch && x.Port == u.Port && u.Port != 0 {
			out = append(out, x)
		}
	}
	return out
}

// CableUsers returns the other ends that use the same cable id.
func (inv *Inventory) CableUsers(u PortUse) []PortUse {
	var out []PortUse
	for _, x := range inv.Ports {
		if x.Owner == u.Owner && x.OwnerKind == u.OwnerKind {
			continue
		}
		if x.Cable != "" && x.Cable == u.Cable {
			out = append(out, x)
		}
	}
	return out
}

// Free returns the free units of a rack.
func (inv *Inventory) Free(rack string) []int {
	units := inv.RackUnits[strings.ToUpper(rack)]
	used := map[int]bool{}
	for _, o := range inv.InRack(rack) {
		for u := o.Unit; u <= o.Top(); u++ {
			used[u] = true
		}
	}
	out := []int{}
	for u := 1; u <= units; u++ {
		if !used[u] {
			out = append(out, u)
		}
	}
	return out
}

// PortCheck is the check result of one node port.
type PortCheck struct {
	Port    string
	Cable   string
	Peer    string // data-a 37
	Network string
	OK      bool
	Message string
	Field   string
}

// CheckPorts checks the ports of a node: switch known, port in range, not
// an uplink port, not used by another cable, cable id unique, network
// matches the switch.
func (inv *Inventory) CheckPorts(name string) []PortCheck {
	var out []PortCheck
	for _, u := range inv.Ports {
		if u.OwnerKind != Name || u.Owner != name {
			continue
		}
		c := PortCheck{Port: u.PortName, Cable: u.Cable, Peer: fmt.Sprintf("%s %d", u.Switch, u.Port), Network: u.Network, OK: true, Field: u.Field}
		c.Message, _ = inv.portProblem(u)
		c.OK = c.Message == ""
		out = append(out, c)
	}
	return out
}

// PortProblem returns the first problem of a port use and the sub field it
// is about (switch, port, network, cable), or "".
func (inv *Inventory) PortProblem(u PortUse) (string, string) { return inv.portProblem(u) }

func (inv *Inventory) portProblem(u PortUse) (string, string) {
	msg, field := inv.portProblemText(u)
	return msg, field
}

func (inv *Inventory) portProblemText(u PortUse) (string, string) {
	sw, ok := inv.Switches[u.Switch]
	if !ok {
		if u.OwnerKind == "CablePlan" && inv.Spines[u.Switch] {
			return "", ""
		}
		return fmt.Sprintf("unknown switch %s", u.Switch), "switch"
	}
	if u.Port < 1 || u.Port > sw.Ports {
		return fmt.Sprintf("%s has ports 1 to %d, not %d", sw.Name, sw.Ports, u.Port), "port"
	}
	if u.OwnerKind == Name {
		for _, up := range sw.Uplinks {
			if up == u.Port {
				return fmt.Sprintf("port %s %d is an uplink port", sw.Name, u.Port), "port"
			}
		}
	}
	if users := inv.PortUsers(u); len(users) > 0 {
		return fmt.Sprintf("port %s %d in use by %s", sw.Name, u.Port, users[0].Owner), "port"
	}
	if u.OwnerKind == Name && u.Network != "" && sw.Network != "" {
		if u.Network == "mgmt" && sw.Network != "mgmt" || u.Network == "data" && sw.Network == "mgmt" {
			return fmt.Sprintf("%s is a %s port, but %s is a %s switch", u.PortName, u.Network, sw.Name, sw.Network), "network"
		}
	}
	if users := inv.CableUsers(u); len(users) > 0 {
		return fmt.Sprintf("cable %s is also used by %s %s", u.Cable, users[0].Owner, users[0].PortName), "cable"
	}
	return "", ""
}
