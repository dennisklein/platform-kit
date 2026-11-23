package bench

// Tests of the area B handlers (kinds, entities, racks, KB, work board,
// impact, diff, gates, policy) with a fake change service over the seed.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

const seedState = "../../seed/state"

// fakeChanges serves trees from directories. Methods it does not override
// panic (the embedded interface is nil).
type fakeChanges struct {
	change.Service
	main    string
	trees   map[string]string // change id -> worktree
	heads   []change.Head
	list    []change.Summary
	gates   *policy.Gates
	renders render.Renderer
}

func (f *fakeChanges) src(ref, dir string) render.Source {
	return render.Source{Ref: ref, Dir: dir, Key: dir + "@" + time.Now().Format(time.RFC3339Nano)}
}

func (f *fakeChanges) MainDir(ctx context.Context) (string, error) { return f.main, nil }

func (f *fakeChanges) Source(ctx context.Context, ref string) (render.Source, error) {
	if ref == "main" {
		return render.Source{Ref: "main", Dir: f.main, SHA: "abc123"}, nil
	}
	if d, ok := f.trees[ref]; ok {
		return render.Source{Ref: ref, Dir: d, Dirty: true}, nil
	}
	return render.Source{}, change.ErrNotFound
}

func (f *fakeChanges) Sources(ctx context.Context, id string) (render.Source, render.Source, error) {
	d, ok := f.trees[id]
	if !ok {
		return render.Source{}, render.Source{}, change.ErrNotFound
	}
	return render.Source{Ref: "main", Dir: f.main, SHA: "abc123"}, render.Source{Ref: id, Dir: d, Dirty: true}, nil
}

func (f *fakeChanges) Heads(ctx context.Context) ([]change.Head, error) { return f.heads, nil }

func (f *fakeChanges) List(ctx context.Context, all bool) ([]change.Summary, error) {
	return f.list, nil
}

