package ci

import (
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/policy"
)

func TestStatus(t *testing.T) {
	var g policy.Gates
	g.Mergeable = true
	g.Approvals.Have, g.Approvals.Need = 2, 2
	g.LabGate.Status = "met"
	g.Risk.Level = policy.RiskHigh
	st := Status(&g, false)
	if st.State != forge.StatusSuccess || st.Context != forge.ContextPolicy || st.Description != "policy gates met: approvals 2 of 2; lab met; risk high" {
		t.Fatalf("%+v", st)
	}
	// Only waiting for approvals, a rebase and the draft: pending, not red.
	g.Mergeable = false
	g.PathRights.OK = true
	g.Approvals.Have = 1
	g.Rebased.Behind = 1
	g.Blockers = []string{"approvals 1 of 2", "behind main by 1"}
	st = Status(&g, true)
	if st.State != forge.StatusPending || st.Description != "waiting: approvals 1 of 2; behind main by 1; draft" {
		t.Fatalf("%+v", st)
	}
	// A lab run still to do waits too.
	g.Rebased.OK, g.Approvals.Have = true, 2
	g.LabGate.Required, g.LabGate.Status = true, "needed"
	g.Blockers = []string{"lab gate: no lab run on the head"}
	if st = Status(&g, false); st.State != forge.StatusPending {
		t.Fatalf("lab needed: %+v", st)
	}
	// A failed lab and a path-rights violation fail.
	g.LabGate.Status = "failed"
	g.Blockers = []string{"lab gate: c-x/run-1 failed"}
	if st = Status(&g, false); st.State != forge.StatusFailure || st.Description != "blocked: lab gate: c-x/run-1 failed" {
		t.Fatalf("lab failed: %+v", st)
	}
	g.LabGate.Status = "met"
	g.PathRights.OK = false
	g.Blockers = []string{"path rights: bot may not write control/x"}
	if st = Status(&g, false); st.State != forge.StatusFailure {
		t.Fatalf("path rights: %+v", st)
	}
	// Other blockers (no changed files) fail.
	g.PathRights.OK = true
	g.Blockers = []string{"no changed files"}
	if st = Status(&g, false); st.State != forge.StatusFailure {
		t.Fatalf("no files: %+v", st)
	}
	g.Blockers = []string{strings.Repeat("x", 300)}
	if st = Status(&g, false); len(st.Description) > 240 {
		t.Fatalf("description too long: %d", len(st.Description))
	}
}
