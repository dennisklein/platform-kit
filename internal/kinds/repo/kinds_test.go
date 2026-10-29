package repo_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// CN035 is the demo node; port is the data-a port of p2.
func CN035(port string) string {
	return `apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: cn-035
spec:
  cluster: defiant
  role: worker
  location:
    rack: GC5A-6Z
    unit: 6
    size: 2
  model: Supermicro SYS-220U-TNR
  bmc:
    mac: 3c:ec:ef:a1:0c:35
  ports:
    - {name: bmc, cable: MGT-0141, peer: {switch: mgmt, port: 37}, network: mgmt}
    - {name: p1, cable: DATA-0201, peer: {switch: data-b, port: 37}, network: data}
    - {name: p2, cable: DATA-0202, peer: {switch: data-a, port: ` + port + `}, network: data}
`
}

func seedFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(seed, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(seed, p)
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func errorsOf(r *repo.Repo, path string) []kinds.Problem {
	var out []kinds.Problem
	for _, p := range r.ProblemsOf(path) {
		if p.Severity == kinds.SevError {
			out = append(out, p)
		}
	}
	return out
}

func TestPortInUse(t *testing.T) {
	reg := all.Registry()
	base := repo.FromFiles(seedFiles(t), reg)
	r := base.WithFile("control/inventory/cn-035.yaml", []byte(CN035("37")))
	errs := errorsOf(r, "control/inventory/cn-035.yaml")
	if len(errs) != 1 || errs[0].Message != "port data-a 37 in use by cn-003" || errs[0].Line != 18 || errs[0].Field != "spec.ports[2].peer.port" {
		t.Fatalf("want port in use on line 18, got %+v", errs)
	}
	r = base.WithFile("control/inventory/cn-035.yaml", []byte(CN035("38")))
	if errs := errorsOf(r, "control/inventory/cn-035.yaml"); len(errs) != 0 {
		t.Fatalf("port 38 is free: %+v", errs)
	}
}

func TestCrossFileChecks(t *testing.T) {
	reg := all.Registry()
	base := repo.FromFiles(seedFiles(t), reg)
	bad := strings.NewReplacer(
		"unit: 6", "unit: 10", // cn-025 is there
		"switch: data-b, port: 37", "switch: data-c, port: 37",
		"DATA-0202", "DATA-0151", // cable of cn-025
		"switch: mgmt, port: 37", "switch: mgmt, port: 48", // uplink port of mgmt
		"cluster: defiant", "cluster: enterprise",
	).Replace(CN035("38"))
	r := base.WithFile("control/inventory/cn-035.yaml", []byte(bad))
	var msgs []string
	for _, p := range errorsOf(r, "control/inventory/cn-035.yaml") {
		msgs = append(msgs, p.Message)
	}
	got := strings.Join(msgs, "\n")
	for _, want := range []string{
		"unknown cluster enterprise",
		"unit 10 in GC5A-6Z is used by cn-025",
		"unknown switch data-c",
		"cable DATA-0151 is also used by cn-025 p1",
		"port mgmt 48 is an uplink port",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	// Unknown rack, article entity and work item references.
	r = base.WithFile("control/inventory/cn-035.yaml", []byte(strings.Replace(CN035("38"), "GC5A-6Z", "GC5A-7A", 1))).
		WithFile("kb/runbooks/add-node.md", []byte("---\ntitle: Add a node\ntype: runbook\nentities: [GC5A-6Z, cn-999]\n---\n# x\n")).
		WithFile("work/items/w-114.yaml", []byte("apiVersion: work.pk.io/v1alpha1\nkind: WorkItem\nid: w-114\ntitle: Compute nodes\nmilestone: nope\nowner: dennis\nkb: [kb/missing.md]\n"))
	checks := map[string]string{
		"control/inventory/cn-035.yaml": "unknown rack GC5A-7A",
		"kb/runbooks/add-node.md":       "unknown entity cn-999",
		"work/items/w-114.yaml":         "unknown milestone nope",
	}
	for path, want := range checks {
		found := false
		for _, p := range errorsOf(r, path) {
			if p.Message == want {
				found = true
				if p.Line == 0 {
					t.Errorf("%s: no line", want)
				}
			}
		}
		if !found {
			t.Errorf("%s: missing %q: %+v", path, want, r.ProblemsOf(path))
		}
	}
	// The article entity problem points into the front matter (file line 4).
	for _, p := range errorsOf(r, "kb/runbooks/add-node.md") {
		if p.Message == "unknown entity cn-999" && p.Line != 4 {
			t.Errorf("article problem line %d, want 4", p.Line)
		}
	}
}

func TestHelmReleaseInterval(t *testing.T) {
	reg := all.Registry()
	files := seedFiles(t)
	p := "clusters/defiant/infra/cilium/helmrelease.yaml"
	r := repo.FromFiles(files, reg).WithFile(p, []byte(strings.Replace(string(files[p]), "interval: 5m", "interval: 5 minutes", 1)))
	errs := errorsOf(r, p)
	if len(errs) != 1 || errs[0].Message != "spec.interval: must be a duration, for example 5m" || errs[0].Line != 7 {
		t.Fatalf("got %+v", errs)
	}
}

func describe(t *testing.T, kind string, old, new []byte, path string) []kinds.SemanticLine {
	t.Helper()
	reg := all.Registry()
	k, ok := reg.Get(kind)
	if !ok || k.Describe == nil {
		t.Fatalf("no describe for %s", kind)
	}
	var od, nd *kinds.Doc
	if old != nil {
		od = repo.ParseFile(path, old).Docs[0]
	}
	if new != nil {
		nd = repo.ParseFile(path, new).Docs[0]
	}
	return k.Describe(od, nd)
}

func texts(ls []kinds.SemanticLine) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Subject+": "+l.Text)
	}
	return out
}

