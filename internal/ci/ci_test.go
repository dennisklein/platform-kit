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
	g.Mergeable = false
	g.Blockers = []string{"approvals 1 of 2", "behind main by 1"}
	st = Status(&g, true)
	if st.State != forge.StatusFailure || st.Description != "blocked: approvals 1 of 2; behind main by 1; draft" {
		t.Fatalf("%+v", st)
	}
	g.Blockers = []string{strings.Repeat("x", 300)}
	if st = Status(&g, false); len(st.Description) > 240 {
		t.Fatalf("description too long: %d", len(st.Description))
	}
}
