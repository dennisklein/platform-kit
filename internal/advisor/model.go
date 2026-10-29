package advisor

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/kinds/overlays"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

// NodeInfo is a Node document, read from a file.
type NodeInfo struct {
	Name    string
	Path    string
	Cluster string
	Role    string
	Rack    string
	Unit    int
	Size    int
	Ports   []node.Port
	// New is true when the node is new in the change.
	New bool
}

// DataPorts returns the ports on data networks.
func (n NodeInfo) DataPorts() []node.Port {
	var out []node.Port
	for _, p := range n.Ports {
		if p.Network != "mgmt" && p.Name != "bmc" {
			out = append(out, p)
		}
	}
	return out
}

// ParseNodes returns the Node documents of a file.
func ParseNodes(p, content string) []NodeInfo {
	var out []NodeInfo
	for _, d := range repo.ParseFile(p, []byte(content)).Docs {
		if d.Kind != "Node" || !strings.HasPrefix(d.APIVersion, "inventory.pk.io/") {
			continue
		}
		n, err := node.FromDoc(d)
		if err != nil {
			continue
		}
		out = append(out, NodeInfo{
			Name: d.Name, Path: p, Cluster: n.Spec.Cluster, Role: string(n.Spec.Role),
			Rack: n.Spec.Location.Rack, Unit: n.Spec.Location.Unit, Size: n.Spec.Location.Size,
			Ports: n.Spec.Ports,
		})
	}
	return out
}

// BGPPolicy is a CiliumBGPPeeringPolicy document.
type BGPPolicy struct {
	Name    string
	Path    string
	Cluster string
	// Rack is the inventory.pk.io/rack label (the rack the policy serves).
	Rack      string
	Hostnames []string
	Peers     []string
}

// Selects reports whether the policy selects a node by hostname.
func (b BGPPolicy) Selects(name string) bool {
	for _, h := range b.Hostnames {
		if h == name {
			return true
		}
	}
	return false
}

// ParseBGP returns the CiliumBGPPeeringPolicy documents of a file.
func ParseBGP(p, content string) []BGPPolicy {
	var out []BGPPolicy
	for _, d := range repo.ParseFile(p, []byte(content)).Docs {
		if d.Kind != "CiliumBGPPeeringPolicy" {
			continue
		}
		b := BGPPolicy{Name: d.Name, Path: p, Cluster: clusterOf(p), Hostnames: overlays.SelectedNodes(d.Data)}
		if labels, ok := kindutil.Get(d.Data, "metadata", "labels").(map[string]any); ok {
			if r, ok := labels["inventory.pk.io/rack"].(string); ok {
				b.Rack = r
			}
		}
		for _, peer := range overlays.Peers(d.Data) {
			b.Peers = append(b.Peers, peer.Address)
		}
		out = append(out, b)
	}
	return out
}

// clusterOf returns <id> of clusters/<id>/...
func clusterOf(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 2 && parts[0] == "clusters" {
		return parts[1]
	}
	return ""
}

// BGPGap is a policy that serves the rack of nodes but does not select them.
type BGPGap struct {
	Policy  BGPPolicy
	Missing []NodeInfo
}

// Names returns the names of the missing nodes.
func (g BGPGap) Names() []string {
	out := make([]string, 0, len(g.Missing))
	for _, n := range g.Missing {
		out = append(out, n.Name)
	}
	return out
}

// FindBGPGaps returns, per policy, the nodes with data ports in the policy's
// rack (and cluster) that the policy does not select.
func FindBGPGaps(nodes []NodeInfo, policies []BGPPolicy) []BGPGap {
	var gaps []BGPGap
	for _, pol := range policies {
		if pol.Rack == "" {
			continue
		}
		var missing []NodeInfo
		for _, n := range nodes {
			if !strings.EqualFold(n.Rack, pol.Rack) || len(n.DataPorts()) == 0 {
				continue
			}
			if pol.Cluster != "" && n.Cluster != "" && n.Cluster != pol.Cluster {
				continue
			}
			if !pol.Selects(n.Name) {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
			gaps = append(gaps, BGPGap{Policy: pol, Missing: missing})
		}
	}
	return gaps
}

// AddHostnames adds node names to the kubernetes.io/hostname In list of the
// named BGP policy in content. It edits the text in place, so comments and
// formatting stay. Names already selected are skipped.
func AddHostnames(content, policy string, names []string) (string, error) {
	var root yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(content))
	lines := strings.SplitAfter(content, "\n")
	for {
		root = yaml.Node{}
		if err := dec.Decode(&root); err != nil {
			return "", fmt.Errorf("no BGP policy %s in the file", policy)
		}
		doc := &root
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
			doc = doc.Content[0]
		}
		if mapGet(doc, "kind") == nil || mapGet(doc, "kind").Value != "CiliumBGPPeeringPolicy" {
			continue
		}
		if md := mapGet(doc, "metadata"); policy != "" && (md == nil || mapGet(md, "name") == nil || mapGet(md, "name").Value != policy) {
			continue
		}
		values := hostnameValues(doc)
		if values == nil {
			return "", fmt.Errorf("BGP policy %s has no kubernetes.io/hostname In list", policy)
		}
		have := map[string]bool{}
		for _, v := range values.Content {
			have[v.Value] = true
		}
		var add []string
		for _, n := range names {
			if !have[n] {
				add = append(add, n)
				have[n] = true
			}
		}
		if len(add) == 0 {
			return content, nil
		}
		if values.Style&yaml.FlowStyle != 0 || len(values.Content) == 0 {
			return addFlow(lines, values, add)
		}
		last := values.Content[len(values.Content)-1]
		li := last.Line - 1
		if li < 0 || li >= len(lines) {
			return "", fmt.Errorf("cannot place the edit")
		}
		tmpl := lines[li]
		col := last.Column - 1
		if col < 0 || col > len(tmpl) {
			return "", fmt.Errorf("cannot place the edit")
		}
		prefix := tmpl[:col]
		eol := "\n"
		if strings.HasSuffix(tmpl, "\r\n") {
			eol = "\r\n"
		}
		var ins []string
		for _, n := range add {
			ins = append(ins, prefix+n+eol)
		}
		if !strings.HasSuffix(tmpl, "\n") {
			lines[li] = tmpl + eol
		}
		out := append(append(append([]string{}, lines[:li+1]...), ins...), lines[li+1:]...)
		return strings.Join(out, ""), nil
	}
}

