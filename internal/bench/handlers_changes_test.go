package bench

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/forge/fake"
	"github.com/dennisklein/platform-kit/internal/gitx"
)

// changeBench is a bench with the change service on a fake forge backed by
// a bare git repo (no policy engine and renderer: gates come from the
// pk/policy status, which the test sets).
type changeBench struct {
	t     *testing.T
	ts    *httptest.Server
	forge *fake.Forge
	svc   *change.Svc
}

const testAdminToken = "pk_adm_test_token_0123456789abcdef0123"

func newChangeBench(t *testing.T) *changeBench {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	bare, err := gitx.Init(ctx, filepath.Join(root, "remote.git"), true)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := gitx.Init(ctx, filepath.Join(root, "seed"), false)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{
		"control/cabling.yaml":                   "uplinks: []\n",
		"clusters/defiant/infra/cilium/bgp.yaml": "nodes:\n  - cn-025\n  - cn-034\n",
		"README.md":                              "# state\n",
	} {
		full := filepath.Join(seed.Dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = seed.Add(ctx)
	if _, err := seed.Commit(ctx, gitx.CommitOptions{Message: "chore: initial state",
		Author: gitx.Signature{Name: "Dennis Klein", Email: "dennis@pk.local"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, "push", "-q", bare.Dir, "main:main"); err != nil {
		t.Fatal(err)
	}
	f := fake.New("platform", "state")
	f.Git = bare
	hub := events.NewHub()
	svc := change.New(change.Options{
		DataDir: filepath.Join(root, "data"), RemoteURL: bare.Dir, Forge: f,
		Token: func(u string) (string, bool) { return "tok-" + u, true },
		User:  "dennis", Events: hub, CIWait: 1500 * time.Millisecond,
	})
	if err := svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Addr: "127.0.0.1:8080", DataDir: filepath.Join(root, "bench"), Demo: true, User: "dennis",
		AdminToken: testAdminToken, PublicURL: "http://127.0.0.1:8080"}
	s, err := New(cfg, Deps{Events: hub, Changes: svc, Forge: f}, fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &changeBench{t: t, ts: ts, forge: f, svc: svc}
}

// call does an API request as dennis and decodes the JSON answer into out.
func (b *changeBench) call(method, path string, body any, want int, out any) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = strings.NewReader(string(buf))
	}
	req, _ := http.NewRequest(method, b.ts.URL+"/api/v1"+path, rd)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		b.t.Fatalf("%s %s: want %d, got %d: %s", method, path, want, res.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			b.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

func (b *changeBench) green(id string) {
	b.t.Helper()
	var ch struct {
		HeadSha string `json:"headSha"`
	}
	b.call("GET", "/changes/"+id, nil, 200, &ch)
	if err := b.forge.SetStatus(context.Background(), forge.CIUser, ch.HeadSha,
		forge.CommitStatus{Context: forge.ContextPolicy, State: forge.StatusSuccess}); err != nil {
		b.t.Fatal(err)
	}
}

type changeJSON struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Local        bool   `json:"local"`
	Dirty        bool   `json:"dirty"`
	BehindTarget int    `json:"behindTarget"`
	Description  string `json:"description"`
	Mr           struct {
		Number int
		Draft  bool
	}
	Lifecycle   []json.RawMessage `json:"lifecycle"`
	Commits     []json.RawMessage `json:"commits"`
	ApprovalsBy []json.RawMessage `json:"approvalsBy"`
	Children    []string          `json:"children"`
	LabRuns     []json.RawMessage `json:"labRuns"`
	Overlaps    []json.RawMessage `json:"overlapsDetail"`
}

func q(p string) string { return url.QueryEscape(p) }

func TestChangeHandlersFlow(t *testing.T) {
	b := newChangeBench(t)

	// Create: worktree, metadata file, required arrays non-nil.
	var ch changeJSON
	b.call("POST", "/changes", map[string]any{"title": "Add cn-035 to defiant", "intent": "one more node", "workItems": []string{"w-114"}}, 201, &ch)
	id := ch.ID
	if !ch.Local || ch.State != "draft" || len(ch.Lifecycle) != 7 || ch.Commits == nil || ch.ApprovalsBy == nil ||
		ch.Children == nil || ch.LabRuns == nil || ch.Overlaps == nil {
		t.Fatalf("created: %+v", ch)
	}
	var fc struct {
		Exists   bool   `json:"exists"`
		ReadOnly bool   `json:"readOnly"`
		Content  string `json:"content"`
		Sha      string `json:"sha"`
		Status   string `json:"status"`
	}
	b.call("GET", "/changes/"+id+"/file?path="+q(".pk/changes/"+id+".yaml"), nil, 200, &fc)
	if !fc.Exists || fc.ReadOnly || !strings.Contains(fc.Content, "w-114") {
		t.Fatalf("metadata file: %+v", fc)
	}

	// Write, path checks, stale base sha.
	b.call("PUT", "/changes/"+id+"/file?path="+q("../etc/passwd"), map[string]any{"content": "x"}, 400, nil)
	b.call("PUT", "/changes/"+id+"/file?path="+q(".git/config"), map[string]any{"content": "x"}, 400, nil)
	var wr struct {
		File struct {
			Exists bool   `json:"exists"`
			Sha    string `json:"sha"`
		} `json:"file"`
	}
	b.call("PUT", "/changes/"+id+"/file?path="+q("clusters/defiant/infra/cilium/bgp.yaml"),
		map[string]any{"content": "nodes:\n  - cn-025\n  - cn-034\n  - cn-035\n"}, 200, &wr)
	if !wr.File.Exists || wr.File.Sha != fileBlobSHA([]byte("nodes:\n  - cn-025\n  - cn-034\n  - cn-035\n")) {
		t.Fatalf("write: %+v", wr)
	}
	b.call("PUT", "/changes/"+id+"/file?path="+q("clusters/defiant/infra/cilium/bgp.yaml"),
		map[string]any{"content": "nodes: []\n", "baseSha": "0000"}, 409, nil)
	b.call("PUT", "/changes/"+id+"/file?path="+q("docs/tmp.md"), map[string]any{"content": "tmp\n"}, 200, nil)
	b.call("DELETE", "/changes/"+id+"/file?path="+q("docs/tmp.md"), nil, 200, nil)
	wt, err := b.svc.Worktree(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "docs")); err == nil {
		t.Fatal("empty dir docs/ left behind")
	}

	var st struct {
		Clean bool `json:"clean"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	b.call("GET", "/changes/"+id+"/status", nil, 200, &st)
	if st.Clean || len(st.Files) != 1 || st.Files[0].Path != "clusters/defiant/infra/cilium/bgp.yaml" {
		t.Fatalf("status: %+v", st)
	}
	var tree struct {
		Entries []struct {
			Name     string `json:"name"`
			Changed  int    `json:"changed"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		} `json:"entries"`
	}
	b.call("GET", "/changes/"+id+"/tree", nil, 200, &tree)
	if len(tree.Entries) == 0 || tree.Entries[0].Name != ".pk" {
		t.Fatalf("tree: %+v", tree)
	}
	for _, e := range tree.Entries {
		if e.Name == "clusters" && e.Changed != 1 {
			t.Fatalf("clusters changed %d", e.Changed)
		}
	}

	// Commit, push (draft MR), ready.
	b.call("POST", "/changes/"+id+"/commits", map[string]any{"message": ""}, 400, nil)
	b.call("POST", "/changes/"+id+"/commits", map[string]any{"message": "feat(cilium): peer cn-035"}, 201, nil)
	var commits []map[string]any
	b.call("GET", "/changes/"+id+"/commits", nil, 200, &commits)
	if len(commits) != 2 {
		t.Fatalf("commits %d", len(commits))
	}
	var push struct {
		Created bool `json:"created"`
		Mr      struct {
			Number int  `json:"number"`
			Draft  bool `json:"draft"`
		} `json:"mr"`
	}
	b.call("POST", "/changes/"+id+"/push", nil, 200, &push)
	if !push.Created || !push.Mr.Draft || push.Mr.Number == 0 {
		t.Fatalf("push: %+v", push)
	}
	b.call("POST", "/changes/"+id+"/ready", nil, 200, &ch)
	if !strings.Contains(ch.Description, "Pk-Change: "+id) {
		t.Fatalf("description: %q", ch.Description)
	}

	// Main moves: another change merges; ours is behind by 1.
	var other changeJSON
	b.call("POST", "/changes", map[string]any{"title": "Cabling"}, 201, &other)
	b.call("PUT", "/changes/"+other.ID+"/file?path="+q("control/cabling.yaml"), map[string]any{"content": "uplinks: [spine-1]\n"}, 200, nil)
	b.call("POST", "/changes/"+other.ID+"/commits", map[string]any{"message": "feat: cabling"}, 201, nil)
	b.call("POST", "/changes/"+other.ID+"/push", nil, 200, nil)
	b.call("POST", "/changes/"+other.ID+"/ready", nil, 200, nil)
	b.call("POST", "/demo/approve", map[string]any{"as": "anna", "changeId": other.ID}, 200, nil)
	b.green(other.ID)
	b.call("POST", "/changes/"+other.ID+"/merge", nil, 200, nil)

	b.call("GET", "/changes/"+id, nil, 200, &ch)
	if ch.BehindTarget != 1 {
		t.Fatalf("behind %d", ch.BehindTarget)
	}
	b.green(id)
	var e struct {
		Error string `json:"error"`
	}
	b.call("POST", "/changes/"+id+"/merge", nil, 409, &e)
	if e.Error != "behind main by 1, rebase first" {
		t.Fatalf("merge error %q", e.Error)
	}
	var rb struct {
		Ok     bool `json:"ok"`
		Change struct {
			BehindTarget int `json:"behindTarget"`
		} `json:"change"`
	}
	b.call("POST", "/changes/"+id+"/rebase", nil, 200, &rb)
	if !rb.Ok || rb.Change.BehindTarget != 0 {
		t.Fatalf("rebase: %+v", rb)
	}

	// Approve as anna and jonas (demo), merge with trailers.
	b.call("POST", "/changes/"+id+"/approve", nil, 403, nil) // own change
	b.call("POST", "/demo/approve", map[string]any{"as": "dennis-agent", "changeId": id}, 403, nil)
	b.call("POST", "/demo/approve", map[string]any{"as": "anna", "changeId": id}, 200, nil)
	b.call("POST", "/demo/approve", map[string]any{"as": "jonas", "changeId": id}, 200, nil)
	b.green(id)
	var mr struct {
		Sha     string `json:"sha"`
		Message string `json:"message"`
	}
	b.call("POST", "/changes/"+id+"/merge", nil, 200, &mr)
	for _, want := range []string{"Merge change " + id + ": Add cn-035 to defiant", "Pk-Change: " + id, "Pk-Work-Item: w-114", "Approved-by: anna, jonas"} {
		if !strings.Contains(mr.Message, want) {
			t.Fatalf("merge message lacks %q:\n%s", want, mr.Message)
		}
	}

	// History: our merge on top, then the other change; filters.
	var hl struct {
		Entries []struct {
			Sha      string   `json:"sha"`
			Change   string   `json:"change"`
			Type     string   `json:"type"`
			Files    []string `json:"files"`
			Trailers []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"trailers"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	// The main worktree follows origin/main on the next refresh.
	if err := b.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.call("GET", "/history", nil, 200, &hl)
	if len(hl.Entries) != 3 || hl.Entries[0].Change != id || hl.Entries[0].Sha != mr.Sha || hl.Entries[1].Change != other.ID ||
		hl.Entries[2].Type != "direct" {
		t.Fatalf("history: %+v", hl)
	}
	b.call("GET", "/history?path="+q("control/**"), nil, 200, &hl)
	if hl.Total != 2 || hl.Entries[0].Change != other.ID {
		t.Fatalf("history path filter: %+v", hl)
	}
	b.call("GET", "/history?q=cabling", nil, 200, &hl)
	if hl.Total != 1 {
		t.Fatalf("history q: %+v", hl)
	}
	var hd struct {
		Parent string `json:"parent"`
		Files  []struct {
			Path    string `json:"path"`
			Unified string `json:"unified"`
		} `json:"files"`
		Semantic []any `json:"semantic"`
	}
	b.call("GET", "/history/"+mr.Sha, nil, 200, &hd)
	if hd.Parent == "" || len(hd.Files) == 0 || hd.Semantic == nil {
		t.Fatalf("history entry: %+v", hd)
	}

	// Time travel: the file before our merge.
	b.call("GET", "/refs/"+hd.Parent+"/file?path="+q("clusters/defiant/infra/cilium/bgp.yaml"), nil, 200, &fc)
	if !fc.ReadOnly || strings.Contains(fc.Content, "cn-035") {
		t.Fatalf("file at parent: %+v", fc)
	}
	var rt struct {
		Sha     string `json:"sha"`
		Entries []any  `json:"entries"`
	}
	b.call("GET", "/refs/main/tree", nil, 200, &rt)
	if rt.Sha != mr.Sha || len(rt.Entries) == 0 {
		t.Fatalf("ref tree: %+v", rt.Sha)
	}

	// List: merged changes only with scope=all.
	var list struct {
		Changes []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"changes"`
		Main struct {
			Sha    string `json:"sha"`
			Change string `json:"change"`
		} `json:"main"`
	}
	b.call("GET", "/changes", nil, 200, &list)
	if len(list.Changes) != 0 || list.Main.Change != id {
		t.Fatalf("open list: %+v", list)
	}
	b.call("GET", "/changes?scope=all", nil, 200, &list)
	if len(list.Changes) != 2 || list.Changes[0].State != "merged" {
		t.Fatalf("all list: %+v", list)
	}
}

func TestChangeHandlersUnavailable(t *testing.T) {
	_, ts := newTestServer(t, false)
	auth := map[string]string{"Authorization": "Bearer " + testAdminToken}
	if r := do(t, ts, "GET", "/api/v1/changes", auth, ""); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no change service: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/api/v1/demo/approve", auth, `{"as":"anna","changeId":"c-aaaaaa"}`); r.StatusCode != http.StatusNotFound {
		t.Fatalf("demo off: %d", r.StatusCode)
	}
}
