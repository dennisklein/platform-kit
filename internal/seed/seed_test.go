package seed

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/dennisklein/platform-kit/internal/forge/fake"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/policy"
	seedfs "github.com/dennisklein/platform-kit/seed"
)

func TestLoadAndInitialTree(t *testing.T) {
	state, changes, err := Load(seedfs.FS())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 12 {
		t.Fatalf("%d changes", len(changes))
	}
	merged := 0
	for _, c := range changes {
		if c.Merged {
			merged++
			if c.MergedAt.IsZero() || c.MergedAt.Before(c.Date) {
				t.Errorf("%s: mergedAt %v before date %v", c.ID, c.MergedAt, c.Date)
			}
		} else if c.Open == nil {
			t.Errorf("%s: open change without open:", c.ID)
		}
		// Commit times (+7 min each) must stay before the merge.
		n := len(c.commits()) + 2
		if c.Merged && c.Date.Add(7*60*1e9*time.Duration(n)).After(c.MergedAt) {
			t.Errorf("%s: commits run past the merge", c.ID)
		}
	}
	if merged != 8 {
		t.Fatalf("%d merged", merged)
	}
	initial, err := InitialTree(state, changes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := initial["control/inventory/cn-025.yaml"]; ok {
		t.Fatal("cn-025 comes with c-9kq1mf, not in the initial tree")
	}
	if _, ok := initial[".pk/policy.yaml"]; !ok {
		t.Fatal("initial tree lacks the policy")
	}
}

func TestProtection(t *testing.T) {
	state, _, err := Load(seedfs.FS())
	if err != nil {
		t.Fatal(err)
	}
	var p policy.Policy
	if err := yaml.Unmarshal(state[policy.Path], &p); err != nil {
		t.Fatal(err)
	}
	bp := Protection(&p)
	if !reflect.DeepEqual(bp.MergeWhitelist, []string{"dennis", "anna", "jonas"}) {
		t.Fatalf("merge whitelist %v", bp.MergeWhitelist)
	}
	if !reflect.DeepEqual(bp.ApprovalsWhitelist, []string{"dennis", "anna", "jonas", "mara"}) {
		t.Fatalf("approvals whitelist %v", bp.ApprovalsWhitelist)
	}
	if !bp.BlockOnOutdated || bp.EnablePush || bp.RequiredApprovals != 1 || !reflect.DeepEqual(bp.RequiredChecks, []string{"pk/policy"}) {
		t.Fatalf("protection %+v", bp)
	}
	users := people{&p}.users()
	if len(users) != 9 {
		t.Fatalf("users %+v", users)
	}
}

// The seed is deterministic: the same seed bytes give the same commits.
// docs/demo.html and the talk deck cite these shas, so a change to the seed
// (or to how change metadata is marshalled) must update them too.
const (
	seedMainSHA    = "382f3ac82581df5e7c4bddfd201e6887905e36ef" // main after the seed: Merge change c-19f4tb (!8)
	seedC2hd7vwSHA = "d4af128073e3aae894d7c09300f850359d1fc76d" // Merge change c-2hd7vw (!2)
)

func TestSeedSHAs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the seed history with git")
	}
	ctx := context.Background()
	state, changes, err := Load(seedfs.FS())
	if err != nil {
		t.Fatal(err)
	}
	var pol policy.Policy
	if err := yaml.Unmarshal(state[policy.Path], &pol); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	bare, err := gitx.Init(ctx, filepath.Join(work, "remote.git"), true)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New("platform", "state")
	f.SetCloneURL("file://" + bare.Dir)
	b := &builder{g: f, pp: people{&pol}, o: Options{WorkDir: work},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), noWaitMerged: true}
	if err := b.build(ctx, state, changes); err != nil {
		t.Fatal(err)
	}
	if got, _ := bare.RevParse(ctx, "refs/heads/main"); got != seedMainSHA {
		t.Errorf("main = %s, want %s (update docs/demo.html and the deck)", got, seedMainSHA)
	}
	out, err := bare.Run(ctx, "log", "--first-parent", "--format=%H %s", "main")
	if err != nil {
		t.Fatal(err)
	}
	var merge string
	for _, l := range strings.Split(out, "\n") {
		if sha, subj, ok := strings.Cut(l, " "); ok && strings.HasPrefix(subj, "Merge change c-2hd7vw:") {
			merge = sha
		}
	}
	if merge != seedC2hd7vwSHA {
		t.Errorf("c-2hd7vw merge = %s, want %s (update docs/demo.html and the deck)", merge, seedC2hd7vwSHA)
	}
}
