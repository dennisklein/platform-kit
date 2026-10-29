package advisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/llm"
)

// slowModel answers only when the context ends.
type slowModel struct{ scripted }

func (s *slowModel) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRisksDeadlineFallsBack(t *testing.T) {
	e, log, _ := newTestEngine(t, newFakeTools(), &slowModel{})
	e.o.Deadline = 50 * time.Millisecond
	start := time.Now()
	res, err := e.Run(testCtx(), Risks, demoID)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the deadline did not stop the model call")
	}
	if !res.Fallback || len(res.Points) < 3 || res.FallbackReason != "the model took longer than 0 s" {
		t.Fatalf("%+v", res)
	}
	entry, _ := log.Get(context.Background(), res.LogID)
	if !strings.HasPrefix(entry.Error, "model too slow") {
		t.Errorf("log error %q", entry.Error)
	}

	e.o.Deadline = 12 * time.Second
	if got := e.fallbackReason(errTooSlow); got != "the model took longer than 12 s" {
		t.Errorf("reason %q", got)
	}
	if got := e.fallbackReason(errUnusable); got != "the model gave no usable answer" {
		t.Errorf("reason %q", got)
	}
	if got := e.fallbackReason(errors.New("connection refused")); got != "the model did not answer" {
		t.Errorf("reason %q", got)
	}
}

func TestRisksCancelIsNoFallback(t *testing.T) {
	e, _, _ := newTestEngine(t, newFakeTools(), &slowModel{})
	e.o.Deadline = time.Minute
	ctx, stop := context.WithCancel(testCtx())
	time.AfterFunc(30*time.Millisecond, stop)
	if _, err := e.Run(ctx, Risks, demoID); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}

func TestUnusableAnswerReason(t *testing.T) {
	e, _, _ := newTestEngine(t, newFakeTools(), &scripted{answer: "I think it is fine."})
	res, err := e.Run(testCtx(), Risks, demoID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fallback || res.FallbackReason != "the model gave no usable answer" {
		t.Fatalf("%+v", res)
	}
}

func TestDefaultDeadline(t *testing.T) {
	if e := New(Options{}); e.o.Deadline != 12*time.Second {
		t.Errorf("deadline %v", e.o.Deadline)
	}
}
