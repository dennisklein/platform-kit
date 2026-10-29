package lab

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds/cluster"
)

// SimScale is how much faster than a real lab the sim driver runs at speed
// 1: the metal run of the demo change (6m 40s lab time, as in the talk)
// takes about 24 seconds.
const SimScale = 17.0

// Sim is the sim driver: it emulates nothing, but computes each check on
// the desired state and reports steps with realistic timing.
type Sim struct {
	// Speed scales the step durations (PK_LAB_SPEED; 2 is twice as fast).
	Speed float64

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewSim returns the sim driver.
func NewSim(speed float64) *Sim {
	if speed <= 0 {
		speed = 1
	}
	return &Sim{Speed: speed, cancels: map[string]context.CancelFunc{}}
}

func (s *Sim) Name() string { return "sim" }

func (s *Sim) Cancel(runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cancels[runID]
	if !ok {
		return errors.New("no such run")
	}
	c()
	return nil
}

func (s *Sim) Start(ctx context.Context, job Job) (<-chan Event, error) {
	if job.Run == nil || job.Head == nil {
		return nil, errors.New("lab: job needs a run and a head")
	}
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[job.Run.ID] = cancel
	s.mu.Unlock()
	ch := make(chan Event, 64)
	r := &simRun{sim: s, job: job, ch: ch, faults: Faults{}, steps: cloneSteps(job.Run.Steps)}
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.cancels, job.Run.ID)
			s.mu.Unlock()
			cancel()
			close(ch)
		}()
		r.run(ctx)
	}()
	return ch, nil
}

func cloneSteps(in []Step) []Step {
	out := make([]Step, len(in))
	for i, s := range in {
		s.Output = append([]string(nil), s.Output...)
		s.Checks = append([]Check(nil), s.Checks...)
		out[i] = s
	}
	return out
}

type simRun struct {
	sim     *Sim
	job     Job
	ch      chan<- Event
	steps   []Step
	cur     *Model
	faults  Faults
	simTime time.Duration
}

func (r *simRun) real(sim time.Duration) time.Duration {
	return time.Duration(float64(sim) / SimScale / r.sim.Speed)
}

