package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pkapi "github.com/dennisklein/platform-kit/api"
)

func TestSpecOps(t *testing.T) {
	ops, tags, err := specOps(pkapi.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) == 0 {
		t.Fatal("no tags")
	}
	byID := map[string]specOp{}
	for _, op := range ops {
		byID[op.id] = op
	}
	get, ok := byID["getChange"]
	if !ok || get.method != http.MethodGet || get.tag != "changes" || get.mcp != "pk_change_get" {
		t.Fatalf("getChange %+v", get)
	}
	if pp := get.pathParams(); len(pp) != 1 || pp[0].name != "id" {
		t.Errorf("path params %+v", pp)
	}
	create := byID["createChange"]
	var props []string
	for _, p := range create.props {
		props = append(props, p.name)
	}
	if !create.body || !strings.Contains(strings.Join(props, ","), "title") {
		t.Errorf("createChange body props %v", props)
	}
	if kebab("getLabRun") != "get-lab-run" || singular("entities") != "entity" || singular("changes") != "change" {
		t.Error("kebab or singular")
	}
}

type seen struct {
	method, path, rawQuery, auth string
	body                         map[string]any
}

// fakeBench records requests and answers from a route table.
func fakeBench(t *testing.T, routes map[string]string) (*httptest.Server, func() []seen) {
	var mu sync.Mutex
	var got []seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := seen{method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if len(b) > 0 {
			_ = json.Unmarshal(b, &s.body)
		}
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
		ans, ok := routes[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no route"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(ans))
	}))
	t.Cleanup(ts.Close)
	return ts, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), got...)
	}
}

