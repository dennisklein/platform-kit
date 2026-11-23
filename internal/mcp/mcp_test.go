package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dennisklein/platform-kit/api"
	pkapi "github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
)

func TestToolsFromSpec(t *testing.T) {
	tools, err := Tools(api.Spec)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s: schema type %v", tl.Name, tl.InputSchema["type"])
		}
		b, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		if strings.Contains(string(b), "$ref") || strings.Contains(string(b), `"nullable"`) {
			t.Errorf("%s: schema keeps OpenAPI-only keys: %s", tl.Name, b)
		}
	}
	for _, n := range []string{"pk_change_list", "pk_change_new", "pk_change_approve", "pk_file_write"} {
		if _, ok := byName[n]; !ok {
			t.Errorf("tool %s missing", n)
		}
	}
	ap := byName["pk_change_approve"]
	if ap.Method != http.MethodPost || ap.Path != "/changes/{id}/approve" {
		t.Errorf("approve: %s %s", ap.Method, ap.Path)
	}
	req, _ := ap.InputSchema["required"].([]string)
	if len(req) == 0 || req[0] != "id" {
		t.Errorf("approve required %v", ap.InputSchema["required"])
	}
}

func TestPlanFlattensBody(t *testing.T) {
	spec := []byte(`
openapi: 3.0.3
paths:
  /things/{id}:
    put:
      operationId: putThing
      x-mcp-tool: pk_thing_put
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
        - {name: dry, in: query, schema: {type: boolean}}
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Thing'}
  /clash/{id}:
    post:
      operationId: clash
      x-mcp-tool: pk_clash
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                id: {type: string}
components:
  schemas:
    Thing:
      type: object
      required: [name]
      properties:
        name: {type: string, nullable: true}
        child: {$ref: '#/components/schemas/Thing'}
`)
	plans, err := plan(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Name != "pk_clash" {
		t.Fatalf("plans %+v", plans)
	}
	clash, put := plans[0], plans[1]
	if clash.Body == nil || clash.Body.Flat {
		t.Errorf("clashing body must stay under body: %+v", clash.Body)
	}
	if !put.Body.Flat || strings.Join(put.Body.Props, ",") != "child,name" {
		t.Errorf("put body %+v", put.Body)
	}
	if got := strings.Join(put.InputSchema["required"].([]string), ","); got != "id,name" {
		t.Errorf("required %s", got)
	}
	props := put.InputSchema["properties"].(map[string]any)
	name := props["name"].(map[string]any)
	if tl, ok := name["type"].([]any); !ok || len(tl) != 2 {
		t.Errorf("nullable name: %v", name)
	}
	child := props["child"].(map[string]any)
	if child["type"] != "object" {
		t.Errorf("recursive child: %v", child)
	}
	path, q, body, err := build(put, map[string]any{"id": "a/b", "dry": true, "name": "x"})
	if err != nil || path != "/things/a%2Fb" || q.Get("dry") != "true" || body.(map[string]any)["name"] != "x" {
		t.Errorf("build: %s %v %v %v", path, q, body, err)
	}
	if _, _, _, err := build(put, map[string]any{}); err == nil {
		t.Error("missing id must fail")
	}
}

type recorder struct {
	mu  sync.Mutex
	evs []pkapi.Presence
	to  []string
}

func (r *recorder) Publish(t events.Type, data any) {}
func (r *recorder) PublishTo(aud string, t events.Type, data any) {
	if p, ok := data.(pkapi.Presence); ok && t == events.Presence {
		r.mu.Lock()
		r.evs = append(r.evs, p)
		r.to = append(r.to, aud)
		r.mu.Unlock()
	}
}

// fakeAPI answers a few routes like the bench does.
func fakeAPI(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/changes", func(w http.ResponseWriter, r *http.Request) {
		id := identity.MustFrom(r.Context())
		if id.User != "dennis-agent" {
			t.Errorf("identity %+v", id)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"c-k7m2qz","title":"Add cn-035 and cn-036","state":"draft"}]`))
	})
	mux.HandleFunc("POST /api/v1/changes/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"agents never approve or merge"}`))
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":"dennis-agent","kind":"agent","mode":"suggest"}`))
	})
	return mux
}

