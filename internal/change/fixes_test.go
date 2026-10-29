package change

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

const fixesPolicy = `apiVersion: pk/v1alpha1
kind: Policy
people:
  - {id: dennis, name: Dennis Klein, email: dennis@pk.local, roles: [platform-admin]}
  - {id: anna, name: Anna Berg, email: anna@pk.local, roles: [platform-admin]}
  - {id: jonas, name: Jonas Weber, email: jonas@pk.local, roles: [platform-admin]}
agents:
  - {id: dennis-agent, actsFor: dennis, can: [push, open-mr, run-lab, mark-ready]}
rules:
  - name: apps
    paths: ["clusters/*/apps/**"]
    writers: [platform-admin, agent]
    approvers: [platform-admin]
    approvals: 1
    risk: low
  - name: docs
    paths: ["kb/**"]
    writers: [platform-admin, agent]
    approvers: [platform-admin]
    approvals: 1
    risk: low
  - name: default
    paths: ["**"]
    writers: [platform-admin, agent]
    approvers: [platform-admin]
    approvals: 2
    labGate: true
    risk: medium
fastPath:
  approvals: 1
  labGate: false
`

func newPolicyHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, map[string]string{policy.Path: fixesPolicy}, true)
}

func (h *harness) gatesOf(id string) *policy.Gates {
	h.t.Helper()
	g, err := h.svc.Gates(h.dennis, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return g
}

const podinfo = "clusters/defiant/apps/podinfo/deploy.yaml"

// A lab run by a reviewer commits only the report and the metadata: the
// reviewer is no writer and the approval counts.
func TestLabReportDoesNotMakeWriter(t *testing.T) {
	h := newPolicyHarness(t)
	ch := h.change("Bump podinfo to 6.9", podinfo, "image: podinfo:6.9.0\n")
	jonas := h.as("jonas")
	run := LabRunRef{Run: ch.ID + "/run-1", SHA: ch.HeadSHA, Result: LabPass, Report: "kb/labs/" + ch.ID + "-run-1.md"}
	if err := h.svc.RecordLabRun(jonas, ch.ID, run, []byte("---\ntitle: report\n---\n")); err != nil {
		t.Fatal(err)
	}
	got, err := h.svc.Get(h.dennis, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Commits[0].Author != "jonas" || got.Commits[0].Subject != "docs(lab): add report for "+run.Run {
		t.Fatalf("report commit %+v", got.Commits[0])
	}
	if _, err := h.forge.Approve(context.Background(), "jonas", got.MR.Number, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := h.gatesOf(ch.ID)
	if !slices.Equal(g.PathRights.Writers, []string{"dennis"}) || !slices.Equal(g.Approvals.By, []string{"jonas"}) || g.Approvals.Have != 1 {
		t.Fatalf("writers %v approvals %+v", g.PathRights.Writers, g.Approvals)
	}

	// A content commit by jonas makes him a writer: his approval stops counting.
	writeT(t, got.Worktree, podinfo, "image: podinfo:6.9.1\n")
	if _, err := h.svc.Commit(jonas, ch.ID, "fix: podinfo 6.9.1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Push(jonas, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.forge.Approve(context.Background(), "jonas", got.MR.Number, "ok again"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	g = h.gatesOf(ch.ID)
	if !slices.Contains(g.PathRights.Writers, "jonas") || g.Approvals.Have != 0 ||
		len(g.Approvals.Ignored) == 0 || g.Approvals.Ignored[len(g.Approvals.Ignored)-1].Reason != "the author or a writer of the change" {
		t.Fatalf("writers %v approvals %+v", g.PathRights.Writers, g.Approvals)
	}
}

func TestRecordOnly(t *testing.T) {
	id := "c-abc234"
	for _, tc := range []struct {
		files []string
		want  bool
	}{
		{[]string{".pk/changes/c-abc234.yaml"}, true},
		{[]string{"kb/labs/c-abc234-run-2.md", ".pk/changes/c-abc234.yaml"}, true},
		{[]string{"kb/labs/c-zzz234-run-1.md"}, false},
		{[]string{"kb/labs/c-abc234-run-1.md", podinfo}, false},
		{nil, false},
	} {
		if got := RecordOnly(id, tc.files); got != tc.want {
			t.Errorf("RecordOnly(%v) = %v", tc.files, got)
		}
	}
}

func TestRevertOf(t *testing.T) {
	for in, want := range map[string]string{
		"Revert c-31d0aa (merge 1a2b3c4): Cabling for GC5A-6Z": "c-31d0aa",
		"Revert c-31d0aa":                     "c-31d0aa",
		"Revert c-31d0aa: oops":               "c-31d0aa",
		"Revert c-31d0aax: no":                "",
		"Reverts the cabling":                 "",
		"2 compute nodes, Revert c-31d0aa no": "",
	} {
		if got := RevertOf(in); got != want {
			t.Errorf("RevertOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// mergeOnMain merges a branch that changes path into main of the forge
// repo, with the Pk-Change trailers of a bench merge.
func (h *harness) mergeOnMain(id, path, content string) string {
	h.t.Helper()
	ctx := context.Background()
	dir := filepath.Join(h.t.TempDir(), "clone")
	if _, err := gitx.Open(h.t.TempDir()).Run(ctx, "clone", "-q", h.bare.Dir, dir); err != nil {
		h.t.Fatal(err)
	}
	g := gitx.Open(dir)
	sig := gitx.Signature{Name: "Jonas Weber", Email: "jonas@pk.local"}
	if _, err := g.Run(ctx, "checkout", "-q", "-b", "change/"+id+"-x"); err != nil {
		h.t.Fatal(err)
	}
	writeT(h.t, dir, path, content)
	_ = g.Add(ctx)
	if _, err := g.Commit(ctx, gitx.CommitOptions{Message: "feat: " + id, Author: sig}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := g.Run(ctx, "checkout", "-q", "main"); err != nil {
		h.t.Fatal(err)
	}
	msg := "Merge change " + id + ": Bump podinfo (!3)\n\nbump\n\nPk-Change: " + id +
		"\nPk-Intent: bump\nPk-Lab-Run: " + id + "/run-1 pass kb/labs/" + id + "-run-1.md\nPk-Author: jonas\nApproved-by: anna, dennis\nReviewed-in: !3\n"
	if _, err := g.Run(ctx, "-c", "user.name=Jonas Weber", "-c", "user.email=jonas@pk.local", "merge", "-q", "--no-ff", "-m", msg, "change/"+id+"-x"); err != nil {
		h.t.Fatal(err)
	}
	if _, err := g.Run(ctx, "push", "-q", "origin", "main"); err != nil {
		h.t.Fatal(err)
	}
	if err := h.svc.Refresh(ctx); err != nil {
		h.t.Fatal(err)
	}
	sha, _ := g.Head(ctx)
	return sha
}

// A revert of a merged change gets the fast path; an intent alone does not.
func TestRevertFastPath(t *testing.T) {
	h := newPolicyHarness(t)
	merge := h.mergeOnMain("c-aaaaaa", "control/cabling.yaml", "uplinks: [spine-1]\n")

	rv, err := h.svc.Create(h.dennis, CreateRequest{Title: `Revert "Cabling"`, Intent: "Revert c-aaaaaa (merge " + merge[:7] + "): Cabling"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, rv.Worktree, "control/cabling.yaml", "uplinks: []\n")
	if _, err := h.svc.Commit(h.dennis, rv.ID, `Revert "Cabling"`, nil); err != nil {
		t.Fatal(err)
	}
	g := h.gatesOf(rv.ID)
	if !g.FastPath || g.Approvals.Need != 1 || g.LabGate.Required {
		t.Fatalf("revert: fast %v need %d lab %v", g.FastPath, g.Approvals.Need, g.LabGate.Required)
	}

	// Same intent, but it changes a file the merge did not touch.
	fake, err := h.svc.Create(h.dennis, CreateRequest{Title: "Sneaky", Intent: "Revert c-aaaaaa: and more"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, fake.Worktree, "control/talos.yaml", "x: 1\n")
	if _, err := h.svc.Commit(h.dennis, fake.ID, "feat: talos", nil); err != nil {
		t.Fatal(err)
	}
	if g := h.gatesOf(fake.ID); g.FastPath || g.Approvals.Need != 2 {
		t.Fatalf("not a revert: fast %v need %d", g.FastPath, g.Approvals.Need)
	}

	// Same intent and only the merged path, but other content than a revert.
	rogue, err := h.svc.Create(h.dennis, CreateRequest{Title: "Revert", Intent: "Revert c-aaaaaa (merge " + merge[:7] + "): Cabling"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, rogue.Worktree, "control/cabling.yaml", "uplinks: [spine-1, rogue-switch-99]\n")
	if _, err := h.svc.Commit(h.dennis, rogue.ID, "feat: not a revert", nil); err != nil {
		t.Fatal(err)
	}
	if g := h.gatesOf(rogue.ID); g.FastPath || g.Approvals.Need != 2 || !g.LabGate.Required {
		t.Fatalf("rogue content: fast %v need %d lab %v", g.FastPath, g.Approvals.Need, g.LabGate.Required)
	}
	// Uncommitted rogue edits on a real revert close the fast path too.
	writeT(t, rv.Worktree, "control/cabling.yaml", "uplinks: [rogue-switch-99]\n")
	if g := h.gatesOf(rv.ID); g.FastPath {
		t.Fatalf("revert with rogue edits: fast path")
	}

	// The merged change has gates too (as merged): no error.
	mg := h.gatesOf("c-aaaaaa")
	if !slices.Equal(mg.Approvals.By, []string{"anna", "dennis"}) || mg.LabGate.Status != "met" || !mg.Mergeable {
		t.Fatalf("merged gates %+v lab %+v blockers %v", mg.Approvals, mg.LabGate, mg.Blockers)
	}
}

type fakeLabs struct {
	lab.Service
	runs []lab.Run
}

func (f *fakeLabs) Runs(context.Context, string) ([]lab.Run, error) { return f.runs, nil }

// A cancelled run (bench restart) leaves the run before it in effect.
func TestCancelledLabRunKeepsPrevious(t *testing.T) {
	h := newHarness(t)
	ch, err := h.svc.Create(h.dennis, CreateRequest{Title: "Add cn-035", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, ch.Worktree, "control/inventory/cn-035.yaml", "name: cn-035\n")
	if _, err := h.svc.Commit(h.dennis, ch.ID, "feat: cn-035", nil); err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Open(ch.Worktree).Head(context.Background())
	run1 := LabRunRef{Run: ch.ID + "/run-1", SHA: head, Result: LabFail, Report: "kb/labs/" + ch.ID + "-run-1.md"}
	if err := h.svc.RecordLabRun(h.dennis, ch.ID, run1, []byte("---\ntitle: r\n---\n")); err != nil {
		t.Fatal(err)
	}
	labs := &fakeLabs{runs: []lab.Run{
		{ID: ch.ID + "/run-2", ChangeID: ch.ID, SHA: head, Result: lab.ResultCancelled},
		{ID: ch.ID + "/run-1", ChangeID: ch.ID, SHA: head, Result: lab.ResultFail, Report: run1.Report},
	}}
	h.svc.o.Labs = func() lab.Service { return labs }
	got, err := h.svc.Get(h.dennis, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LabState != "fail" || got.LabRun != run1.Run || got.State != Lab {
		t.Fatalf("lab %s %s state %s", got.LabState, got.LabRun, got.State)
	}
	// The cancelled run is still listed.
	if n := len(got.LabRuns); n != 2 || got.LabRuns[1].Result != LabCancelled {
		t.Fatalf("runs %+v", got.LabRuns)
	}
}

type fakeFlux struct {
	fluxsim.Client
	st fluxsim.Status
}

func (f *fakeFlux) Status(context.Context) (*fluxsim.Status, error) { return &f.st, nil }

// verified needs the cluster healthy now.
func TestVerifiedNeedsHealthyCluster(t *testing.T) {
	h := newHarness(t)
	merge := h.mergeOnMain("c-bbbbbb", podinfo, "image: podinfo:6.9.0\n")
	rev := &fluxsim.Revision{SHA: merge}
	flux := &fakeFlux{st: fluxsim.Status{Clusters: []fluxsim.ClusterStatus{
		{ID: "defiant", Ring: "canary", Applied: rev, Verified: rev, Healthy: true},
		{ID: "phoenix", Ring: "prod", Healthy: true},
	}}}
	h.svc.o.Flux = func() fluxsim.Client { return flux }
	got, err := h.svc.Get(h.dennis, "c-bbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Verified || len(got.Rollout) != 1 || !got.Rollout[0].Verified {
		t.Fatalf("healthy: %s %+v", got.State, got.Rollout)
	}
	// "Make it fail": Flux keeps the verified revision, health checks fail.
	flux.st.Clusters[0].Healthy, flux.st.Clusters[0].Failing = false, true
	got, _ = h.svc.Get(h.dennis, "c-bbbbbb")
	if got.State != Deployed || got.Rollout[0].Verified || !got.Rollout[0].Deployed {
		t.Fatalf("failing: %s %+v", got.State, got.Rollout)
	}
}

// Closing a change with uncommitted edits removes it for real.
func TestCloseDirtyAndUnreviewed(t *testing.T) {
	h := newHarness(t)
	ctx := h.dennis
	ch, err := h.svc.Create(ctx, CreateRequest{Title: "Draft with edits", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, ch.Worktree, podinfo, "image: podinfo:7.0.0\n")
	if got, _ := h.svc.Get(ctx, ch.ID); !got.Dirty {
		t.Fatal("not dirty")
	}
	sum, err := h.svc.Close(ctx, ch.ID)
	if err != nil || sum.State != Closed {
		t.Fatalf("close %+v %v", sum, err)
	}
	if _, err := os.Stat(ch.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree kept: %v", err)
	}
	all, _ := h.svc.List(ctx, true)
	for _, c := range all {
		if c.ID == ch.ID {
			t.Fatalf("closed change listed again: %+v", c)
		}
	}

	// Pushed without an MR: the branch on the forge goes too.
	ch2, err := h.svc.Create(ctx, CreateRequest{Title: "Pushed only", Intent: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Open(ch2.Worktree).Run(context.Background(), "push", "-q", h.bare.Dir, ch2.Branch+":"+ch2.Branch); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.svc.Get(ctx, ch2.ID); !got.Pushed || got.MR != nil {
		t.Fatalf("pushed without MR: %+v", got.Summary)
	}
	if _, err := h.svc.Close(ctx, ch2.ID); err != nil {
		t.Fatal(err)
	}
	if out, _ := h.bare.Run(context.Background(), "branch", "--list", ch2.Branch); out != "" {
		t.Fatalf("remote branch kept: %q", out)
	}
	open, _ := h.svc.List(ctx, false)
	if len(open) != 0 {
		t.Fatalf("open after close: %+v", open)
	}
	if _, err := h.svc.Gates(ctx, ch2.ID); err == nil {
		t.Fatal("gates of a closed change")
	}
}

// Overlap entries carry the state of now, not the state seen when their
// objects were first collected (for example before the forge state loaded).
func TestOverlapStateIsFresh(t *testing.T) {
	h := newHarness(t)
	a, err := h.svc.Create(h.dennis, CreateRequest{Title: "Add cn-035", Intent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.svc.Create(h.dennis, CreateRequest{Title: "BGP peers", Intent: "b"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, b.Worktree, "control/inventory/cn-035.yaml", "name: cn-035\n")
	if _, err := h.svc.Commit(h.dennis, b.ID, "feat: cn-035", nil); err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Open(b.Worktree).Head(context.Background())
	if err := h.svc.RecordLabRun(h.dennis, b.ID, LabRunRef{Run: b.ID + "/run-1", SHA: head, Result: LabFail}, nil); err != nil {
		t.Fatal(err)
	}
	key := render.ObjectRef{Cluster: "defiant", APIVersion: "cilium.io/v2alpha1", Kind: "CiliumBGPPeeringPolicy", Name: "rack-6z"}
	keys := map[render.ObjectKey]render.ObjectRef{render.KeyOf(key): key}
	h.svc.cmu.Lock()
	h.svc.objects[a.ID] = objEntry{title: "A", author: "dennis", state: Draft, keys: keys}
	h.svc.objects[b.ID] = objEntry{title: "B", author: "dennis", state: Draft, keys: keys}
	h.svc.cmu.Unlock()
	got, err := h.svc.Get(h.dennis, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Overlaps) != 1 || got.Overlaps[0].Change != b.ID || got.Overlaps[0].State != Lab {
		t.Fatalf("overlaps %+v", got.Overlaps)
	}
}