func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDescribe(t *testing.T) {
	const ch = "../../../seed/changes/"
	cases := []struct {
		kind     string
		old, new []byte
		path     string
		want     []string
	}{
		{"Node", nil, []byte(CN035("38")), "control/inventory/cn-035.yaml", []string{"cn-035: new compute node in GC5A-6Z, U6"}},
		{"Node", read(t, ch+"02-c-2hd7vw/before/control/inventory/cn-003.yaml"), read(t, ch+"02-c-2hd7vw/after/control/inventory/cn-003.yaml"),
			"control/inventory/cn-003.yaml", []string{"cn-003 p2: data-b 37 to data-a 37"}},
		{"Node", read(t, ch+"06-c-u7x2bd/before/control/inventory/cn-025.yaml"), read(t, ch+"06-c-u7x2bd/after/control/inventory/cn-025.yaml"),
			"control/inventory/cn-025.yaml", []string{"cn-025: label firmware=2.4"}},
		{"Node", read(t, ch+"05-c-m1gr8t/before/control/inventory/cn-003.yaml"), read(t, ch+"05-c-m1gr8t/after/control/inventory/cn-003.yaml"),
			"control/inventory/cn-003.yaml", []string{"cn-003: stored as v1beta1 (was v1alpha1)"}},
		{"CiliumBGPPeeringPolicy", read(t, ch+"10-c-p4x8ma/before/clusters/defiant/infra/cilium/bgp-rack-6z.yaml"),
			read(t, ch+"10-c-p4x8ma/after/clusters/defiant/infra/cilium/bgp-rack-6z.yaml"), "clusters/defiant/infra/cilium/bgp-rack-6z.yaml",
			[]string{"rack-6z: 2 new BGP peers"}},
		{"CiliumBGPPeeringPolicy", read(t, seed+"/clusters/defiant/infra/cilium/bgp-rack-6z.yaml"),
			[]byte(strings.Replace(string(read(t, seed+"/clusters/defiant/infra/cilium/bgp-rack-6z.yaml")), "          - cn-034\n", "          - cn-034\n          - cn-035\n          - cn-036\n", 1)),
			"clusters/defiant/infra/cilium/bgp-rack-6z.yaml", []string{"rack-6z: BGP policy selects 2 more nodes"}},
		{"Deployment", read(t, ch+"11-c-6m1pqd/before/clusters/defiant/apps/podinfo/deployment.yaml"), read(t, ch+"11-c-6m1pqd/after/clusters/defiant/apps/podinfo/deployment.yaml"),
			"clusters/defiant/apps/podinfo/deployment.yaml", []string{"podinfo: image 6.8.0 to 6.9.0"}},
		{"Jbod", read(t, seed+"/control/inventory/jb-05.yaml"), []byte(strings.Replace(string(read(t, seed+"/control/inventory/jb-05.yaml")), "{slot: 17, state: ok, serial: WD-ZR117K53}", "{slot: 17, state: replace, serial: WD-ZR117K53}", 1)),
			"control/inventory/jb-05.yaml", []string{"jb-05 drawer 1 slot 17: ok to replace; the OSD drains after the merge"}},
	}
	for _, c := range cases {
		got := texts(describe(t, c.kind, c.old, c.new, c.path))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s:\n got %q\nwant %q", c.kind, c.path, got, c.want)
		}
	}
}

