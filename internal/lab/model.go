package lab

import (
	"net/netip"
	"path"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/emulation"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/render"
)

// Model is the desired state of one tree for one cluster: the parsed repo,
// the inventory and the render of the cluster. Checks of the sim driver are
// computations on it.
type Model struct {
	Ref     string
	SHA     string
	Cluster string
	Repo    kinds.Repo
	Inv     *node.Inventory
	Set     *render.ObjectSet
	// Errors are validation errors of the files that feed the cluster.
	Errors    []kinds.Problem
	Emulation *emulation.EmulationProfile
}

// NewModel builds the model of a tree. set may be nil (no cluster).
func NewModel(r kinds.Repo, set *render.ObjectSet, cluster string, errors []kinds.Problem) *Model {
	m := &Model{Cluster: cluster, Repo: r, Inv: node.LoadInventory(r), Set: set, Errors: errors}
	if set == nil {
		m.Set = render.NewObjectSet(cluster)
	}
	if ds := r.ByKind(emulation.Name); len(ds) > 0 {
		var p emulation.EmulationProfile
		if err := ds[0].Decode(&p); err == nil {
			m.Emulation = &p
		}
	}
	return m
}

// Machines returns the rendered Machines of the cluster by name.
func (m *Model) Machines() map[string]render.Object {
	out := map[string]render.Object{}
	for _, o := range m.Set.Objects {
		if o.Kind == node.MachineKind && o.APIVersion == node.MachineAPIVersion {
			out[o.Name] = o
		}
	}
	return out
}

