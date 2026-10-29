package ci

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/forge/fake"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
)

const testPolicy = `apiVersion: pk/v1alpha1
kind: Policy
people:
  - {id: dennis, name: Dennis Klein, email: dennis@pk.local, roles: [platform-admin]}
  - {id: anna, name: Anna Berg, email: anna@pk.local, roles: [platform-admin]}
rules:
  - name: docs
    paths: ["docs/**"]
    writers: [platform-admin]
    approvers: [platform-admin]
    approvals: 1
    risk: low
`

func writeFile(t *testing.T, dir, p, c string) {
	t.Helper()
	full := filepath.Join(dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func policyStatuses(t *testing.T, f *fake.Forge, sha string) []forge.CommitStatus {
	t.Helper()
	all, err := f.Statuses(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	var out []forge.CommitStatus
	for _, s := range all {
		if s.Context == forge.ContextPolicy {
			out = append(out, s)
		}
	}
	return out
}

// The loop posts pk/policy on the PR head: failure without approvals,
// success after one, and nothing again while the result stays the same.
func TestPassPostsPolicyStatus(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bare, err := gitx.Init(ctx, filepath.Join(root, "remote.git"), true)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := gitx.Init(ctx, filepath.Join(root, "seed"), false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, seed.Dir, policy.Path, testPolicy)
	writeFile(t, seed.Dir, "docs/a.md", "a\n")
	_ = seed.Add(ctx)
	if _, err := seed.Commit(ctx, gitx.CommitOptions{Message: "chore: initial state",
		Author: gitx.Signature{Name: "Dennis Klein", Email: "dennis@pk.local"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, "push", "-q", bare.Dir, "main:main"); err != nil {
		t.Fatal(err)
	}
	f := fake.New("platform", "state")
	f.Git = bare
	tok := func(u string) (string, bool) { return "tok-" + u, true }

	// dennis opens a change and marks it ready.
	var bench *change.Svc
	bench = change.New(change.Options{DataDir: filepath.Join(root, "bench"), RemoteURL: bare.Dir, Forge: f, Token: tok, User: "dennis",
		Policy: policy.NewEngine(func(ctx context.Context) (string, error) { return bench.MainDir(ctx) })})
	if err := bench.Init(ctx); err != nil {
		t.Fatal(err)
	}
	dennis := identity.With(ctx, identity.Identity{User: "dennis", Kind: identity.Human, Name: "Dennis Klein", Email: "dennis@pk.local"})
	ch, err := bench.Create(dennis, change.CreateRequest{Title: "Docs", Intent: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, ch.Worktree, "docs/b.md", "b\n")
	if _, err := bench.Commit(dennis, ch.ID, "docs: b", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := bench.Push(dennis, ch.ID); err != nil {
		t.Fatal(err)
	}
	if ch, err = bench.MarkReady(dennis, ch.ID); err != nil {
		t.Fatal(err)
	}

	c, err := New(ctx, Options{DataDir: filepath.Join(root, "ci"), Forge: f, RemoteURL: bare.Dir, Interval: time.Second,
		Policy: func(mainDir func(context.Context) (string, error)) policy.Engine { return policy.NewEngine(mainDir) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	sts := policyStatuses(t, f, ch.HeadSHA)
	if len(sts) != 1 || sts[0].State != forge.StatusFailure || sts[0].Description != "blocked: approvals 0 of 1" {
		t.Fatalf("first pass: %+v", sts)
	}
	if err := c.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Approve(ctx, "anna", ch.MR.Number, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := c.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	sts = policyStatuses(t, f, ch.HeadSHA)
	if len(sts) != 1 || sts[0].State != forge.StatusSuccess {
		t.Fatalf("after approval: %+v", sts)
	}
}
