package bench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds/labkind"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/rings"
)

type fakeLabs struct{ runs []lab.Run }

func (f *fakeLabs) RunChange(ctx context.Context, changeID, scenario string) (*lab.Run, error) {
	if changeID == "c-busy01" {
		return nil, &lab.BusyError{Run: "c-busy01/run-2"}
	}
	if scenario == "nope" {
		return nil, lab.ErrScenario
	}
	r := lab.Run{ID: changeID + "/run-1", ChangeID: changeID, Number: 1, Result: lab.ResultRunning, Driver: "sim",
		Steps: lab.Plan(lab.LabSpec{Provision: "metal"}, "defiant")}
	return &r, nil
}

func (f *fakeLabs) Get(ctx context.Context, id string) (*lab.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, lab.ErrNotFound
}

func (f *fakeLabs) Runs(ctx context.Context, changeID string) ([]lab.Run, error) { return f.runs, nil }

func (f *fakeLabs) Definitions(ctx context.Context) ([]lab.LabFile, error) {
	l := lab.LabFile{APIVersion: "pk/v1alpha1", Kind: "Lab"}
	l.Metadata.Name = "defiant-twin"
	l.Spec.Purpose = "twin"
	l.Spec.Topology = labkind.TopologyRef{Name: "defiant"}
	l.Spec.Follow = &labkind.Ref{Ref: "main"}
	return []lab.LabFile{l}, nil
}

func (f *fakeLabs) Topology(ctx context.Context, ref, cluster string) (*lab.Topology, error) {
	return &lab.Topology{Name: cluster, Ref: ref}, nil
}

func (f *fakeLabs) Scenarios() []string { return []string{"metal", "gitops"} }

type fakeFlux struct{}

func (fakeFlux) Status(ctx context.Context) (*fluxsim.Status, error) {
	at := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	return &fluxsim.Status{Main: "abc", Clusters: []fluxsim.ClusterStatus{
		{ID: "pegasus", Role: "storage", Ring: "prod", Ref: "ring/prod", Healthy: true, Applied: &fluxsim.Revision{SHA: "abc", At: at}},
		{ID: "defiant", Role: "tenants", Ring: "canary", Ref: "main", Healthy: false, Failing: true,
			Applied: &fluxsim.Revision{SHA: "abc", At: at, Change: "c-k7m2qz"}, Message: "Machine/cn-036 not Ready (demo fault)"},
	}, Twins: []fluxsim.ClusterStatus{{ID: "defiant-twin", Twin: "defiant", Ring: "lab", Ref: "main", Healthy: true}}}, nil
}

func (fakeFlux) Objects(ctx context.Context, cluster string) ([]fluxsim.Object, error) {
	return nil, nil
}

func (fakeFlux) SetFailing(ctx context.Context, cluster string, failing bool) (*fluxsim.ClusterStatus, error) {
	return &fluxsim.ClusterStatus{ID: cluster, Failing: failing}, nil
}

type fakeRings struct{ held map[string]string }

func (f *fakeRings) ring(name string) *rings.Ring {
	return &rings.Ring{Ring: name, Clusters: []string{"defiant"}, Following: "ring/" + name, Tag: "ring/" + name,
		SHA: "abc", Gate: rings.GateManual, Reason: "high-risk paths: control/cabling.yaml (inventory)",
		Held: f.held[name] != "", HeldBy: f.held[name]}
}

func (f *fakeRings) List(ctx context.Context) ([]rings.Ring, error) {
	return []rings.Ring{*f.ring("canary"), *f.ring("prod")}, nil
}

func (f *fakeRings) Promote(ctx context.Context, who identity.Identity, ring, sha string) (*rings.Ring, error) {
	if ring == "prod" {
		return nil, rings.ErrConflict
	}
	return f.ring(ring), nil
}

func (f *fakeRings) Hold(ctx context.Context, who identity.Identity, ring string) (*rings.Ring, error) {
	f.held[ring] = who.User
	return f.ring(ring), nil
}

func (f *fakeRings) Release(ctx context.Context, who identity.Identity, ring string) (*rings.Ring, error) {
	delete(f.held, ring)
	return f.ring(ring), nil
}

