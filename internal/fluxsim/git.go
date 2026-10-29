package fluxsim

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/gitx"
)

// GitSource is a bare clone of the state repo. It fetches main and the
// ring tags, and exports trees per commit.
type GitSource struct {
	URL   string
	User  string
	Token string
	Dir   string
	// KeepTrees is how many exported trees stay on disk.
	KeepTrees int
	// Creds, when set, is read before each fetch: pk forge-init --reset
	// writes new tokens (and maybe a new URL) into the credentials file.
	Creds func() (url, token string, err error)

	mu   sync.Mutex
	init bool
}

func (g *GitSource) repo() *gitx.Repo { return gitx.Open(filepath.Join(g.Dir, "repo.git")) }

func (g *GitSource) ensure(ctx context.Context) error {
	if g.Creds != nil {
		if url, tok, err := g.Creds(); err == nil && url != "" {
			if url != g.URL {
				g.URL, g.init = url, false
			}
			g.Token = tok
		}
	}
	if g.init {
		return nil
	}
	dir := filepath.Join(g.Dir, "repo.git")
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if _, err := gitx.Init(ctx, dir, true); err != nil {
			return err
		}
	}
	r := g.repo()
	if _, err := r.Run(ctx, "remote", "get-url", "origin"); err != nil {
		if _, err := r.Run(ctx, "remote", "add", "origin", g.URL); err != nil {
			return err
		}
	} else if _, err := r.Run(ctx, "remote", "set-url", "origin", g.URL); err != nil {
		return err
	}
	g.init = true
	return nil
}

func (g *GitSource) Fetch(ctx context.Context) (Heads, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensure(ctx); err != nil {
		return Heads{}, err
	}
	r := g.repo()
	if err := r.FetchAuth(ctx, g.User, g.Token, "origin", "+refs/heads/main:refs/heads/main", "+refs/tags/ring/*:refs/tags/ring/*"); err != nil {
		return Heads{}, err
	}
	main, err := r.RevParse(ctx, "refs/heads/main")
	if err != nil {
		return Heads{}, err
	}
	h := Heads{Main: main, Rings: map[string]string{}}
	out, err := r.Run(ctx, "for-each-ref", "--format=%(refname:strip=3) %(objectname) %(*objectname)", "refs/tags/ring/")
	if err != nil {
		return Heads{}, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		sha := f[1]
		if len(f) > 2 && f[2] != "" {
			sha = f[2] // annotated tag: the commit
		}
		h.Rings[f[0]] = sha
	}
	return h, nil
}

func (g *GitSource) Tree(ctx context.Context, sha string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	trees := filepath.Join(g.Dir, "trees")
	dir := filepath.Join(trees, sha)
	if _, err := os.Stat(filepath.Join(dir, ".pk-export")); err == nil {
		now := time.Now()
		_ = os.Chtimes(dir, now, now)
		return dir, nil
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := g.repo().Export(ctx, sha, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".pk-export"), []byte(sha+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	g.prune(trees)
	return dir, nil
}

// prune keeps the newest KeepTrees exports.
func (g *GitSource) prune(trees string) {
	keep := g.KeepTrees
	if keep <= 0 {
		keep = 12
	}
	ents, err := os.ReadDir(trees)
	if err != nil || len(ents) <= keep {
		return
	}
	type ent struct {
		name string
		mod  int64
	}
	var es []ent
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			es = append(es, ent{e.Name(), fi.ModTime().UnixNano()})
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].mod > es[j].mod })
	for _, e := range es[min(keep, len(es)):] {
		_ = os.RemoveAll(filepath.Join(trees, e.name))
	}
}

func (g *GitSource) Change(ctx context.Context, sha string) string {
	out, err := g.repo().Run(ctx, "log", "-1", "--format=%(trailers:key=Pk-Change,valueonly,separator=%x2C)", sha)
	if err != nil {
		return ""
	}
	v, _, _ := strings.Cut(strings.TrimSpace(out), ",")
	return strings.TrimSpace(v)
}
