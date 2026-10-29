// Package ci is pk ci: it polls the forge for open pull requests and posts
// the commit status pk/policy on each head sha, with the same gate
// evaluation as the bench. Branch protection on main requires pk/policy.
package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/forge/gitea"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// Options of pk ci.
type Options struct {
	// CredentialsPath is the forge credentials file; ci acts as forge.CIUser.
	CredentialsPath string
	// DataDir holds the clone for renders.
	DataDir string
	// Interval between polls (default 3 s).
	Interval time.Duration
	// Once runs one pass and exits (tests, e2e).
	Once bool
	// Policy makes the policy engine for the main tree (required).
	Policy func(mainDir func(context.Context) (string, error)) policy.Engine
	// Renderer renders the impact for risk rules (optional).
	Renderer render.Renderer
	// Forge overrides the forge from the credentials (tests).
	Forge     forge.Forge
	RemoteURL string
	Log       *slog.Logger
}

// Checker evaluates and posts pk/policy.
type Checker struct {
	o     Options
	svc   *change.Svc
	forge forge.Forge
	log   *slog.Logger
	// posted remembers the last status per sha.
	posted map[string]string
	// gitea client and credentials file time, to reload new tokens
	// (pk forge-init --reset makes new ones).
	gitea    *gitea.Client
	credsMod time.Time
}

// New builds a checker. It waits for the credentials file when ctx allows.
func New(ctx context.Context, o Options) (*Checker, error) {
	if o.Log == nil {
		o.Log = slog.Default().With("component", "ci")
	}
	if o.Interval <= 0 {
		o.Interval = 3 * time.Second
	}
	if o.Policy == nil {
		return nil, errors.New("pk ci: no policy engine")
	}
	f, remote, token := o.Forge, o.RemoteURL, func(string) (string, bool) { return "", false }
	var g *gitea.Client
	var mod time.Time
	if f == nil {
		cr, err := waitCredentials(ctx, o.CredentialsPath, o.Log)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(o.CredentialsPath); err == nil {
			mod = fi.ModTime()
		}
		g = gitea.FromCredentials(cr)
		f, remote = g, g.Repo().CloneURL
		token = func(user string) (string, bool) {
			if user != forge.CIUser {
				return "", false
			}
			return g.Token(forge.CIUser)
		}
	}
	c := &Checker{o: o, forge: f, log: o.Log, posted: map[string]string{}, gitea: g, credsMod: mod}
	eng := o.Policy(func(ctx context.Context) (string, error) { return c.svc.MainDir(ctx) })
	c.svc = change.New(change.Options{
		DataDir: filepath.Join(o.DataDir, "ci"), RemoteURL: remote, Forge: f, Token: token,
		User: forge.CIUser, Renderer: o.Renderer, Policy: eng, Log: o.Log,
	})
	return c, nil
}

func waitCredentials(ctx context.Context, path string, log *slog.Logger) (*forge.Credentials, error) {
	for i := 0; ; i++ {
		cr, err := forge.LoadCredentials(path)
		if err == nil {
			if _, ok := cr.Token(forge.CIUser); ok {
				return cr, nil
			}
			err = fmt.Errorf("%s has no token for %s", path, forge.CIUser)
		}
		if i%10 == 0 {
			log.Info("waiting for forge credentials", "path", path, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("forge credentials: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// Run polls the forge and posts pk/policy until ctx ends.
func Run(ctx context.Context, o Options) error {
	c, err := New(ctx, o)
	if err != nil {
		return err
	}
	return c.Run(ctx)
}

// Run polls until ctx ends (or once).
func (c *Checker) Run(ctx context.Context) error {
	t := time.NewTicker(c.o.Interval)
	defer t.Stop()
	failing := false
	for {
		err := c.Pass(ctx)
		if c.o.Once {
			return err
		}
		if err != nil && !failing {
			c.log.Warn("ci pass", "err", err)
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// reloadCredentials loads new tokens when the credentials file changed and
// forgets what was posted (the forge may have been rebuilt).
func (c *Checker) reloadCredentials() {
	if c.gitea == nil || c.o.CredentialsPath == "" {
		return
	}
	fi, err := os.Stat(c.o.CredentialsPath)
	if err != nil || !fi.ModTime().After(c.credsMod) {
		return
	}
	cr, err := forge.LoadCredentials(c.o.CredentialsPath)
	if err != nil {
		return
	}
	for u, t := range cr.Tokens {
		c.gitea.SetToken(u, t)
	}
	c.credsMod = fi.ModTime()
	c.posted = map[string]string{}
	c.log.Info("forge credentials reloaded", "path", c.o.CredentialsPath)
}

// Pass evaluates every open pull request once.
func (c *Checker) Pass(ctx context.Context) error {
	c.reloadCredentials()
	ctx = identity.With(ctx, identity.Identity{User: forge.CIUser, Kind: identity.Bot})
	if err := c.svc.Refresh(ctx); err != nil {
		return err
	}
	prs, err := c.forge.ListPRs(ctx, forge.PROpen)
	if err != nil {
		return err
	}
	var errs []error
	for _, pr := range prs {
		id, _, ok := change.ParseBranch(pr.Head)
		if !ok || pr.HeadSHA == "" {
			continue
		}
		if err := c.check(ctx, id, pr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Status is the pk/policy status for gates: success with a summary, or
// failure with the blockers.
func Status(g *policy.Gates, draft bool) forge.CommitStatus {
	st := forge.CommitStatus{Context: forge.ContextPolicy, State: forge.StatusSuccess}
	var parts []string
	if g.Mergeable {
		parts = append(parts, fmt.Sprintf("approvals %d of %d", g.Approvals.Have, g.Approvals.Need))
		if g.LabGate.Status != "" && g.LabGate.Status != "na" {
			parts = append(parts, "lab "+g.LabGate.Status)
		}
		if g.Risk.Level != "" {
			parts = append(parts, "risk "+string(g.Risk.Level))
		}
		st.Description = "policy gates met: " + strings.Join(parts, "; ")
	} else {
		st.State = forge.StatusFailure
		parts = append(parts, g.Blockers...)
		if len(parts) == 0 {
			parts = append(parts, "not mergeable")
		}
		if draft {
			parts = append(parts, "draft")
		}
		st.Description = "blocked: " + strings.Join(parts, "; ")
	}
	if len(st.Description) > 240 {
		st.Description = st.Description[:237] + "..."
	}
	return st
}

func (c *Checker) check(ctx context.Context, id string, pr forge.PR) error {
	g, err := c.svc.Gates(ctx, id)
	if err != nil {
		return err
	}
	st := Status(g, pr.Draft)
	key := fmt.Sprintf("%d|%s|%s", pr.Number, st.State, st.Description)
	if c.posted[pr.HeadSHA] == key {
		return nil
	}
	cur, err := c.forge.Statuses(ctx, pr.HeadSHA)
	if err != nil {
		return err
	}
	for _, s := range cur {
		if s.Context == forge.ContextPolicy && s.State == st.State && s.Description == st.Description {
			c.posted[pr.HeadSHA] = key
			return nil
		}
	}
	if err := c.forge.SetStatus(ctx, forge.CIUser, pr.HeadSHA, st); err != nil {
		return err
	}
	c.posted[pr.HeadSHA] = key
	c.log.Info("pk/policy", "change", id, "mr", pr.Number, "sha", pr.HeadSHA[:min(7, len(pr.HeadSHA))], "state", st.State, "detail", st.Description)
	return nil
}