// MachineNames returns the sorted names of the rendered Machines.
func (m *Model) MachineNames() []string {
	var out []string
	for n := range m.Machines() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Rule returns the emulation rule of a node (first match wins), or nil.
func (m *Model) Rule(n *node.Node) *emulation.MachineRule {
	if m.Emulation == nil || n == nil {
		return nil
	}
	for i := range m.Emulation.Spec.Machines {
		r := &m.Emulation.Spec.Machines[i]
		if r.Match.Model != "" {
			if ok, _ := path.Match(r.Match.Model, n.Spec.Model); !ok {
				continue
			}
		}
		if r.Match.Role != "" && r.Match.Role != string(n.Spec.Role) {
			continue
		}
		return r
	}
	return nil
}

// Link is one cabled port of a node with the network of its switch.
type Link struct {
	Port    string // p1, bmc
	Switch  string
	SwPort  int
	Network string // mgmt, data-a, data-b (from the switch)
	Cable   string
}

// Links returns the cabled ports of a node.
func (m *Model) Links(name string) []Link {
	n := m.Inv.Nodes[name]
	if n == nil {
		return nil
	}
	var out []Link
	for _, p := range n.Spec.Ports {
		l := Link{Port: p.Name, Switch: p.Peer.Switch, SwPort: p.Peer.Port, Cable: p.Cable, Network: p.Network}
		if sw, ok := m.Inv.Switches[p.Peer.Switch]; ok && sw.Network != "" {
			l.Network = sw.Network
		}
		out = append(out, l)
	}
	return out
}

// DataNetworks returns the data networks a node is cabled to, sorted.
func (m *Model) DataNetworks(name string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range m.Links(name) {
		if l.Port == "bmc" || l.Network == "mgmt" || l.Network == "" || seen[l.Network] {
			continue
		}
		seen[l.Network] = true
		out = append(out, l.Network)
	}
	sort.Strings(out)
	return out
}

// BMCLink returns the BMC port of a node, or nil.
func (m *Model) BMCLink(name string) *Link {
	for _, l := range m.Links(name) {
		if l.Port == "bmc" {
			return &l
		}
	}
	return nil
}

// BGPPolicy is a rendered CiliumBGPPeeringPolicy.
type BGPPolicy struct {
	Name      string
	Rack      string // inventory.pk.io/rack label
	Selector  map[string]any
	Neighbors []string // peer addresses without prefix length
	Source    kinds.SourceRef
}

// BGPPolicies returns the rendered BGP peering policies of the cluster.
func (m *Model) BGPPolicies() []BGPPolicy {
	var out []BGPPolicy
	for _, k := range m.Set.Keys() {
		o := m.Set.Objects[k]
		if o.Kind != "CiliumBGPPeeringPolicy" {
			continue
		}
		p := BGPPolicy{Name: o.Name, Source: o.Source}
		if labels, ok := kindutil.Get(o.Body, "metadata", "labels").(map[string]any); ok {
			p.Rack, _ = labels["inventory.pk.io/rack"].(string)
		}
		p.Selector, _ = kindutil.Get(o.Body, "spec", "nodeSelector").(map[string]any)
		vrs, _ := kindutil.Get(o.Body, "spec", "virtualRouters").([]any)
		seen := map[string]bool{}
		for _, vr := range vrs {
			vm, _ := vr.(map[string]any)
			ns, _ := vm["neighbors"].([]any)
			for _, n := range ns {
				nm, _ := n.(map[string]any)
				addr, _ := nm["peerAddress"].(string)
				addr, _, _ = strings.Cut(addr, "/")
				if addr != "" && !seen[addr] {
					seen[addr] = true
					p.Neighbors = append(p.Neighbors, addr)
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// NodeLabels are the labels a node carries in the cluster: the labels of
// its Machine plus kubernetes.io/hostname.
func (m *Model) NodeLabels(name string) map[string]string {
	out := map[string]string{"kubernetes.io/hostname": name}
	if o, ok := m.Machines()[name]; ok {
		if labels, ok := kindutil.Get(o.Body, "metadata", "labels").(map[string]any); ok {
			for k, v := range labels {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
	} else if n := m.Inv.Nodes[name]; n != nil {
		for k, v := range n.Metadata.Labels {
			out[k] = v
		}
	}
	return out
}

// Selects reports whether a Kubernetes label selector (matchLabels and
// matchExpressions) selects a label set. An empty selector selects all.
func Selects(sel map[string]any, labels map[string]string) bool {
	if ml, ok := sel["matchLabels"].(map[string]any); ok {
		for k, v := range ml {
			if s, _ := v.(string); labels[k] != s {
				return false
			}
		}
	}
	exprs, _ := sel["matchExpressions"].([]any)
	for _, e := range exprs {
		em, _ := e.(map[string]any)
		key, _ := em["key"].(string)
		op, _ := em["operator"].(string)
		var values []string
		if vs, ok := em["values"].([]any); ok {
			for _, v := range vs {
				if s, ok := v.(string); ok {
					values = append(values, s)
				}
			}
		}
		val, has := labels[key]
		switch op {
		case "In":
			if !has || !contains(values, val) {
				return false
			}
		case "NotIn":
			if has && contains(values, val) {
				return false
			}
		case "Exists":
			if !has {
				return false
			}
		case "DoesNotExist":
			if has {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// LabNetwork is an emulated network with its routers.
type LabNetwork struct {
	Name    string
	Prefix  netip.Prefix
	Routers []string
	Emulate bool
}

// Networks returns the emulated networks of the emulation profile.
func (m *Model) Networks() []LabNetwork {
	if m.Emulation == nil {
		return nil
	}
	var out []LabNetwork
	for name, n := range m.Emulation.Spec.Networks {
		ln := LabNetwork{Name: name, Routers: n.Routers, Emulate: n.Emulate == nil || *n.Emulate}
		if n.IPv4 != nil {
			ln.Prefix, _ = netip.ParsePrefix(n.IPv4.CIDR)
		}
		out = append(out, ln)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NetworkOf returns the emulated network that contains addr, or nil.
func (m *Model) NetworkOf(addr string) *LabNetwork {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return nil
	}
	for _, n := range m.Networks() {
		if n.Prefix.IsValid() && n.Prefix.Contains(ip) {
			return &n
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
