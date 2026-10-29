package gitea

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
)

func TestSplitDraft(t *testing.T) {
	for in, want := range map[string]struct {
		title string
		draft bool
	}{
		"WIP: Add cn-035":   {"Add cn-035", true},
		"[WIP] Add cn-035":  {"Add cn-035", true},
		"Add cn-035":        {"Add cn-035", false},
		"  WIP:Add cn-035 ": {"Add cn-035", true},
	} {
		title, draft := SplitDraft(in)
		if title != want.title || draft != want.draft {
			t.Errorf("SplitDraft(%q) = %q %v", in, title, draft)
		}
	}
	if got := draftTitle("WIP: x", false); got != "x" {
		t.Errorf("draftTitle = %q", got)
	}
	if got := draftTitle("x", true); got != "WIP: x" {
		t.Errorf("draftTitle = %q", got)
	}
}

// TestLive runs against a real Gitea when PK_TEST_GITEA_URL is set (with
// admin pk-admin / pk-admin-demo).
func TestLive(t *testing.T) {
	base := os.Getenv("PK_TEST_GITEA_URL")
	if base == "" {
		t.Skip("PK_TEST_GITEA_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := New(Options{URL: base, Owner: "pktest", Repo: "live", AdminUser: "pk-admin", AdminPassword: "pk-admin-demo"})
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"t-dennis", "t-anna"} {
		if err := c.EnsureUser(ctx, forge.User{Login: u, FullName: u, Password: "pk-test-pass"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateToken(ctx, u, "pk-test-pass", "pk-test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EnsureOrg(ctx, "pktest"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRepo(ctx, "pktest", "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureRepo(ctx, "pktest", "live", false); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"t-dennis", "t-anna"} {
		if err := c.AddCollaborator(ctx, u, "write"); err != nil {
			t.Fatal(err)
		}
	}
	tok, _ := c.Token("t-dennis")
	dir := t.TempDir()
	r, err := gitx.Init(ctx, filepath.Join(dir, "w"), false)
	if err != nil {
		t.Fatal(err)
	}
	sig := gitx.Signature{Name: "Dennis", Email: "t-dennis@pk.local"}
	_ = os.WriteFile(filepath.Join(r.Dir, "a.txt"), []byte("a\n"), 0o644)
	_ = r.Add(ctx)
	if _, err := r.Commit(ctx, gitx.CommitOptions{Message: "initial", Author: sig}); err != nil {
		t.Fatal(err)
	}
	_ = r.Config(ctx, "remote.origin.url", c.Repo().CloneURL)
	if err := r.Push(ctx, gitx.PushOptions{Refspecs: []string{"main:main"}, User: "t-dennis", Token: tok}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, "checkout", "-q", "-b", "change/c-aaaaaa-test"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(r.Dir, "b.txt"), []byte("b\n"), 0o644)
	_ = r.Add(ctx)
	head, err := r.Commit(ctx, gitx.CommitOptions{Message: "add b", Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Push(ctx, gitx.PushOptions{Refspecs: []string{"change/c-aaaaaa-test:change/c-aaaaaa-test"}, User: "t-dennis", Token: tok}); err != nil {
		t.Fatal(err)
	}
	pr, err := c.CreatePR(ctx, "t-dennis", forge.PRCreate{Title: "Test", Body: "intent\n\nPk-Change: c-aaaaaa", Head: "change/c-aaaaaa-test", Base: "main", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Draft || pr.Title != "Test" || pr.HeadSHA != head {
		t.Fatalf("pr = %+v", pr)
	}
	if found, err := c.FindPR(ctx, "change/c-aaaaaa-test"); err != nil || found.Number != pr.Number {
		t.Fatalf("FindPR = %v, %v", found, err)
	}
	draft := false
	if pr, err = c.UpdatePR(ctx, "t-dennis", pr.Number, forge.PRUpdate{Draft: &draft}); err != nil || pr.Draft {
		t.Fatalf("UpdatePR = %+v, %v", pr, err)
	}
	if _, err := c.Approve(ctx, "t-dennis", pr.Number, ""); err == nil {
		t.Fatal("self approval must fail")
	}
	if _, err := c.Approve(ctx, "t-anna", pr.Number, "ok"); err != nil {
		t.Fatal(err)
	}
	rs, err := c.ListReviews(ctx, pr.Number)
	if err != nil || len(rs) != 1 || rs[0].User != "t-anna" || rs[0].State != forge.ReviewApproved || rs[0].SHA != head {
		t.Fatalf("reviews = %+v, %v", rs, err)
	}
	if err := c.SetStatus(ctx, "t-dennis", head, forge.CommitStatus{Context: forge.ContextPolicy, State: forge.StatusPending, Description: "wait"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := c.SetStatus(ctx, "t-dennis", head, forge.CommitStatus{Context: forge.ContextPolicy, State: forge.StatusSuccess, Description: "ok"}); err != nil {
		t.Fatal(err)
	}
	sts, err := c.Statuses(ctx, head)
	if err != nil || len(sts) != 1 || sts[0].State != forge.StatusSuccess {
		t.Fatalf("statuses = %+v, %v", sts, err)
	}
	if err := c.SetBranchProtection(ctx, forge.BranchProtection{Branch: "main", RequiredChecks: []string{forge.ContextPolicy},
		MergeWhitelist: []string{"t-dennis"}, ApprovalsWhitelist: []string{"t-anna"}, RequiredApprovals: 1, BlockOnOutdated: true}); err != nil {
		t.Fatal(err)
	}
	bp, err := c.GetBranchProtection(ctx, "main")
	if err != nil || !bp.BlockOnOutdated || len(bp.RequiredChecks) != 1 {
		t.Fatalf("protection = %+v, %v", bp, err)
	}
	if _, err := c.Merge(ctx, "t-anna", pr.Number, forge.MergeOptions{Title: "x", Message: "y"}); err == nil {
		t.Fatal("merge by a user outside the whitelist must fail")
	} else {
		t.Logf("merge by t-anna: %v", err)
	}
	msg := "intent line\n\nPk-Change: c-aaaaaa\nApproved-by: t-anna\nReviewed-in: !1"
	res, err := c.Merge(ctx, "t-dennis", pr.Number, forge.MergeOptions{Title: "Merge change c-aaaaaa: Test (!1)", Message: msg, HeadSHA: head, DeleteBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.FetchAuth(ctx, "t-dennis", tok, "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	log, err := r.Log(ctx, "origin/main", "-n", "1")
	if err != nil || log[0].SHA != res.SHA {
		t.Fatalf("log = %+v, %v", log, err)
	}
	full := log[0].Subject + "\n\n" + log[0].Body
	if !strings.HasPrefix(full, "Merge change c-aaaaaa: Test (!1)\n\nintent line") {
		t.Fatalf("merge message = %q", full)
	}
	var found bool
	for _, tr := range log[0].Trailers {
		if tr.Key == "Pk-Change" && tr.Value == "c-aaaaaa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trailers = %+v in %q", log[0].Trailers, full)
	}
	if _, err := c.ListBranches(ctx, "change/"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBranch(ctx, "t-dennis", "change/c-aaaaaa-test"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("branch should be gone: %v", err)
	}
}
