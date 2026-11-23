package chat

import (
	"testing"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/llm"
)

func TestSuggestionMovesToTheSurfaceThatSuggestsItNow(t *testing.T) {
	s, _, ev := newSvc(t, newFake(t), llm.NewMock(nil))
	ctx := ctxAs("dennis")
	edit := advisor.Suggestion{ChangeID: cid, Path: "a.yaml", Before: "a: 1\n", After: "a: 2\n", Title: "set a", By: "advisor:risks", LogID: "l1"}
	first, err := s.AddSuggestion(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	// The same surface again: the same suggestion, unchanged.
	again, _ := s.AddSuggestion(ctx, edit)
	if again.ID != first.ID || again.By != "advisor:risks" {
		t.Fatalf("same surface %+v", again)
	}
	// Chat suggests the same edit: one suggestion, credited to chat.
	edit.By, edit.LogID = "chat", "l2"
	n := len(ev.got)
	moved, err := s.AddSuggestion(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != first.ID || moved.By != "chat" || moved.LogID != "l2" {
		t.Fatalf("moved %+v", moved)
	}
	if len(ev.got) != n+1 || ev.got[n] != events.SuggestionCreated {
		t.Errorf("events %v", ev.got)
	}
	list, _ := s.Suggestions(ctx, cid)
	if len(list) != 1 || list[0].By != "chat" {
		t.Errorf("list %+v", list)
	}
}