// withAgent stands in for the bench auth: a bearer token is the agent.
func withAgent(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pk_agt_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := identity.With(r.Context(), identity.Identity{User: "dennis-agent", Kind: identity.Agent, ActsFor: "dennis"})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// Connect connects a go-sdk client to an MCP endpoint with a bearer token.
func connect(t *testing.T, url, token string) *sdk.ClientSession {
	t.Helper()
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: url,
		HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestClientRoundTrip(t *testing.T) {
	rec := &recorder{}
	h, err := New(Options{Spec: api.Spec, API: fakeAPI(t), Events: rec})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(withAgent(h))
	defer ts.Close()
	cs := connect(t, ts.URL, "pk_agt_test")
	ctx := context.Background()

	if cs.InitializeResult().Instructions == "" {
		t.Error("no instructions")
	}
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.Tools) < 20 {
		t.Errorf("only %d tools", len(lt.Tools))
	}

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_change_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "c-k7m2qz") {
		t.Errorf("list: %+v", res.Content[0])
	}

	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_change_approve", Arguments: map[string]any{"id": "c-k7m2qz"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*sdk.TextContent).Text; !res.IsError || got != "denied: agents never approve or merge" {
		t.Errorf("approve: error=%v %q", res.IsError, got)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.evs) != 2 || rec.to[0] != "dennis" || rec.evs[1].ChangeId != "c-k7m2qz" ||
		rec.evs[1].Text != "pk_change_approve c-k7m2qz (failed: denied: agents never approve or merge)" {
		t.Errorf("presence %+v to %v", rec.evs, rec.to)
	}
	if c, ok := Recent.Last("dennis-agent"); !ok || c.Tool != "pk_change_approve" {
		t.Errorf("recent %+v", c)
	}
}

func TestNoTokenRejected(t *testing.T) {
	h, err := New(Options{Spec: api.Spec, API: fakeAPI(t)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(withAgent(h))
	defer ts.Close()
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	if _, err := c.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: ts.URL}, nil); err == nil {
		t.Error("connect without a token must fail")
	}
}