func (f *fakeChanges) Gates(ctx context.Context, id string) (*policy.Gates, error) {
	if f.gates == nil {
		return nil, errors.New("no gates")
	}
	return f.gates, nil
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func demoNode(name, unit, mgmt, datab, dataa string, cables [3]string) string {
	return strings.NewReplacer("NAME", name, "UNIT", unit, "MGMT", mgmt, "DATAB", datab, "DATAA", dataa,
		"C1", cables[0], "C2", cables[1], "C3", cables[2]).Replace(`apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: NAME
spec:
  cluster: defiant
  role: worker
  location:
    rack: GC5A-6Z
    unit: UNIT
    size: 2
  model: Supermicro SYS-220U-TNR
  bmc:
    mac: 3c:ec:ef:a1:0c:99
  ports:
    - {name: bmc, cable: C1, peer: {switch: mgmt, port: MGMT}, network: mgmt}
    - {name: p1, cable: C2, peer: {switch: data-b, port: DATAB}, network: data}
    - {name: p2, cable: C3, peer: {switch: data-a, port: DATAA}, network: data}
`)
}

func writeFile(t *testing.T, dir, p, content string) {
	t.Helper()
	full := filepath.Join(dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// demoSetup returns a server over main (seed/state), the demo change
// c-k7m2qz (cn-035, cn-036, BGP edit; saved, not committed) and c-p4x8ma
// (BGP peers on rack-6z).
func demoSetup(t *testing.T) (*Server, *fakeChanges, *events.Hub) {
	t.Helper()
	main := copyDir(t, seedState)
	demo := copyDir(t, seedState)
	writeFile(t, demo, "control/inventory/cn-035.yaml", demoNode("cn-035", "6", "37", "37", "38", [3]string{"MGT-0141", "DATA-0201", "DATA-0202"}))
	writeFile(t, demo, "control/inventory/cn-036.yaml", demoNode("cn-036", "8", "39", "39", "40", [3]string{"MGT-0142", "DATA-0203", "DATA-0204"}))
	bgp := "clusters/defiant/infra/cilium/bgp-rack-6z.yaml"
	b, _ := os.ReadFile(filepath.Join(seedState, bgp))
	writeFile(t, demo, bgp, strings.Replace(string(b), "          - cn-034\n", "          - cn-034\n          - cn-035\n          - cn-036\n", 1))
	writeFile(t, demo, ".pk/changes/c-k7m2qz.yaml", "apiVersion: pk/v1alpha1\nkind: Change\nid: c-k7m2qz\ntitle: Add cn-035 and cn-036 to defiant\nauthor: dennis\nagent: \"\"\ntarget: main\nparent: main\nworkItems: [w-114]\n")
	p4 := copyDir(t, seedState)
	pb, err := os.ReadFile("../../seed/changes/10-c-p4x8ma/after/" + bgp)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p4, bgp, string(pb))

	reg := all.Registry()
	fc := &fakeChanges{main: main, trees: map[string]string{"c-k7m2qz": demo, "c-p4x8ma": p4}}
	fc.heads = []change.Head{
		{ID: "c-k7m2qz", Title: "Add cn-035 and cn-036 to defiant", Author: "dennis", State: change.Lab, Worktree: demo, Dirty: true},
		{ID: "c-p4x8ma", Title: "BGP peers for org-a and org-b", Author: "anna", State: change.Draft}, // Heads only knows draft/review
	}
	fc.list = []change.Summary{
		{ID: "c-k7m2qz", Title: "Add cn-035 and cn-036 to defiant", Author: "dennis", State: change.Lab, WorkItems: []string{"w-114"}},
		{ID: "c-p4x8ma", Title: "BGP peers for org-a and org-b", Author: "anna", State: change.Review, WorkItems: []string{"w-112"}, MR: &change.MR{Number: 97}},
		{ID: "c-31d0aa", Title: "Cabling for GC5A-6Z", Author: "dennis", State: change.Deployed, WorkItems: []string{"w-108"}, MergedAt: time.Now()},
	}
	hub := events.NewHub()
	eng := render.New(reg)
	cfg := config.Config{Addr: "127.0.0.1:8080", DataDir: t.TempDir(), User: "dennis",
		AdminToken: "pk_adm_test_token_0123456789abcdef0123", PublicURL: "http://127.0.0.1:8080"}
	pol := policy.NewEngine(fc.MainDir)
	s, err := New(cfg, Deps{Events: hub, Changes: fc, Kinds: reg, Renderer: eng, Policy: pol}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Gates as the change service computes them: impact objects + policy.
	imp, err := eng.Impact(context.Background(), render.Source{Ref: "main", Dir: main}, render.Source{Ref: "c-k7m2qz", Dir: demo})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pol.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	author, _ := p.Identify("dennis")
	g := p.Evaluate(policy.GateInput{ChangeID: "c-k7m2qz", Author: author, Writers: []identity.Identity{author}, Paths: imp.Paths(), Objects: imp.Objects(), Target: "main"})
	fc.gates = &g
	return s, fc, hub
}

func TestDisplayOf(t *testing.T) {
	sel := map[string]any{"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "In",
		"values": []any{"cn-025", "cn-026", "cn-027", "cn-036"}}}}
	cases := []struct {
		v    any
		want string
	}{
		{sel, "kubernetes.io/hostname in cn-025 … cn-036 (4)"},
		{map[string]any{"matchLabels": map[string]any{"app": "podinfo"}}, "app=podinfo"},
		{[]any{map[string]any{"name": "bmc"}, map[string]any{"name": "p1"}}, "bmc, p1"},
		{[]any{map[string]any{"name": "podinfo", "image": "ghcr.io/stefanprodan/podinfo:6.8.0"}}, "ghcr.io/stefanprodan/podinfo:6.8.0"},
		{map[string]any{"labels": map[string]any{"app": "podinfo"}}, "labels: app=podinfo"},
		{[]any{map[string]any{"localASN": 64512}}, "1 item"},
	}
	for _, c := range cases {
		if got := displayOf("", c.v, false); got != c.want {
			t.Errorf("displayOf(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestImpactHandler(t *testing.T) {
	s, _, hub := demoSetup(t)
	ctx := context.Background()
	res, err := s.GetImpact(ctx, api.GetImpactRequestObject{Id: "c-k7m2qz"})
	if err != nil {
		t.Fatal(err)
	}
	imp := api.Impact(res.(api.GetImpact200JSONResponse))
	var got []string
	for _, c := range imp.Clusters {
		for _, k := range c.Kustomizations {
			for _, o := range k.Objects {
				got = append(got, c.Cluster+" "+string(o.Op)+" "+o.Kind+"/"+o.Name)
			}
		}
	}
	want := "defiant add Machine/cn-035,defiant add Machine/cn-036,defiant change CiliumBGPPeeringPolicy/rack-6z"
	if strings.Join(got, ",") != want {
		t.Errorf("objects %v", got)
	}
	if imp.Summary.Text != "defiant +2 ~1" || imp.Summary.Added != 2 || imp.Summary.Changed != 1 || len(imp.Summary.Clusters) != 3 {
		t.Errorf("summary %+v", imp.Summary)
	}
	for _, c := range imp.Summary.Clusters {
		if c.Cluster != "defiant" && c.Added+c.Changed+c.Deleted != 0 {
			t.Errorf("%s changes: %+v", c.Cluster, c)
		}
	}
	if imp.Risk.Level != api.High || imp.Summary.Risk != api.High {
		t.Errorf("risk %+v", imp.Risk)
	}
	if len(imp.Overlaps) != 1 || imp.Overlaps[0].Change != "c-p4x8ma" || imp.Overlaps[0].Objects[0].Kind != "CiliumBGPPeeringPolicy" || imp.Overlaps[0].State != api.ChangeStateReview {
		t.Errorf("overlaps %+v", imp.Overlaps)
	}
	if len(imp.Semantic) != 3 || imp.Semantic[0].Text != "new compute node in GC5A-6Z, U6" || imp.Semantic[2].Subject != "rack-6z" {
		t.Errorf("semantic %+v", imp.Semantic)
	}
	if !imp.Head.Dirty || imp.Base.Ref != "main" {
		t.Errorf("refs %+v %+v", imp.Base, imp.Head)
	}

	// Views.
	for view, check := range map[api.DiffViewKind]func(api.Diff) bool{
		api.DiffViewKindSemantic: func(d api.Diff) bool { return len(d.Semantic) == 3 },
		api.DiffViewKindSource:   func(d api.Diff) bool { return len(d.Files) == 4 },
		api.DiffViewKindRendered: func(d api.Diff) bool { return len(d.Objects) == 3 && strings.Contains(d.Objects[0].Diff, "+") },
	} {
		r, err := s.GetDiff(ctx, api.GetDiffRequestObject{Id: "c-k7m2qz", Params: api.GetDiffParams{View: view}})
		if err != nil {
			t.Fatal(err)
		}
		if d := api.Diff(r.(api.GetDiff200JSONResponse)); !check(d) {
			t.Errorf("view %s: %+v", view, d)
		}
	}
	r, _ := s.GetDiff(ctx, api.GetDiffRequestObject{Id: "c-k7m2qz", Params: api.GetDiffParams{View: api.DiffViewKindRendered, Cluster: "phoenix"}})
	if d := api.Diff(r.(api.GetDiff200JSONResponse)); len(d.Objects) != 0 || d.Objects == nil {
		t.Errorf("phoenix: %+v", d.Objects)
	}

	// Gates: risk high, 2 approvals, lab gate.
	gr, err := s.GetGates(ctx, api.GetGatesRequestObject{Id: "c-k7m2qz"})
	if err != nil {
		t.Fatal(err)
	}
	g := api.Gates(gr.(api.GetGates200JSONResponse))
	if g.Risk.Level != api.High || g.Approvals.Need != 2 || !g.LabGate.Required || g.Mergeable {
		t.Errorf("gates %+v", g)
	}

	// Render after save publishes render.updated.
	ch, cancel := hub.Subscribe("", 0)
	defer cancel()
	sum := s.renderAfterSave(ctx, "c-k7m2qz")
	if sum == nil || sum.Text != "defiant +2 ~1" {
		t.Fatalf("summary %+v", sum)
	}
	select {
	case ev := <-ch:
		if ev.Type != events.RenderUpdated {
			t.Errorf("event %s", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Error("no render.updated")
	}
}

func TestEntityHandlers(t *testing.T) {
	s, _, _ := demoSetup(t)
	ctx := context.Background()

	res, err := s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "Node", Name: "cn-035", Params: api.GetEntityParams{Ref: "c-k7m2qz"}})
	if err != nil {
		t.Fatal(err)
	}
	e := api.Entity(res.(api.GetEntity200JSONResponse))
	if e.Change.Id != "c-k7m2qz" || e.Change.Status != api.FileChangeAdded || len(e.Semantic) != 1 {
		t.Errorf("change %+v semantic %+v", e.Change, e.Semantic)
	}
	fields := map[string]api.EntityField{}
	for _, f := range e.Fields {
		fields[f.Field] = f
	}
	if f := fields["spec.location"]; f.Display != "GC5A-6Z U6 (2U)" || f.Truth != api.Intent || f.Group != "Intent" || f.Source.Line != 8 {
		t.Errorf("location %+v", f)
	}
	if f := fields["spec.model"]; f.Owner != "asset-db" || f.Truth != api.Asserted || f.Group != "Hardware facts" {
		t.Errorf("model %+v", f)
	}
	if f := fields["spec.bmc.mac"]; f.Truth != api.Asserted || f.Group != "Hardware facts" {
		t.Errorf("bmc %+v", f)
	}
	if f := fields["spec.bmc.fixture"]; f.Truth != api.Derived || f.Display != "qemu + Redfish sim supermicro-x12" || f.Source.Path != ".pk/emulation.yaml" {
		t.Errorf("emulation %+v", f)
	}
	if e.Title != "compute node" {
		t.Errorf("title %q", e.Title)
	}
	if len(e.Ports) != 3 || !e.Ports[2].Ok || e.ApplyPath != "Flux, metal operator" {
		t.Errorf("ports %+v apply %q", e.Ports, e.ApplyPath)
	}
	refs := map[string]api.EntityRef{}
	for _, r := range e.References {
		refs[r.Field] = r
	}
	if r := refs["spec.location.rack"]; !r.Exists || r.Kind != "Rack" || r.Path != "control/inventory/racks/gc5a-6z.yaml" {
		t.Errorf("rack ref %+v", r)
	}
	if r := refs["spec.ports[1].peer.switch"]; !r.Exists || r.Name != "data-b" {
		t.Errorf("switch ref %+v (all %+v)", r, e.References)
	}
	// The BGP policy selects cn-035: an entity backlink.
	found := false
	for _, b := range e.Backlinks.Entities {
		if b.Kind == "CiliumBGPPeeringPolicy" && b.Name == "rack-6z" {
			found = true
		}
	}
	if !found {
		t.Errorf("backlinks %+v", e.Backlinks.Entities)
	}
	if len(e.Backlinks.Changes) != 1 || e.Backlinks.Changes[0].Id != "c-k7m2qz" {
		t.Errorf("change backlinks %+v", e.Backlinks.Changes)
	}

	// Port in use shows on the entity and the rack.
	fc := s.d.Changes.(*fakeChanges)
	writeFile(t, fc.trees["c-k7m2qz"], "control/inventory/cn-035.yaml", demoNode("cn-035", "6", "37", "37", "37", [3]string{"MGT-0141", "DATA-0201", "DATA-0202"}))
	res, _ = s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "node", Name: "cn-035", Params: api.GetEntityParams{Ref: "c-k7m2qz"}})
	e = api.Entity(res.(api.GetEntity200JSONResponse))
	if e.Ports[2].Ok || e.Ports[2].Message != "port data-a 37 in use by cn-003" {
		t.Errorf("port check %+v", e.Ports)
	}

	// Rack view with highlights.
	rr, err := s.GetRack(ctx, api.GetRackRequestObject{Name: "GC5A-6Z", Params: api.GetRackParams{Ref: "c-k7m2qz"}})
	if err != nil {
		t.Fatal(err)
	}
	rk := api.Rack(rr.(api.GetRack200JSONResponse))
	hl := map[string]api.RackOccupantHighlight{}
	for _, o := range rk.Occupants {
		hl[o.Name] = o.Highlight
	}
	if hl["cn-035"] != api.RackOccupantHighlightAdded || hl["cn-036"] != api.RackOccupantHighlightAdded || hl["cn-025"] != api.RackOccupantHighlightNone {
		t.Errorf("highlights %v", hl)
	}
	if rk.Units != 47 || rk.Row != "6" || len(rk.Free) == 0 {
		t.Errorf("rack %+v", rk)
	}
	for _, u := range rk.Free {
		if u == 6 || u == 7 || u == 8 || u == 9 {
			t.Errorf("unit %d is not free", u)
		}
	}

	// Entities list and a 404.
	lr, err := s.ListEntities(ctx, api.ListEntitiesRequestObject{Params: api.ListEntitiesParams{Kind: "Node"}})
	if err != nil {
		t.Fatal(err)
	}
	if l := lr.(api.ListEntities200JSONResponse); len(l) != 15 || l[0].Cluster == "" {
		t.Errorf("nodes on main: %d %+v", len(l), l[0])
	}
	if _, err := s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "Node", Name: "cn-035"}); statusOf(err).Status != http.StatusNotFound {
		t.Errorf("cn-035 on main: %v", err)
	}

	// The HelmRelease hides the SOPS token.
	res, err = s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "HelmRelease", Name: "cilium"})
	if err != nil {
		t.Fatal(err)
	}
	e = api.Entity(res.(api.GetEntity200JSONResponse))
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "ENC[") {
		t.Errorf("secret leaked in entity")
	}
	// HelmRelease cilium exists in every cluster: the first in path order,
	// the others as alternatives, one picked by cluster or path.
	if e.Cluster != "defiant" || e.Path != "clusters/defiant/infra/cilium/helmrelease.yaml" || len(e.Alternatives) != 2 ||
		e.Alternatives[0].Cluster != "pegasus" || e.Alternatives[1].Cluster != "phoenix" {
		t.Errorf("cilium: cluster %q path %q alternatives %+v", e.Cluster, e.Path, e.Alternatives)
	}
	res, err = s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "HelmRelease", Name: "cilium", Params: api.GetEntityParams{Cluster: "phoenix"}})
	if err != nil {
		t.Fatal(err)
	}
	if e = api.Entity(res.(api.GetEntity200JSONResponse)); e.Cluster != "phoenix" || e.Path != "clusters/phoenix/infra/cilium/helmrelease.yaml" || len(e.Alternatives) != 2 {
		t.Errorf("cilium of phoenix: %q %q %d", e.Cluster, e.Path, len(e.Alternatives))
	}
	res, err = s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "HelmRelease", Name: "cilium", Params: api.GetEntityParams{Path: "clusters/pegasus/infra/cilium/helmrelease.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if e = api.Entity(res.(api.GetEntity200JSONResponse)); e.Cluster != "pegasus" {
		t.Errorf("cilium by path: %q", e.Cluster)
	}
	if _, err := s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "HelmRelease", Name: "cilium", Params: api.GetEntityParams{Cluster: "nowhere"}}); statusOf(err).Status != http.StatusNotFound {
		t.Errorf("cilium of an unknown cluster: %v", err)
	}
	// Unique names have no alternatives.
	res, _ = s.GetEntity(ctx, api.GetEntityRequestObject{Kind: "Node", Name: "cn-003"})
	if e = api.Entity(res.(api.GetEntity200JSONResponse)); len(e.Alternatives) != 0 || e.Cluster != "defiant" {
		t.Errorf("cn-003: %q %+v", e.Cluster, e.Alternatives)
	}
}

