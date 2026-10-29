package change

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/lab"
)

// An approval keeps counting after a rebase and after record commits (lab
// report), and stops counting when new content is committed on top.
func TestStaleApprovals(t *testing.T) {
	h := newPolicyHarness(t)
	ch := h.change("Bump podinfo", podinfo, "image: podinfo:6.9.0\n")
	if _, err := h.svc.Approve(h.as("anna"), ch.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	approvedBy := func(step string, want ...string) {
		t.Helper()
		g := h.gatesOf(ch.ID)
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(g.Approvals.By, want) {
			t.Fatalf("%s: approvals by %v (ignored %+v), want %v", step, g.Approvals.By, g.Approvals.Ignored, want)
		}
	}
	approvedBy("on the head", "anna")

	// 1. A lab report commit (records of the change only).
	got, _ := h.svc.Get(h.dennis, ch.ID)
	run := LabRunRef{Run: ch.ID + "/run-1", SHA: got.HeadSHA, Result: LabPass, Report: "kb/labs/" + ch.ID + "-run-1.md"}
	if err := h.svc.RecordLabRun(h.as("jonas"), ch.ID, run, []byte("---\ntitle: report\n---\n")); err != nil {
		t.Fatal(err)
	}
	if after, _ := h.svc.Get(h.dennis, ch.ID); after.HeadSHA == got.HeadSHA {
		t.Fatal("the lab report made no commit")
	}
	approvedBy("after the lab report", "anna")

	// 2. A rebase onto a new main (another change merged).
	h.mergeOnMain("c-bbbbbb", "control/cabling.yaml", "uplinks: [spine-1]\n")
	if rb, err := h.svc.Rebase(h.dennis, ch.ID); err != nil || !rb.OK {
		t.Fatalf("rebase %+v %v", rb, err)
	}
	approvedBy("after the rebase", "anna")

	// 3. New content: the approval is on an older head.
	got, _ = h.svc.Get(h.dennis, ch.ID)
	writeT(t, got.Worktree, podinfo, "image: evil/podinfo:latest\n")
	if _, err := h.svc.Commit(h.as("jonas"), ch.ID, "feat: other image", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Push(h.as("jonas"), ch.ID); err != nil {
		t.Fatal(err)
	}
	approvedBy("after new content")
	g := h.gatesOf(ch.ID)
	if g.Mergeable || len(g.Approvals.Ignored) != 1 || g.Approvals.Ignored[0].Reason != "approved an older head" {
		t.Fatalf("stale approval: mergeable %v ignored %+v", g.Mergeable, g.Approvals.Ignored)
	}
	h.green(ch.ID)
	if _, err := h.svc.Merge(h.dennis, ch.ID); err == nil {
		t.Fatal("merged with an approval of an older head")
	}
	full, _ := h.svc.Get(h.dennis, ch.ID)
	if len(full.Approvals) != 1 || full.Approvals[0].Counts || full.Approvals[0].Reason != "approved an older head" {
		t.Fatalf("change approvals %+v", full.Approvals)
	}

	// A new approval of the head counts again (the old one is not listed twice).
	if _, err := h.svc.Approve(h.as("anna"), ch.ID, "ok again"); err != nil {
		t.Fatal(err)
	}
	approvedBy("re-approved", "anna")
	if g := h.gatesOf(ch.ID); len(g.Approvals.Ignored) != 0 {
		t.Fatalf("ignored %+v", g.Approvals.Ignored)
	}
	full, _ = h.svc.Get(h.dennis, ch.ID)
	if len(full.Approvals) != 1 || !full.Approvals[0].Counts {
		t.Fatalf("change approvals after re-approval %+v", full.Approvals)
	}
}

// Close: an agent closes only changes of the person it acts for; another
// person closes only the MR and leaves the author's work alone.
func TestCloseRights(t *testing.T) {
	h := newPolicyHarness(t)
	jonas := h.as("jonas")
	ch, err := h.svc.Create(jonas, CreateRequest{Title: "Jonas work", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, ch.Worktree, podinfo, "image: podinfo:7\n")
	if _, err := h.svc.Commit(jonas, ch.ID, "feat: x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Close(h.agent, ch.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent closed jonas's change: %v", err)
	}
	if _, err := h.svc.Close(h.dennis, ch.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("dennis closed jonas's change without an MR: %v", err)
	}
	if _, err := os.Stat(ch.Worktree); err != nil {
		t.Fatalf("worktree gone: %v", err)
	}
	// With an MR another person closes the MR only.
	if _, err := h.svc.Push(jonas, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.MarkReady(jonas, ch.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := h.svc.Close(h.dennis, ch.ID); err != nil || out.State != Closed {
		t.Fatalf("dennis closes jonas's MR: %+v %v", out, err)
	}
	if _, err := os.Stat(ch.Worktree); err != nil {
		t.Fatalf("jonas's worktree removed by dennis: %v", err)
	}
	if got, _ := h.svc.Get(h.dennis, ch.ID); got == nil || got.State != Closed {
		t.Fatalf("state after close %+v", got)
	}

	// The agent closes a change of dennis (the person it acts for).
	own, err := h.svc.Create(h.dennis, CreateRequest{Title: "Mine", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Close(h.agent, own.ID); err != nil {
		t.Fatalf("agent closes dennis's change: %v", err)
	}
}

// A lab run written into .pk/changes/<id>.yaml by hand does not meet the lab
// gate and is not posted as pk/lab success: only the lab store counts.
func TestForgedLabRunInMeta(t *testing.T) {
	h := newPolicyHarness(t)
	labs := &fakeLabs{}
	h.svc.o.Labs = func() lab.Service { return labs }
	ch, err := h.svc.Create(h.agent, CreateRequest{Title: "Cabling", Intent: "x"})
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, ch.Worktree, "control/cabling.yaml", "uplinks: [spine-9]\n")
	if _, err := h.svc.Commit(h.agent, ch.ID, "feat: cabling", nil); err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Open(ch.Worktree).Head(context.Background())
	m := ch.Meta
	m.LabRuns = []LabRunRef{{Run: ch.ID + "/run-7", SHA: head, Result: LabPass}}
	writeT(t, ch.Worktree, MetaPath(ch.ID), string(MarshalMeta(m)))
	if g := h.gatesOf(ch.ID); g.LabGate.Status == "met" {
		t.Fatalf("forged run met the lab gate: %+v", g.LabGate)
	}
	if _, err := h.svc.Commit(h.agent, ch.ID, "chore: meta", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Push(h.agent, ch.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := h.svc.Get(h.dennis, ch.ID)
	sts, _ := h.forge.Statuses(context.Background(), got.HeadSHA)
	for _, st := range sts {
		if st.Context == forge.ContextLab && st.State == forge.StatusSuccess {
			t.Fatalf("pk/lab success posted for a forged run: %+v", st)
		}
	}
	if _, err := h.svc.MarkReady(h.agent, ch.ID); err == nil {
		t.Fatal("marked ready with a forged lab run")
	}
	if len(got.LabRuns) != 1 || got.LabState == "pass" {
		t.Fatalf("lab %s runs %+v", got.LabState, got.LabRuns)
	}

	// The same run known to the lab store counts.
	labs.runs = []lab.Run{{ID: ch.ID + "/run-7", ChangeID: ch.ID, SHA: head, Result: lab.ResultPass}}
	if g := h.gatesOf(ch.ID); g.LabGate.Status != "met" {
		t.Fatalf("real run: %+v", g.LabGate)
	}
}

// A restart while the forge is down: the change list comes from the local
// clone and the last forge poll, marked offline, instead of failing.
func TestStartWithForgeDown(t *testing.T) {
	h := newHarness(t)
	ch := h.change("Bump podinfo", podinfo, "image: podinfo:6.9.0\n")
	if err := h.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := h.svc.o
	o.RemoteURL = filepath.Join(t.TempDir(), "gone.git")
	o.Forge = nil
	svc := New(o)
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("init with the forge down: %v", err)
	}
	if !svc.ForgeOffline() {
		t.Fatal("not offline")
	}
	list, err := svc.List(h.dennis, true)
	if err != nil {
		t.Fatal(err)
	}
	var got *Summary
	for i := range list {
		if list[i].ID == ch.ID {
			got = &list[i]
		}
	}
	if got == nil || got.State != Review {
		t.Fatalf("change from the cache: %+v (list %d)", got, len(list))
	}
	if _, err := svc.Get(h.dennis, ch.ID); err != nil {
		t.Fatalf("get offline: %v", err)
	}
}
