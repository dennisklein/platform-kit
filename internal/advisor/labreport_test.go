package advisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// p4x8maTools serves the seeded c-p4x8ma: a failed run that exists only as
// a KB report.
type p4x8maTools struct{ *fakeTools }

const p4Report = "kb/labs/c-p4x8ma-run-1.md"

func (f p4x8maTools) Change(ctx context.Context, id string) (*api.Change, error) {
	if id != "c-p4x8ma" {
		return f.fakeTools.Change(ctx, id)
	}
	ch := &api.Change{Id: id, Title: "BGP peers for org-a and org-b", Author: "anna", State: api.ChangeStateLab, Target: "main",
		HeadSha: "p4head", LabState: api.LabStateFail,
		LabRuns: []api.LabRunRef{{Run: "c-p4x8ma/run-1", Result: api.LabResultFail, Sha: "p4head", Report: p4Report}}}
	ch.Gates.LabGate.Required = true
	ch.Gates.LabGate.Status = api.LabGateStatusFailed
	return ch, nil
}

func newP4Tools(t *testing.T) p4x8maTools {
	b, err := os.ReadFile(filepath.Join("..", "..", "seed", "changes", "10-c-p4x8ma", "after", p4Report))
	if err != nil {
		t.Fatal(err)
	}
	ft := newFakeTools()
	delete(ft.runs, "c-p4x8ma")
	ft.files["c-p4x8ma\x00"+p4Report] = string(b)
	return p4x8maTools{ft}
}

const p4Want = "run-1 failed: sessions to 10.6.0.3 and 10.6.0.4 not established; the lab emulates only the router 10.6.0.1."

func TestRisksNameTheLabFailures(t *testing.T) {
	log := llm.NewFileLog(filepath.Join(t.TempDir(), "log.jsonl"))
	for _, recipe := range []string{Risks, LabGaps} {
		e := New(Options{Tools: newP4Tools(t), LLM: llm.NewMock(nil), Log: log})
		res, err := e.Run(testCtx(), recipe, "c-p4x8ma")
		if err != nil {
			t.Fatal(err)
		}
		var lab *Point
		for i := range res.Points {
			if res.Points[i].Title == "Lab failed" {
				lab = &res.Points[i]
			}
		}
		if lab == nil {
			t.Fatalf("%s: no lab point in\n%s", recipe, pointTexts(res.Points))
		}
		if lab.Text != p4Want {
			t.Errorf("%s: text %q", recipe, lab.Text)
		}
		var kinds []string
		for _, s := range lab.Sources {
			kinds = append(kinds, s.Kind+":"+s.Label)
		}
		if strings.Join(kinds, " ") != "lab:lab run-1 kb:c-p4x8ma-run-1.md" {
			t.Errorf("%s: sources %v", recipe, kinds)
		}
		found := false
		for _, s := range res.Reading {
			found = found || s.Ref == p4Report
		}
		if !found {
			t.Errorf("%s: report not in reading %+v", recipe, res.Reading)
		}
	}
}

func TestFailureText(t *testing.T) {
	for in, want := range map[string]string{
		"bgp: session to 10.6.0.3 not established|bgp: session to 10.6.0.4 not established": "sessions to 10.6.0.3 and 10.6.0.4 not established",
		"cn-036: no BGP peering policy selects it":                                          "cn-036: no BGP peering policy selects it",
		"bgp: session to 10.6.0.3 not established|nodes: cn-035 not Ready":                  "session to 10.6.0.3 not established; cn-035 not Ready",
		"": "",
	} {
		var fails []string
		if in != "" {
			fails = strings.Split(in, "|")
		}
		if got := failureText(fails); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
}

func TestReportWhy(t *testing.T) {
	if got := reportWhy("---\nx: y\n---\n# T\n\n- FAIL: a\n"); got != "" {
		t.Errorf("list end: %q", got)
	}
	if got := reportWhy("# T\n\nThe lab emulates only\nthe router 10.6.0.1. Add a peer."); got != "The lab emulates only the router 10.6.0.1." {
		t.Errorf("why %q", got)
	}
}