func TestConvertNode(t *testing.T) {
	const ch = "../../../seed/changes/05-c-m1gr8t/"
	before := repo.ParseFile("control/inventory/cn-003.yaml", read(t, ch+"before/control/inventory/cn-003.yaml")).Docs[0]
	after := repo.ParseFile("control/inventory/cn-003.yaml", read(t, ch+"after/control/inventory/cn-003.yaml")).Docs[0]
	conv, err := node.Convert("v1alpha1", before.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conv, after.Data) {
		t.Fatalf("converted:\n%v\nwant\n%v", conv, after.Data)
	}
	// A v1alpha1 file still renders a Machine with the location.
	n, err := node.FromDoc(before)
	if err != nil || node.LocationText(n.Spec.Location) != "GC5A-6Y U30" {
		t.Fatalf("FromDoc: %+v %v", n, err)
	}
}

func TestSkeletons(t *testing.T) {
	reg := all.Registry()
	for _, k := range reg.List() {
		if k.Skeleton == nil {
			continue
		}
		b, err := k.Skeleton("x-1")
		if err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
		f := repo.ParseFile(k.NewPath("x-1"), b)
		if f.Syntax != nil || len(f.Docs) != 1 {
			t.Errorf("%s skeleton: %+v", k.Name, f.Syntax)
		}
		if got, ok := reg.Match(f.Path, f.Docs[0]); !ok || got.Name != k.Name {
			t.Errorf("%s skeleton does not match its kind", k.Name)
		}
	}
}

// A form or agent fills a new Node skeleton with one {set}: keys land in
// schema order and no value is lost.
func TestSetFieldsSchemaOrder(t *testing.T) {
	reg := all.Registry()
	k, _ := reg.Get("Node")
	skel, err := k.Skeleton("cn-035")
	if err != nil {
		t.Fatal(err)
	}
	p := k.NewPath("cn-035")
	out, err := schema.SetFieldsWith(skel, map[string]any{
		"spec.cluster":       "defiant",
		"spec.location.rack": "GC5A-6Z",
		"spec.location.unit": float64(6),
		"spec.serial":        "S4R7K2J0035",
		"spec.model":         "Supermicro SYS-220U-TNR",
		"spec.bmc.mac":       "3c:ec:ef:a1:0c:35",
		"spec.ports": []any{
			map[string]any{"network": "mgmt", "peer": map[string]any{"port": float64(37), "switch": "mgmt"}, "name": "bmc", "cable": "MGT-0141"},
		},
	}, repo.SchemaRawLookup(reg, p))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	order := []string{"cluster: defiant", "role: worker", "rack: GC5A-6Z", "unit: 6", "model: Supermicro", "serial: S4R7K2J0035", "mac: 3c:ec", "name: bmc", "cable: MGT-0141", "switch: mgmt", "port: 37", "network: mgmt"}
	at := -1
	for _, w := range order {
		i := strings.Index(s, w)
		if i < 0 || i < at {
			t.Fatalf("%q missing or out of order in\n%s", w, s)
		}
		at = i
	}
	r := repo.FromFiles(map[string][]byte{p: out}, reg)
	for _, pr := range r.ProblemsOf(p) {
		if pr.Source == kinds.SourceSchema {
			t.Errorf("schema problem %s", pr.Message)
		}
	}
}
