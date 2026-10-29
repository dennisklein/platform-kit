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
		want := p4Want
		if recipe == LabGaps {
			// The "Lab gap" point names what the lab emulates.
			want = "run-1 failed: sessions to 10.6.0.3 and 10.6.0.4 not established."
		}
		if lab.Text != want {
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
		"bgp: session to 10.6.0.3 not established|bgp: session to 10.6.0.4 not established":                                        "sessions to 10.6.0.3 and 10.6.0.4 not established",
		"cn-036: no BGP peering policy selects it":                                                                                 "cn-036: no BGP peering policy selects it",
		"bgp: session to 10.6.0.3 not established|nodes: cn-035 not Ready":                                                         "session to 10.6.0.3 not established; cn-035 not Ready",
		"cn-040: mgmt has ports 1 to 48, not 49|cn-040: data-b has ports 1 to 48, not 49|cn-040: data-a has ports 1 to 48, not 50": "cn-040: mgmt has ports 1 to 48, not 49; data-b has ports 1 to 48, not 49; data-a has ports 1 to 48, not 50",
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
	if got := reportWhy("# T\n\nRaw results (steps) stay on the bench in `labs/c/run-1/`."); got != "" {
		t.Errorf("raw results: %q", got)
	}
}

func TestLabGapsOfRecordedRun(t *testing.T) {
	e := New(Options{Tools: newP4Tools(t), LLM: llm.NewMock(nil), Log: llm.NewFileLog(filepath.Join(t.TempDir(), "log.jsonl"))})
	res, err := e.Run(testCtx(), LabGaps, "c-p4x8ma")
	if err != nil {
		t.Fatal(err)
	}
	all := pointTexts(res.Points)
	for _, want := range []string{
		"What the lab tests: run-1 failed. Its report lists topology, BMC, install and join as passed, BGP as failed and checks as skipped.",
		"Lab gap: The lab emulates only the router 10.6.0.1, so the BGP peers 10.6.0.3 and 10.6.0.4 are not tested.",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	for _, bad := range []string{"not on this bench", "run-1 fail ("} {
		if strings.Contains(all, bad) {
			t.Errorf("%q in\n%s", bad, all)
		}
	}
}

// sameCount renders a change that keeps the number of objects.
type sameCount struct{ *fakeTools }

func (f sameCount) Impact(ctx context.Context, id string) (*api.Impact, error) {
	imp, err := f.fakeTools.Impact(ctx, id)
	if err == nil && len(imp.Clusters) > 0 {
		imp.Clusters[0].ObjectsBase, imp.Clusters[0].ObjectsHead = 21, 21
	}
	return imp, err
}

func TestExplainSameObjectCount(t *testing.T) {
	e := New(Options{Tools: sameCount{newFakeTools()}, LLM: llm.NewMock(nil), Log: llm.NewFileLog(filepath.Join(t.TempDir(), "log.jsonl"))})
	res, err := e.Run(testCtx(), ExplainDiff, demoID)
	if err != nil {
		t.Fatal(err)
	}
	all := pointTexts(res.Points)
	if !strings.Contains(all, "The object count stays at 21.") || strings.Contains(all, "from 21 to 21") {
		t.Fatalf("%s", all)
	}
}