// addFlow rewrites a flow list ([a, b]) or an empty list on one line.
func addFlow(lines []string, values *yaml.Node, add []string) (string, error) {
	li := values.Line - 1
	if li < 0 || li >= len(lines) {
		return "", fmt.Errorf("cannot place the edit")
	}
	l := lines[li]
	open := strings.Index(l[max(0, values.Column-1):], "[")
	if open < 0 {
		return "", fmt.Errorf("cannot edit the hostname list")
	}
	open += max(0, values.Column-1)
	end := strings.Index(l[open:], "]")
	if end < 0 {
		return "", fmt.Errorf("the hostname list spans lines; edit it by hand")
	}
	end += open
	inner := strings.TrimSpace(l[open+1 : end])
	items := []string{}
	if inner != "" {
		items = append(items, inner)
	}
	items = append(items, add...)
	lines[li] = l[:open+1] + strings.Join(items, ", ") + l[end:]
	return strings.Join(lines, ""), nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func hostnameValues(doc *yaml.Node) *yaml.Node {
	ns := mapGet(mapGet(doc, "spec"), "nodeSelector")
	exprs := mapGet(ns, "matchExpressions")
	if exprs == nil || exprs.Kind != yaml.SequenceNode {
		return nil
	}
	for _, e := range exprs.Content {
		if k := mapGet(e, "key"); k == nil || k.Value != "kubernetes.io/hostname" {
			continue
		}
		if op := mapGet(e, "operator"); op == nil || op.Value != "In" {
			continue
		}
		if v := mapGet(e, "values"); v != nil && v.Kind == yaml.SequenceNode {
			return v
		}
	}
	return nil
}

// ScopeOf returns the Conventional Commit scope of a repo path.
func ScopeOf(p string) string {
	parts := strings.Split(p, "/")
	switch {
	case p == ".pk/policy.yaml":
		return "policy"
	case strings.HasPrefix(p, ".pk/labs/"), p == ".pk/emulation.yaml":
		return "lab"
	case strings.HasPrefix(p, ".pk/"):
		return "pk"
	case p == "control/cabling.yaml":
		return "cabling"
	case strings.HasPrefix(p, "control/inventory/"):
		return "inventory"
	case strings.HasPrefix(p, "control/talos/"):
		return "talos"
	case strings.HasPrefix(p, "kb/labs/"):
		return "lab"
	case strings.HasPrefix(p, "kb/"):
		return "kb"
	case strings.HasPrefix(p, "work/"):
		return "work"
	case len(parts) >= 4 && parts[0] == "clusters" && (parts[2] == "infra" || parts[2] == "apps"):
		return parts[3]
	case len(parts) >= 3 && parts[0] == "clusters":
		return parts[1]
	case len(parts) > 1:
		return parts[0]
	}
	return strings.TrimSuffix(path.Base(p), path.Ext(p))
}

var (
	reEntityName = regexp.MustCompile(`\b([a-z]{2,4}-\d{2,3})\b`)
	reUnit       = regexp.MustCompile(`\bU(\d{1,2})\b`)
	rePortText   = regexp.MustCompile(`\b(mgmt|data-a|data-b|data|spine-[a-z0-9]+)\s+(\d{1,3})\b`)
)

// EntityNames returns names like cn-035 in text, in order, without repeats.
func EntityNames(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reEntityName.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] && !strings.HasPrefix(m[1], "c-") && !strings.HasPrefix(m[1], "w-") {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Units returns unit numbers like U6 in text.
func Units(text string) []int {
	var out []int
	for _, m := range reUnit.FindAllStringSubmatch(text, -1) {
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		out = append(out, n)
	}
	return out
}

// SwitchPort is a port on a switch.
type SwitchPort struct {
	Switch string
	Port   int
}

func (s SwitchPort) String() string { return fmt.Sprintf("%s %d", s.Switch, s.Port) }

// PortsInText returns switch ports named in text ("data-b 37 to data-a 37").
func PortsInText(text string) []SwitchPort {
	var out []SwitchPort
	for _, m := range rePortText.FindAllStringSubmatch(text, -1) {
		var n int
		fmt.Sscanf(m[2], "%d", &n)
		out = append(out, SwitchPort{Switch: m[1], Port: n})
	}
	return out
}

// joinAnd joins words as "a, b and c".
func joinAnd(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
