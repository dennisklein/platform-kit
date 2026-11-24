package main

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

// The pure half of pk-metal top: everything it reads goes in as a topInput,
// and buildTop turns it into the dashboard's facts. Nothing here talks to a
// cluster, a BMC or a terminal, so a test can feed it fixed objects.

// topInput is one reading of everything the dashboard shows.
type topInput struct {
	Hosts     []metalv1alpha1.Host
	Endpoints []metalv1alpha1.ManagementEndpoint
	Claims    []metalv1alpha1.HostClaim
	Ops       []metalv1alpha1.HostOperation
	Nodes     []osv1alpha1.TalosNode
	Clusters  []osv1alpha1.TalosCluster
	Rollouts  []osv1alpha1.TalosRollout
	// K8sNodes are the workload cluster's Nodes; nil when that cluster is not
	// watched or not read yet.
	K8sNodes []corev1.Node
	// Fleet is the simulator's state endpoint; nil without one (real
	// hardware) or before its first answer. FleetURL is set when one is
	// watched at all, FleetErr when its last read failed.
	Fleet    *fleet.State
	FleetURL string
	FleetErr string
	// Consoles are the last lines of each fleet machine's console log, by
	// machine name.
	Consoles map[string][]string
	// Loading names what is not read yet (a watch that has not synced).
	Loading []string
}

// state is how a dashboard fact reads at a glance.
type state int

const (
	stIdle state = iota // nothing yet, or off
	stOK                // as it should be
	stWait              // on its way, or waiting for a person
	stBad               // failed, or needs a person
)

// mark is the symbol a state shows with, readable without colour.
func (s state) mark() string {
	switch s {
	case stOK:
		return "●"
	case stWait:
		return "◐"
	case stBad:
		return "✗"
	}
	return "○"
}

// stage is one station of the flow line: BMC, media, VM, node.
type stage struct {
	Name  string
	Text  string
	State state
}

type topData struct {
	Clusters  []clusterLine
	Rollouts  []rolloutStatus // the ones that have not succeeded
	NodeLine  string
	Attention int
	FleetLine string
	Loading   []string
	Tiles     []tile
}

type clusterLine struct {
	Name                    string
	Bootstrapped, Available state
	Why                     string // the reason of a condition that is not True
	ControlPlanes, CPReady  int32
	Workers, WorkersReady   int32
	KubernetesVersion       string
}

// tile is one machine.
type tile struct {
	Title    string
	Subtitle string

	HostName  string
	Phase     string
	BlockedOn string
	Message   string
	Attention bool

	ClaimNS, ClaimName string
	OnHold             bool

	// BMC, from the cluster's ManagementEndpoint and, with the fleet, the
	// simulator's own view.
	BMCAddress string
	BMCPhase   string
	Power      string
	Override   string
	Media      []string
	LastCall   string
	HangCycles int

	// The machine behind the BMC (fleet only).
	Sim        bool
	VM         string
	VMState    state
	ConsoleLog string
	Console    []string

	Op *opLine

	NodeName, NodeAddress string
	TalosReady            state
	TalosWhy              string
	K8sReady              state
	K8sKnown              bool

	Flow []stage
}

// opLine is the operation that matters for a machine now.
type opLine struct {
	Namespace, Name string
	Phase           string
	Step            string // "5/9 setBootOverride"
	Approval        string
	Waits           bool // waits for a person's approval
	Failed          bool
	Message         string
}