func (r *simRun) emit(i int) {
	st := r.steps[i]
	st.Output = append([]string(nil), st.Output...)
	st.Checks = append([]Check(nil), st.Checks...)
	r.ch <- Event{Run: r.job.Run.ID, Step: &st}
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stepResult is what a step computed.
type stepResult struct {
	lines  []string
	checks []Check
	detail string
	sim    time.Duration
	// fatal stops the run (a stage failed: later steps are skipped).
	fatal bool
}

func (r *simRun) run(ctx context.Context) {
	r.cur = r.job.Head
	if r.job.Spec.Provision != "metal" && r.job.Base != nil {
		r.cur = r.job.Base
	}
	result := ResultPass
	stopped := false
	for i := range r.steps {
		st := &r.steps[i]
		if st.Type == StepReport {
			continue // the service writes the report
		}
		if stopped {
			st.Status = StepSkipped
			r.emit(i)
			continue
		}
		st.Status = StepRunning
		st.StartedAt = time.Now().UTC()
		r.emit(i)
		res := r.exec(st)
		// Stream the output lines over the step time.
		total := r.real(res.sim)
		per := total
		if len(res.lines) > 0 {
			per = total / time.Duration(len(res.lines)+1)
		}
		start := r.simTime
		for j, line := range res.lines {
			if err := sleep(ctx, per); err != nil {
				r.cancelled(i)
				return
			}
			at := start + res.sim*time.Duration(j+1)/time.Duration(len(res.lines)+1)
			st.Output = append(st.Output, fmt.Sprintf("[%s] %s", clock(at), line))
			r.emit(i)
		}
		if err := sleep(ctx, per); err != nil {
			r.cancelled(i)
			return
		}
		r.simTime += res.sim
		st.Checks = append(st.Checks, res.checks...)
		st.SimulatedMs = res.sim.Milliseconds()
		st.Status = StepPass
		for _, c := range res.checks {
			if !c.OK {
				st.Status = StepFail
			}
		}
		if res.detail != "" {
			st.Detail = res.detail
		}
		if st.Status == StepFail {
			result = ResultFail
			if res.fatal {
				stopped = true
			}
		}
		st.FinishedAt = time.Now().UTC()
		r.emit(i)
	}
	r.ch <- Event{Run: r.job.Run.ID, Done: true, Result: result}
}

func (r *simRun) cancelled(i int) {
	r.steps[i].Status = StepFail
	r.steps[i].Detail = "cancelled"
	r.steps[i].FinishedAt = time.Now().UTC()
	r.emit(i)
	for j := i + 1; j < len(r.steps); j++ {
		if r.steps[j].Type != StepReport {
			r.steps[j].Status = StepSkipped
			r.emit(j)
		}
	}
	r.ch <- Event{Run: r.job.Run.ID, Done: true, Result: ResultCancelled}
}

// clock formats lab time as mm:ss.
func clock(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func (r *simRun) scenarioStep(name string) *ScenarioStep {
	for i := range r.job.Spec.Scenario {
		if r.job.Spec.Scenario[i].Name == name {
			return &r.job.Spec.Scenario[i]
		}
	}
	return nil
}

func (r *simRun) exec(st *Step) stepResult {
	if !st.Scenario {
		switch st.Type {
		case StepTopology:
			return r.topology()
		case StepBMC:
			return r.bmcStage()
		case StepInstall:
			if st.Name == "restore" {
				return r.restore()
			}
			return r.install()
		case StepJoin:
			return r.join()
		}
		return stepResult{sim: 2 * time.Second}
	}
	s := r.scenarioStep(st.Name)
	if s == nil {
		return stepResult{sim: time.Second, checks: []Check{{Name: st.Name, Detail: "step not in the scenario"}}}
	}
	switch {
	case s.Check != nil:
		return r.check(s)
	case s.GitOps != nil:
		return r.gitops(s)
	case s.Action != nil && s.Action.BMC != nil:
		return r.bmcAction(s)
	case s.Fault != nil:
		return r.fault(s)
	case s.Wait != nil:
		d := ParseDuration(s.Wait.Duration)
		return stepResult{sim: d, detail: Human(d), lines: []string{"waiting " + Human(d)}}
	case s.Exec != nil:
		cmd := strings.Join(s.Exec.Command, " ")
		return stepResult{sim: 15 * time.Second, detail: "exit 0",
			lines:  []string{fmt.Sprintf("run %s %s (simulated)", s.Exec.Image, cmd), "exit 0"},
			checks: []Check{{Name: "exec: " + s.Exec.Image, OK: true, Detail: s.Exec.Image + ": exit 0 (simulated)"}}}
	}
	return stepResult{sim: time.Second}
}

func (r *simRun) topology() stepResult {
	t := r.job.Topology
	res := stepResult{sim: 5 * time.Second, detail: "generate"}
	if t == nil {
		res.lines = []string{"no cluster impacted, no machines to emulate"}
		res.detail = "nothing to emulate"
		return res
	}
	res.lines = append(res.lines, "generated from control/inventory, control/cabling.yaml, .pk/emulation.yaml")
	res.lines = append(res.lines, t.Summary()+", substrate "+t.Substrate+", uplink "+t.Uplink)
	for _, name := range r.job.Nodes {
		tm := t.Machine(name)
		if tm == nil {
			res.checks = append(res.checks, Check{Name: "topology", Subject: name, Detail: name + ": not in the topology of " + t.Name})
			res.fatal = true
			continue
		}
		res.lines = append(res.lines, fmt.Sprintf("%s: %s, %s, %d NICs", name, tm.Machine.Backend, orNone(tm.BMC.Fixture), len(tm.NICs)))
	}
	for _, p := range t.Problems {
		for _, n := range r.job.Nodes {
			if p.Severity == "error" && strings.HasPrefix(p.Message, n+":") {
				res.checks = append(res.checks, Check{Name: "topology", Subject: n, Detail: p.Message})
				res.fatal = true
			}
		}
	}
	return res
}

func orNone(s string) string {
	if s == "" {
		return "no BMC fixture"
	}
	return s
}

func (r *simRun) bmcStage() stepResult {
	res := stepResult{sim: 8*time.Second + time.Duration(len(r.job.Nodes))*2*time.Second, detail: "Redfish sim"}
	for _, name := range r.job.Nodes {
		n := r.cur.Inv.Nodes[name]
		l := r.cur.BMCLink(name)
		rule := r.cur.Rule(n)
		switch {
		case n == nil:
			res.checks = append(res.checks, Check{Name: "bmc", Subject: name, Detail: name + ": not in the inventory"})
		case l == nil:
			res.checks = append(res.checks, Check{Name: "bmc", Subject: name, Detail: name + ": no BMC port"})
		case rule == nil || rule.BMC.Fixture == "":
			res.checks = append(res.checks, Check{Name: "bmc", Subject: name, Detail: fmt.Sprintf("%s: no emulation fixture for model %q", name, n.Spec.Model)})
		default:
			res.lines = append(res.lines, fmt.Sprintf("%s: Redfish sim %s on %s %d, MAC %s", name, rule.BMC.Fixture, l.Switch, l.SwPort, n.Spec.BMC.MAC))
		}
	}
	res.fatal = len(res.checks) > 0
	return res
}

func (r *simRun) install() stepResult {
	res := stepResult{sim: 70*time.Second + time.Duration(len(r.job.Nodes))*4*time.Second, detail: "Talos"}
	talos := ""
	if d := r.cur.Repo.Find(cluster.Name, r.cur.Cluster); d != nil {
		var c cluster.Cluster
		if err := d.Decode(&c); err == nil {
			talos = c.Spec.Talos
		}
	}
	if talos != "" {
		if !fileExists(r.cur, talos) {
			res.checks = append(res.checks, Check{Name: "install", Subject: r.cur.Cluster, Detail: fmt.Sprintf("%s: talconfig %s is missing", r.cur.Cluster, talos)})
			res.fatal = true
			return res
		}
		res.lines = append(res.lines, "machine config from "+talos)
	}
	machines := r.cur.Machines()
	for _, name := range r.job.Nodes {
		if _, ok := machines[name]; !ok {
			res.checks = append(res.checks, Check{Name: "install", Subject: name, Detail: fmt.Sprintf("%s: no Machine in the render of %s", name, r.cur.Cluster)})
			continue
		}
		disk := "/dev/sda"
		if t := r.job.Topology; t != nil {
			if tm := t.Machine(name); tm != nil && len(tm.Machine.Disks) > 0 {
				disk = fmt.Sprintf("/dev/sda (%d GiB, %s)", tm.Machine.Disks[0].SizeGiB, tm.Machine.Disks[0].Name)
			}
		}
		res.lines = append(res.lines, fmt.Sprintf("%s: vMedia boot, Talos installed on %s", name, disk))
	}
	res.fatal = len(res.checks) > 0
	return res
}

func fileExists(m *Model, p string) bool {
	type finder interface{ Paths() []string }
	if f, ok := m.Repo.(finder); ok {
		for _, x := range f.Paths() {
			if x == p {
				return true
			}
		}
		return false
	}
	return true
}

func (r *simRun) join() stepResult {
	res := stepResult{sim: 25 * time.Second, detail: r.cur.Cluster}
	for _, name := range r.job.Nodes {
		n := r.cur.Inv.Nodes[name]
		if n == nil || n.Spec.Role == "" {
			res.checks = append(res.checks, Check{Name: "join", Subject: name, Detail: name + ": no role, cannot join"})
			continue
		}
		res.lines = append(res.lines, fmt.Sprintf("%s joined %s as %s", name, r.cur.Cluster, n.Spec.Role))
	}
	res.fatal = len(res.checks) > 0
	return res
}

func (r *simRun) restore() stepResult {
	res := stepResult{sim: 20 * time.Second, detail: "twin snapshot"}
	from := r.job.Spec.From
	if from == "" {
		from = r.cur.Cluster + "-twin@nightly"
	}
	res.lines = []string{fmt.Sprintf("restored %s: %d machines, Flux on %s", from, len(r.cur.Machines()), r.cur.Ref)}
	return res
}

func (r *simRun) nodesUnderTest() []string {
	if len(r.job.Nodes) > 0 {
		return r.job.Nodes
	}
	return r.cur.MachineNames()
}

func (r *simRun) check(s *ScenarioStep) stepResult {
	var res stepResult
	var parts []string
	nodes := r.nodesUnderTest()
	if s.Check.Flux != "" {
		res.sim += 10 * time.Second
		c := r.cur.FluxReady()
		res.checks = append(res.checks, c)
		res.lines = append(res.lines, c.Detail)
		n := len(r.cur.Set.Order)
		if c.OK {
			parts = append(parts, fmt.Sprintf("%d of %d ready", n, n))
		} else {
			parts = append(parts, "not ready")
		}
	}
	if s.Check.Nodes != "" {
		res.sim += 15 * time.Second
		ok := 0
		for _, n := range nodes {
			c := r.cur.NodeReady(n)
			if c.OK {
				ok++
			}
			res.checks = append(res.checks, c)
			res.lines = append(res.lines, c.Detail)
		}
		parts = append(parts, fmt.Sprintf("%d of %d", ok, len(nodes)))
	}
	if s.Check.BGP != "" {
		res.sim += 20 * time.Second
		b := r.cur.BGP(nodes, r.faults)
		res.checks = append(res.checks, b.Checks...)
		for _, c := range b.Checks {
			res.lines = append(res.lines, c.Detail)
		}
		if b.Up < b.Total {
			parts = append(parts, fmt.Sprintf("%d of %d peers up", b.Up, b.Total))
		} else {
			parts = append(parts, peers(b.Up))
		}
	}
	if len(parts) == 1 {
		res.detail = parts[0]
		return res
	}
	// A step with several checks reports all checks of the run so far.
	ok, total := 0, 0
	for _, st := range r.steps {
		for _, c := range st.Checks {
			total++
			if c.OK {
				ok++
			}
		}
	}
	for _, c := range res.checks {
		total++
		if c.OK {
			ok++
		}
	}
	res.detail = fmt.Sprintf("%d of %d", ok, total)
	return res
}

func peers(n int) string {
	if n == 1 {
		return "1 peer up"
	}
	return fmt.Sprintf("%d peers up", n)
}

func (r *simRun) gitops(s *ScenarioStep) stepResult {
	res := stepResult{sim: 30 * time.Second}
	switch s.GitOps.SwitchTo {
	case "base":
		if r.job.Base != nil {
			r.cur = r.job.Base
		}
	default:
		r.cur = r.job.Head
	}
	ref := r.cur.Ref
	if r.cur.SHA != "" {
		ref += "@" + short(r.cur.SHA)
	}
	res.detail = ref
	res.lines = []string{"GitRepository state now follows " + ref, fmt.Sprintf("Flux reconciles %d Kustomizations of %s", len(r.cur.Set.Order), r.cur.Cluster)}
	return res
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (r *simRun) bmcAction(s *ScenarioStep) stepResult {
	a := s.Action.BMC
	c, d := r.cur.PowerCycle(a.Machine, a.Power)
	res := stepResult{sim: d, checks: []Check{c}}
	if !c.OK {
		res.sim = 10 * time.Second
		res.detail = "failed"
		res.lines = []string{c.Detail}
		return res
	}
	res.detail = "back in " + Human(d)
	res.lines = []string{fmt.Sprintf("Redfish %s %s", a.Power, a.Machine), a.Machine + ": POST, Talos boot", c.Detail}
	return res
}

func (r *simRun) fault(s *ScenarioStep) stepResult {
	f := s.Fault
	res := stepResult{sim: 20 * time.Second}
	if f.State == "up" {
		r.faults[f.Link] = false
		res.detail = "restored"
		res.lines = []string{f.Link + " links up"}
		return res
	}
	r.faults[f.Link] = true
	nodes := f.Machines
	if len(nodes) == 0 {
		nodes = r.nodesUnderTest()
	}
	res.lines = append(res.lines, f.Link+" links down")
	via := map[string]bool{}
	for _, n := range nodes {
		c := r.cur.LinkDown(n, r.faults)
		res.checks = append(res.checks, c)
		res.lines = append(res.lines, c.Detail)
		if c.OK {
			for _, net := range r.cur.DataNetworks(n) {
				if !r.faults[net] {
					via[net] = true
				}
			}
		}
	}
	for _, c := range res.checks {
		if !c.OK {
			res.detail = "traffic lost"
			return res
		}
	}
	var nets []string
	for n := range via {
		nets = append(nets, n)
	}
	if len(nets) > 0 {
		sort.Strings(nets)
		res.detail = "traffic via " + strings.Join(nets, ", ")
	}
	return res
}
