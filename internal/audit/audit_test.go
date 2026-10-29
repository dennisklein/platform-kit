package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLog(t *testing.T) {
	ctx := context.Background()
	l := NewFileLog(filepath.Join(t.TempDir(), "audit", "audit.jsonl"))
	if got, err := l.List(ctx, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
	t0 := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	_ = l.Append(ctx, Entry{Time: t0, Actor: "dennis", Action: AgentApprove, Target: "dennis-agent"})
	_ = l.Append(ctx, Entry{Time: t0.Add(time.Minute), Actor: "dennis", Action: LLMDelete, Target: "l-1"})
	got, err := l.List(ctx, 10)
	if err != nil || len(got) != 2 || got[0].Action != LLMDelete || got[0].ID == "" {
		t.Fatalf("list: %+v %v", got, err)
	}
}
