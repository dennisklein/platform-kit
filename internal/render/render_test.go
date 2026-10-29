package render_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

const seed = "../../seed/state"

func copyTree(t *testing.T, src string) string {
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

func node(name string, unit, mgmt, datab, dataa int, cables [3]string) string {
	return strings.NewReplacer("NAME", name, "UNIT", itoa(unit), "MGMT", itoa(mgmt), "DATAB", itoa(datab), "DATAA", itoa(dataa),
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

func itoa(i int) string { return fmtInt(i) }

func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

// demoWorktree is the change "Add cn-035 and cn-036 to defiant": two new
// nodes (saved, not committed) and the BGP policy edit.
func demoWorktree(t *testing.T) string {
	dir := copyTree(t, seed)
	write := func(p, content string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("control/inventory/cn-035.yaml", node("cn-035", 6, 37, 37, 38, [3]string{"MGT-0141", "DATA-0201", "DATA-0202"}))
	write("control/inventory/cn-036.yaml", node("cn-036", 8, 39, 39, 40, [3]string{"MGT-0142", "DATA-0203", "DATA-0204"}))
	bgp := filepath.Join(dir, "clusters/defiant/infra/cilium/bgp-rack-6z.yaml")
	b, _ := os.ReadFile(bgp)
	write("clusters/defiant/infra/cilium/bgp-rack-6z.yaml", strings.Replace(string(b), "          - cn-034\n", "          - cn-034\n          - cn-035\n          - cn-036\n", 1))
	os.MkdirAll(filepath.Join(dir, ".pk/changes"), 0o755)
	write(".pk/changes/c-k7m2qz.yaml", "apiVersion: pk/v1alpha1\nkind: Change\nid: c-k7m2qz\ntitle: Add cn-035 and cn-036 to defiant\n")
	return dir
}

func TestRenderSeed(t *testing.T) {
	e := render.New(all.Registry())
	src := render.Source{Ref: "main", Dir: seed}
	cl, err := e.Clusters(context.Background(), src)
	if err != nil || len(cl) != 3 {
		t.Fatalf("clusters %+v %v", cl, err)
	}
	set, err := e.Render(context.Background(), src, "defiant")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range set.Problems {
		t.Errorf("problem: %+v", p)
	}
	machines, kinds := 0, map[string]int{}
	for _, o := range set.Objects {
		kinds[o.Kind]++
		if o.Kind == "Machine" {
			machines++
			if o.Kustomization != "infra-metal" {
				t.Errorf("machine in %s", o.Kustomization)
			}
		}
	}
	if machines != 11 { // cn-003, cn-025..cn-034
		t.Errorf("machines %d", machines)
	}
	hr, ok := set.Objects[render.ObjectKey("defiant/helm.toolkit.fluxcd.io/v2/HelmRelease/kube-system/cilium")]
	if !ok {
		t.Fatalf("no HelmRelease; kinds %v", kinds)
	}
	if _, has := hr.Body["sops"]; has {
		t.Error("sops metadata must be dropped")
	}
	if hr.Source.Path != "clusters/defiant/infra/cilium/helmrelease.yaml" || hr.Source.Line != 1 {
		t.Errorf("source map %+v", hr.Source)
	}
	if md := hr.Body["metadata"].(map[string]any); md["annotations"] != nil {
		t.Errorf("origin annotation not stripped: %v", md)
	}
	if strings.Contains(render.ObjectYAML(&hr), "ENC[") {
		t.Error("rendered YAML shows a SOPS value")
	}
	dep := set.Objects[render.ObjectKey("defiant/apps/v1/Deployment/podinfo/podinfo")]
	if dep.Source.Path != "clusters/defiant/apps/podinfo/deployment.yaml" {
		t.Errorf("deployment source %+v", dep.Source)
	}
	m := set.Objects[render.ObjectKey("defiant/metal.pk.io/v1alpha1/Machine//cn-025")]
	if m.Source.Path != "control/inventory/cn-025.yaml" || m.Body["spec"].(map[string]any)["location"] != "GC5A-6Z U10" {
		t.Errorf("machine %+v", m)
	}
	// Pegasus gets only sn-05 as Machine.
	ps, _ := e.Render(context.Background(), src, "pegasus")
	if _, ok := ps.Objects[render.ObjectKey("pegasus/metal.pk.io/v1alpha1/Machine//sn-05")]; !ok {
		t.Error("pegasus has no Machine sn-05")
	}
}

func TestDemoImpact(t *testing.T) {
	e := render.New(all.Registry())
	head := demoWorktree(t)
	imp, err := e.Impact(context.Background(), render.Source{Ref: "main", Dir: seed}, render.Source{Ref: "c-k7m2qz", Dir: head, Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, c := range imp.Clusters {
		for _, k := range c.Kustomizations {
			for _, o := range k.Objects {
				sign := map[kinds.Op]string{kinds.OpAdd: "+", kinds.OpChange: "~", kinds.OpDelete: "-"}[o.Op]
				got[c.Cluster] = append(got[c.Cluster], sign+o.Ref.Kind+"/"+o.Ref.Name+" "+k.Name)
			}
		}
	}
	want := []string{"+Machine/cn-035 infra-metal", "+Machine/cn-036 infra-metal", "~CiliumBGPPeeringPolicy/rack-6z cilium"}
	if strings.Join(got["defiant"], ",") != strings.Join(want, ",") {
		t.Errorf("defiant impact %v", got["defiant"])
	}
	if len(got["phoenix"])+len(got["pegasus"]) != 0 {
		t.Errorf("phoenix/pegasus must not change: %v", got)
	}
	if imp.Clusters[0].Cluster != "defiant" || imp.SummaryText() != "defiant +2 ~1" {
		t.Errorf("summary %q first %s", imp.SummaryText(), imp.Clusters[0].Cluster)
	}
	var sem []string
	for _, l := range imp.Semantic {
		sem = append(sem, l.Subject+": "+l.Text)
	}
	// Same order as the impact objects (infra-metal before cilium), as in the talk.
	wantSem := []string{"cn-035: new compute node in GC5A-6Z, U6", "cn-036: new compute node in GC5A-6Z, U8", "rack-6z: BGP policy selects 2 more nodes"}
	if strings.Join(sem, "|") != strings.Join(wantSem, "|") {
		t.Errorf("semantic %q", sem)
	}
	if len(imp.Files) != 4 {
		t.Errorf("files %d", len(imp.Files))
	}
	for _, c := range imp.Clusters {
		for _, k := range c.Kustomizations {
			for _, o := range k.Objects {
				if o.Diff == "" || o.Source.Path == "" {
					t.Errorf("object %s: diff or source missing", o.Ref.Key())
				}
				if o.Ref.Kind == "CiliumBGPPeeringPolicy" && !strings.Contains(o.Diff, "+      - cn-035") {
					t.Errorf("bgp diff:\n%s", o.Diff)
				}
			}
		}
	}
	if len(imp.Problems) != 0 {
		t.Errorf("problems %+v", imp.Problems)
	}
	for _, f := range imp.Files {
		if strings.HasSuffix(f.Unified, "\n \n") {
			t.Errorf("%s: diff ends with an empty context line:\n%s", f.Path, f.Unified)
		}
	}

	// Risk from the policy: high, 2 approvals, lab gate.
	pol, err := policy.LoadFile(filepath.Join(seed, policy.Path))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := pol.Identify("dennis")
	g := pol.Evaluate(policy.GateInput{ChangeID: "c-k7m2qz", Author: id, Paths: imp.Paths(), Objects: imp.Objects()})
	if g.Risk.Level != policy.RiskHigh || g.Approvals.Need != 2 || !g.LabGate.Required {
		t.Errorf("gates risk %s need %d lab %v", g.Risk.Level, g.Approvals.Need, g.LabGate.Required)
	}

	// Overlap with c-p4x8ma (BGP peers on rack-6z).
	p4 := copyTree(t, seed)
	b, _ := os.ReadFile("../../seed/changes/10-c-p4x8ma/after/clusters/defiant/infra/cilium/bgp-rack-6z.yaml")
	os.WriteFile(filepath.Join(p4, "clusters/defiant/infra/cilium/bgp-rack-6z.yaml"), b, 0o644)
	other, err := e.Impact(context.Background(), render.Source{Ref: "main", Dir: seed}, render.Source{Ref: "c-p4x8ma", Dir: p4})
	if err != nil {
		t.Fatal(err)
	}
	ov := render.Overlaps(imp.Objects(), map[string][]render.ObjectRef{"c-p4x8ma": other.Objects()})
	if len(ov["c-p4x8ma"]) != 1 || ov["c-p4x8ma"][0].Kind != "CiliumBGPPeeringPolicy" {
		t.Errorf("overlap %+v", ov)
	}
	if other.Semantic[0].Text != "2 new BGP peers" {
		t.Errorf("p4x8ma semantic %+v", other.Semantic)
	}
}

// A file that does not parse fails its Kustomization: Flux keeps the
// applied objects, so the impact shows no deletions but names the failure.
func TestFailedBuildKeepsObjects(t *testing.T) {
	e := render.New(all.Registry())
	head := copyTree(t, seed)
	broken := []byte("apiVersion: cilium.io/v2alpha1\nkind: CiliumBGPPeeringPolicy\nmetadata:\n  name: rack-6z\n spec: [\n")
	os.WriteFile(filepath.Join(head, "clusters/defiant/infra/cilium/bgp-rack-6z.yaml"), broken, 0o644)
	os.WriteFile(filepath.Join(head, "control/inventory/cn-034.yaml"), []byte("kind: Node\nspec: {\n"), 0o644)
	imp, err := e.Impact(context.Background(), render.Source{Ref: "main", Dir: seed}, render.Source{Ref: "c-broken", Dir: head, Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range imp.Clusters {
		if _, _, d := c.Counts(); d != 0 {
			t.Errorf("%s: %d objects deleted by a broken file", c.Cluster, d)
		}
	}
	// control/inventory feeds infra-metal on every cluster.
	if got := imp.SummaryText(); got != "defiant (infra-metal, cilium fails to build), pegasus (infra-metal fails to build), phoenix (infra-metal fails to build)" {
		t.Errorf("summary %q", got)
	}
	if len(imp.Problems) == 0 {
		t.Error("no render problems")
	}
}

func TestUnified(t *testing.T) {
	got := render.Unified("x", "", "a\nb\n")
	want := "--- /dev/null\n+++ b/x\n@@ -0,0 +1,2 @@\n+a\n+b\n"
	if got != want {
		t.Errorf("added:\n%q\nwant\n%q", got, want)
	}
	got = render.Unified("x", "a\nb\nc\n", "a\nB\nc")
	want = "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"
	if got != want {
		t.Errorf("changed:\n%q\nwant\n%q", got, want)
	}
}

func TestRenderOnSaveIsFast(t *testing.T) {
	e := render.New(all.Registry())
	head := demoWorktree(t)
	base := render.Source{Ref: "main", Dir: seed}
	if _, err := e.Impact(context.Background(), base, render.Source{Dir: head}); err != nil {
		t.Fatal(err)
	}
	// A save changes one file; main is cached.
	p := filepath.Join(head, "control/inventory/cn-036.yaml")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "unit: 8", "unit: 4", 1)), 0o644)
	start := time.Now()
	imp, err := e.Impact(context.Background(), base, render.Source{Dir: head})
	if err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	t.Logf("render on save: %v", d)
	if d > 300*time.Millisecond {
		t.Errorf("render on save took %v", d)
	}
	found := false
	for _, l := range imp.Semantic {
		if l.Subject == "cn-036" && strings.Contains(l.Text, "U4") {
			found = true
		}
	}
	if !found {
		t.Errorf("the save is not in the impact: %+v", imp.Semantic)
	}
}

func TestOpenSteps(t *testing.T) {
	e := render.New(all.Registry())
	head := copyTree(t, seed)
	for _, f := range []string{"control/cabling.yaml", "work/items/w-108.yaml"} {
		b, _ := os.ReadFile("../../seed/changes/09-c-31d0aa/after/" + f)
		os.WriteFile(filepath.Join(head, f), b, 0o644)
	}
	imp, err := e.Impact(context.Background(), render.Source{Dir: seed}, render.Source{Dir: head})
	if err != nil {
		t.Fatal(err)
	}
	if imp.SummaryText() != "no change" {
		t.Errorf("cabling renders nothing: %s", imp.SummaryText())
	}
	if len(imp.OpenSteps) != 1 || imp.OpenSteps[0].Kind != "CablePlan" || imp.OpenSteps[0].ApplyPath != kinds.ApplyManual {
		t.Errorf("open steps %+v", imp.OpenSteps)
	}
	var sem []string
	for _, l := range imp.Semantic {
		sem = append(sem, l.Subject+": "+l.Text)
	}
	if len(sem) < 2 || !strings.Contains(strings.Join(sem, "\n"), "w-108: linked to c-31d0aa") {
		t.Errorf("semantic %q", sem)
	}
}
