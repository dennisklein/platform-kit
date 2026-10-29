package lab

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/labkind"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// BusyError is returned when a change already has a running lab.
type BusyError struct{ Run string }

func (e *BusyError) Error() string { return "lab run " + e.Run + " is still running" }

// Errors of the lab service (the bench maps them to HTTP statuses).
var (
	// ErrNoRight: an agent or bot lacks the run-lab right (403).
	ErrNoRight = errors.New("the policy does not allow this identity to run labs")
	// ErrNotFound: unknown run, lab or cluster (404).
	ErrNotFound = errors.New("not found")
	// ErrNoWorktree: the report goes into the worktree, so a change lab
	// needs a local worktree (409).
	ErrNoWorktree = errors.New("change has no local worktree")
	// ErrScenario: unknown scenario (400).
	ErrScenario = errors.New("unknown scenario")
)

// ChangeInfo is what the lab service needs to know about a change.
type ChangeInfo struct {
	ID       string
	Title    string
	Author   string
	Branch   string
	HeadSHA  string
	Worktree string
	// LabRuns are the recorded run ids (metadata file or MR description).
	LabRuns []string
}

// RunRef is a finished run recorded on the change.
type RunRef struct {
	Run    string
	SHA    string
	Result Result
	Report string
}

// Changes is the part of the change service the labs use. The bench wires
// an adapter over change.Service (the change package imports lab, so lab
// does not import change).
type Changes interface {
	Info(ctx context.Context, id string) (*ChangeInfo, error)
	MainDir(ctx context.Context) (string, error)
	// Sources: base is the merge base with the target, head the worktree.
	Sources(ctx context.Context, id string) (base, head render.Source, err error)
	// Source of a ref (main, change id, branch or sha).
	Source(ctx context.Context, ref string) (render.Source, error)
	// RecordLabRun writes the report into the worktree, records the run
	// in the change and commits (and posts pk/lab when pushed).
	RecordLabRun(ctx context.Context, id string, run RunRef, report []byte) error
	// Updated publishes change.updated for the rail.
	Updated(ctx context.Context, id, reason string)
}

// Options of the lab service.
type Options struct {
	// Dir keeps raw results (<data>/labs).
	Dir      string
	Speed    float64
	Host     string
	Changes  Changes
	Registry kinds.Registry
	Renderer render.Renderer
	// Policy decides the run-lab right of agents and bots and the lab gate
	// paths in the report; nil allows all.
	Policy policy.Engine
	Events events.Publisher
	Driver Driver
	Log    *slog.Logger
}

// Svc is the lab service of the bench.
type Svc struct {
	o     Options
	store store

	mu     sync.Mutex
	runs   map[string]*Run
	active map[string]string // change id → run id ("" while starting)
}

var _ Service = (*Svc)(nil)

