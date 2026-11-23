package fluxsim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/render"
)

const seed = "../../seed/state"

type fakeSource struct {
	mu      sync.Mutex
	heads   Heads
	trees   map[string]string
	changes map[string]string
}

func (f *fakeSource) Fetch(ctx context.Context) (Heads, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := Heads{Main: f.heads.Main, Rings: map[string]string{}}
	for k, v := range f.heads.Rings {
		h.Rings[k] = v
	}
	return h, nil
}
func (f *fakeSource) Tree(ctx context.Context, sha string) (string, error) { return f.trees[sha], nil }
func (f *fakeSource) Change(ctx context.Context, sha string) string        { return f.changes[sha] }

func copyTree(t *testing.T, edit map[string]string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(seed, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(seed, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, s := range edit {
		full := filepath.Join(dst, p)
		if strings.HasPrefix(s, "s/") {
			parts := strings.SplitN(s[2:], "/", 2)
			b, _ := os.ReadFile(full)
			s = strings.Replace(string(b), parts[0], parts[1], 1)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

const cn035 = `apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: cn-035
spec:
  cluster: defiant
  role: worker
  location: {rack: GC5A-6Z, unit: 6, size: 2}
  model: Supermicro SYS-220U-TNR
  bmc: {mac: "3c:ec:ef:a1:0c:35"}
  ports:
    - {name: bmc, cable: MGT-0141, peer: {switch: mgmt, port: 37}, network: mgmt}
    - {name: p1, cable: DATA-0201, peer: {switch: data-b, port: 37}, network: data}
    - {name: p2, cable: DATA-0202, peer: {switch: data-a, port: 38}, network: data}
`

type clock struct{ t time.Time }

func (c *clock) now() time.Time             { return c.t }
func (c *clock) add(d time.Duration) *clock { c.t = c.t.Add(d); return c }

func newEngine(t *testing.T, src Source, statePath string, c *clock) *Engine {
	reg := all.Registry()
	return NewEngine(EngineOptions{Source: src, Registry: reg, Renderer: render.New(reg), ApplyDelay: 4 * time.Second,
		VerifyDelay: 6 * time.Second, StatePath: statePath, Now: c.now})
}

func cluster(t *testing.T, e *Engine, id string) ClusterStatus {
	t.Helper()
	st := e.Status()
	for _, c := range append(st.Clusters, st.Twins...) {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no cluster %s in %+v", id, st)
	return ClusterStatus{}
}

func sha(c *Revision) string {
	if c == nil {
		return ""
	}
	return c.SHA
}

func TestEngineFollowsMain(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{heads: Heads{Main: "sha1"}, trees: map[string]string{"sha1": copyTree(t, nil)},
		changes: map[string]string{"sha1": "c-19f4tb", "sha2": "c-k7m2qz"}}
	c := &clock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	state := filepath.Join(t.TempDir(), "state.json")
	e := newEngine(t, src, state, c)
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	st := e.Status()
	if len(st.Clusters) != 3 || len(st.Twins) != 1 || st.Twins[0].ID != "defiant-twin" || st.Twins[0].Twin != "defiant" {
		t.Fatalf("clusters %+v twins %+v", st.Clusters, st.Twins)
	}
	e.Step(ctx)
	d := cluster(t, e, "defiant")
	if d.Applied != nil || d.Pending == nil || d.Pending.SHA != "sha1" || d.Pending.Change != "c-19f4tb" {
		t.Fatalf("pending: %+v", d)
	}
	e.Step(ctx) // no time passed
	c.add(4 * time.Second)
	e.Step(ctx)
	d = cluster(t, e, "defiant")
	if sha(d.Applied) != "sha1" || d.Verified != nil || d.Objects == 0 || d.Applied.Change != "c-19f4tb" {
		t.Fatalf("applied: %+v", d)
	}
	c.add(6 * time.Second)
	e.Step(ctx)
	d = cluster(t, e, "defiant")
	if sha(d.Verified) != "sha1" || !d.Healthy || !strings.Contains(d.Message, "health checks pass") {
		t.Fatalf("verified: %+v", d)
	}
	if tw := cluster(t, e, "defiant-twin"); sha(tw.Verified) != "sha1" || tw.Objects != d.Objects {
		t.Fatalf("twin: %+v", tw)
	}

	// A merge adds cn-035: applied after the delay, the Machine is there.
	src.mu.Lock()
	src.trees["sha2"] = copyTree(t, map[string]string{"control/inventory/cn-035.yaml": cn035})
	src.heads.Main = "sha2"
	src.mu.Unlock()
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e.Step(ctx)
	c.add(4 * time.Second)
	e.Step(ctx)
	objs, _ := e.Objects("defiant")
	found := false
	for _, o := range objs {
		if o.Kind == node.MachineKind && o.Name == "cn-035" {
			found = !o.Ready
		}
	}
	if !found {
		t.Fatal("Machine/cn-035 not applied (or ready before verify)")
	}
	if d := cluster(t, e, "defiant"); sha(d.Applied) != "sha2" || sha(d.Verified) != "sha1" || d.Applied.Change != "c-k7m2qz" {
		t.Fatalf("after merge: %+v", d)
	}
	// pegasus does not change, but follows main too.
	if p := cluster(t, e, "pegasus"); sha(p.Applied) != "sha2" {
		t.Fatalf("pegasus: %+v", p)
	}

	// Demo fault: the health check fails on the newest Machine.
	if _, err := e.SetFailing("defiant", true); err != nil {
		t.Fatal(err)
	}
	c.add(6 * time.Second)
	e.Step(ctx)
	d = cluster(t, e, "defiant")
	if sha(d.Verified) != "sha1" || d.Healthy || !strings.Contains(d.Message, "Machine/cn-035 not Ready") {
		t.Fatalf("failing: %+v", d)
	}
	// Heal: verified after the health checks.
	if _, err := e.SetFailing("defiant", false); err != nil {
		t.Fatal(err)
	}
	e.Step(ctx)
	if d := cluster(t, e, "defiant"); sha(d.Verified) != "sha1" {
		t.Fatalf("verified too early: %+v", d)
	}
	c.add(6 * time.Second)
	e.Step(ctx)
	if d := cluster(t, e, "defiant"); sha(d.Verified) != "sha2" || !d.Healthy || d.Failing {
		t.Fatalf("healed: %+v", d)
	}

	// Restart: the state comes back and the objects are rebuilt.
	e2 := newEngine(t, src, state, c)
	e2.Step(ctx)
	if d := cluster(t, e2, "defiant"); sha(d.Verified) != "sha2" || d.Objects != len(objs) || !d.Healthy {
		t.Fatalf("restored: %+v", d)
	}
	for _, k := range cluster(t, e2, "defiant").Kustomizations {
		if !k.Ready {
			t.Fatalf("restored Kustomization not ready: %+v", k)
		}
	}
	objs2, _ := e2.Objects("defiant")
	for _, o := range objs2 {
		if !o.Ready {
			t.Fatalf("restored object not ready: %+v", o)
		}
	}
}

func TestEngineRingAndBrokenRender(t *testing.T) {
	ctx := context.Background()
	bad := copyTree(t, map[string]string{"clusters/defiant/infra/cilium/helmrelease.yaml": "s/interval: 5m/interval: 5 minutes"})
	src := &fakeSource{heads: Heads{Main: "bad", Rings: map[string]string{"canary": "good"}},
		trees: map[string]string{"good": copyTree(t, nil), "bad": bad}}
	c := &clock{t: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	e := newEngine(t, src, "", c)
	if err := e.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e.Step(ctx)
	c.add(5 * time.Second)
	e.Step(ctx)
	// defiant (ring canary) follows the ring tag; phoenix (prod) follows main.
	d := cluster(t, e, "defiant")
	if d.Ref != "ring/canary" || sha(d.Applied) != "good" {
		t.Fatalf("defiant: %+v", d)
	}
	// main is broken for defiant only; phoenix renders fine.
	if p := cluster(t, e, "phoenix"); sha(p.Applied) != "bad" {
		t.Fatalf("phoenix: %+v", p)
	}
	tw := cluster(t, e, "defiant-twin")
	if tw.Applied != nil || tw.Healthy || !strings.Contains(tw.Message, "Kustomization failed") || !strings.Contains(tw.Message, "helmrelease.yaml") {
		t.Fatalf("twin on broken main: %+v", tw)
	}
}
