package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

const seed = "../../../seed/state"

// TestSeedIsClean: the seed state has no errors (warnings and infos allowed).
func TestSeedIsClean(t *testing.T) {
	r, err := repo.Load(seed, all.Registry())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ByKind("Node")) != 15 {
		t.Errorf("nodes: %d", len(r.ByKind("Node")))
	}
	for _, p := range r.Problems() {
		if p.Severity == "error" {
			t.Errorf("%s:%d: %s (%s)", p.Path, p.Line, p.Message, p.Source)
		} else {
			t.Logf("%s %s:%d: %s", p.Severity, p.Path, p.Line, p.Message)
		}
	}
}

func TestLoadSkipsGitFileAndExportMarker(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /x/.git/worktrees/c\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".pk-export"), []byte("abc\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("a: 1\n"), 0o644)
	r, err := repo.Load(dir, all.Registry())
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Paths(); len(got) != 1 || got[0] != "a.yaml" {
		t.Fatalf("paths %v", got)
	}
}
