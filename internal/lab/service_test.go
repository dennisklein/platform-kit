package lab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

type fakeChanges struct {
	mu       sync.Mutex
	base     string
	head     string
	recorded []RunRef
	reports  map[string]string
	updated  int
}

func (f *fakeChanges) Info(ctx context.Context, id string) (*ChangeInfo, error) {
	return &ChangeInfo{ID: id, Title: "Add cn-035 and cn-036 to defiant", Author: "dennis", Branch: "change/" + id + "-add-cn-035",
		HeadSHA: "a41f2c9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Worktree: f.head, LabRuns: []string{}}, nil
}
func (f *fakeChanges) MainDir(ctx context.Context) (string, error) { return f.base, nil }
func (f *fakeChanges) Sources(ctx context.Context, id string) (render.Source, render.Source, error) {
	return render.Source{Ref: "main", SHA: "5e81d0a0000", Dir: f.base, Key: "base"},
		render.Source{Ref: id, SHA: "a41f2c9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Dir: f.head, Key: "head"}, nil
}
func (f *fakeChanges) Source(ctx context.Context, ref string) (render.Source, error) {
	if ref == "main" {
		return render.Source{Ref: "main", Dir: f.base, Key: "base"}, nil
	}
	return render.Source{Ref: ref, Dir: f.head, Key: "head"}, nil
}
func (f *fakeChanges) RecordLabRun(ctx context.Context, id string, run RunRef, report []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, run)
	if f.reports == nil {
		f.reports = map[string]string{}
	}
	f.reports[run.Report] = string(report)
	return nil
}
func (f *fakeChanges) Updated(ctx context.Context, id, reason string) {
	f.mu.Lock()
	f.updated++
	f.mu.Unlock()
}

type recorder struct {
	mu  sync.Mutex
	evs []events.Type
	fin []any
}

func (r *recorder) Publish(t events.Type, data any) { r.PublishTo("", t, data) }
func (r *recorder) PublishTo(_ string, t events.Type, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, t)
	if t == events.LabFinished {
		r.fin = append(r.fin, data)
	}
}

func writeTree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for p, b := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newService(t *testing.T, head map[string][]byte) (*Svc, *fakeChanges, *recorder, string) {
	t.Helper()
	fc := &fakeChanges{base: writeTree(t, seedFiles(t)), head: writeTree(t, head)}
	rec := &recorder{}
	reg := all.Registry()
	dir := t.TempDir()
	eng := policy.NewEngine(func(ctx context.Context) (string, error) { return fc.base, nil })
	s, err := New(Options{Dir: dir, Speed: 1e6, Changes: fc, Registry: reg, Renderer: render.New(reg), Policy: eng, Events: rec})
	if err != nil {
		t.Fatal(err)
	}
	return s, fc, rec, dir
}

