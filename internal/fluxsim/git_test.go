package fluxsim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
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

// origin makes a bare repo with one commit on main and returns its path
// and the commit.
func origin(t *testing.T, root, name, content string) (string, string) {
	bare := filepath.Join(root, name+".git")
	work := filepath.Join(root, name)
	runGit(t, root, "init", "-q", "--bare", bare)
	runGit(t, root, "init", "-q", work)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "init")
	runGit(t, work, "push", "-q", bare, "HEAD:refs/heads/main")
	runGit(t, work, "push", "-q", bare, "HEAD:refs/tags/ring/canary")
	return bare, runGit(t, work, "rev-parse", "HEAD")
}

// After pk forge-init --reset the credentials file names new tokens or a
// new repo: the source reads it before each fetch, and stale ring tags go.
func TestGitSourceReloadsCredentials(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, shaA := origin(t, root, "a", "a\n")
	b, shaB := origin(t, root, "b", "b\n")
	runGit(t, root, "--git-dir="+b, "tag", "-d", "ring/canary")

	url := a
	src := &GitSource{Dir: filepath.Join(root, "flux"), Creds: func() (string, string, error) { return url, "", nil }}
	h, err := src.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Main != shaA || h.Rings["canary"] != shaA {
		t.Fatalf("heads a = %+v", h)
	}
	url = b
	if h, err = src.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if h.Main != shaB || len(h.Rings) != 0 {
		t.Fatalf("heads b = %+v", h)
	}
}