func (f *fakeRings) HeldBy(ring string) string { return f.held[ring] }

func newLabServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := config.Config{Addr: "127.0.0.1:8080", DataDir: t.TempDir(), User: "dennis",
		AdminToken: "pk_adm_test_token_0123456789abcdef0123", PublicURL: "http://127.0.0.1:8080"}
	run := lab.Run{ID: "c-k7m2qz/run-1", ChangeID: "c-k7m2qz", Number: 1, Result: lab.ResultPass, Driver: "sim",
		Steps: lab.Plan(lab.LabSpec{Provision: "metal"}, "defiant"), Summary: "12 of 12 checks pass"}
	s, err := New(cfg, Deps{Labs: &fakeLabs{runs: []lab.Run{run}}, Flux: fakeFlux{},
		Rings: &fakeRings{held: map[string]string{}}}, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg.AdminToken
}

func TestLabHandlers(t *testing.T) {
	s, token := newLabServer(t)
	ts := httptestServer(t, s)
	auth := map[string]string{"Authorization": "Bearer " + token}

	res := do(t, ts, "GET", "/api/v1/labs/runs/c-k7m2qz%2Frun-1", auth, "")
	var run api.LabRun
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&run) != nil || run.Id != "c-k7m2qz/run-1" || len(run.Steps) != 5 {
		t.Fatalf("getLabRun: %d %+v", res.StatusCode, run)
	}
	if res := do(t, ts, "GET", "/api/v1/labs/runs/c-k7m2qz%2Frun-9", auth, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run: %d", res.StatusCode)
	}
	res = do(t, ts, "POST", "/api/v1/changes/c-k7m2qz/labs", auth, `{}`)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("runLab: %d", res.StatusCode)
	}
	if res := do(t, ts, "POST", "/api/v1/changes/c-busy01/labs", auth, `{}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("busy: %d", res.StatusCode)
	}
	if res := do(t, ts, "POST", "/api/v1/changes/c-k7m2qz/labs", auth, `{"scenario":"nope"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad scenario: %d", res.StatusCode)
	}

	res = do(t, ts, "GET", "/api/v1/labs", auth, "")
	var list api.LabList
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&list) != nil {
		t.Fatalf("listLabs: %d", res.StatusCode)
	}
	if len(list.Definitions) != 1 || list.Definitions[0].Status.Id != "defiant-twin" || !list.Definitions[0].Status.Healthy ||
		list.Definitions[0].Follow != "main" || len(list.Runs) != 1 {
		t.Fatalf("labs = %+v", list)
	}

	res = do(t, ts, "GET", "/api/v1/clusters", auth, "")
	var cs []api.Cluster
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&cs) != nil {
		t.Fatalf("listClusters: %d", res.StatusCode)
	}
	if len(cs) != 2 || cs[0].Id != "defiant" || cs[1].RingTag != "ring/prod" || !cs[0].Failing || cs[0].Applied.Change != "c-k7m2qz" || !cs[0].Reachable {
		t.Fatalf("clusters = %+v", cs)
	}

	if res := do(t, ts, "POST", "/api/v1/rings/canary/hold", auth, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("hold: %d", res.StatusCode)
	}
	res = do(t, ts, "GET", "/api/v1/rings", auth, "")
	var rs []api.RingStatus
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&rs) != nil || len(rs) != 2 || !rs[0].Held ||
		rs[0].Gate != "manual" || rs[0].Following != "ring/canary" {
		t.Fatalf("listRings: %d %+v", res.StatusCode, rs)
	}
	res = do(t, ts, "GET", "/api/v1/clusters", auth, "")
	if json.NewDecoder(res.Body).Decode(&cs) != nil || !cs[0].Held {
		t.Fatalf("held cluster = %+v", cs)
	}
	if res := do(t, ts, "POST", "/api/v1/rings/prod/promote", auth, `{}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("promote prod: %d", res.StatusCode)
	}
	if res := do(t, ts, "POST", "/api/v1/rings/canary/release", auth, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", res.StatusCode)
	}
}

func httptestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}
