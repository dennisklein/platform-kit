package bench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	specpkg "github.com/dennisklein/platform-kit/api"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/llm"
	"github.com/dennisklein/platform-kit/internal/mcp"
)

// llmFakeChanges answers List; other methods panic (nil interface).
type llmFakeChanges struct {
	change.Service
}

func (llmFakeChanges) List(ctx context.Context, all bool) ([]change.Summary, error) {
	return []change.Summary{{ID: "c-k7m2qz", Title: "Add cn-035 and cn-036", Branch: "change/c-k7m2qz-add-cn-035",
		Target: "main", Author: "dennis", AuthorKind: identity.Human, State: change.State("draft")}}, nil
}

func (llmFakeChanges) MainDir(ctx context.Context) (string, error) {
	return "", errors.New("no main in this test")
}

type fakeIndex struct{}

func (fakeIndex) Search(ctx context.Context, q string, limit int) ([]llm.Hit, error) {
	return []llm.Hit{{Document: llm.Document{Kind: "kb", Ref: "kb/bgp.md", Title: "BGP peering", Path: "kb/bgp.md"}, Score: 1.5, Snippet: "rack 6Z"}}, nil
}
func (fakeIndex) Rebuild(ctx context.Context) error { return nil }
func (fakeIndex) Mode() string                      { return "bm25" }

const adminTok = "pk_adm_test_token_0123456789abcdef0123"

func newLLMTestServer(t *testing.T) (*Server, *httptest.Server, *events.Hub, *llm.FileLog) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{Addr: "127.0.0.1:8080", DataDir: dir, Demo: true, User: "dennis",
		AdminToken: adminTok, PublicURL: "http://127.0.0.1:8080"}
	hub := events.NewHub()
	lg := llm.NewFileLog(filepath.Join(dir, "llm", "log.jsonl"))
	d := Deps{Events: hub, Changes: llmFakeChanges{}, LLMLog: lg, Search: fakeIndex{}}
	d.MCP = func(h http.Handler) http.Handler {
		m, err := mcp.New(mcp.Options{Spec: specpkg.Spec, API: h, BasePath: BasePath, Events: hub})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	s, err := New(cfg, d, fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, hub, lg
}

func jsonDo(t *testing.T, ts *httptest.Server, method, path, token, body string, out any) int {
	t.Helper()
	res := do(t, ts, method, path, map[string]string{"Authorization": "Bearer " + token}, body)
	b, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode/100 == 2 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	if res.StatusCode/100 != 2 {
		t.Logf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	return res.StatusCode
}

// hostTransport sends the bench host and the bearer token.
type hostTransport struct{ token string }

func (h hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPWithGoSDKClient(t *testing.T) {
	_, ts, hub, _ := newLLMTestServer(t)

	var appr api.AgentApproval
	if c := jsonDo(t, ts, "POST", "/api/v1/agents/dennis-agent/approve", adminTok, `{"mode":"suggest","hours":4}`, &appr); c != 200 {
		t.Fatalf("approve agent: %d", c)
	}
	if !strings.HasPrefix(appr.Token, "pk_agt_") || appr.McpUrl != "http://127.0.0.1:8080/mcp" || len(appr.Snippets) != 2 {
		t.Fatalf("approval %+v", appr)
	}
	wantCmd := `claude mcp add --transport http platform-kit http://127.0.0.1:8080/mcp --header "Authorization: Bearer ` + appr.Token + `"`
	if appr.Snippets[0].Command != wantCmd {
		t.Errorf("claude command %q", appr.Snippets[0].Command)
	}
	// The allow rule of the README: Claude Code runs the tools without a prompt.
	wantAllow := `mkdir -p .claude && echo '{"permissions":{"allow":["mcp__platform-kit"]}}' > .claude/settings.local.json`
	if appr.Snippets[0].Allow != wantAllow || appr.Snippets[0].AllowFlag != "claude --allowedTools mcp__platform-kit" {
		t.Errorf("claude allow %q / %q", appr.Snippets[0].Allow, appr.Snippets[0].AllowFlag)
	}
	if appr.Snippets[1].Allow != "" {
		t.Errorf("opencode allow %q", appr.Snippets[1].Allow)
	}
	if appr.Grant.ExpiresAt.Sub(appr.Grant.ApprovedAt) != 4*time.Hour {
		t.Errorf("hours not honored: %v .. %v", appr.Grant.ApprovedAt, appr.Grant.ExpiresAt)
	}
	var oc map[string]any
	if err := json.Unmarshal([]byte(appr.Snippets[1].Content), &oc); err != nil || appr.Snippets[1].File != "opencode.json" {
		t.Errorf("opencode snippet %v %s", err, appr.Snippets[1].Content)
	}
	if appr.Grant.ActsFor != "dennis" || appr.Grant.Mode != api.Suggest {
		t.Errorf("grant %+v", appr.Grant)
	}
	// Agents cannot approve agents.
	if c := jsonDo(t, ts, "POST", "/api/v1/agents/dennis-agent/approve", appr.Token, `{}`, nil); c != 403 {
		t.Errorf("agent approving: %d", c)
	}

	evs, cancel := hub.Subscribe("dennis", hub.LastID())
	defer cancel()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "1"}, nil)
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: hostTransport{appr.Token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, n := range []string{"pk_change_list", "pk_change_new", "pk_change_approve", "pk_file_write", "pk_lab_run"} {
		if !names[n] {
			t.Errorf("tool %s missing (%d tools)", n, len(tools.Tools))
		}
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_change_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	if res.IsError || !strings.Contains(text, "c-k7m2qz") {
		t.Errorf("pk_change_list: %v %s", res.IsError, text)
	}

	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_change_approve", Arguments: map[string]any{"id": "c-k7m2qz"}})
	if err != nil {
		t.Fatal(err)
	}
	text = res.Content[0].(*sdk.TextContent).Text
	if !res.IsError || text != "denied: agents never approve or merge" {
		t.Errorf("pk_change_approve: error=%v %q", res.IsError, text)
	}

	// Presence events reach the person the agent acts for.
	var seen []string
	deadline := time.After(2 * time.Second)
loop:
	for len(seen) < 2 {
		select {
		case e := <-evs:
			if e.Type == events.Presence {
				seen = append(seen, e.Data.(api.Presence).Tool)
			}
		case <-deadline:
			break loop
		}
	}
	if strings.Join(seen, ",") != "pk_change_list,pk_change_approve" {
		t.Errorf("presence %v", seen)
	}

	// The agent list shows the last tool.
	var agents []api.AgentGrant
	jsonDo(t, ts, "GET", "/api/v1/agents", adminTok, "", &agents)
	if len(agents) != 1 || agents[0].LastTool != "pk_change_approve" || !agents[0].Active {
		t.Errorf("agents %+v", agents)
	}

	// Revoke: the token stops working.
	if c := jsonDo(t, ts, "DELETE", "/api/v1/agents/dennis-agent/grant", adminTok, "", nil); c != 200 {
		t.Fatalf("revoke %d", c)
	}
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_change_list", Arguments: map[string]any{}}); err == nil {
		t.Error("call after revoke must fail")
	}
}

func TestMCPNeedsAgentToken(t *testing.T) {
	_, ts, _, _ := newLLMTestServer(t)
	client := sdk.NewClient(&sdk.Implementation{Name: "x", Version: "1"}, nil)
	_, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: hostTransport{adminTok}}}, nil)
	if err == nil {
		t.Error("the admin token must not open MCP")
	}
}

