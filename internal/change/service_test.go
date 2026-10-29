package change

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/forge/fake"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
)

type harness struct {
	t      *testing.T
	svc    *Svc
	forge  *fake.Forge
	bare   *gitx.Repo
	hub    *events.Hub
	dennis context.Context
	agent  context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, nil, false)
}

// newHarnessWith seeds extra files into main and, with withPolicy, wires
// the policy engine (a .pk/policy.yaml in extra).
func newHarnessWith(t *testing.T, extra map[string]string, withPolicy bool) *harness {
	t.Helper()
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
	for p, c := range map[string]string{
		"control/cabling.yaml":                      "uplinks: []\n",
		"clusters/defiant/infra/cilium/bgp.yaml":    "nodes:\n  - cn-025\n  - cn-034\n",
		"clusters/defiant/apps/podinfo/deploy.yaml": "image: podinfo:6.8.0\n",
	} {
		writeT(t, seed.Dir, p, c)
	}
	for p, c := range extra {
		writeT(t, seed.Dir, p, c)
	}
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
	hub := events.NewHub()
	var svc *Svc
	o := Options{
		DataDir: filepath.Join(root, "data"), RemoteURL: bare.Dir, Forge: f,
		Token: func(u string) (string, bool) { return "tok-" + u, true },
		User:  "dennis", Events: hub, CIWait: 1500 * time.Millisecond,
	}
	if withPolicy {
		o.Policy = policy.NewEngine(func(ctx context.Context) (string, error) { return svc.MainDir(ctx) })
	}
	svc = New(o)
	if err := svc.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, svc: svc, forge: f, bare: bare, hub: hub}
	h.dennis = identity.With(ctx, identity.Identity{User: "dennis", Kind: identity.Human, Name: "Dennis Klein", Email: "dennis@pk.local"})
	h.agent = identity.With(ctx, identity.Identity{User: "dennis-agent", Kind: identity.Agent, ActsFor: "dennis"})
	return h
}

