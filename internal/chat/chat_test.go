package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/inproc"
	"github.com/dennisklein/platform-kit/internal/llm"
)

const (
	cid     = "c-k7m2qz"
	bgpPath = "clusters/defiant/infra/cilium/bgp-rack-6z.yaml"
)

type fake struct {
	files  map[string]string
	writes int
	hits   []llm.Hit
}

func newFake(t *testing.T) *fake {
	b, err := os.ReadFile(filepath.Join("..", "..", "seed", "state", bgpPath))
	if err != nil {
		t.Fatal(err)
	}
	return &fake{files: map[string]string{
		cid + "\x00" + bgpPath:                          string(b),
		cid + "\x00control/inventory/cn-037.yaml":       "kind: Node\n",
		cid + "\x00clusters/defiant/apps/x/deploy.yaml": "kind: Deployment\n",
	}}
}

func (f *fake) Change(ctx context.Context, id string) (*api.Change, error) {
	return &api.Change{Id: id, Title: "x"}, nil
}
func (f *fake) Changes(ctx context.Context) ([]api.ChangeSummary, error)   { return nil, nil }
func (f *fake) Impact(ctx context.Context, id string) (*api.Impact, error) { return &api.Impact{}, nil }
func (f *fake) Diff(ctx context.Context, id string) (*api.Diff, error)     { return &api.Diff{}, nil }
func (f *fake) Status(ctx context.Context, id string) (*api.WorktreeStatus, error) {
	return &api.WorktreeStatus{}, nil
}
func (f *fake) Problems(ctx context.Context, id string) ([]api.Problem, error) { return nil, nil }
func (f *fake) Tree(ctx context.Context, ref string) ([]api.FileNode, error) {
	var out []api.FileNode
	for k := range f.files {
		r, p, _ := strings.Cut(k, "\x00")
		if r == ref {
			n := api.FileNode{Path: p, Type: api.FileNodeTypeFile}
			if p == bgpPath {
				n.Kind = "CiliumBGPPeeringPolicy"
			}
			out = append(out, n)
		}
	}
	return []api.FileNode{{Type: api.FileNodeTypeDir, Children: out}}, nil
}
func (f *fake) ReadFile(ctx context.Context, ref, p string) (*api.FileContent, error) {
	c, ok := f.files[ref+"\x00"+p]
	if !ok {
		return nil, &inproc.Error{Status: 404, Msg: p + " does not exist"}
	}
	return &api.FileContent{Path: p, Content: c, Exists: true}, nil
}
func (f *fake) WriteFile(ctx context.Context, changeID, p, content string) (*api.WriteFileResult, error) {
	f.files[changeID+"\x00"+p] = content
	f.writes++
	return &api.WriteFileResult{}, nil
}
func (f *fake) History(ctx context.Context, p api.ListHistoryParams) ([]api.HistoryEntry, error) {
	if p.Entity != "cn-003" {
		return nil, nil
	}
	return []api.HistoryEntry{
		{Sha: "m1abcdef9", Change: "c-m1gr8t", Title: "Migrate Node to v1beta1", Author: "sync-bot", Date: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)},
		{Sha: "2hd7vw000", Change: "c-2hd7vw", Title: "Move cn-003 to data-a 37", Intent: "free data-b ports for rack 6Z", Author: "jonas",
			ApprovedBy: []string{"anna", "dennis"}, Mr: "!2", Date: time.Date(2026, 9, 12, 10, 15, 0, 0, time.UTC)},
	}, nil
}
func (f *fake) HistoryEntry(ctx context.Context, sha string) (*api.HistoryDetail, error) {
	if sha == "2hd7vw000" {
		return &api.HistoryDetail{Entry: api.HistoryEntry{Change: "c-2hd7vw", Mr: "!2"},
			Semantic: []api.SemanticLine{{Subject: "cn-003 p2", Text: "data-b 37 to data-a 37", Op: api.OpChange}}}, nil
	}
	return &api.HistoryDetail{Semantic: []api.SemanticLine{{Subject: "cn-003", Text: "stored as v1beta1 (was v1alpha1)"}}}, nil
}
func (f *fake) LabRuns(ctx context.Context, id string) ([]api.LabRun, error) { return nil, nil }
func (f *fake) Entity(ctx context.Context, kind, name, ref string) (*api.Entity, error) {
	return nil, errors.New("no")
}
func (f *fake) Search(ctx context.Context, q string, limit int) ([]llm.Hit, error) {
	if f.hits != nil {
		return f.hits, nil
	}
	return []llm.Hit{{Document: llm.Document{Kind: "kb", Ref: "kb/runbooks/add-node.md", Title: "Add a node"}, Snippet: "Rack, cable, inventory"}}, nil
}

type rec struct {
	events.Nop
	got []events.Type
}

func (r *rec) PublishTo(_ string, t events.Type, _ any) { r.got = append(r.got, t) }

func ctxAs(user string) context.Context {
	return identity.With(context.Background(), identity.Identity{User: user, Kind: identity.Human})
}