func runPK(t *testing.T, url string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("PK_URL", url)
	t.Setenv("PK_TOKEN", "tok")
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestGeneratedCommands(t *testing.T) {
	ts, got := fakeBench(t, map[string]string{
		"GET /api/v1/changes/c-k7m2qz":                 `{"id":"c-k7m2qz","title":"Add cn-035"}`,
		"PUT /api/v1/changes/c-k7m2qz/file":            `{"file":{"path":"a.yaml"}}`,
		"GET /api/v1/labs/runs/c-k7m2qz%2Frun-1":       `{"id":"c-k7m2qz/run-1","result":"pass"}`,
		"POST /api/v1/changes/c-k7m2qz/advisors/risks": `{"recipe":"risks","points":[]}`,
	})
	out, err := runPK(t, ts.URL, "changes", "get-change", "c-k7m2qz", "--json")
	if err != nil || !strings.Contains(out, `"title": "Add cn-035"`) {
		t.Fatalf("get-change: %v\n%s", err, out)
	}
	if _, err := runPK(t, ts.URL, "change", "write-file", "c-k7m2qz", "--path", "control/x.yaml", "--data", `{"content":"a: 1\n"}`); err == nil {
		t.Error("write-file is a files command, not a changes command")
	}
	if _, err := runPK(t, ts.URL, "files", "write-file", "c-k7m2qz", "--path", "control/x.yaml", "--data", `{"content":"a: 1\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runPK(t, ts.URL, "labs", "get-lab-run", "c-k7m2qz/run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runPK(t, ts.URL, "llm", "run-advisor", "c-k7m2qz", "risks"); err != nil {
		t.Fatal(err)
	}
	reqs := got()
	if len(reqs) != 4 {
		t.Fatalf("requests %+v", reqs)
	}
	if reqs[0].auth != "Bearer tok" {
		t.Errorf("auth %q", reqs[0].auth)
	}
	w := reqs[1]
	if w.method != http.MethodPut || w.rawQuery != "path=control%2Fx.yaml" || w.body["content"] != "a: 1\n" {
		t.Errorf("write-file %+v", w)
	}
	if reqs[2].path != "/api/v1/labs/runs/c-k7m2qz%2Frun-1" {
		t.Errorf("lab run path %s", reqs[2].path)
	}
	if reqs[3].method != http.MethodPost || reqs[3].path != "/api/v1/changes/c-k7m2qz/advisors/risks" {
		t.Errorf("advisor %+v", reqs[3])
	}
}

func TestErrorAnswer(t *testing.T) {
	ts, _ := fakeBench(t, map[string]string{})
	_, err := runPK(t, ts.URL, "changes", "get-change", "c-zzzzzz")
	if err == nil || !strings.Contains(err.Error(), "no route [404]") {
		t.Errorf("err %v", err)
	}
}

func TestEditSetsTypedValues(t *testing.T) {
	ts, got := fakeBench(t, map[string]string{
		"GET /api/v1/entities": `[{"kind":"Node","name":"cn-035","path":"control/inventory/cn-035.yaml"},{"kind":"Node","name":"cn-036","path":"control/inventory/cn-036.yaml"}]`,
		"PUT /api/v1/changes/c-k7m2qz/file": `{"file":{"path":"control/inventory/cn-035.yaml","problems":[{"severity":"error","line":31,"message":"port data-a 37 in use by cn-003"}]},` +
			`"impact":{"text":"defiant +1","added":1,"changed":0,"deleted":0,"clusters":[]}}`,
	})
	out, err := runPK(t, ts.URL, "edit", "cn-035", "--change", "c-k7m2qz",
		"--set", "spec.location.unit=6", "--set", "spec.cluster=defiant", "--set", `spec.serial="0042"`,
		"--set", "spec.ports=[{name: p2, peer: {switch: data-a, port: 37}}]")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"saved control/inventory/cn-035.yaml in c-k7m2qz", "error line 31: port data-a 37 in use by cn-003", "impact: defiant +1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	reqs := got()
	put := reqs[len(reqs)-1]
	set, _ := put.body["set"].(map[string]any)
	if set["spec.location.unit"] != float64(6) || set["spec.cluster"] != "defiant" || set["spec.serial"] != "0042" {
		t.Errorf("set %v", set)
	}
	ports, _ := set["spec.ports"].([]any)
	if len(ports) != 1 {
		t.Fatalf("ports %v", set["spec.ports"])
	}
	if p, _ := ports[0].(map[string]any); p["name"] != "p2" {
		t.Errorf("port %v", ports[0])
	}
	if put.rawQuery != "path=control%2Finventory%2Fcn-035.yaml" {
		t.Errorf("query %s", put.rawQuery)
	}
}

func TestLabRunWaitPrintsSteps(t *testing.T) {
	ts, _ := fakeBench(t, map[string]string{
		"POST /api/v1/changes/c-k7m2qz/labs": `{"id":"c-k7m2qz/run-1","result":"running","scenario":"metal","steps":[{"name":"topology","title":"topology: generate","status":"running"}]}`,
		"GET /api/v1/labs/runs/c-k7m2qz%2Frun-1": `{"id":"c-k7m2qz/run-1","result":"pass","summary":"12 of 12 checks pass","steps":[` +
			`{"name":"topology","title":"topology: generate","status":"pass"},{"name":"checks","title":"checks","status":"pass","detail":"12 of 12"}]}`,
	})
	out, err := runPK(t, ts.URL, "lab", "run", "c-k7m2qz", "--wait")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"c-k7m2qz/run-1 started (metal)", "pass    topology: generate", "pass    checks: 12 of 12", "c-k7m2qz/run-1: pass 12 of 12 checks pass"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentApprovePrintsAllowRule(t *testing.T) {
	allow := `mkdir -p .claude && echo '{"permissions":{"allow":["mcp__platform-kit"]}}' > .claude/settings.local.json`
	ab, _ := json.Marshal(allow)
	ts, got := fakeBench(t, map[string]string{
		"POST /api/v1/agents/dennis-agent/approve": `{"token":"pk_agt_x","mcpUrl":"http://127.0.0.1:8080/mcp",` +
			`"grant":{"id":"dennis-agent","mode":"navigate","expiresAt":"2026-10-29T12:00:00Z"},"snippets":[` +
			`{"client":"claude-code","file":".mcp.json","content":"{}","command":"claude mcp add x","allow":` + string(ab) + `,"allowFlag":"claude --allowedTools mcp__platform-kit"},` +
			`{"client":"opencode","file":"opencode.json","content":"{}"}]}`,
	})
	out, err := runPK(t, ts.URL, "agents", "approve", "dennis-agent", "--hours", "2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"claude mcp add x\n# Allow the platform-kit tools", "\n" + allow + "\n", "# Other way: start the agent with claude --allowedTools mcp__platform-kit"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if b := got()[0].body; b["hours"] != float64(2) {
		t.Errorf("hours not sent: %v", b)
	}
}