func writeT(t *testing.T, dir, p, c string) {
	t.Helper()
	full := filepath.Join(dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) as(user string) context.Context {
	return identity.With(context.Background(), identity.Identity{User: user, Kind: identity.Human, Name: user, Email: user + "@pk.local"})
}

// change makes a change with one committed file, pushed and ready.
func (h *harness) change(title, path, content string) *Change {
	h.t.Helper()
	ch, err := h.svc.Create(h.dennis, CreateRequest{Title: title, Intent: "intent of " + title})
	if err != nil {
		h.t.Fatal(err)
	}
	writeT(h.t, ch.Worktree, path, content)
	if _, err := h.svc.Commit(h.dennis, ch.ID, "feat: "+title, nil); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.svc.Push(h.dennis, ch.ID); err != nil {
		h.t.Fatal(err)
	}
	if ch, err = h.svc.MarkReady(h.dennis, ch.ID); err != nil {
		h.t.Fatal(err)
	}
	return ch
}

func (h *harness) green(id string) {
	h.t.Helper()
	ch, err := h.svc.Get(h.dennis, id)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.forge.SetStatus(context.Background(), forge.CIUser, ch.HeadSHA, forge.CommitStatus{Context: forge.ContextPolicy, State: forge.StatusSuccess}); err != nil {
		h.t.Fatal(err)
	}
}

func TestChangeLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := h.dennis
	sub, cancel := h.hub.Subscribe("dennis", 0)
	defer cancel()

	ch, err := h.svc.Create(ctx, CreateRequest{Title: "Add cn-035 and cn-036 to defiant",
		Intent: "2 compute nodes for tenants, in GC5A-6Z U6 and U8", WorkItems: []string{"w-114"}})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(ch.ID) || ch.Branch != Branch(ch.ID, "Add cn-035 and cn-036 to defiant") || !ch.Local || ch.State != Draft ||
		ch.Group != GroupMine || ch.Summary.Commits != 1 || ch.Author != "dennis" {
		t.Fatalf("created: %+v", ch.Summary)
	}
	if ev := <-sub; ev.Type != events.ChangeCreated {
		t.Fatalf("event %s", ev.Type)
	}
	meta, err := os.ReadFile(filepath.Join(ch.Worktree, MetaPath(ch.ID)))
	if err != nil || !strings.Contains(string(meta), "workItems:\n  - w-114") {
		t.Fatalf("meta file: %s %v", meta, err)
	}

	// A human commit and an agent commit.
	writeT(t, ch.Worktree, "control/inventory/cn-035.yaml", "name: cn-035\n")
	st, _ := h.svc.Status(ctx, ch.ID)
	if len(st) != 1 || st[0].Status != "untracked" {
		t.Fatalf("status %+v", st)
	}
	c1, err := h.svc.Commit(ctx, ch.ID, "feat(inventory): add cn-035", nil)
	if err != nil || c1.AuthorName != "Dennis Klein" || c1.Author != "dennis" || len(c1.Files) != 1 {
		t.Fatalf("commit %+v %v", c1, err)
	}
	if _, err := h.svc.Commit(ctx, ch.ID, "nothing", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty commit: %v", err)
	}
	writeT(t, ch.Worktree, "clusters/defiant/infra/cilium/bgp.yaml", "nodes:\n  - cn-025\n  - cn-034\n  - cn-035\n")
	c2, err := h.svc.Commit(h.agent, ch.ID, "feat(cilium): peer cn-035", nil)
	if err != nil || c2.Agent != "dennis-agent" || c2.OnBehalfOf != "dennis" || c2.AuthorEmail != "dennis-agent@pk.local" {
		t.Fatalf("agent commit %+v %v", c2, err)
	}

	// Push opens a draft MR.
	pr, err := h.svc.Push(ctx, ch.ID)
	if err != nil || !pr.Created || pr.MR == nil || !pr.MR.Draft || len(pr.Pushed) != 3 {
		t.Fatalf("push %+v %v", pr, err)
	}
	got, _ := h.svc.Get(ctx, ch.ID)
	if !got.Pushed || got.MR == nil || got.State != Draft || got.AheadRemote != 0 || got.Agent != "dennis-agent" {
		t.Fatalf("after push %+v", got.Summary)
	}
	if !strings.Contains(got.Description, "Pk-Change: "+ch.ID) {
		t.Fatalf("description %q", got.Description)
	}

	// Ready: metadata moves into the description.
	ready, err := h.svc.MarkReady(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.MR.Draft || ready.State != Review {
		t.Fatalf("ready %+v", ready.Summary)
	}
	if _, err := os.Stat(filepath.Join(ch.Worktree, MetaPath(ch.ID))); !os.IsNotExist(err) {
		t.Fatal("metadata file must be deleted at ready")
	}
	for _, want := range []string{"Pk-Agent: dennis-agent", "Pk-Work-Item: w-114", "Pk-Author: dennis", "Pk-Title: Add cn-035"} {
		if !strings.Contains(ready.Description, want) {
			t.Fatalf("description lacks %q:\n%s", want, ready.Description)
		}
	}
	if ready.Meta.ID != ch.ID || ready.Title != "Add cn-035 and cn-036 to defiant" {
		t.Fatalf("meta from description: %+v", ready.Meta)
	}

	// Approvals: not the author, never agents.
	if _, err := h.svc.Approve(ctx, ch.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self approve: %v", err)
	}
	if _, err := h.svc.Approve(h.agent, ch.ID, ""); !errors.Is(err, ErrAgentDeny) {
		t.Fatalf("agent approve: %v", err)
	}
	if _, err := h.svc.ApproveAs(ctx, "anna", ch.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Merge(h.agent, ch.ID); !errors.Is(err, ErrAgentDeny) {
		t.Fatalf("agent merge: %v", err)
	}
	// Without pk/policy the merge waits and fails.
	if _, err := h.svc.Merge(ctx, ch.ID); !errors.Is(err, ErrGates) {
		t.Fatalf("merge without CI: %v", err)
	}
	h.green(ch.ID)
	res, err := h.svc.Merge(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := h.bare.Log(ctx, "main", "--merges", "-n", "1")
	if err != nil || len(log) != 1 || log[0].SHA != res.SHA {
		t.Fatalf("merge log %+v %v", log, err)
	}
	ts := log[0].Trailers
	if log[0].Subject != "Merge change "+ch.ID+": Add cn-035 and cn-036 to defiant (!1)" ||
		Trailer(ts, TrailerChange) != ch.ID || Trailer(ts, TrailerAgent) != "dennis-agent" ||
		Trailer(ts, TrailerReviewedIn) != "!1" || Trailer(ts, TrailerWorkItem) != "w-114" {
		t.Fatalf("merge commit %q %+v", log[0].Subject, ts)
	}
	if _, err := os.Stat(ch.Worktree); !os.IsNotExist(err) {
		t.Fatal("worktree must be removed after merge")
	}
	merged, err := h.svc.Get(ctx, ch.ID)
	if err != nil || merged.State != Merged || merged.MergeSHA != res.SHA {
		t.Fatalf("merged %+v %v", merged, err)
	}
	// Files, commits and the MR of a merged change come from the merge.
	if merged.Files == 0 || len(merged.Commits) == 0 || merged.MR == nil || merged.MR.Number != 1 {
		t.Fatalf("merged detail: files %d commits %d mr %+v", merged.Files, len(merged.Commits), merged.MR)
	}
	if base, head, err := h.svc.Sources(ctx, ch.ID); err != nil || base.SHA == "" || head.SHA != res.SHA {
		t.Fatalf("merged sources %+v %+v %v", base, head, err)
	}
	open, _ := h.svc.List(ctx, false)
	if len(open) != 0 {
		t.Fatalf("open changes %+v", open)
	}
	all, _ := h.svc.List(ctx, true)
	if len(all) != 1 || all[0].State != Merged {
		t.Fatalf("all changes %+v", all)
	}
}

func TestRebaseBehindAndConflict(t *testing.T) {
	h := newHarness(t)
	ctx := h.dennis
	a := h.change("Cabling for GC5A-6Z", "control/cabling.yaml", "uplinks:\n  - spine-1\n")
	b := h.change("Add cn-035", "control/inventory/cn-035.yaml", "name: cn-035\n")
	h.green(a.ID)
	if _, err := h.svc.ApproveAs(ctx, "anna", a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Merge(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := h.svc.Get(ctx, b.ID)
	if got.BehindTarget != 1 {
		t.Fatalf("behind %d", got.BehindTarget)
	}
	h.green(b.ID)
	_, err := h.svc.Merge(ctx, b.ID)
	var be *BehindError
	if !errors.As(err, &be) || err.Error() != "behind main by 1, rebase first" {
		t.Fatalf("merge behind: %v", err)
	}
	oldHead := got.HeadSHA
	rb, err := h.svc.Rebase(ctx, b.ID)
	if err != nil || !rb.OK || rb.From != oldHead || rb.To == oldHead {
		t.Fatalf("rebase %+v %v", rb, err)
	}
	got, _ = h.svc.Get(ctx, b.ID)
	if got.BehindTarget != 0 || got.AheadRemote != 0 || got.HeadSHA != rb.To {
		t.Fatalf("after rebase %+v", got.Summary)
	}
	remote, _ := h.bare.RevParse(ctx, "refs/heads/"+got.Branch)
	if remote != rb.To {
		t.Fatalf("force push with lease did not update the branch: %s", remote)
	}
	h.green(b.ID)
	if _, err := h.svc.Merge(ctx, b.ID); err != nil {
		t.Fatal(err)
	}

	// Conflict: both change the same line.
	c := h.change("Podinfo 6.9", "clusters/defiant/apps/podinfo/deploy.yaml", "image: podinfo:6.9.0\n")
	d := h.change("Podinfo 6.9.1", "clusters/defiant/apps/podinfo/deploy.yaml", "image: podinfo:6.9.1\n")
	h.green(c.ID)
	if _, err := h.svc.Merge(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.Rebase(ctx, d.ID)
	var ce *gitx.ErrConflict
	if !errors.As(err, &ce) || len(ce.Paths) != 1 || ce.Paths[0] != "clusters/defiant/apps/podinfo/deploy.yaml" {
		t.Fatalf("conflict: %v", err)
	}
	st, _ := h.svc.Status(ctx, d.ID)
	if len(st) != 0 {
		t.Fatalf("rebase must be aborted, status %+v", st)
	}
}

func TestCheckoutAndClose(t *testing.T) {
	h := newHarness(t)
	ctx := h.dennis
	ch := h.change("Bump podinfo", "clusters/defiant/apps/podinfo/deploy.yaml", "image: podinfo:6.9.0\n")
	// Remove the local worktree, then check it out again (someone else's view).
	if err := h.svc.repo.WorktreeRemove(ctx, ch.Worktree, true); err != nil {
		t.Fatal(err)
	}
	got, err := h.svc.Get(ctx, ch.ID)
	if err != nil || got.Local || !got.Pushed {
		t.Fatalf("remote only %+v %v", got, err)
	}
	dir, live, err := h.svc.Resolve(ctx, ch.ID)
	if err != nil || live {
		t.Fatalf("resolve %s %v %v", dir, live, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "clusters/defiant/apps/podinfo/deploy.yaml")); err != nil || string(b) != "image: podinfo:6.9.0\n" {
		t.Fatalf("export %s %v", b, err)
	}
	sum, err := h.svc.Checkout(ctx, ch.ID)
	if err != nil || !sum.Local {
		t.Fatalf("checkout %+v %v", sum, err)
	}
	heads, err := h.svc.Heads(ctx)
	if err != nil || len(heads) != 1 || heads[0].HeadSHA != got.HeadSHA || heads[0].Worktree == "" {
		t.Fatalf("heads %+v %v", heads, err)
	}
	base, head, err := h.svc.Sources(ctx, ch.ID)
	if err != nil || base.Dir == "" || head.Dir == "" || base.SHA == head.SHA {
		t.Fatalf("sources %+v %+v %v", base, head, err)
	}
	closed, err := h.svc.Close(ctx, ch.ID)
	if err != nil || closed.State != Closed {
		t.Fatalf("close %+v %v", closed, err)
	}
	got, _ = h.svc.Get(ctx, ch.ID)
	if got.State != Closed {
		t.Fatalf("state after close %s", got.State)
	}
	open, _ := h.svc.List(ctx, false)
	if len(open) != 0 {
		t.Fatalf("open %+v", open)
	}
}

func TestRecordLabRun(t *testing.T) {
	h := newHarness(t)
	ctx := h.dennis
	ch, err := h.svc.Create(ctx, CreateRequest{Title: "Add cn-035", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, ch.Worktree, "control/inventory/cn-035.yaml", "name: cn-035\n")
	if _, err := h.svc.Commit(ctx, ch.ID, "feat: cn-035", nil); err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Open(ch.Worktree).Head(ctx)
	run := LabRunRef{Run: ch.ID + "/run-1", SHA: head, Result: LabPass, Report: "kb/labs/" + ch.ID + "-run-1.md"}
	if err := h.svc.RecordLabRun(ctx, ch.ID, run, []byte("---\ntitle: report\n---\n")); err != nil {
		t.Fatal(err)
	}
	got, _ := h.svc.Get(ctx, ch.ID)
	if got.LabState != "pass" || got.LabRun != run.Run || len(got.Meta.LabRuns) != 1 || got.Meta.LabRuns[0].SHA != head {
		t.Fatalf("lab %+v %+v", got.Summary, got.Meta)
	}
	if got.Commits[0].Subject != "docs(lab): add report for "+run.Run {
		t.Fatalf("report commit %q", got.Commits[0].Subject)
	}
	// A content change after the run invalidates it.
	writeT(t, ch.Worktree, "control/inventory/cn-036.yaml", "name: cn-036\n")
	got, _ = h.svc.Get(ctx, ch.ID)
	if got.LabState != "none" {
		t.Fatalf("stale run counts: %s", got.LabState)
	}
}

func TestForgeResetDropsWorktrees(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ch, err := h.svc.Create(h.dennis, CreateRequest{Title: "Old forge change", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// forge-init --reset: main gets a new history.
	seed, err := gitx.Init(ctx, filepath.Join(t.TempDir(), "reseed"), false)
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, seed.Dir, "control/cabling.yaml", "uplinks: [new]\n")
	_ = seed.Add(ctx)
	if _, err := seed.Commit(ctx, gitx.CommitOptions{Message: "chore: initial state",
		Author: gitx.Signature{Name: "Dennis Klein", Email: "dennis@pk.local"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, "push", "-q", "-f", h.bare.Dir, "main:main"); err != nil {
		t.Fatal(err)
	}
	var resetSince time.Time
	h.svc.o.OnForgeReset = func(_ context.Context, since time.Time) { resetSince = since }
	if err := h.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ch.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree of the old forge kept: %v", err)
	}
	if resetSince.IsZero() {
		t.Fatal("OnForgeReset not called")
	}
	open, _ := h.svc.List(h.dennis, false)
	for _, c := range open {
		if c.ID == ch.ID {
			t.Fatalf("old change still listed: %+v", c)
		}
	}
	// A rebuild notice (credentials generation) keeps changes opened after it.
	before := time.Now().Add(-time.Hour)
	ch2, err := h.svc.Create(h.dennis, CreateRequest{Title: "New forge change", Intent: "y"})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.ForgeReset(ctx, before)
	if _, err := os.Stat(ch2.Worktree); err != nil {
		t.Fatalf("change opened after the rebuild was dropped: %v", err)
	}
	h.svc.ForgeReset(ctx, time.Now().Add(time.Hour))
	if _, err := os.Stat(ch2.Worktree); !os.IsNotExist(err) {
		t.Fatalf("change opened before the rebuild kept: %v", err)
	}
	// Each rebuild is announced once (forge.reset), with the new main.
	evs, cancel := h.hub.Subscribe("dennis", 1)
	defer cancel()
	main, _ := h.svc.repo.RevParse(ctx, mainRef)
	var resets []ForgeResetEvent
	for len(evs) > 0 {
		ev := <-evs
		if ev.Type == events.ForgeReset {
			resets = append(resets, ev.Data.(ForgeResetEvent))
		}
	}
	if len(resets) != 3 || resets[0].Main != main || resets[1].Since.IsZero() || resets[2].Main != main {
		t.Fatalf("forge.reset events %+v (main %s)", resets, main)
	}
}
