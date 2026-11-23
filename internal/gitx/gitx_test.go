package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var dennis = Signature{Name: "Dennis Klein", Email: "dennis@pk.local"}

func write(t *testing.T, dir, path, content string) {
	t.Helper()
	p := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommitLogTrailersWorktreeRebase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	r, err := Init(ctx, filepath.Join(root, "repo"), false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, r.Dir, "a.yaml", "a: 1\n")
	if err := r.Add(ctx); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	base, err := r.Commit(ctx, CommitOptions{Message: "initial", Author: Signature{Name: dennis.Name, Email: dennis.Email, When: when}})
	if err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(root, "wt")
	if err := r.WorktreeAdd(ctx, wt, "change/c-abcdef-x", "main", true); err != nil {
		t.Fatal(err)
	}
	w := Open(wt)
	write(t, wt, "b.yaml", "b: 1\n")
	st, err := w.Status(ctx)
	if err != nil || len(st) != 1 || st[0].Code != "?" || st[0].Path != "b.yaml" {
		t.Fatalf("status: %+v %v", st, err)
	}
	_ = w.Add(ctx)
	_, err = w.Commit(ctx, CommitOptions{
		Message:  "feat: add b",
		Author:   Signature{Name: "dennis-agent", Email: "dennis-agent@pk.local"},
		Trailers: []Trailer{{"Pk-Agent", "dennis-agent"}, {"Pk-On-Behalf-Of", "dennis"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := w.Log(ctx, "main..HEAD")
	if err != nil || len(log) != 1 {
		t.Fatalf("log: %+v %v", log, err)
	}
	if got := log[0].Trailers; len(got) != 2 || got[1].Value != "dennis" {
		t.Fatalf("trailers: %+v", got)
	}

	// main moves on: ahead 1, behind 1, then rebase.
	write(t, r.Dir, "a.yaml", "a: 2\n")
	_ = r.Add(ctx)
	if _, err := r.Commit(ctx, CommitOptions{Message: "main moved", Author: dennis}); err != nil {
		t.Fatal(err)
	}
	ahead, behind, err := w.AheadBehind(ctx, "main", "HEAD")
	if err != nil || ahead != 1 || behind != 1 {
		t.Fatalf("ahead/behind: %d %d %v", ahead, behind, err)
	}
	if err := w.Rebase(ctx, "main", dennis); err != nil {
		t.Fatal(err)
	}
	if ok, _ := w.IsAncestor(ctx, "main", "HEAD"); !ok {
		t.Fatal("main is not an ancestor after rebase")
	}

	// Conflict: both change a.yaml.
	write(t, wt, "a.yaml", "a: 3\n")
	_ = w.Add(ctx)
	_, _ = w.Commit(ctx, CommitOptions{Message: "a3", Author: dennis})
	write(t, r.Dir, "a.yaml", "a: 4\n")
	_ = r.Add(ctx)
	_, _ = r.Commit(ctx, CommitOptions{Message: "a4", Author: dennis})
	err = w.Rebase(ctx, "main", dennis)
	var ce *ErrConflict
	if !errors.As(err, &ce) || len(ce.Paths) != 1 || ce.Paths[0] != "a.yaml" {
		t.Fatalf("want conflict on a.yaml, got %v", err)
	}
	if b, _ := w.CurrentBranch(ctx); b != "change/c-abcdef-x" {
		t.Fatalf("rebase not aborted, branch %q", b)
	}

	ents, err := r.LsTree(ctx, base, "")
	if err != nil || len(ents) != 1 || ents[0].Path != "a.yaml" {
		t.Fatalf("ls-tree: %+v %v", ents, err)
	}
	if b, err := r.ShowFile(ctx, base, "a.yaml"); err != nil || string(b) != "a: 1\n" {
		t.Fatalf("show: %q %v", b, err)
	}
	if _, err := r.ShowFile(ctx, base, "nope.yaml"); !IsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
	wts, err := r.WorktreeList(ctx)
	if err != nil || len(wts) != 2 || wts[1].Branch != "change/c-abcdef-x" {
		t.Fatalf("worktrees: %+v %v", wts, err)
	}
	exp := filepath.Join(root, "export")
	if err := r.Export(ctx, base, exp); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(exp, "a.yaml")); string(b) != "a: 1\n" {
		t.Fatalf("export: %q", b)
	}
}

func TestParseTrailers(t *testing.T) {
	msg := "Merge change c-k7m2qz: Add cn-035 (!12)\n\n2 compute nodes\n\nPk-Change: c-k7m2qz\nApproved-by: anna, jonas\n"
	ts := ParseTrailers(msg)
	if len(ts) != 2 || ts[0].Key != "Pk-Change" || ts[1].Value != "anna, jonas" {
		t.Fatalf("got %+v", ts)
	}
	if ParseTrailers("subject only") != nil {
		t.Fatal("subject only has no trailers")
	}
	if ParseTrailers("s\n\nnot a trailer line\nPk-Change: x") != nil {
		t.Fatal("mixed paragraph is not trailers")
	}
}

func TestRedact(t *testing.T) {
	got := redact("fatal: unable to access 'http://dennis:s3cret@gitea:3000/platform/state.git/'")
	if want := "fatal: unable to access 'http://<redacted>@gitea:3000/platform/state.git/'"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestImmutableArgs(t *testing.T) {
	a := "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	b := "e08763f23a03b7572c72cd49e904abcf3a5a7687"
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"rev-parse", "--verify", "--quiet", a + "^{commit}"}, true},
		{[]string{"rev-parse", a + "^1^{commit}"}, true},
		{[]string{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"}, false},
		{[]string{"rev-parse", "refs/remotes/origin/main^{commit}"}, false},
		{[]string{"show", a + ":.pk/changes/c-abc234.yaml"}, true},
		{[]string{"show", "main:x"}, false},
		{[]string{"diff", "--name-status", "-z", "--find-renames", a, b}, true},
		{[]string{"diff", "--name-status", "-z", "--find-renames", a}, false}, // work tree
		{[]string{"diff", "--no-color", a, "--", "x"}, false},
		{[]string{"diff", "--cached", a, b}, false},
		{[]string{"log", "--format=%H", a + ".." + b, "--no-merges"}, true},
		{[]string{"log", "--format=%H", a, "-n", "50"}, true},
		{[]string{"log", "--format=%H", a, "--grep", "x"}, false},
		{[]string{"log", "--format=%H", "refs/remotes/origin/main", "-n", "50"}, false},
		{[]string{"merge-base", a, b}, true},
		{[]string{"status", "--porcelain"}, false},
		{[]string{"commit", "-m", a}, false},
	} {
		if got := immutableArgs(tc.args); got != tc.want {
			t.Errorf("immutableArgs(%q) = %v", tc.args, got)
		}
	}
}