func TestLLMLogAndSearch(t *testing.T) {
	_, ts, _, lg := newLLMTestServer(t)
	ctx := context.Background()
	for _, e := range []llm.LogEntry{
		{ID: "l-aaaa", Time: time.Now().Add(-time.Minute), User: "dennis", Surface: "chat", Model: "mock", Mock: true, Prompt: "why cn-003", Answer: "c-2hd7vw moved it"},
		{ID: "l-bbbb", Time: time.Now(), User: "anna", Surface: "advisor", Recipe: "risks", ChangeID: "c-k7m2qz", Model: "mock", Mock: true, Prompt: "risks", Answer: "overlap"},
	} {
		if _, err := lg.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var list api.LlmLogList
	jsonDo(t, ts, "GET", "/api/v1/llm/log?q=recipe:risks", adminTok, "", &list)
	if list.Total != 1 || len(list.Entries) != 1 || list.Entries[0].Id != "l-bbbb" {
		t.Errorf("list %+v", list)
	}
	var rated api.LlmLogEntry
	if c := jsonDo(t, ts, "POST", "/api/v1/llm/log/l-aaaa/rating", adminTok, `{"rating":"up"}`, &rated); c != 200 || rated.Rating != "up" {
		t.Errorf("rate %d %+v", c, rated)
	}
	res := do(t, ts, "GET", "/api/v1/llm/log/export", map[string]string{"Authorization": "Bearer " + adminTok}, "")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || strings.Count(string(b), "\n") != 2 {
		t.Errorf("export %d %q", res.StatusCode, b)
	}
	if c := jsonDo(t, ts, "DELETE", "/api/v1/llm/log/l-aaaa", adminTok, "", nil); c != 204 {
		t.Errorf("delete %d", c)
	}
	if c := jsonDo(t, ts, "DELETE", "/api/v1/llm/log/l-aaaa", adminTok, "", nil); c != 404 {
		t.Errorf("delete again %d", c)
	}
	var audit []map[string]any
	jsonDo(t, ts, "GET", "/api/v1/audit", adminTok, "", &audit)
	raw, _ := json.Marshal(audit)
	if !strings.Contains(string(raw), "l-aaaa") {
		t.Errorf("audit lacks the delete: %s", raw)
	}

	var sr api.SearchResult
	jsonDo(t, ts, "GET", "/api/v1/search?q=bgp", adminTok, "", &sr)
	if sr.Mode != "bm25" || len(sr.Hits) != 1 || sr.Hits[0].Ref != "kb/bgp.md" {
		t.Errorf("search %+v", sr)
	}
}