func newSvc(t *testing.T, f *fake, client llm.Client) (*Svc, *llm.FileLog, *rec) {
	dir := t.TempDir()
	log := llm.NewFileLog(filepath.Join(dir, "log.jsonl"))
	ev := &rec{}
	s, err := New(Options{Tools: f, LLM: client, Log: log, Events: ev, Path: filepath.Join(dir, "suggestions.json")})
	if err != nil {
		t.Fatal(err)
	}
	return s, log, ev
}

func ask(q string) Request {
	return Request{ChangeID: cid, Messages: []Message{{Role: "user", Content: q}}}
}

func TestMockWhy(t *testing.T) {
	s, log, _ := newSvc(t, newFake(t), llm.NewMock(nil))
	a, err := s.Ask(ctxAs("dennis"), ask("Why does cn-003 use port 37 on data-a?"))
	if err != nil {
		t.Fatal(err)
	}
	want := "c-2hd7vw moved cn-003 p2 from data-b 37 to data-a 37, to free data-b ports for rack 6Z. Approved by anna and dennis."
	if !strings.HasPrefix(a.Text, want) {
		t.Fatalf("got %q", a.Text)
	}
	if len(a.Sources) < 2 || a.Sources[0].Label != "trailer c-2hd7vw" || a.Sources[1].Label != "MR !2" {
		t.Errorf("sources %+v", a.Sources)
	}
	e, _ := log.Get(context.Background(), a.LogID)
	if e.Surface != llm.SurfaceChat || e.User != "dennis" || !strings.Contains(e.Prompt, "cn-003") {
		t.Errorf("log %+v", e)
	}
}

func TestSecretRefused(t *testing.T) {
	const hr = "clusters/defiant/infra/cilium/helmrelease.yaml"
	raw, err := os.ReadFile(filepath.Join("..", "..", "seed", "state", hr))
	if err != nil {
		t.Fatal(err)
	}
	text, n := llm.NewRedactor().Redact(string(raw))
	if n == 0 {
		t.Fatal("the seed helmrelease has no secret")
	}
	for _, client := range []llm.Client{llm.NewMock(nil), &script{}} {
		f := newFake(t)
		f.hits = []llm.Hit{{Document: llm.Document{Kind: "file", Ref: hr, Path: hr, Title: hr, Text: text, Redactions: n}}}
		s, log, _ := newSvc(t, f, client)
		a, err := s.Ask(ctxAs("dennis"), ask("What is the hubble UI token?"))
		if err != nil || !a.Refused || a.Text != RefusalText {
			t.Fatalf("%v %+v", err, a)
		}
		// The log shows the retrieved context as sent: secret removed.
		e, _ := log.Get(context.Background(), a.LogID)
		if e.Redactions != n || !strings.Contains(e.Prompt, "token: [redacted]") || strings.Contains(e.Prompt, "ENC[") ||
			len(e.Sources) != 1 || e.DurationMs == 0 {
			t.Fatalf("log entry %+v", e)
		}
	}
	if IsSecretQuestion("Why does cn-003 use port 37?") || IsSecretQuestion("token rotation runbook") {
		t.Error("false positive")
	}
}

