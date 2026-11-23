package rings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
)

// GitRepo is a bare clone of the state repo under Dir/repo.git. It fetches
// main and the ring tags as pk-ci and pushes ring tags as the acting user
// (tokens from the forge credentials file).
type GitRepo struct {
	Dir string
	// URL of the state repo; empty means from the credentials file.
	URL string
	// ForgeURL, Owner and Repo build the URL when the credentials file has
	// none (or ForgeURLSet overrides its URL).
	ForgeURL, Owner, Repo string
	ForgeURLSet           bool
	CredentialsPath       string
	// MinFetch is the shortest time between fetches (default 2s).
	MinFetch time.Duration

	mu      sync.Mutex
	init    bool
	fetched time.Time
}

func (g *GitRepo) repo() *gitx.Repo { return gitx.Open(filepath.Join(g.Dir, "repo.git")) }

func (g *GitRepo) creds() (*forge.Credentials, string, error) {
	cr, err := forge.LoadCredentials(g.CredentialsPath)
	if err != nil {
		if g.URL != "" {
			return &forge.Credentials{}, g.URL, nil
		}
		return nil, "", fmt.Errorf("forge credentials: %w", err)
	}
	url := g.URL
	if url == "" {
		base, owner, repo := g.ForgeURL, g.Owner, g.Repo
		if !g.ForgeURLSet && cr.URL != "" {
			base = cr.URL
		}
		if cr.Owner != "" {
			owner, repo = cr.Owner, cr.Repo
		}
		url = strings.TrimRight(base, "/") + "/" + owner + "/" + repo + ".git"
	}
	return cr, url, nil
}

func (g *GitRepo) ensureLocked(ctx context.Context) (*forge.Credentials, error) {
	cr, url, err := g.creds()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(g.Dir, "repo.git")
	if !g.init {
		if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
			if _, err := gitx.Init(ctx, dir, true); err != nil {
				return nil, err
			}
		}
	}
	r := g.repo()
	if _, err := r.Run(ctx, "remote", "get-url", "origin"); err != nil {
		if _, err := r.Run(ctx, "remote", "add", "origin", url); err != nil {
			return nil, err
		}
	} else if _, err := r.Run(ctx, "remote", "set-url", "origin", url); err != nil {
		return nil, err
	}
	g.init = true
	return cr, nil
}

// Refresh fetches main and the ring tags, at most every MinFetch (negative:
// always) unless a wanted commit is missing.
func (g *GitRepo) Refresh(ctx context.Context, want ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	min := g.MinFetch
	if min == 0 {
		min = 2 * time.Second
	}
	if g.init && time.Since(g.fetched) < min {
		missing := false
		for _, sha := range want {
			if sha != "" && !g.repo().Exists(ctx, sha+"^{commit}") {
				missing = true
			}
		}
		if !missing {
			return nil
		}
	}
	return g.fetchLocked(ctx)
}

func (g *GitRepo) fetchLocked(ctx context.Context) error {
	cr, err := g.ensureLocked(ctx)
	if err != nil {
		return err
	}
	tok, _ := cr.Token(forge.CIUser)
	if err := g.repo().FetchAuth(ctx, forge.CIUser, tok, "origin", "+refs/heads/main:refs/heads/main",
		"+refs/tags/ring/*:refs/tags/ring/*"); err != nil {
		return err
	}
	g.fetched = time.Now()
	return nil
}

func (g *GitRepo) Resolve(ctx context.Context, rev string) (string, error) {
	return g.repo().RevParse(ctx, rev+"^{commit}")
}

func (g *GitRepo) IsAncestor(ctx context.Context, a, b string) bool {
	if a == b {
		return true
	}
	ok, err := g.repo().IsAncestor(ctx, a, b)
	return err == nil && ok
}

func (g *GitRepo) Paths(ctx context.Context, from, to string) ([]string, error) {
	out, err := g.repo().Run(ctx, "diff", "--name-only", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

func (g *GitRepo) Change(ctx context.Context, sha string) string {
	out, err := g.repo().Run(ctx, "log", "-1", "--format=%(trailers:key=Pk-Change,valueonly,separator=%x2C)", sha)
	if err != nil {
		return ""
	}
	v, _, _ := strings.Cut(strings.TrimSpace(out), ",")
	return strings.TrimSpace(v)
}

// SetTag force-pushes a lightweight tag ring/<ring> at sha as user.
func (g *GitRepo) SetTag(ctx context.Context, user, ring, sha string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.ensureLocked(ctx)
	if err != nil {
		return err
	}
	tok, ok := cr.Token(user)
	if !ok && g.URL == "" {
		return fmt.Errorf("no forge token for %s", user)
	}
	ref := "refs/tags/ring/" + ring
	if err := g.repo().Push(ctx, gitx.PushOptions{User: user, Token: tok, Refspecs: []string{"+" + sha + ":" + ref}}); err != nil {
		return err
	}
	_, _ = g.repo().Run(ctx, "update-ref", ref, sha)
	return nil
}
