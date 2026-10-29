package seed

import (
	"reflect"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

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