func TestKindsKBWork(t *testing.T) {
	s, _, _ := demoSetup(t)
	ctx := context.Background()
	kr, _ := s.ListKinds(ctx, api.ListKindsRequestObject{})
	kl := kr.(api.ListKinds200JSONResponse)
	var node api.Kind
	for _, k := range kl {
		if k.Name == "Node" {
			node = k
		}
	}
	if node.ApiVersion != "inventory.pk.io/v1beta1" || len(node.Hooks) != 5 || node.SchemaUrl == "" {
		t.Errorf("node kind %+v", node)
	}
	sr, err := s.GetKindSchema(ctx, api.GetKindSchemaRequestObject{Kind: "Node", Params: api.GetKindSchemaParams{Version: "v1alpha1"}})
	if err != nil {
		t.Fatal(err)
	}
	if sch := sr.(api.GetKindSchema200JSONResponse); sch["properties"] == nil {
		t.Errorf("schema %v", sch)
	}

	ar, err := s.ListArticles(ctx, api.ListArticlesRequestObject{Params: api.ListArticlesParams{Entity: "jb-05"}})
	if err != nil {
		t.Fatal(err)
	}
	if l := ar.(api.ListArticles200JSONResponse); len(l) == 0 || l[0].Path == "" {
		t.Errorf("articles for jb-05: %+v", l)
	}
	gr, err := s.GetArticle(ctx, api.GetArticleRequestObject{Params: api.GetArticleParams{Path: "kb/runbooks/replace-failed-disk.md"}})
	if err != nil {
		t.Fatal(err)
	}
	a := api.Article(gr.(api.GetArticle200JSONResponse))
	if a.Type != api.ArticleTypeRunbook || len(a.Entities) != 3 || !a.Entities[0].Exists || strings.HasPrefix(a.Body, "---") {
		t.Errorf("article %+v", a)
	}
	// w-114 lists the add-node runbook, so the demo change cites it.
	gr, err = s.GetArticle(ctx, api.GetArticleRequestObject{Params: api.GetArticleParams{Path: "kb/runbooks/add-node.md"}})
	if err != nil {
		t.Fatal(err)
	}
	a = api.Article(gr.(api.GetArticle200JSONResponse))
	if !slices.ContainsFunc(a.CitedBy, func(l api.ChangeLink) bool { return l.Id == "c-k7m2qz" }) {
		t.Errorf("add-node cited by %+v", a.CitedBy)
	}

	wr, err := s.GetWorkBoard(ctx, api.GetWorkBoardRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	wb := api.WorkBoard(wr.(api.GetWorkBoard200JSONResponse))
	st := map[string]api.WorkStatus{}
	for _, it := range wb.Items {
		st[it.Id] = it.Status
	}
	if st["w-114"] != api.InProgress || st["w-112"] != api.InReview || st["w-108"] != api.Done || st["w-117"] != api.Planned {
		t.Errorf("statuses %v", st)
	}
	if len(wb.Milestones) != 1 || wb.Milestones[0].Done != 1 || wb.Milestones[0].Items < 4 {
		t.Errorf("milestones %+v", wb.Milestones)
	}

	// Policy summary and forge rules.
	pr, err := s.GetPolicySummary(ctx, api.GetPolicySummaryRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	ps := api.PolicySummary(pr.(api.GetPolicySummary200JSONResponse))
	if len(ps.People) == 0 || len(ps.Rules) == 0 || len(ps.Problems) != 0 {
		t.Errorf("policy %+v", ps)
	}
	fr, err := s.GetForgeRules(ctx, api.GetForgeRulesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if f := fr.(api.GetForgeRules200JSONResponse); f.BranchProtection.Branch != "main" || !strings.Contains(f.Codeowners, "@") {
		t.Errorf("forge rules %+v", f)
	}
}

func TestFileHelpers(t *testing.T) {
	s, fc, _ := demoSetup(t)
	ctx := context.Background()
	p := "clusters/defiant/infra/cilium/helmrelease.yaml"
	stored, _ := os.ReadFile(filepath.Join(fc.main, p))
	f := s.fileContent(ctx, fc.main, p, stored)
	if strings.Contains(f.Content, "ENC[") || len(f.Secrets) != 2 || f.Kind.Name != "HelmRelease" || len(f.Problems) != 0 {
		t.Errorf("file %+v", f)
	}
	edited := strings.Replace(f.Content, "interval: 5m", "interval: 5 minutes", 1)
	back, err := s.unmaskContent(p, []byte(edited), stored)
	if err != nil || !strings.Contains(string(back), "ENC[") {
		t.Fatalf("unmask: %v", err)
	}
	probs := s.fileContent(ctx, fc.main, p, back).Problems
	if len(probs) != 1 || probs[0].Message != "spec.interval: must be a duration, for example 5m" || probs[0].Line != 7 {
		t.Errorf("problems %+v", probs)
	}
	np, content, err := s.newFileSkeleton("Node", "cn-037")
	if err != nil || np != "control/inventory/cn-037.yaml" || !strings.Contains(string(content), "cn-037") {
		t.Errorf("skeleton %s %v", np, err)
	}
	out, err := s.setFields("control/inventory/cn-003.yaml", mustRead(t, filepath.Join(fc.main, "control/inventory/cn-003.yaml")), map[string]any{"spec.ports[2].peer.port": 38})
	if err != nil || !strings.Contains(string(out), "{switch: data-a, port: 38}") {
		t.Errorf("setFields: %v\n%s", err, out)
	}
	if ps := s.problemsFor(ctx, fc.trees["c-k7m2qz"], ""); len(ps) != 0 {
		for _, p := range ps {
			if p.Severity == api.ProblemSeverityError {
				t.Errorf("demo tree problem %+v", p)
			}
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDeriveWorkStatus(t *testing.T) {
	cases := []struct {
		states []api.ChangeState
		want   api.WorkStatus
	}{
		{nil, api.Planned},
		{[]api.ChangeState{api.ChangeStateClosed}, api.Planned},
		{[]api.ChangeState{api.ChangeStateLab}, api.InProgress},
		{[]api.ChangeState{api.ChangeStateApproved}, api.InReview},
		{[]api.ChangeState{api.ChangeStateVerified, api.ChangeStateDraft}, api.InProgress},
		{[]api.ChangeState{api.ChangeStateMerged, api.ChangeStateDeployed}, api.Done},
	}
	for _, c := range cases {
		var links []api.ChangeLink
		for i, s := range c.states {
			links = append(links, api.ChangeLink{Id: "c-" + string(rune('a'+i)), State: s})
		}
		if got, _ := DeriveWorkStatus(links); got != c.want {
			t.Errorf("%v: got %s want %s", c.states, got, c.want)
		}
	}
}

// TestBHTTPNoNulls calls the area B endpoints over HTTP: 200 and no null
// arrays in the JSON.
func TestBHTTPNoNulls(t *testing.T) {
	s, _, _ := demoSetup(t)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	auth := map[string]string{"Authorization": "Bearer pk_adm_test_token_0123456789abcdef0123"}
	for _, p := range []string{
		"/kinds", "/kinds/HelmRelease/schema", "/entities?kind=Node", "/entities/Node/cn-035?ref=c-k7m2qz", "/entities/Rack/GC5A-6Z",
		"/racks/GC5A-6Z?ref=c-k7m2qz", "/kb", "/kb/article?path=kb/runbooks/add-node.md", "/work",
		"/changes/c-k7m2qz/impact", "/changes/c-k7m2qz/diff?view=rendered", "/changes/c-k7m2qz/diff?view=source",
		"/changes/c-k7m2qz/gates", "/policy", "/policy/forge-rules",
	} {
		res := do(t, ts, "GET", BasePath+p, auth, "")
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Errorf("%s: %d %s", p, res.StatusCode, b)
			continue
		}
		if strings.Contains(string(b), ":null") {
			t.Errorf("%s: null in %s", p, b)
		}
	}
}
