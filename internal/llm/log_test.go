package llm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "llm", "log.jsonl")
	l := NewFileLog(path)
	id1, err := l.Append(ctx, LogEntry{User: "dennis", Surface: SurfaceAdvisor, Recipe: "risks", ChangeID: "c-k7m2qz", Model: "mock", Prompt: "risks of c-k7m2qz", Answer: "BGP order"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := l.Append(ctx, LogEntry{User: "dennis", Surface: SurfaceChat, Model: "qwen", Prompt: "Why does cn-003 use port 37?", Answer: "c-2hd7vw moved it"})
	if id1 == id2 || !strings.HasPrefix(id1, "l-") {
		t.Fatalf("ids %s %s", id1, id2)
	}

	list, total, _ := l.List(ctx, "", 0)
	if total != 2 || list[0].ID != id2 {
		t.Fatalf("newest first: %+v", list)
	}
	list, total, _ = l.List(ctx, "cn-003 PORT", 10)
	if total != 1 || list[0].ID != id2 {
		t.Fatalf("search: %d %+v", total, list)
	}
	if _, total, _ = l.List(ctx, "surface:advisor", 10); total != 1 {
		t.Fatalf("field filter: %d", total)
	}

	if _, err := l.Rate(ctx, id1, "sideways"); err == nil {
		t.Fatal("bad rating accepted")
	}
	e, err := l.Rate(ctx, id1, "up")
	if err != nil || e.Rating != "up" {
		t.Fatalf("rate: %v %+v", err, e)
	}

	// A new instance reads the file back (ratings survive).
	l2 := NewFileLog(path)
	got, err := l2.Get(ctx, id1)
	if err != nil || got.Rating != "up" || got.Recipe != "risks" {
		t.Fatalf("reload: %v %+v", err, got)
	}
	if err := l2.Delete(ctx, id1); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.Get(ctx, id1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted entry still there: %v", err)
	}
	if err := l2.Delete(ctx, "l-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
	var buf bytes.Buffer
	if err := l2.Export(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "\n") != 1 || !strings.Contains(buf.String(), id2) {
		t.Fatalf("export: %s", buf.String())
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), id1) {
		t.Fatal("deleted entry still in the file")
	}
}

func TestFileLogSkipsTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	os.WriteFile(path, []byte(`{"id":"l-a","user":"x","surface":"chat","model":"m","prompt":"p","answer":"a"}`+"\n"+`{"id":"l-b","us`), 0o600)
	l := NewFileLog(path)
	list, total, err := l.List(context.Background(), "", 0)
	if err != nil || total != 1 || list[0].ID != "l-a" {
		t.Fatalf("%v %d %+v", err, total, list)
	}
}

func TestFileLogAppendAfterTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	os.WriteFile(path, []byte(`{"id":"l-a","user":"x","surface":"chat","model":"m","prompt":"p","answer":"a"}`+"\n"+`{"id":"l-b","us`), 0o600)
	ctx := context.Background()
	if _, err := NewFileLog(path).Append(ctx, LogEntry{ID: "l-c", User: "y"}); err != nil {
		t.Fatal(err)
	}
	_, total, _ := NewFileLog(path).List(ctx, "", 0)
	if total != 2 {
		t.Fatalf("total %d", total)
	}
}