func runLab(t *testing.T, s *Svc, ctx context.Context, id string) *Run {
	t.Helper()
	r, err := s.RunChange(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.Wait(wctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func human(ctx context.Context) context.Context {
	return identity.With(ctx, identity.Identity{User: "dennis", Kind: identity.Human})
}

func TestRunChangePass(t *testing.T) {
	s, fc, rec, dir := newService(t, demoFiles(t, "cn-035", "cn-036"))
	ctx := human(context.Background())
	r := runLab(t, s, ctx, "c-k7m2qz")
	if r.Result != ResultPass {
		t.Fatalf("result %s: %s %v", r.Result, r.Summary, r.Failures)
	}
	if r.ID != "c-k7m2qz/run-1" || r.Scenario != ScenarioMetal || r.Cluster != "defiant" || strings.Join(r.Nodes, ",") != "cn-035,cn-036" {
		t.Fatalf("run = %+v", r)
	}
	if r.Summary != "12 of 12 checks pass" {
		t.Fatalf("summary = %q", r.Summary)
	}
	// The lab time of the talk: pk_lab_run ok run-3 pass, 6m 40s.
	if lt := Human(time.Duration(r.SimulatedMs) * time.Millisecond); lt != "6m 40s" {
		t.Fatalf("lab time = %s", lt)
	}
	var titles []string
	for _, st := range r.Steps {
		titles = append(titles, st.Title+"/"+st.Detail+"/"+string(st.Status))
	}
	want := []string{"topology/generate/pass", "BMC/Redfish sim/pass", "install/Talos/pass", "join/defiant/pass",
		"nodes Ready/2 of 2/pass", "BGP/2 peers up/pass", "power cycle cn-036/back in 3m 10s/pass",
		"data-b link down/traffic via data-a/pass", "checks/12 of 12/pass", "report/written/pass"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("steps:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(r.GatePaths, ",") != "control/inventory/**,control/cabling.yaml,clusters/*/infra/cilium/**" {
		t.Fatalf("gate paths = %v", r.GatePaths)
	}
	if len(fc.recorded) != 1 || fc.recorded[0].Result != ResultPass || fc.recorded[0].Report != "kb/labs/c-k7m2qz-run-1.md" {
		t.Fatalf("recorded = %+v", fc.recorded)
	}
	report := fc.reports["kb/labs/c-k7m2qz-run-1.md"]
	if out := os.Getenv("LAB_REPORT_OUT"); out != "" {
		_ = os.WriteFile(out, []byte(report), 0o644)
	}
	for _, s := range []string{"type: lab-report", "entities: [defiant, cn-035, cn-036]", "changes: [c-k7m2qz]", "result: pass",
		"| Machines under test | cn-035, cn-036 |", "`Pk-Lab-Run: c-k7m2qz/run-1 pass kb/labs/c-k7m2qz-run-1.md`",
		"- pass: bmc: power cycle: cn-036: back in 3m 10s", "kind: Lab", "name: c-k7m2qz-run-1",
		// The same lab time as the run page; globs as code, not bold.
		"Lab time 6m 40s", "| Lab gate | `control/inventory/**`, `control/cabling.yaml`, `clusters/*/infra/cilium/**` met |"} {
		if !strings.Contains(report, s) {
			t.Errorf("report lacks %q:\n%s", s, report)
		}
	}
	for _, f := range []string{"run.json", "lab.yaml", "topology.json", "report.md"} {
		if _, err := os.Stat(filepath.Join(dir, "c-k7m2qz", "run-1", f)); err != nil {
			t.Errorf("raw result %s: %v", f, err)
		}
	}
	rec.mu.Lock()
	steps, fin := 0, len(rec.fin)
	for _, e := range rec.evs {
		if e == events.LabStep {
			steps++
		}
	}
	rec.mu.Unlock()
	if steps < 20 || fin != 1 {
		t.Fatalf("events: %d lab.step, %d lab.finished", steps, fin)
	}

	// The next run is run-2; a restart keeps both.
	r2 := runLab(t, s, ctx, "c-k7m2qz")
	if r2.ID != "c-k7m2qz/run-2" {
		t.Fatalf("second run %s", r2.ID)
	}
	s2, err := New(Options{Dir: dir, Changes: fc, Registry: all.Registry(), Renderer: render.New(all.Registry())})
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := s2.Runs(ctx, "c-k7m2qz")
	if len(runs) != 2 || runs[0].ID != "c-k7m2qz/run-2" || runs[1].Result != ResultPass {
		t.Fatalf("after restart: %+v", runs)
	}
}

func TestRunChangeMissingBGP(t *testing.T) {
	s, fc, _, _ := newService(t, demoFiles(t, "cn-035"))
	r := runLab(t, s, human(context.Background()), "c-k7m2qz")
	if r.Result != ResultFail {
		t.Fatalf("result = %s", r.Result)
	}
	if len(r.Failures) != 1 || r.Failures[0] != "cn-036: no BGP peering policy selects it" {
		t.Fatalf("failures = %q", r.Failures)
	}
	if !strings.HasSuffix(r.Summary, "· cn-036: no BGP peering policy selects it") {
		t.Fatalf("summary = %q", r.Summary)
	}
	if fc.recorded[0].Result != ResultFail {
		t.Fatal("not recorded as fail")
	}
	if !strings.Contains(fc.reports[r.Report], "- FAIL: bgp: selected: cn-036: no BGP peering policy selects it") {
		t.Fatalf("report:\n%s", fc.reports[r.Report])
	}
}

func TestRunChangeGitOpsAndRights(t *testing.T) {
	files := seedFiles(t)
	p := "clusters/defiant/apps/podinfo/deployment.yaml"
	files[p] = []byte(strings.Replace(string(files[p]), "podinfo:6.8.0", "podinfo:6.9.0", 1))
	s, _, _, _ := newService(t, files)
	agent := identity.With(context.Background(), identity.Identity{User: "dennis-agent", Kind: identity.Agent, ActsFor: "dennis"})
	r := runLab(t, s, agent, "c-6m1pqd")
	if r.Result != ResultPass || r.Scenario != ScenarioGitOps || r.Provision != "direct" || r.Fresh || len(r.Nodes) != 0 {
		t.Fatalf("run = %+v", r)
	}
	if r.By != "dennis-agent" {
		t.Fatalf("by = %q", r.By)
	}
	bot := identity.With(context.Background(), identity.Identity{User: "sync-bot", Kind: identity.Bot})
	if _, err := s.RunChange(bot, "c-6m1pqd", ""); err == nil || !strings.Contains(err.Error(), "run labs") {
		t.Fatalf("bot err = %v", err)
	}
	if _, err := s.RunChange(human(context.Background()), "c-6m1pqd", "nope"); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}

func TestBusy(t *testing.T) {
	s, _, _, _ := newService(t, demoFiles(t, "cn-035", "cn-036"))
	s.o.Driver = NewSim(1) // slow: the run is still going
	ctx := human(context.Background())
	r, err := s.RunChange(ctx, "c-k7m2qz", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RunChange(ctx, "c-k7m2qz", "")
	if be, ok := err.(*BusyError); !ok || be.Run != r.ID {
		t.Fatalf("err = %v", err)
	}
	if err := s.o.Driver.Cancel(r.ID); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Wait(wctx, "c-k7m2qz"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, r.ID)
	if got.Result != ResultCancelled {
		t.Fatalf("result = %s", got.Result)
	}
}

func TestTopologyService(t *testing.T) {
	s, _, _, _ := newService(t, demoFiles(t, "cn-035", "cn-036"))
	ctx := context.Background()
	topo, err := s.Topology(ctx, "main", "defiant-twin")
	if err != nil {
		t.Fatal(err)
	}
	if topo.Name != "defiant-twin" || len(topo.Machines) != 3 {
		t.Fatalf("twin topology = %s %d", topo.Name, len(topo.Machines))
	}
	// 3 cabled ports per machine plus the switch uplinks of the cabling.
	for _, l := range topo.Links {
		if m, _, _ := strings.Cut(l.A, ":"); strings.HasPrefix(m, "cn-") && topo.Machine(m) == nil {
			t.Fatalf("link of %s in the twin: %+v", m, l)
		}
	}
	if topo.Machines[0].Machine.Backend != "talos-docker" {
		t.Fatalf("twin backend = %q", topo.Machines[0].Machine.Backend)
	}
	if len(topo.Links) < 9 {
		t.Fatalf("twin links = %d", len(topo.Links))
	}
	if _, err := s.Topology(ctx, "main", "nowhere"); err == nil {
		t.Fatal("unknown cluster accepted")
	}
	defs, err := s.Definitions(ctx)
	if err != nil || len(defs) != 1 || defs[0].Spec.Purpose != "twin" {
		t.Fatalf("definitions = %+v %v", defs, err)
	}
}

// Forget drops the runs of an old forge state and keeps newer ones.
func TestForgetOldRuns(t *testing.T) {
	dir := t.TempDir()
	st := store{dir: dir}
	t0 := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	for _, r := range []*Run{
		{ID: "c-31d0aa/run-2", ChangeID: "c-31d0aa", Result: ResultPass, StartedAt: t0.Add(-time.Hour)},
		{ID: "c-p4x8ma/run-2", ChangeID: "c-p4x8ma", Result: ResultFail, StartedAt: t0.Add(-time.Minute)},
		{ID: "c-p4x8ma/run-3", ChangeID: "c-p4x8ma", Result: ResultPass, StartedAt: t0.Add(time.Minute)},
	} {
		if err := st.saveRun(r); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Forget(t0); n != 2 {
		t.Fatalf("forgot %d runs", n)
	}
	runs, _ := s.Runs(context.Background(), "")
	if len(runs) != 1 || runs[0].ID != "c-p4x8ma/run-3" {
		t.Fatalf("runs %+v", runs)
	}
	if _, err := os.Stat(filepath.Join(dir, "c-31d0aa")); !os.IsNotExist(err) {
		t.Fatalf("raw results of the old run kept: %v", err)
	}
	// After a restart the old runs stay gone.
	s2, _ := New(Options{Dir: dir})
	if runs, _ := s2.Runs(context.Background(), ""); len(runs) != 1 {
		t.Fatalf("after restart %+v", runs)
	}
}
