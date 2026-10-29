package chat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// feed records the published events with their data.
type feed struct {
	mu  sync.Mutex
	to  []string
	evs []any
}

func (f *feed) Publish(t events.Type, d any) { f.PublishTo("", t, d) }
func (f *feed) PublishTo(to string, _ events.Type, d any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.to = append(f.to, to)
	f.evs = append(f.evs, d)
}

func (f *feed) text(id string) (string, []StepEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	var steps []StepEvent
	for _, e := range f.evs {
		switch d := e.(type) {
		case DeltaEvent:
			if d.RequestID != id {
				continue
			}
			if d.Reset {
				b.Reset()
			}
			b.WriteString(d.Text)
		case StepEvent:
			if d.RequestID == id {
				steps = append(steps, d)
			}
		}
	}
	return b.String(), steps
}

func liveSvc(t *testing.T, client llm.Client) (*Svc, *feed) {
	f := &feed{}
	s, err := New(Options{Tools: newFake(t), LLM: client, Events: f})
	if err != nil {
		t.Fatal(err)
	}
	s.sleep = func(context.Context, time.Duration) {}
	return s, f
}

func TestLiveBuiltInAnswer(t *testing.T) {
	s, f := liveSvc(t, llm.NewMock(nil))
	req := ask("Why does cn-003 use port 37 on data-a?")
	req.RequestID = "r1"
	a, err := s.Ask(ctxAs("dennis"), req)
	if err != nil {
		t.Fatal(err)
	}
	text, steps := f.text("r1")
	if text != a.Text {
		t.Errorf("streamed %q, answer %q", text, a.Text)
	}
	// Each step goes out running, then done.
	if len(a.Steps) != 2 || len(steps) != 4 || steps[0].Step.State != "running" || steps[1].Step.State != "ok" {
		t.Fatalf("steps %+v events %+v", a.Steps, steps)
	}
	if a.Steps[0].Label != "history cn-003" || a.Steps[0].Summary != "2 merges" {
		t.Errorf("step %+v", a.Steps[0])
	}
	for _, to := range f.to {
		if to != "dennis" {
			t.Errorf("event for %q, want only the asker", to)
		}
	}
	// Without a request id, nothing is published.
	f.evs = nil
	if _, err := s.Ask(ctxAs("dennis"), ask("Why does cn-003 use port 37 on data-a?")); err != nil || len(f.evs) != 0 {
		t.Errorf("events without request id: %d (%v)", len(f.evs), err)
	}
}

// streamer is a model that streams its answer and can be slow.
type streamer struct {
	script
	block bool
}

func (m *streamer) CompleteStream(ctx context.Context, req llm.Request, onText func(string)) (*llm.Response, error) {
	if m.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	resp, err := m.Complete(ctx, req)
	if err == nil && len(resp.Message.ToolCalls) == 0 {
		for _, w := range strings.SplitAfter(resp.Message.Content, " ") {
			onText(w)
		}
	}
	return resp, err
}

func TestLiveModelStreamsAndShowsTools(t *testing.T) {
	m := &streamer{script: script{steps: []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("1", "history", map[string]string{"entity": "cn-003"})}},
		{Role: llm.RoleAssistant, Content: "c-2hd7vw moved it to data-a 37."},
	}}}
	s, f := liveSvc(t, m)
	req := ask("Why cn-003?")
	req.RequestID = "r2"
	a, err := s.Ask(ctxAs("dennis"), req)
	if err != nil {
		t.Fatal(err)
	}
	text, steps := f.text("r2")
	if text != "c-2hd7vw moved it to data-a 37." || a.Text != text || a.Fallback {
		t.Errorf("streamed %q answer %+v", text, a)
	}
	if len(a.Steps) != 1 || a.Steps[0].Label != "history cn-003" || a.Steps[0].Summary != "2 merges" || len(steps) != 2 {
		t.Errorf("steps %+v", a.Steps)
	}
}