func TestLabStatusWaitsForRunningRun(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/labs/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if r.PathValue("run") != "c-k7m2qz/run-1" {
			t.Errorf("run %q", r.PathValue("run"))
		}
		result := "running"
		if n >= 3 {
			result = "pass"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c-k7m2qz/run-1","result":"` + result + `"}`))
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":"dennis-agent","kind":"agent"}`))
	})
	h, err := New(Options{Spec: api.Spec, API: mux, LabWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(withAgent(h))
	defer ts.Close()
	cs := connect(t, ts.URL, "pk_agt_test")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "pk_lab_status", Arguments: map[string]any{"run": "c-k7m2qz/run-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*sdk.TextContent).Text; res.IsError || !strings.Contains(got, `"pass"`) {
		t.Errorf("status: %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("calls %d", calls)
	}
}

func TestFocusForRack(t *testing.T) {
	tp := toolPlan{Tool: Tool{Name: "pk_entity_get"}}
	f, ok := focusFor(tp, map[string]any{"kind": "Rack", "name": "GC5A-6Z"}, nil)
	if !ok || f.Route != "/assets/racks/GC5A-6Z" {
		t.Errorf("rack focus %+v", f)
	}
	f, ok = focusFor(tp, map[string]any{"kind": "Node", "name": "cn-035"}, nil)
	if !ok || f.Route != "/assets/Node/cn-035" {
		t.Errorf("node focus %+v", f)
	}
}

func TestFocusForAdviseRunsTheRecipe(t *testing.T) {
	tp := toolPlan{Tool: Tool{Name: "pk_advise"}}
	f, ok := focusFor(tp, map[string]any{"id": "c-k7m2qz", "recipe": "risks"}, nil)
	if !ok || f.Route != "/changes/c-k7m2qz/review?advisor=risks" || f.View != pkapi.FocusViewReview {
		t.Errorf("advise focus %+v", f)
	}
	f, ok = focusFor(toolPlan{Tool: Tool{Name: "pk_render_impact"}}, map[string]any{"id": "c-k7m2qz"}, nil)
	if !ok || f.Route != "/changes/c-k7m2qz/review" {
		t.Errorf("impact focus %+v", f)
	}
}

func TestAdviseFollowTagsTheAgent(t *testing.T) {
	var got pkapi.FocusUpdate
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/changes/{id}/advisors/{recipe}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"recipe":"risks","title":"Risks","points":[],"model":"mock","mock":true,"fallback":true,"logId":"l1"}`))
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":"dennis-agent","kind":"agent","grant":{"mode":"navigate"}}`))
	})
	mux.HandleFunc("PUT /api/v1/focus", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"by":"dennis-agent"}`))
	})
	h, err := New(Options{Spec: api.Spec, API: mux})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(withAgent(h))
	defer ts.Close()
	cs := connect(t, ts.URL, "pk_agt_test")
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "pk_advise", Arguments: map[string]any{"id": "c-k7m2qz", "recipe": "risks"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Route != "/changes/c-k7m2qz/review?advisor=risks&by=dennis%2Fagent" {
		t.Errorf("route %q", got.Route)
	}
	// Built-in mode: the agent reads the same model name as the UI, and no fallback.
	var out struct {
		Model    string `json:"model"`
		Mock     bool   `json:"mock"`
		Fallback bool   `json:"fallback"`
		LogID    string `json:"logId"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "built-in" || out.Fallback || !out.Mock || out.LogID != "l1" {
		t.Errorf("result %+v", out)
	}
}

func TestBuiltinModelKeepsModelAnswers(t *testing.T) {
	tp := toolPlan{Tool: Tool{Name: "pk_advise"}}
	in := `{"model":"qwen2.5:7b","mock":false,"fallback":true}`
	if got := string(builtinModel(tp, []byte(in))); got != in {
		t.Errorf("model answer changed: %s", got)
	}
	other := toolPlan{Tool: Tool{Name: "pk_change_get"}}
	if got := string(builtinModel(other, []byte(`{"mock":true,"model":"mock"}`))); got != `{"mock":true,"model":"mock"}` {
		t.Errorf("other tool changed: %s", got)
	}
}

func TestHandlerPresenceReplacesCallPresence(t *testing.T) {
	rec := &recorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/focus", func(w http.ResponseWriter, r *http.Request) {
		// The bench sends the "look here" card itself in suggest mode.
		tc := events.ToolCallOf(r.Context())
		if tc == nil || tc.Tool != "pk_focus" {
			t.Errorf("tool call %+v", tc)
		} else {
			tc.Sent = true
		}
		_, _ = w.Write([]byte(`{"by":"dennis"}`))
	})
	mux.HandleFunc("GET /api/v1/changes", func(w http.ResponseWriter, r *http.Request) {
		if events.ToolCallOf(r.Context()) == nil {
			t.Error("no tool call in the request context")
		}
		_, _ = w.Write([]byte(`[]`))
	})
	h, err := New(Options{Spec: api.Spec, API: mux, Events: rec})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(withAgent(h))
	defer ts.Close()
	cs := connect(t, ts.URL, "pk_agt_test")
	ctx := context.Background()
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_focus", Arguments: map[string]any{"changeId": "c-k7m2qz", "view": "change"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "pk_change_list", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.evs) != 1 || rec.evs[0].Tool != "pk_change_list" {
		t.Errorf("presence %+v", rec.evs)
	}
}
