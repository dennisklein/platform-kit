package chat

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
)

// Pace of an answer from data (built-in, or the fallback) while someone
// watches: each step shows at least stepMin, and the text comes in small
// chunks over at most typeMax, so the whole reveal stays well under 2 s.
const (
	stepMin   = 200 * time.Millisecond
	stepQuick = 90 * time.Millisecond
	typeMax   = 900 * time.Millisecond
	chunkMin  = 14 * time.Millisecond
	chunkMax  = 40 * time.Millisecond
	flushGap  = 40 * time.Millisecond
	chunkSize = 3 // words per chunk
)

// StepEvent is the data of a chat.step event (API schema ChatStepEvent).
type StepEvent struct {
	RequestID string       `json:"requestId"`
	Step      api.ChatStep `json:"step"`
}

// DeltaEvent is the data of a chat.delta event (API schema ChatDeltaEvent).
type DeltaEvent struct {
	RequestID string `json:"requestId"`
	Text      string `json:"text"`
	Reset     bool   `json:"reset,omitempty"`
}

// progress records the steps of one answer and, when a client watches
// (request id), publishes them and the text as it comes to the person who
// asked.
type progress struct {
	pub   events.Publisher
	to    string
	id    string
	pace  bool
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)

	mu       sync.Mutex
	steps    []Step
	starts   []time.Time
	pending  strings.Builder
	flushed  time.Time
	streamed bool // text went out since the last reset
}

func (s *Svc) newProgress(ctx context.Context, requestID string) *progress {
	p := &progress{pub: s.o.Events, id: requestID, now: s.now, sleep: s.sleep}
	if id, ok := identity.From(ctx); ok {
		p.to = id.Principal()
	}
	return p
}

// live reports a client that watches this answer.
func (p *progress) live() bool { return p.id != "" && p.pub != nil && p.to != "" }

func (p *progress) publish(t events.Type, data any) {
	if p.live() {
		p.pub.PublishTo(p.to, t, data)
	}
}

func stepAPI(s Step) api.ChatStep {
	return api.ChatStep{Id: s.ID, Tool: s.Tool, Label: s.Label, Summary: s.Summary, State: api.ChatStepState(s.State), Ms: s.Ms}
}

// StepsToAPI converts the steps of an answer.
func StepsToAPI(steps []Step) []api.ChatStep {
	out := make([]api.ChatStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, stepAPI(s))
	}
	return out
}

// begin starts a step and returns its number.
func (p *progress) begin(tool, label string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
	st := Step{ID: len(p.steps) + 1, Tool: tool, Label: label, State: "running"}
	p.steps = append(p.steps, st)
	p.starts = append(p.starts, p.now())
	p.publish(events.ChatStep, StepEvent{RequestID: p.id, Step: stepAPI(st)})
	return st.ID
}

// end finishes a step. A paced answer keeps the running state visible for
// stepMin first.
func (p *progress) end(ctx context.Context, n int, summary string, failed bool) {
	p.mu.Lock()
	start := p.starts[n-1]
	p.mu.Unlock()
	if p.pace && p.live() {
		if d := stepMin - p.now().Sub(start); d > 0 {
			p.sleep(ctx, d)
		}
	}
	p.finish(n, summary, failed)
}

// quick records a step that is part of a larger lookup that already ran
// (for example the files an advisor recipe read).
func (p *progress) quick(ctx context.Context, tool, label, summary string) {
	n := p.begin(tool, label)
	if p.pace && p.live() {
		p.sleep(ctx, stepQuick)
	}
	p.finish(n, summary, false)
}

func (p *progress) finish(n int, summary string, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	start := p.starts[n-1]
	st := &p.steps[n-1]
	st.Summary, st.State = summary, "ok"
	if failed {
		st.State = "failed"
	}
	st.Ms = max(p.now().Sub(start).Milliseconds(), 0)
	p.publish(events.ChatStep, StepEvent{RequestID: p.id, Step: stepAPI(*st)})
}

// do runs fn as one step; fn returns the summary.
func (p *progress) do(ctx context.Context, tool, label string, fn func() (string, error)) error {
	n := p.begin(tool, label)
	sum, err := fn()
	if err != nil && sum == "" {
		sum = shortErr(err)
	}
	p.end(ctx, n, sum, err != nil)
	return err
}

// text adds streamed answer text; it goes out in small batches.
func (p *progress) text(s string) {
	if s == "" || !p.live() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending.WriteString(s)
	if p.now().Sub(p.flushed) >= flushGap {
		p.flushLocked()
	}
}

func (p *progress) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
}

func (p *progress) flushLocked() {
	if p.pending.Len() == 0 {
		return
	}
	p.publish(events.ChatDelta, DeltaEvent{RequestID: p.id, Text: p.pending.String()})
	p.pending.Reset()
	p.flushed = p.now()
	p.streamed = true
}

// reset drops the text so far (the model called tools after all, or the
// data answers instead).
func (p *progress) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.streamed && p.pending.Len() == 0 {
		return
	}
	p.pending.Reset()
	p.streamed = false
	p.publish(events.ChatDelta, DeltaEvent{RequestID: p.id, Text: "", Reset: true})
}

// typeOut sends a finished text: in small chunks when paced, else at once.
func (p *progress) typeOut(ctx context.Context, text string) {
	if !p.live() || text == "" {
		return
	}
	p.reset()
	if !p.pace {
		p.text(text)
		p.flush()
		return
	}
	chunks := chunkWords(text, chunkSize)
	delay := min(max(typeMax/time.Duration(len(chunks)), chunkMin), chunkMax)
	for i, c := range chunks {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		p.pending.WriteString(c)
		p.flushLocked()
		p.mu.Unlock()
		if i < len(chunks)-1 {
			p.sleep(ctx, delay)
		}
	}
}

// chunkWords splits text into pieces of n words, each with the space that
// follows it, so the pieces join to the text.
func chunkWords(text string, n int) []string {
	var out []string
	start, words := 0, 0
	inWord := false
	for i, r := range text {
		if unicode.IsSpace(r) {
			if inWord {
				words++
				inWord = false
			}
			continue
		}
		if !inWord && words >= n {
			out = append(out, text[start:i])
			start, words = i, 0
		}
		inWord = true
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// snapshot returns the steps so far.
func (p *progress) snapshot() []Step {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Step(nil), p.steps...)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && len(s)-i > 8 {
		s = s[i+2:]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