func TestMockPolicySuggestionAcceptReject(t *testing.T) {
	f := newFake(t)
	s, _, ev := newSvc(t, f, llm.NewMock(nil))
	ctx := ctxAs("dennis")
	a, err := s.Ask(ctx, ask("Add cn-037 to the rack-6z BGP policy"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "I suggest 1 line in bgp-rack-6z.yaml: it adds cn-037 to rack-6z, so cn-037 gets a BGP session. Review the diff below, then accept or reject it." || len(a.Suggestions) != 1 {
		t.Fatalf("%q %+v", a.Text, a.Suggestions)
	}
	sg := a.Suggestions[0]
	if sg.Status != advisor.StatusPending || sg.By != "chat" || sg.LogID != a.LogID || !strings.Contains(sg.Diff, "+          - cn-037") {
		t.Fatalf("suggestion %+v", sg)
	}
	if len(ev.got) != 1 || ev.got[0] != events.SuggestionCreated {
		t.Errorf("events %v", ev.got)
	}
	// The same question again keeps one pending suggestion.
	a2, _ := s.Ask(ctx, ask("Add cn-037 to the rack-6z BGP policy"))
	list, _ := s.Suggestions(ctx, cid)
	if len(list) != 1 || a2.Suggestions[0].ID != sg.ID {
		t.Fatalf("duplicates: %d", len(list))
	}
	acc, err := s.Accept(ctx, sg.ID)
	if err != nil || acc.Status != advisor.StatusAccepted || acc.DecidedBy != "dennis" || f.writes != 1 {
		t.Fatalf("accept %v %+v", err, acc)
	}
	if !strings.Contains(f.files[cid+"\x00"+bgpPath], "- cn-037") {
		t.Fatal("not written")
	}
	if _, err := s.Accept(ctx, sg.ID); !errors.Is(err, ErrDecided) {
		t.Fatalf("second accept: %v", err)
	}

	// A suggestion on a file that changed since is stale.
	a3, _ := s.Ask(ctx, ask("Add cn-038 to the rack-6z policy"))
	f.files[cid+"\x00"+bgpPath] += "# hand edit\n"
	if _, err := s.Accept(ctx, a3.Suggestions[0].ID); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	r, err := s.Reject(ctx, a3.Suggestions[0].ID)
	if err != nil || r.Status != advisor.StatusRejected {
		t.Fatalf("reject %v %+v", err, r)
	}
	// Suggestions survive a restart.
	s2, err := New(Options{Tools: f, Path: s.o.Path})
	if err != nil {
		t.Fatal(err)
	}
	list, _ = s2.Suggestions(ctx, cid)
	if len(list) != 2 {
		t.Fatalf("reload %d", len(list))
	}
	if a4, _ := s.Ask(ctx, Request{Messages: []Message{{Role: "user", Content: "Add cn-037 to the rack-6z BGP policy"}}}); !strings.HasPrefix(a4.Text, "Open a change first") {
		t.Errorf("no change: %q", a4.Text)
	}
}

func TestMockRetrieval(t *testing.T) {
	s, _, _ := newSvc(t, newFake(t), llm.NewMock(nil))
	a, _ := s.Ask(ctxAs("dennis"), ask("How do I add a node?"))
	if !strings.Contains(a.Text, "The KB article **Add a node** says: Rack, cable, inventory") || a.Sources[0].Kind != "kb" || !a.Mock || !a.Fallback {
		t.Fatalf("%+v", a)
	}
	if _, err := s.Ask(ctxAs("dennis"), Request{}); !errors.Is(err, ErrNoQuestion) {
		t.Fatalf("empty: %v", err)
	}
}

// script is a model that calls tools first and then answers.
type script struct {
	steps []llm.Message
	got   []llm.Request
}

func (s *script) Model() string { return "qwen3" }
func (s *script) Mock() bool    { return false }
func (s *script) Embed(context.Context, []string) ([][]float32, error) {
	return nil, llm.ErrNoEmbeddings
}
func (s *script) Ping(context.Context) error { return nil }
func (s *script) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	s.got = append(s.got, req)
	if len(s.steps) == 0 {
		return nil, errors.New("script ended")
	}
	m := s.steps[0]
	s.steps = s.steps[1:]
	return &llm.Response{Model: "qwen3", Message: m}, nil
}

func call(id, name string, args any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: name, Arguments: b}
}

func TestToolLoop(t *testing.T) {
	f := newFake(t)
	m := &script{steps: []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("1", "history", map[string]string{"entity": "cn-003"})}},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			call("2", "read_file", map[string]string{"path": bgpPath}),
			call("3", "suggest_edit", map[string]any{"path": bgpPath, "title": "peer cn-040", "edits": []map[string]string{{"old": "          - cn-034\n", "new": "          - cn-034\n          - cn-040\n"}}}),
		}},
		{Role: llm.RoleAssistant, Content: "c-2hd7vw moved it. I suggest one line."},
	}}
	s, log, _ := newSvc(t, f, m)
	a, err := s.Ask(ctxAs("dennis"), ask("Why cn-003, and peer cn-040"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "c-2hd7vw moved it. I suggest one line." || a.Model != "qwen3" || a.Mock || a.Fallback {
		t.Fatalf("%+v", a)
	}
	if len(a.Suggestions) != 1 || !strings.Contains(a.Suggestions[0].After, "- cn-040") || a.Suggestions[0].Title != "peer cn-040" {
		t.Fatalf("suggestions %+v", a.Suggestions)
	}
	labels := map[string]bool{}
	for _, src := range a.Sources {
		labels[src.Label] = true
	}
	if !labels["trailer c-2hd7vw"] || !labels["bgp-rack-6z.yaml"] {
		t.Errorf("sources %+v", a.Sources)
	}
	last := m.got[len(m.got)-1]
	var toolMsgs int
	for _, msg := range last.Messages {
		if msg.Role == llm.RoleTool {
			toolMsgs++
			if !strings.HasPrefix(msg.Content, "<data>") {
				t.Errorf("tool result not in a data block: %s", msg.Content)
			}
		}
	}
	if toolMsgs != 3 || len(last.Tools) != 9 {
		t.Errorf("tool msgs %d tools %d", toolMsgs, len(last.Tools))
	}
	e, _ := log.Get(context.Background(), a.LogID)
	if !strings.Contains(e.Prompt, "suggest_edit") || e.Model != "qwen3" {
		t.Errorf("log %+v", e)
	}

	// A model error falls back to the data answer.
	m.steps = nil
	a, err = s.Ask(ctxAs("dennis"), ask("Why does cn-003 use port 37 on data-a?"))
	if err != nil || !strings.HasPrefix(a.Text, "c-2hd7vw moved cn-003 p2") || !a.Fallback {
		t.Fatalf("fallback %v %v %q", err, a.Fallback, a.Text)
	}
	e, _ = log.Get(context.Background(), a.LogID)
	if e.Error == "" {
		t.Error("error not logged")
	}
}
