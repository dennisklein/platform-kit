package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/bench"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/forge/gitea"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// wireChanges sets the forge adapter and the change service (area A).
//
// The Gitea client is built from the config (PK_FORGE_URL, owner, repo);
// the tokens come from the credentials file of pk forge-init, which may
// appear after the bench starts (compose). The file is re-read when it
// changes (forge-init --reset makes new tokens). Until then change calls
// fail with "forge unreachable" and the poll keeps retrying.
func wireChanges(ctx context.Context, cfg config.Config, d *bench.Deps) error {
	log := slog.Default().With("component", "change")
	opts := gitea.Options{URL: cfg.ForgeURL, Owner: cfg.ForgeOwner, Repo: cfg.ForgeRepo}
	cr, credsErr := forge.LoadCredentials(cfg.ForgeCredentials)
	if credsErr == nil {
		if os.Getenv("PK_FORGE_URL") == "" && cr.URL != "" {
			opts.URL = cr.URL
		}
		if cr.Owner != "" {
			opts.Owner, opts.Repo = cr.Owner, cr.Repo
		}
		opts.Tokens = cr.Tokens
	}
	g := gitea.New(opts)
	d.Forge = g

	var svc *change.Svc
	eng := d.Policy
	if eng == nil {
		eng = policy.NewEngine(func(ctx context.Context) (string, error) { return svc.MainDir(ctx) })
	}
	rd := d.Renderer
	if rd == nil {
		rd = render.New(all.Registry())
	}
	svc = change.New(change.Options{
		DataDir:   cfg.DataDir,
		RemoteURL: g.Repo().CloneURL,
		Forge:     g,
		Token:     g.Token,
		User:      cfg.User,
		Policy:    eng,
		Renderer:  rd,
		Labs:      func() lab.Service { return d.Labs },
		Flux:      func() fluxsim.Client { return d.Flux },
		Events:    d.Events,
		Log:       log,
		// Lab runs of the old forge state test shas that are gone.
		OnForgeReset: func(_ context.Context, since time.Time) { forgetLabRuns(d.Labs, since, log) },
	})
	d.Changes = svc

	gen := filepath.Join(cfg.DataDir, "state", "forge-generation")
	if credsErr == nil {
		noteGeneration(ctx, svc, gen, cr.Generation, log)
	}
	go watchCredentials(ctx, cfg.ForgeCredentials, g, credsErr == nil, func(cr *forge.Credentials) {
		noteGeneration(ctx, svc, gen, cr.Generation, log)
	}, log)
	go svc.Run(ctx, cfg.PollInterval)
	return nil
}

// watchCredentials loads the tokens into the client when the credentials
// file appears or changes.
func watchCredentials(ctx context.Context, path string, g *gitea.Client, loaded bool, onLoad func(*forge.Credentials), log *slog.Logger) {
	var last time.Time
	if fi, err := os.Stat(path); err == nil && loaded {
		last = fi.ModTime()
	} else {
		log.Info("waiting for forge credentials", "path", path)
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fi, err := os.Stat(path)
		if err != nil || !fi.ModTime().After(last) {
			continue
		}
		cr, err := forge.LoadCredentials(path)
		if err != nil {
			continue
		}
		for u, tok := range cr.Tokens {
			g.SetToken(u, tok)
		}
		last = fi.ModTime()
		log.Info("forge credentials loaded", "path", path, "users", len(cr.Tokens))
		onLoad(cr)
	}
}

// noteGeneration drops the local change worktrees when the forge state was
// rebuilt since the bench last saw it (pk forge-init --reset).
func noteGeneration(ctx context.Context, svc *change.Svc, file, gen string, log *slog.Logger) {
	if gen == "" {
		return
	}
	old, err := os.ReadFile(file)
	if err == nil && string(old) == gen {
		return
	}
	if err == nil && len(old) > 0 {
		log.Info("forge state was rebuilt", "generation", gen)
		since, err := time.Parse(time.RFC3339Nano, gen)
		if err != nil {
			since = time.Now()
		}
		svc.ForgeReset(ctx, since)
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	_ = os.WriteFile(file, []byte(gen), 0o644)
}

// forgetLabRuns drops the lab runs started before a forge rebuild.
func forgetLabRuns(l lab.Service, since time.Time, log *slog.Logger) {
	f, ok := l.(interface{ Forget(time.Time) int })
	if !ok {
		return
	}
	if n := f.Forget(since); n > 0 {
		log.Info("forge state was rebuilt: dropped old lab runs", "runs", n)
	}
}

// forgeGeneration returns the time of the last forge rebuild the bench saw.
func forgeGeneration(cfg config.Config) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "state", "forge-generation"))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t, err == nil
}