func TestDeadlineAnswersFromData(t *testing.T) {
	m := &streamer{block: true}
	f := &feed{}
	s, err := New(Options{Tools: newFake(t), LLM: m, Events: f, Deadline: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.sleep = func(context.Context, time.Duration) {}
	req := ask("Why does cn-003 use port 37 on data-a?")
	req.RequestID = "r3"
	a, err := s.Ask(ctxAs("dennis"), req)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Fallback || a.FallbackReason != "the model took longer than 0 s" && !strings.Contains(a.FallbackReason, "took longer") {
		t.Errorf("fallback %v %q", a.Fallback, a.FallbackReason)
	}
	if !strings.HasPrefix(a.Text, "c-2hd7vw moved cn-003 p2") {
		t.Errorf("text %q", a.Text)
	}
	if a.Steps[0].Tool != "model" || a.Steps[0].State != "failed" {
		t.Errorf("first step %+v", a.Steps[0])
	}
	if text, _ := f.text("r3"); text != a.Text {
		t.Errorf("streamed %q", text)
	}
	// The asker stopped: no answer.
	ctx, cancel := context.WithCancel(ctxAs("dennis"))
	cancel()
	if _, err := s.Ask(ctx, req); err == nil {
		t.Error("answer after cancel")
	}
}

func TestChunkWords(t *testing.T) {
	text := "one two  three four\nfive six seven"
	cs := chunkWords(text, 3)
	if strings.Join(cs, "") != text || len(cs) != 3 {
		t.Fatalf("%q", cs)
	}
}

func TestRetrievalCleansKBArticles(t *testing.T) {
	f := newFake(t)
	f.hits = []llm.Hit{
		{Document: llm.Document{Kind: "kb", Ref: "kb/runbooks/add-node.md", Title: "Add a compute node to a cluster", Text: "---\ntitle: Add a compute node to a cluster\ntype: runbook\nentities: [GC5A-6Z]\n---\n# Add a compute node to a cluster\n\nA compute node is one file in `control/inventory/`. The bench renders it into\na `Machine` for its cluster.\n\n## Steps\n\n1. Open a change and link the work item:\n\n   ```\n   pk change new \"Add cn-035\"\n   ```\n\n2. Add the node from kind **Node**. Set the cluster.\n3. Run the change lab.\n"}},
		{Document: llm.Document{Kind: "trailer", Ref: "c-9kq1mf", Title: "trailer c-9kq1mf: Add rack"}},
	}
	s, _, _ := newSvc(t, f, llm.NewMock(nil))
	a, err := s.Ask(ctxAs("dennis"), ask("How do I add a compute node?"))
	if err != nil {
		t.Fatal(err)
	}
	want := "The runbook **Add a compute node to a cluster** has 3 steps:\n\n1. Open a change and link the work item\n2. Add the node from kind **Node**.\n3. Run the change lab.\n\nStep 1 starts with:\n\n```\npk change new \"Add cn-035\"\n```"
	if a.Text != want {
		t.Errorf("got\n%s\nwant\n%s", a.Text, want)
	}
	if strings.Contains(a.Text, "entities:") || len(a.Sources) != 2 {
		t.Errorf("front matter or sources: %+v", a)
	}
	a, _ = s.Ask(ctxAs("dennis"), ask("What is a compute node?"))
	if a.Text != "The runbook **Add a compute node to a cluster** covers this.\n\nA compute node is one file in `control/inventory/`. The bench renders it into a `Machine` for its cluster.\n\n- Add the node from kind **Node**. Set the cluster." {
		t.Errorf("passage %q", a.Text)
	}
}

func TestGreeting(t *testing.T) {
	s, _, _ := newSvc(t, newFake(t), llm.NewMock(nil))
	for _, q := range []string{"hello, who are you?", "Hi!", "What can you do?", "help"} {
		a, _ := s.Ask(ctxAs("dennis"), ask(q))
		if !strings.HasPrefix(a.Text, "Hi, I am the chat of platform-kit.") || !strings.Contains(a.Text, "What are the risks of "+cid+"?") {
			t.Errorf("%q: %q", q, a.Text)
		}
	}
}

func TestRecipeFor(t *testing.T) {
	for q, want := range map[string]string{
		"Summarize the risks of my change":                    "risks",
		"Is it safe to merge c-v6jhcj?":                       "risks",
		"In which order should we merge?":                     "merge-order",
		"What does the lab not test?":                         "lab-gaps",
		"Explain the rendered diff of c-6m1pqd":               "explain-diff",
		"What happened in the break-glass incident?":          "",
		"What are the risk rules in the policy?":              "",
		"What does c-p4x8ma change and why did its lab fail?": "",
	} {
		if got := recipeFor(q); got != want {
			t.Errorf("%q: got %q, want %q", q, got, want)
		}
	}
}

func TestSentencesAndClip(t *testing.T) {
	if got := firstSentences("One two. Three four. Five.", 2, 100); got != "One two. Three four." {
		t.Errorf("%q", got)
	}
	if got := clip("alpha beta gamma delta", 14); got != "alpha beta…" {
		t.Errorf("%q", got)
	}
	if got := portRanges([]int{1, 2, 3, 5, 7, 8}); got != "1–3, 5, 7–8" {
		t.Errorf("%q", got)
	}
}
