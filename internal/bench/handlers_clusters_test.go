package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/gitx"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@pk.local",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, dir, path, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A revert undoes configuration but keeps the records of the change: its
// lab report, work item edits and change metadata.
func TestKeepRecords(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	writeT(t, dir, "control/cabling.yaml", "uplinks: []\n")
	writeT(t, dir, "work/items/w-108.yaml", "id: w-108\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "base")
	writeT(t, dir, "control/cabling.yaml", "uplinks: [6z]\n")
	writeT(t, dir, "kb/labs/c-31d0aa-run-1.md", "# report\n")
	writeT(t, dir, "work/items/w-108.yaml", "id: w-108\nchanges: [c-31d0aa]\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "Merge change c-31d0aa")
	gitT(t, dir, "revert", "--no-commit", "HEAD")

	n, err := keepRecords(ctx, gitx.Open(dir), dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("paths still reverted = %d", n)
	}
	if got := gitT(t, dir, "diff", "--cached", "--name-only", "HEAD"); got != "control/cabling.yaml" {
		t.Fatalf("staged = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "kb/labs/c-31d0aa-run-1.md")); err != nil {
		t.Fatalf("lab report removed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "work/items/w-108.yaml")); !strings.Contains(string(b), "c-31d0aa") {
		t.Fatalf("work item reverted: %s", b)
	}
}