// New loads the past runs and returns the service.
func New(o Options) (*Svc, error) {
	if o.Driver == nil {
		o.Driver = NewSim(o.Speed)
	}
	if o.Events == nil {
		o.Events = events.Nop{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Host == "" {
		o.Host = "local"
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Svc{o: o, store: store{dir: o.Dir}, runs: map[string]*Run{}, active: map[string]string{}}
	runs, err := s.store.load()
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		s.runs[r.ID] = r
	}
	return s, nil
}

func (s *Svc) Scenarios() []string { return []string{ScenarioMetal, ScenarioGitOps} }

func copyRun(r *Run) Run {
	c := *r
	c.Steps = cloneSteps(r.Steps)
	c.Nodes = append([]string(nil), r.Nodes...)
	c.GatePaths = append([]string(nil), r.GatePaths...)
	c.Failures = append([]string(nil), r.Failures...)
	return c
}

func (s *Svc) Get(ctx context.Context, runID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("lab run %s: %w", runID, ErrNotFound)
	}
	c := copyRun(r)
	return &c, nil
}

func (s *Svc) Runs(ctx context.Context, changeID string) ([]Run, error) {
	s.mu.Lock()
	var rs []*Run
	for _, r := range s.runs {
		if changeID == "" || r.ChangeID == changeID {
			rs = append(rs, r)
		}
	}
	sortRuns(rs)
	out := make([]Run, 0, len(rs))
	for _, r := range rs {
		out = append(out, copyRun(r))
	}
	s.mu.Unlock()
	return out, nil
}

// Forget drops the runs that started before t, with their raw results:
// runs of a forge state that was rebuilt since (pk forge-init --reset), whose
// changes and shas are gone. Running runs stay. It returns the number dropped.
func (s *Svc) Forget(t time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.runs {
		if r.Result == ResultRunning || !r.StartedAt.Before(t) {
			continue
		}
		dir := s.store.runDir(id)
		if err := os.RemoveAll(dir); err != nil {
			s.o.Log.Warn("lab forget", "run", id, "err", err)
			continue
		}
		_ = os.Remove(filepath.Dir(dir)) // the change dir, when empty
		delete(s.runs, id)
		n++
	}
	return n
}

// Definitions lists the Lab files on main.
func (s *Svc) Definitions(ctx context.Context) ([]LabFile, error) {
	dir, err := s.o.Changes.MainDir(ctx)
	if err != nil {
		return nil, err
	}
	r, err := repo.Load(dir, s.o.Registry)
	if err != nil {
		return nil, err
	}
	return labFiles(r), nil
}

func labFiles(r *repo.Repo) []LabFile {
	var out []LabFile
	for _, d := range r.ByKind(labkind.Name) {
		var l LabFile
		if err := d.Decode(&l); err != nil {
			continue
		}
		if l.Metadata.Name == "" {
			l.Metadata.Name = d.Name
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// Topology generates the topology of a ref. cluster may be a cluster id or
// the name of a Lab (a twin: its topology, scaled to its machines).
func (s *Svc) Topology(ctx context.Context, ref, cluster string) (*Topology, error) {
	if ref == "" {
		ref = "main"
	}
	src, err := s.o.Changes.Source(ctx, ref)
	if err != nil {
		return nil, err
	}
	r, err := repo.Load(src.Dir, s.o.Registry)
	if err != nil {
		return nil, err
	}
	m, err := ModelOf(ctx, s.o.Renderer, r, src, "")
	if err != nil {
		return nil, err
	}
	var scale []string
	backend := ""
	name := cluster
	if cluster != "" && !hasCluster(r, cluster) {
		var lab *LabFile
		for _, l := range labFiles(r) {
			if l.Metadata.Name == cluster {
				lab = &l
				break
			}
		}
		if lab == nil || lab.Spec.Topology.Name == "" {
			return nil, fmt.Errorf("cluster or lab %q: %w", cluster, ErrNotFound)
		}
		cluster = lab.Spec.Topology.Name
		if lab.Spec.Fidelity == "scaled" && lab.Spec.Scale != nil {
			scale, backend = lab.Spec.Scale.Machines, lab.Spec.Scale.Backend
		}
	}
	t := GenerateTopology(m, cluster, ref)
	if name != "" {
		t.Name = name
	}
	if len(scale) > 0 {
		t.Scale(scale)
	}
	if backend != "" {
		// A scaled twin runs its machines on the twin's backend
		// (talos-docker), not the qemu of a metal lab.
		for i := range t.Machines {
			if t.Machines[i].Machine.Backend != "none" {
				t.Machines[i].Machine.Backend = backend
			}
		}
	}
	if t.SHA == "" {
		t.SHA = src.SHA
	}
	return t, nil
}

func hasCluster(r *repo.Repo, id string) bool {
	for _, c := range render.ClustersOf(r) {
		if c.ID == id {
			return true
		}
	}
	return false
}

// mayRun checks the run-lab right of agents and bots.
func (s *Svc) mayRun(ctx context.Context, who identity.Identity) error {
	if who.IsZero() || who.IsHuman() || s.o.Policy == nil {
		return nil
	}
	p, err := s.o.Policy.Current(ctx)
	if err != nil {
		return err
	}
	var can []policy.Right
	for _, a := range p.Agents {
		if a.ID == who.User {
			can = a.Can
		}
	}
	for _, b := range p.Bots {
		if b.ID == who.User {
			can = b.Can
		}
	}
	for _, c := range can {
		if c == policy.RightRunLab {
			return nil
		}
	}
	return fmt.Errorf("%s: %w", who.User, ErrNoRight)
}

// prepared is a run that is ready to start.
type prepared struct {
	run  *Run
	job  Job
	info ReportInfo
}

func (s *Svc) RunChange(ctx context.Context, changeID, scenario string) (*Run, error) {
	who, _ := identity.From(ctx)
	if err := s.mayRun(ctx, who); err != nil {
		return nil, err
	}
	ch, err := s.o.Changes.Info(ctx, changeID)
	if err != nil {
		return nil, err
	}
	if ch.Worktree == "" {
		return nil, fmt.Errorf("%w: check out %s to run its lab (the report goes into the worktree)", ErrNoWorktree, changeID)
	}
	s.mu.Lock()
	if id, busy := s.active[changeID]; busy {
		s.mu.Unlock()
		if id == "" {
			id = changeID + " (starting)"
		}
		return nil, &BusyError{Run: id}
	}
	s.active[changeID] = ""
	number := 0
	for _, r := range s.runs {
		if r.ChangeID == changeID {
			number = max(number, r.Number)
		}
	}
	s.mu.Unlock()
	for _, lr := range ch.LabRuns {
		number = max(number, RunNumber(lr))
	}
	number++

	p, err := s.prepare(ctx, ch, number, scenario, who)
	if err != nil {
		s.mu.Lock()
		delete(s.active, changeID)
		s.mu.Unlock()
		return nil, err
	}
	run := p.run
	_ = s.store.saveYAML(run.ID, "lab.yaml", run.LabObject)
	_ = s.store.saveJSON(run.ID, "topology.json", p.job.Topology)

	bg := context.WithoutCancel(ctx)
	evs, err := s.o.Driver.Start(bg, p.job)
	if err != nil {
		s.mu.Lock()
		delete(s.active, changeID)
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	s.runs[run.ID] = run
	s.active[changeID] = run.ID
	_ = s.store.saveRun(run)
	c := copyRun(run)
	s.mu.Unlock()
	s.o.Log.Info("lab run started", "run", run.ID, "scenario", run.Scenario, "cluster", run.Cluster, "nodes", run.Nodes, "sha", short(run.SHA))
	go s.follow(bg, run.ID, evs, p.info)
	go s.changeUpdated(bg, changeID)
	return &c, nil
}

// prepare resolves the sources, picks the cluster, the machines under test
// and the scenario, and builds the Lab, the topology and the models.
func (s *Svc) prepare(ctx context.Context, ch *ChangeInfo, number int, scenario string, who identity.Identity) (*prepared, error) {
	id := ch.ID
	base, head, err := s.o.Changes.Sources(ctx, id)
	if err != nil {
		return nil, err
	}
	sha := ch.HeadSHA
	if sha == "" {
		sha = head.SHA
	}
	if head.Dirty || (sha != "" && head.SHA != sha) {
		// The lab tests the committed head; saved files are not in it.
		if head, err = s.o.Changes.Source(ctx, sha); err != nil {
			return nil, err
		}
	}
	head.Ref = id
	br, err := repo.Load(base.Dir, s.o.Registry)
	if err != nil {
		return nil, err
	}
	hr, err := repo.Load(head.Dir, s.o.Registry)
	if err != nil {
		return nil, err
	}
	paths := relevantPaths(id, render.ChangedPaths(br, hr))

	// Scenario: a Lab file of the change, a built-in, or the default.
	var custom *LabFile
	if scenario != "" && scenario != ScenarioMetal && scenario != ScenarioGitOps {
		for _, l := range labFiles(hr) {
			if l.Metadata.Name == scenario && l.Spec.Purpose == "change" {
				custom = &l
				break
			}
		}
		if custom == nil {
			return nil, fmt.Errorf("%w %q (built-in: %s, %s, or a Lab file with purpose change in .pk/labs)",
				ErrScenario, scenario, ScenarioMetal, ScenarioGitOps)
		}
	}
	if scenario == "" {
		scenario = DefaultScenario(paths)
	}

	cluster := s.pickCluster(ctx, base, head, br, hr, scenario)
	if custom != nil && custom.Spec.Topology.Name != "" {
		cluster = custom.Spec.Topology.Name
	}
	var bm, hm *Model
	if hm, err = ModelOf(ctx, s.o.Renderer, hr, head, cluster); err != nil {
		return nil, err
	}
	if bm, err = ModelOf(ctx, s.o.Renderer, br, base, cluster); err != nil {
		return nil, err
	}
	hm.Ref, hm.SHA = id, sha
	bm.Ref = "main"

	nodes := nodesUnderTest(bm, hm, br, hr, cluster, scenario == ScenarioMetal || custom != nil)

	labName := fmt.Sprintf("%s-run-%d", id, number)
	branch := ch.Branch
	lab := LabFile{APIVersion: "pk/v1alpha1", Kind: labkind.Name}
	lab.Metadata.Name = labName
	if custom != nil {
		lab.Spec = custom.Spec
	} else {
		steps, err := ScenarioSteps(scenario, nodes, FaultNetwork(hm, nodes))
		if err != nil {
			return nil, err
		}
		lab.Spec = LabSpec{Purpose: "change", Topology: labkind.TopologyRef{Name: cluster}, Scenario: steps,
			Results:  &labkind.Results{Collect: []string{"conditions", "events", "console-logs", "flux-status"}, Report: "markdown"},
			Lifetime: &labkind.Lifetime{TTL: "4h", KeepOnFailure: true}}
		if scenario == ScenarioMetal {
			lab.Spec.Fidelity, lab.Spec.Provision = "replica", "metal"
		} else {
			lab.Spec.Fidelity, lab.Spec.Provision = "scaled", "direct"
			lab.Spec.From = cluster + "-twin@nightly"
		}
	}
	lab.Spec.Purpose = "change"
	lab.Spec.Base = &labkind.Ref{Ref: "main"}
	lab.Spec.Change = &labkind.Ref{Ref: branch}
	if lab.Spec.Host == "" {
		lab.Spec.Host = s.o.Host
	}
	if lab.Spec.Provision == "" {
		lab.Spec.Provision = "direct"
	}
	if lab.Spec.Fidelity == "" {
		lab.Spec.Fidelity = "scaled"
	}

	var topo *Topology
	if cluster != "" {
		topo = GenerateTopology(hm, cluster, id)
		topo.SHA = sha
	}
	run := &Run{
		ID: RunID(id, number), ChangeID: id, Number: number, Lab: labName, SHA: sha, Ref: branch,
		Scenario: scenario, Driver: s.o.Driver.Name(), Host: lab.Spec.Host, Provision: lab.Spec.Provision,
		Fidelity: lab.Spec.Fidelity, Fresh: lab.Spec.From == "", Result: ResultRunning, By: who.User,
		StartedAt: time.Now().UTC(), Cluster: cluster, Nodes: nodes, LabObject: &lab,
		GatePaths: s.gatePaths(ctx, paths),
	}
	if run.By == "" {
		run.By = ch.Author
	}
	run.Steps = Plan(lab.Spec, cluster)
	info := ReportInfo{Title: ch.Title, Branch: branch, BaseSHA: base.SHA}
	if topo != nil {
		info.Topology = topo.Summary()
	}
	return &prepared{run: run, info: info,
		job: Job{Run: run, Spec: lab.Spec, Topology: topo, Base: bm, Head: hm, Nodes: nodes}}, nil
}

// relevantPaths drops the change metadata and the change's own lab reports.
func relevantPaths(id string, paths []string) []string {
	var out []string
	for _, p := range paths {
		if policy.IsMeta(p) {
			continue
		}
		if ok, _ := doublestar.Match("kb/labs/"+id+"-run-*.md", p); ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// pickCluster picks the cluster the lab emulates: the impacted cluster with
// the most Machine changes, else the most changes. A metal change that
// renders nothing (cabling) uses the cluster with the most Machines.
func (s *Svc) pickCluster(ctx context.Context, base, head render.Source, br, hr *repo.Repo, scenario string) string {
	best, bestScore := "", 0
	if imp, err := s.o.Renderer.Impact(ctx, base, head); err == nil {
		for _, c := range imp.Clusters {
			score := 0
			for _, k := range c.Kustomizations {
				for _, o := range k.Objects {
					score++
					if o.Ref.Kind == node.MachineKind {
						score += 100
					}
				}
			}
			if score > bestScore {
				best, bestScore = c.Cluster, score
			}
		}
	}
	if best != "" || scenario != ScenarioMetal {
		return best
	}
	most := 0
	for _, c := range render.ClustersOf(hr) {
		m, err := ModelOf(ctx, s.o.Renderer, hr, head, c.ID)
		if err != nil {
			continue
		}
		if n := len(m.Machines()); n > most {
			best, most = c.ID, n
		}
	}
	return best
}

// nodesUnderTest are the nodes of the cluster the change adds or changes;
// else the nodes that changed BGP policies select; else (metal) all.
func nodesUnderTest(bm, hm *Model, br, hr *repo.Repo, cluster string, metal bool) []string {
	if cluster == "" {
		return nil
	}
	var out []string
	for name, n := range hm.Inv.Nodes {
		if n.Spec.Cluster != cluster {
			continue
		}
		d := hm.Inv.NodeDocs[name]
		if d == nil || !repo.SameContent(br, hr, d.Path) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		before := map[string]BGPPolicy{}
		for _, p := range bm.BGPPolicies() {
			before[p.Name] = p
		}
		for _, p := range hm.BGPPolicies() {
			if old, ok := before[p.Name]; ok && sameStrings(old.Neighbors, p.Neighbors) && fmt.Sprint(old.Selector) == fmt.Sprint(p.Selector) {
				continue
			}
			for _, name := range hm.MachineNames() {
				if Selects(p.Selector, hm.NodeLabels(name)) && !contains(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	if len(out) == 0 && metal {
		out = hm.MachineNames()
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// gatePaths are the globs of the rules with a lab gate that the changed
// paths match, in policy order.
func (s *Svc) gatePaths(ctx context.Context, paths []string) []string {
	if s.o.Policy == nil {
		return nil
	}
	p, err := s.o.Policy.Current(ctx)
	if err != nil || p == nil {
		return nil
	}
	hit := map[string]bool{}
	for _, path := range paths {
		if r := p.MatchRule(path); r != nil && r.LabGate {
			hit[r.Name] = true
		}
	}
	var out []string
	for _, r := range p.Rules {
		if !hit[r.Name] {
			continue
		}
		for _, g := range r.Paths {
			if !contains(out, g) {
				out = append(out, g)
			}
		}
	}
	return out
}

// follow applies the driver events to the run, publishes them and writes
// the report at the end.
func (s *Svc) follow(ctx context.Context, runID string, evs <-chan Event, info ReportInfo) {
	result := ResultFail
	done := false
	for ev := range evs {
		if ev.Done {
			result, done = ev.Result, true
			continue
		}
		if ev.Step != nil {
			s.updateStep(runID, *ev.Step)
		}
	}
	if !done {
		result = ResultCancelled
	}
	s.finish(ctx, runID, result, info)
}

func (s *Svc) updateStep(runID string, st Step) {
	s.mu.Lock()
	r := s.runs[runID]
	if r == nil || st.Index < 0 || st.Index >= len(r.Steps) {
		s.mu.Unlock()
		return
	}
	r.Steps[st.Index] = st
	r.Count()
	r.Summary = fmt.Sprintf("%d of %d checks pass so far", r.ChecksPassed, r.ChecksTotal)
	_ = s.store.saveRun(r)
	changeID := r.ChangeID
	s.mu.Unlock()
	s.o.Events.Publish(events.LabStep, StepEventToAPI(runID, changeID, st))
}

func (s *Svc) finish(ctx context.Context, runID string, result Result, info ReportInfo) {
	s.mu.Lock()
	r := s.runs[runID]
	if r == nil {
		s.mu.Unlock()
		return
	}
	ri := len(r.Steps) - 1
	r.Count()
	r.Result = result
	r.FinishedAt = time.Now().UTC()
	r.SimulatedMs = 0
	for _, st := range r.Steps {
		r.SimulatedMs += st.SimulatedMs
	}
	r.Summary = r.SummaryText()
	write := result == ResultPass || result == ResultFail
	if write {
		r.Report = ReportPath(r.ChangeID, r.Number)
		r.Steps[ri].Status = StepRunning
		r.Steps[ri].StartedAt = time.Now().UTC()
	} else {
		r.Steps[ri].Status = StepSkipped
		r.Steps[ri].Detail = "not written"
	}
	_ = s.store.saveRun(r)
	step := r.Steps[ri]
	changeID := r.ChangeID
	s.mu.Unlock()
	s.o.Events.Publish(events.LabStep, StepEventToAPI(runID, changeID, step))

	var recErr error
	if write {
		s.mu.Lock()
		snapshot := copyRun(r)
		s.mu.Unlock()
		// The report states the lab time with its own step, as the run
		// page shows it once the report is written.
		snapshot.SimulatedMs += reportStepMs
		report := Report(&snapshot, info)
		_ = s.store.saveFile(runID, "report.md", report)
		ref := RunRef{Run: runID, SHA: snapshot.SHA, Result: result, Report: snapshot.Report}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		recErr = s.o.Changes.RecordLabRun(rctx, changeID, ref, report)
		cancel()
		if recErr != nil {
			s.o.Log.Warn("lab report not recorded", "run", runID, "err", recErr)
		}
	}

	s.mu.Lock()
	if write {
		st := &r.Steps[ri]
		st.FinishedAt = time.Now().UTC()
		st.SimulatedMs = reportStepMs
		if recErr != nil {
			st.Status = StepFail
			st.Detail = "not written"
			st.Output = append(st.Output, "the report was not committed: "+recErr.Error())
			r.Report = ""
		} else {
			st.Status = StepPass
			st.Detail = "written"
			st.Output = append(st.Output, "committed "+ReportPath(changeID, r.Number)+" (docs(lab): add report for "+runID+")",
				"Pk-Lab-Run: "+r.Trailer())
		}
		r.SimulatedMs += st.SimulatedMs
	}
	_ = s.store.saveRun(r)
	step = r.Steps[ri]
	final := copyRun(r)
	delete(s.active, changeID)
	s.mu.Unlock()
	s.o.Events.Publish(events.LabStep, StepEventToAPI(runID, changeID, step))
	s.o.Events.Publish(events.LabFinished, RunToAPI(&final))
	s.o.Log.Info("lab run finished", "run", runID, "result", final.Result, "summary", final.Summary)
	s.changeUpdated(ctx, changeID)
}

// reportStepMs is the lab time of the report step.
const reportStepMs = 5000

// changeUpdated publishes change.updated so the rail shows the lab state.
func (s *Svc) changeUpdated(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.o.Changes.Updated(ctx, id, "lab")
}

// Active returns the running run of a change ("" when none).
func (s *Svc) Active(changeID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[changeID]
}

// Wait blocks until the change has no running lab or ctx ends (tests, CLI).
func (s *Svc) Wait(ctx context.Context, changeID string) error {
	for {
		s.mu.Lock()
		_, busy := s.active[changeID]
		s.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