// buildTop joins the cluster's objects and the fleet's machines into tiles.
// Hosts and fleet machines are matched by serial, then by BMC address.
func buildTop(in topInput) topData {
	rep := buildReport(in.Hosts, in.Endpoints, in.Claims, in.Ops, in.Nodes)
	d := topData{Loading: in.Loading}

	talosReady := map[string]*metav1.Condition{}
	for i := range in.Nodes {
		talosReady[in.Nodes[i].Name] = meta.FindStatusCondition(in.Nodes[i].Status.Conditions, osv1alpha1.ConditionReady)
	}
	k8sReady := map[string]bool{}
	for _, n := range in.K8sNodes {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				k8sReady[n.Name] = c.Status == corev1.ConditionTrue
			}
		}
	}

	// The header: each cluster's health and the nodes.
	for _, c := range in.Clusters {
		cl := clusterLine{Name: c.Name, ControlPlanes: c.Status.ControlPlanes, CPReady: c.Status.ReadyControlPlanes,
			Workers: c.Status.Workers, WorkersReady: c.Status.ReadyWorkers, KubernetesVersion: c.Status.KubernetesVersion}
		cl.Bootstrapped, cl.Why = condState(c.Status.Conditions, osv1alpha1.ConditionBootstrapped, "")
		cl.Available, cl.Why = condState(c.Status.Conditions, osv1alpha1.ConditionAvailable, cl.Why)
		d.Clusters = append(d.Clusters, cl)
	}
	ready := 0
	for _, n := range in.Nodes {
		if c := talosReady[n.Name]; c != nil && c.Status == metav1.ConditionTrue {
			ready++
		}
	}
	d.NodeLine = fmt.Sprintf("TalosNodes Ready %d/%d", ready, len(in.Nodes))

	// Rollouts, read as pk-metal status reads them.
	var ros statusReport
	addRollouts(&ros, in.Rollouts)
	for _, r := range ros.Rollouts {
		if r.State == osv1alpha1.ReasonRolloutSucceeded || r.State == osv1alpha1.ReasonNothingToDo {
			continue
		}
		d.Rollouts = append(d.Rollouts, r)
		if r.NeedsAttention {
			d.Attention++
		}
	}
	if in.K8sNodes != nil {
		k := 0
		for _, r := range k8sReady {
			if r {
				k++
			}
		}
		d.NodeLine += fmt.Sprintf("   Kubernetes nodes Ready %d/%d", k, len(in.K8sNodes))
	}
	switch {
	case in.FleetURL == "":
	case in.FleetErr != "":
		d.FleetLine = "fleet " + in.FleetURL + ": " + in.FleetErr
	default:
		d.FleetLine = "fleet " + in.FleetURL
	}

	epByName := map[string]*metalv1alpha1.ManagementEndpoint{}
	for i := range in.Endpoints {
		epByName[in.Endpoints[i].Name] = &in.Endpoints[i]
	}
	hostByName := map[string]*metalv1alpha1.Host{}
	for i := range in.Hosts {
		hostByName[in.Hosts[i].Name] = &in.Hosts[i]
	}
	held := map[string]bool{}
	for _, c := range rep.Claims {
		held[nsName(c.Namespace, c.Name)] = c.OnHold
	}

	hostTile := func(row hostStatus) tile {
		t := tile{Title: row.Name, HostName: row.Name, Phase: row.Phase, BlockedOn: row.BlockedOn,
			Message: row.Message, Attention: row.NeedsAttention}
		if row.Vendor != "" {
			t.Subtitle = row.Vendor
		}
		if ns, name, ok := strings.Cut(row.Claim, "/"); ok {
			t.ClaimNS, t.ClaimName, t.OnHold = ns, name, held[row.Claim]
		}
		h := hostByName[row.Name]
		for _, p := range h.Status.ManagedBy {
			if ep := epByName[p.EndpointRef.Name]; ep != nil {
				t.BMCAddress, t.BMCPhase = ep.Spec.Address, string(ep.Status.Phase)
				break
			}
		}
		if row.Node != nil {
			t.NodeName, t.NodeAddress = row.Node.Name, row.Node.Address
			t.TalosReady, t.TalosWhy = condState(nodeConds(in.Nodes, row.Node.Name), osv1alpha1.ConditionReady, "")
			if r, ok := k8sReady[row.Node.Name]; ok {
				t.K8sKnown = true
				t.K8sReady = map[bool]state{true: stOK, false: stBad}[r]
			}
		}
		t.Op = currentOp(in.Ops, row.Name)
		return t
	}

	// With the fleet, its order is the machines' order; hosts that match
	// no fleet machine follow by name.
	used := map[string]bool{}
	if in.Fleet != nil {
		for _, m := range in.Fleet.Machines {
			var t tile
			if row, ok := matchHost(m, rep.Hosts, in.Hosts, epByName, used); ok {
				t = hostTile(row)
				t.Title = row.Name + " · " + m.Name
				used[row.Name] = true
			} else {
				t = tile{Title: m.Name, Phase: "no Host yet"}
			}
			t.Subtitle = m.Personality
			addSim(&t, m, in.Consoles[m.Name])
			d.Tiles = append(d.Tiles, t)
		}
	}
	for _, row := range rep.Hosts {
		if !used[row.Name] {
			d.Tiles = append(d.Tiles, hostTile(row))
		}
	}
	for i := range d.Tiles {
		d.Tiles[i].Flow = flow(&d.Tiles[i])
		if d.Tiles[i].Attention || (d.Tiles[i].Op != nil && (d.Tiles[i].Op.Waits || d.Tiles[i].Op.Failed)) {
			d.Attention++
		}
	}
	return d
}

