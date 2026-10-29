package lab

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
)

// Faults is the injected fault state of a run: networks whose links are down.
type Faults map[string]bool

// Down lists the networks that are down.
func (f Faults) Down() []string {
	var out []string
	for n, d := range f {
		if d {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// NodeReady checks that a node can come up: it is in the inventory, renders
// a Machine for the cluster, has a role and a BMC port, a free location in
// a known rack and valid ports.
func (m *Model) NodeReady(name string) Check {
	c := Check{Name: "nodes Ready", Subject: name}
	fail := func(format string, args ...any) Check {
		c.Detail = name + ": " + fmt.Sprintf(format, args...)
		return c
	}
	n := m.Inv.Nodes[name]
	if n == nil {
		return fail("not in the inventory")
	}
	if n.Spec.Cluster != m.Cluster {
		return fail("belongs to cluster %q, not %s", n.Spec.Cluster, m.Cluster)
	}
	if _, ok := m.Machines()[name]; !ok {
		return fail("no Machine in the render of %s", m.Cluster)
	}
	if n.Spec.Role == "" {
		return fail("no role")
	}
	if m.BMCLink(name) == nil {
		return fail("no BMC port")
	}
	loc := n.Spec.Location
	rack := strings.ToUpper(loc.Rack)
	units, ok := m.Inv.RackUnits[rack]
	if loc.Rack == "" || !ok {
		return fail("unknown rack %q", loc.Rack)
	}
	top := loc.Unit + max(loc.Size, 1) - 1
	if loc.Unit < 1 || top > units {
		return fail("U%d-U%d is outside rack %s (U1-U%d)", loc.Unit, top, loc.Rack, units)
	}
	for _, o := range m.Inv.Occupants {
		if o.Kind == node.Name && o.Name == name {
			if others := m.Inv.Overlaps(o); len(others) > 0 {
				return fail("%s U%d is used by %s", loc.Rack, others[0].Unit, others[0].Name)
			}
			break
		}
	}
	for _, p := range m.Inv.CheckPorts(name) {
		if !p.OK {
			return fail("%s", p.Message)
		}
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%s: Ready, %s U%d, %d ports", name, loc.Rack, loc.Unit, len(n.Spec.Ports))
	return c
}

// BGPResult is the outcome of the BGP checks on a set of nodes.
type BGPResult struct {
	Checks []Check
	// Up and Total count sessions (node, neighbor).
	Up, Total int
}

// BGP checks that every node with a data port in a rack that has a
// CiliumBGPPeeringPolicy is selected by a policy, and that the sessions of
// the selected nodes to the policy neighbors come up: the neighbor is a
// router in an emulated network and the node has a link up to it.
func (m *Model) BGP(nodes []string, faults Faults) BGPResult {
	var res BGPResult
	policies := m.BGPPolicies()
	type session struct {
		addr    string
		policy  string
		up      []string
		reason  string
		network string
	}
	sessions := map[string]*session{}
	var order []string
	for _, name := range nodes {
		c := Check{Name: "bgp: selected", Subject: name}
		n := m.Inv.Nodes[name]
		if n == nil {
			c.Detail = name + ": not in the inventory"
			res.Checks = append(res.Checks, c)
			continue
		}
		data := m.DataNetworks(name)
		labels := m.NodeLabels(name)
		var selecting []BGPPolicy
		rackHasPolicy := false
		for _, p := range policies {
			if Selects(p.Selector, labels) {
				selecting = append(selecting, p)
			}
			if p.Rack != "" && strings.EqualFold(p.Rack, n.Spec.Location.Rack) {
				rackHasPolicy = true
			}
		}
		switch {
		case len(selecting) > 0:
			names := make([]string, len(selecting))
			for i, p := range selecting {
				names[i] = p.Name
			}
			c.OK = true
			c.Detail = fmt.Sprintf("%s: selected by %s", name, strings.Join(names, ", "))
		case len(data) == 0:
			c.OK = true
			c.Detail = name + ": no data port, no BGP"
		case rackHasPolicy:
			c.Detail = name + ": no BGP peering policy selects it"
		default:
			c.OK = true
			c.Detail = fmt.Sprintf("%s: rack %s has no BGP peering policy", name, n.Spec.Location.Rack)
		}
		res.Checks = append(res.Checks, c)
		for _, p := range selecting {
			for _, addr := range p.Neighbors {
				key := p.Name + "/" + addr
				s := sessions[key]
				if s == nil {
					s = &session{addr: addr, policy: p.Name}
					sessions[key] = s
					order = append(order, key)
				}
				res.Total++
				ln := m.NetworkOf(addr)
				switch {
				case ln == nil:
					s.reason = "no emulated network for " + addr
					continue
				case !contains(ln.Routers, addr):
					s.reason = fmt.Sprintf("no router at %s in network %s", addr, ln.Name)
					continue
				}
				s.network = ln.Name
				if faults[ln.Name] {
					s.reason = ln.Name + " link down"
					continue
				}
				if !contains(data, ln.Name) {
					if s.reason == "" {
						s.reason = name + " has no link to " + ln.Name
					}
					continue
				}
				s.up = append(s.up, name)
				res.Up++
			}
		}
	}
	for _, key := range order {
		s := sessions[key]
		c := Check{Name: "bgp: session", Subject: s.addr}
		if len(s.up) > 0 && s.reason == "" {
			c.OK = true
			c.Detail = fmt.Sprintf("BGP session to %s established (%s via %s)", s.addr, nodeCount(len(s.up)), s.network)
		} else {
			c.Detail = fmt.Sprintf("BGP session to %s not established", s.addr)
			if s.reason != "" {
				c.Detail += " (" + s.reason + ")"
			}
		}
		res.Checks = append(res.Checks, c)
	}
	return res
}

func nodeCount(n int) string {
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}

// PowerCycleTime is the simulated time a machine needs to come back after a
// power cycle on its BMC fixture.
func PowerCycleTime(fixture string) time.Duration {
	if strings.HasPrefix(fixture, "huawei") {
		return 3*time.Minute + 50*time.Second
	}
	return 3*time.Minute + 10*time.Second
}

// PowerCycle checks a BMC power action: the node needs a BMC cable to a
// mgmt switch and an emulation fixture for its model.
func (m *Model) PowerCycle(name, power string) (Check, time.Duration) {
	if power == "" {
		power = "PowerCycle"
	}
	c := Check{Name: "bmc: " + powerWords(power), Subject: name}
	n := m.Inv.Nodes[name]
	if n == nil {
		c.Detail = name + ": not in the inventory"
		return c, 0
	}
	l := m.BMCLink(name)
	if l == nil || l.Cable == "" {
		c.Detail = name + ": no BMC cable"
		return c, 0
	}
	if l.Network != "mgmt" {
		c.Detail = fmt.Sprintf("%s: BMC cable goes to %s, not a mgmt switch", name, l.Switch)
		return c, 0
	}
	r := m.Rule(n)
	if r == nil || r.BMC.Fixture == "" {
		c.Detail = fmt.Sprintf("%s: no emulation fixture for model %q", name, n.Spec.Model)
		return c, 0
	}
	d := PowerCycleTime(r.BMC.Fixture)
	if power == "ForceOff" {
		d = 5 * time.Second
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%s: back in %s (Redfish %s on %s %d)", name, Human(d), r.BMC.Fixture, l.Switch, l.SwPort)
	return c, d
}

// LinkDown checks a link fault: the node keeps traffic when it has a data
// link on another network.
func (m *Model) LinkDown(name string, faults Faults) Check {
	c := Check{Name: "fault: link down", Subject: name}
	data := m.DataNetworks(name)
	var up []string
	for _, n := range data {
		if !faults[n] {
			up = append(up, n)
		}
	}
	down := faults.Down()
	switch {
	case len(data) == 0:
		c.Detail = name + ": no data link"
	case len(up) == 0:
		c.Detail = fmt.Sprintf("%s: only %s, traffic lost", name, strings.Join(data, ", "))
	default:
		c.OK = true
		c.Detail = fmt.Sprintf("%s: traffic via %s", name, strings.Join(up, ", "))
		if len(down) > 0 && len(up) == len(data) {
			c.Detail = fmt.Sprintf("%s: no %s link, traffic via %s", name, strings.Join(down, ", "), strings.Join(up, ", "))
		}
	}
	return c
}

// FluxReady checks that Flux can apply the cluster: no render problems and
// no validation errors in the files that feed it.
func (m *Model) FluxReady() Check {
	c := Check{Name: "flux: all-ready", Subject: m.Cluster}
	var errs []kinds.Problem
	for _, p := range m.Set.Problems {
		if p.Severity == kinds.SevError {
			errs = append(errs, p)
		}
	}
	errs = append(errs, m.Errors...)
	if len(errs) > 0 {
		p := errs[0]
		c.Detail = fmt.Sprintf("%s: %s: %s", m.Cluster, p.Path, p.Message)
		if len(errs) > 1 {
			c.Detail += fmt.Sprintf(" (and %d more)", len(errs)-1)
		}
		return c
	}
	n := len(m.Set.Order)
	if n == 0 {
		n = len(m.Set.Kustomizations)
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%s: %d of %d Kustomizations ready, %d objects", m.Cluster, n, n, len(m.Set.Objects))
	return c
}

// Human formats a duration like 3m 10s.
func Human(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - 60*m
	if m >= 60 {
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm %02ds", m, s)
}
