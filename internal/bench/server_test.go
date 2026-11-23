package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/forge"
)

func newTestServer(t *testing.T, demo bool) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Config{Addr: "127.0.0.1:8080", DataDir: t.TempDir(), Demo: demo, User: "dennis",
		AdminToken: "pk_adm_test_token_0123456789abcdef0123", PublicURL: "http://127.0.0.1:8080"}
	spa := fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html><title>pk</title>")},
		"assets/app-1.js": {Data: []byte("console.log(1)")},
	}
	s, err := New(cfg, Deps{}, spa)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, ts *httptest.Server, method, path string, hdr map[string]string, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	req.Host = "127.0.0.1:8080"
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestHostAndOrigin(t *testing.T) {
	_, ts := newTestServer(t, true)
	if r := do(t, ts, "GET", "/api/v1/me", map[string]string{"Host": "evil.example:8080"}, ""); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", r.StatusCode)
	}
	if r := do(t, ts, "GET", "/healthz", map[string]string{"Host": "bench:8080"}, ""); r.StatusCode != 200 {
		t.Fatalf("healthz: %d", r.StatusCode)
	}
	auth := map[string]string{"Authorization": "Bearer pk_adm_test_token_0123456789abcdef0123", "Origin": "http://evil.example"}
	if r := do(t, ts, "GET", "/api/v1/me", auth, ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", r.StatusCode)
	}
	auth["Origin"] = "http://localhost:8080"
	if r := do(t, ts, "GET", "/api/v1/me", auth, ""); r.StatusCode != 200 {
		t.Fatalf("own origin: %d", r.StatusCode)
	}
}

func TestDemoLoginAndMe(t *testing.T) {
	_, ts := newTestServer(t, true)
	if r := do(t, ts, "GET", "/api/v1/me", nil, ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: %d", r.StatusCode)
	}
	// A plain HTTP client (curl, an agent's shell) gets no session.
	if r := do(t, ts, "GET", "/", nil, ""); len(r.Cookies()) != 0 {
		t.Fatalf("demo cookie for a non-browser client: %v", r.Cookies())
	}
	nav := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	r := do(t, ts, "GET", "/changes/c-k7m2qz", nav, "")
	if r.StatusCode != 200 {
		t.Fatalf("page: %d", r.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", cookie)
	}
	r = do(t, ts, "GET", "/api/v1/me", map[string]string{"Cookie": CookieName + "=" + cookie.Value}, "")
	var me api.Me
	if err := json.NewDecoder(r.Body).Decode(&me); err != nil || me.Id != "dennis" || me.Kind != api.Human || !me.Demo {
		t.Fatalf("me: %+v %v", me, err)
	}
	// Operations whose service is not wired answer 503 with the error schema.
	r = do(t, ts, "GET", "/api/v1/changes", map[string]string{"Cookie": CookieName + "=" + cookie.Value}, "")
	var e api.Error
	if r.StatusCode != http.StatusServiceUnavailable || json.NewDecoder(r.Body).Decode(&e) != nil || e.Error == "" {
		t.Fatalf("listChanges: %d %+v", r.StatusCode, e)
	}
	// Slashes in path params arrive escaped and decoded.
	r = do(t, ts, "GET", "/api/v1/labs/runs/c-k7m2qz%2Frun-1", map[string]string{"Cookie": CookieName + "=" + cookie.Value}, "")
	if r.StatusCode != http.StatusNotImplemented && r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("lab run route: %d", r.StatusCode)
	}
}

func TestNoDemoNeedsToken(t *testing.T) {
	_, ts := newTestServer(t, false)
	r := do(t, ts, "GET", "/", nil, "")
	if len(r.Cookies()) != 0 {
		t.Fatal("no cookie without demo")
	}
	r = do(t, ts, "GET", "/?token=pk_adm_test_token_0123456789abcdef0123", nil, "")
	if r.StatusCode != http.StatusSeeOther || len(r.Cookies()) != 1 || r.Header.Get("Location") != "/" {
		t.Fatalf("token login: %d %v", r.StatusCode, r.Header)
	}
}

func TestAgentTokenFocusAndEvents(t *testing.T) {
	s, ts := newTestServer(t, true)
	token, _, err := s.grants.Approve("dennis-agent", "dennis", []string{"push"}, ModeNavigate, time.Hour, "dennis")
	if err != nil {
		t.Fatal(err)
	}
	agent := map[string]string{"Authorization": "Bearer " + token}
	if r := do(t, ts, "POST", "/mcp", nil, "{}"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mcp without token: %d", r.StatusCode)
	}
	if r := do(t, ts, "POST", "/mcp", agent, "{}"); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("mcp with token: %d", r.StatusCode)
	}

	// The person's event stream sees the agent move the focus.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/events", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer pk_adm_test_token_0123456789abcdef0123")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	br := bufio.NewReader(res.Body)
	readUntil := func(prefix string) string {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
	}
	readUntil(": connected")

	r := do(t, ts, "PUT", "/api/v1/focus", agent, `{"changeId":"c-k7m2qz","path":"control/inventory/cn-035.yaml","view":"editor"}`)
	var fo api.Focus
	if err := json.NewDecoder(r.Body).Decode(&fo); err != nil || fo.By != "dennis-agent" || fo.ChangeId != "c-k7m2qz" {
		t.Fatalf("focus: %+v %v", fo, err)
	}
	if ev := readUntil("event: "); strings.TrimSpace(ev) != "event: focus.changed" {
		t.Fatalf("event %q", ev)
	}
	data := readUntil("data: ")
	if !strings.Contains(data, `"by":"dennis-agent"`) {
		t.Fatalf("data %q", data)
	}
}

func TestSPAFallback(t *testing.T) {
	_, ts := newTestServer(t, true)
	r := do(t, ts, "GET", "/assets/app-1.js", nil, "")
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", r.StatusCode, r.Header)
	}
	if r := do(t, ts, "GET", "/assets/missing.js", nil, ""); r.StatusCode != 404 {
		t.Fatalf("missing asset: %d", r.StatusCode)
	}
	r = do(t, ts, "GET", "/history/abc", nil, "")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "<title>pk</title>") {
		t.Fatalf("index fallback: %d %s", r.StatusCode, b)
	}
}

func TestForgeUnreachableMessage(t *testing.T) {
	err := fmt.Errorf("%w: git -c http.extraHeader=<redacted> ls-remote --heads origin refs/heads/change/c-abc234*: fatal: unable to access 'http://gitea:3000/platform/state.git/': Could not resolve host: gitea", forge.ErrUnreachable)
	he := statusOf(err)
	if he.Status != http.StatusBadGateway || he.Msg != MsgForgeUnreachable || he.Detail != "Could not resolve host: gitea" {
		t.Fatalf("%+v", he)
	}
}