func nodeConds(nodes []osv1alpha1.TalosNode, name string) []metav1.Condition {
	for _, n := range nodes {
		if n.Name == name {
			return n.Status.Conditions
		}
	}
	return nil
}

// condState reads one condition: True is OK, False bad, Unknown on its way,
// absent idle. why keeps the first reason found of a condition that is not
// True.
func condState(conds []metav1.Condition, t, why string) (state, string) {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return stIdle, why
	}
	if why == "" && c.Status != metav1.ConditionTrue {
		why = c.Reason
	}
	switch c.Status {
	case metav1.ConditionTrue:
		return stOK, why
	case metav1.ConditionFalse:
		return stBad, why
	}
	return stWait, why
}

// matchHost finds the Host behind a fleet machine: by serial, else by the
// BMC address its endpoint names.
func matchHost(m fleet.MachineState, rows []hostStatus, hosts []metalv1alpha1.Host,
	eps map[string]*metalv1alpha1.ManagementEndpoint, used map[string]bool) (hostStatus, bool) {
	byName := map[string]hostStatus{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	for _, h := range hosts {
		if used[h.Name] || h.Status.Identity == nil || m.Serial == "" {
			continue
		}
		if strings.EqualFold(h.Status.Identity.Serial, m.Serial) {
			return byName[h.Name], true
		}
	}
	for _, h := range hosts {
		if used[h.Name] {
			continue
		}
		for _, p := range h.Status.ManagedBy {
			if ep := eps[p.EndpointRef.Name]; ep != nil && hostPort(ep.Spec.Address) == hostPort(m.BMC) {
				return byName[h.Name], true
			}
		}
	}
	return hostStatus{}, false
}

// hostPort reduces https://10.0.0.1:8441/redfish/v1 and 10.0.0.1:8441 alike
// to 10.0.0.1:8441.
func hostPort(addr string) string {
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimSuffix(addr, "/")
}

// addSim fills the simulator's side of a tile.
func addSim(t *tile, m fleet.MachineState, console []string) {
	t.Sim = true
	if t.BMCAddress == "" {
		t.BMCAddress = m.BMC
	}
	t.Power = orDash(m.Power)
	if o := m.BootOverride; o.Target != "" && o.Target != "None" && o.Enabled != "" && o.Enabled != "Disabled" {
		t.Override = o.Target + "/" + o.Enabled
	} else {
		t.Override = "none"
	}
	for _, s := range m.Media {
		if s.Inserted {
			t.Media = append(t.Media, s.ID+": "+imageName(s.ImageURL))
		}
	}
	if n := len(m.Requests); n > 0 {
		r := m.Requests[n-1]
		t.LastCall = fmt.Sprintf("%s %s %d", r.Method, shortPath(r.Path), r.Status)
	}
	t.HangCycles = m.Backend.HangCycles
	switch {
	case m.Backend.Kind == fleet.BackendNone:
		t.VM, t.VMState = "none (simulated power only)", stIdle
		if m.Backend.Running {
			t.VMState = stOK
		}
	case !m.Backend.Running:
		t.VM, t.VMState = "off", stIdle
	default:
		p := m.Backend.BootProgress
		t.VM, t.VMState = "running", stWait
		if p != "" {
			t.VM += " " + p
		}
		if p == "OSRunning" || m.Backend.Kind == fleet.BackendDocker {
			t.VMState = stOK
		}
	}
	if m.Backend.Kind == fleet.BackendDocker {
		t.VM = "container " + t.VM
	}
	t.ConsoleLog = m.ConsoleLog
	t.Console = console
}

// imageName is the last part of an image URL: what tells two images apart.
func imageName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return raw
	}
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	return parts[len(parts)-1]
}

// shortPath drops what every request shares: the Redfish root, and the
// Actions segment of an action.
func shortPath(p string) string {
	return strings.Replace(strings.TrimPrefix(p, "/redfish/v1"), "/Actions/", "/", 1)
}

// currentOp is the operation to show for a host: the oldest unfinished one,
// else the newest one if it failed. Succeeded ones are history.
func currentOp(ops []metalv1alpha1.HostOperation, host string) *opLine {
	var mine []metalv1alpha1.HostOperation
	for _, op := range ops {
		if op.Spec.HostRef.Name == host {
			mine = append(mine, op)
		}
	}
	slices.SortFunc(mine, func(a, b metalv1alpha1.HostOperation) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	var pick *metalv1alpha1.HostOperation
	for i := range mine {
		if !finished(&mine[i]) {
			pick = &mine[i]
			break
		}
	}
	if pick == nil && len(mine) > 0 && opPhase(&mine[len(mine)-1]) == metalv1alpha1.OperationFailed {
		pick = &mine[len(mine)-1]
	}
	if pick == nil {
		return nil
	}
	op := pick
	l := &opLine{Namespace: op.Namespace, Name: op.Name, Phase: orDash(string(opPhase(op))),
		Waits: waitsForApproval(op), Failed: opPhase(op) == metalv1alpha1.OperationFailed}
	done, total := len(op.Status.Steps), len(op.Spec.Actions)
	switch {
	case l.Failed && done > 0:
		l.Step = fmt.Sprintf("%d/%d %s failed", done, total, op.Status.Steps[done-1].Action)
	case done < total:
		l.Step = fmt.Sprintf("%d/%d %s", done+1, total, actionKey(op.Spec.Actions[done]))
	default:
		l.Step = fmt.Sprintf("%d/%d done", done, total)
	}
	switch {
	case op.Spec.Approval.ApprovedBy != "":
		l.Approval = "approved by " + op.Spec.Approval.ApprovedBy
	case l.Waits:
		l.Approval = "waits for approval"
	case op.Spec.Approval.Required != nil && !*op.Spec.Approval.Required:
		l.Approval = "no approval needed"
	default:
		l.Approval = "not approved"
	}
	l.Message = explain(op.Status.Conditions, op.Status.BlockedOn, metalv1alpha1.ConditionExecuted)
	return l
}

// actionKey is the union key of an action, the name the operator records
// its steps under.
func actionKey(a metalv1alpha1.HostOperationAction) string {
	return a.Key()
}

// hostState is how the Host reads: bad when it, or its operation, failed or
// is stuck; waiting while an approval or a step is due; OK when nothing
// blocks it.
func (t *tile) hostState() state {
	switch {
	case t.HostName == "":
		return stIdle
	case t.Op != nil && t.Op.Failed:
		return stBad
	case strings.HasPrefix(t.BlockedOn, "Approval("):
		return stWait
	case t.Attention:
		return stBad
	case t.BlockedOn != "":
		return stWait
	case t.Phase == string(metalv1alpha1.HostProvisioned) || t.Phase == string(metalv1alpha1.HostAvailable):
		return stOK
	}
	return stWait
}

// flow is the line BMC → media → VM → node, or, without the simulator, BMC
// → host → node: where along the way the machine is.
func flow(t *tile) []stage {
	bmcStage := stage{Name: "BMC"}
	switch {
	case t.Sim && t.Power == "On":
		bmcStage.Text, bmcStage.State = "on", stOK
	case t.Sim:
		bmcStage.Text, bmcStage.State = strings.ToLower(orDash(t.Power)), stIdle
	case t.BMCPhase == string(metalv1alpha1.EndpointReady):
		bmcStage.Text, bmcStage.State = "ready", stOK
	case t.BMCPhase == string(metalv1alpha1.EndpointUnreachable) || t.BMCPhase == string(metalv1alpha1.EndpointMismatch):
		bmcStage.Text, bmcStage.State = strings.ToLower(t.BMCPhase), stBad
	default:
		bmcStage.Text, bmcStage.State = strings.ToLower(orDash(t.BMCPhase)), stWait
	}

	nodeStage := stage{Name: "node", Text: "-", State: stIdle}
	if t.NodeName != "" {
		nodeStage.Text, nodeStage.State = "not ready", t.TalosReady
		switch {
		case t.K8sKnown && t.K8sReady == stOK && t.TalosReady == stOK:
			nodeStage.Text = "Ready"
		case t.TalosReady == stOK:
			nodeStage.Text = "Ready"
			if t.K8sKnown {
				nodeStage.Text, nodeStage.State = "Talos ready, kubelet not", stWait
			}
		case t.TalosReady == stIdle:
			nodeStage.Text = "waiting"
		case t.TalosWhy != "":
			nodeStage.Text = t.TalosWhy
		}
	}

	if !t.Sim {
		return []stage{bmcStage, {Name: "host", Text: orDash(t.Phase), State: t.hostState()}, nodeStage}
	}

	mediaStage := stage{Name: "media", Text: "none", State: stIdle}
	if n := len(t.Media); n > 0 {
		mediaStage.Text, mediaStage.State = fmt.Sprintf("%d mounted", n), stOK
	}
	vmStage := stage{Name: "VM", Text: t.VM, State: t.VMState}
	if t.HangCycles > 0 && vmStage.State != stOK {
		vmStage.State = stBad
	}
	return []stage{bmcStage, mediaStage, vmStage, nodeStage}
}
