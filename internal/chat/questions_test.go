package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

func seedState(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "seed", "state", p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func platformFake(t *testing.T) *fake {
	f := newFake(t)
	for _, p := range []string{
		"control/talos/defiant/talconfig.yaml", "control/talos/phoenix/talconfig.yaml",
		"clusters/defiant/cluster.yaml", "clusters/pegasus/cluster.yaml", "clusters/phoenix/cluster.yaml",
		"clusters/defiant/infra/cilium/helmrelease.yaml",
	} {
		f.files["main\x00"+p] = seedState(t, p)
		f.files[cid+"\x00"+p] = seedState(t, p)
	}
	for _, n := range []string{"cn-025", "cn-026", "cp-01", "cp-02", "cp-03"} {
		p := "control/inventory/" + n + ".yaml"
		f.files[cid+"\x00"+p] = seedState(t, p)
	}
	return f
}

func TestPlatformVersions(t *testing.T) {
	s, _, _ := newSvc(t, platformFake(t), llm.NewMock(nil))
	for _, c := range []struct{ q, want string }{
		{"Which Kubernetes version runs on defiant?", "**defiant** runs Kubernetes **v1.33.4** on Talos **v1.10.6**. That is the desired state on main, in `control/talos/defiant/talconfig.yaml`"},
		{"Which Talos version runs on defiant?", "**defiant** runs Talos **v1.10.6**, with Kubernetes **v1.33.4**."},
		{"What k8s version is phoenix on?", "**phoenix** runs Kubernetes **v1.33.4**"},
		{"What Kubernetes version do the clusters run?", "Both clusters with a talconfig, defiant and phoenix, run Kubernetes **v1.33.4** on Talos **v1.10.6**."},
		{"Which Talos version runs on pegasus?", "**pegasus** has no talconfig in the state repo"},
		{"Is defiant on the latest Kubernetes?", "I can't check upstream releases"},
	} {
		a, err := s.Ask(ctxAs("dennis"), ask(c.q))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(a.Text, c.want) || strings.Contains(a.Text, "cilium") {
			t.Errorf("%q: %q", c.q, a.Text)
		}
		if len(a.Sources) == 0 || a.Sources[0].Label != "talconfig.yaml" {
			t.Errorf("%q: sources %+v", c.q, a.Sources)
		}
		noFailedStep(t, a)
	}
	// The edit names the field of the talconfig.
	a, _ := s.Ask(ctxAs("dennis"), ask("Upgrade Kubernetes on defiant to v1.34.1"))
	if !strings.Contains(a.Text, "`control/talos/defiant/talconfig.yaml` (`kubernetesVersion`, now v1.33.4)") {
		t.Errorf("edit: %q", a.Text)
	}
}

func TestCountNodesByRole(t *testing.T) {
	s, _, _ := newSvc(t, platformFake(t), llm.NewMock(nil))
	a, _ := s.Ask(ctxAs("dennis"), ask("How many control plane nodes does phoenix have?"))
	if a.Text != "**phoenix** has **3 control plane nodes** (cp-01, cp-02 and cp-03)." {
		t.Errorf("%q", a.Text)
	}
	a, _ = s.Ask(ctxAs("dennis"), ask("How many worker nodes are there?"))
	if !strings.HasPrefix(a.Text, "The inventory has **2 worker nodes** (cn-025 and cn-026).") {
		t.Errorf("%q", a.Text)
	}
}

func TestFileAnswerNamesTheQuestion(t *testing.T) {
	const hr = "clusters/defiant/infra/cilium/helmrelease.yaml"
	h := llm.Hit{Document: llm.Document{Kind: "file", Ref: hr, Path: hr, Text: seedState(t, hr)}}
	q := "Which Kubernetes version runs on defiant?"
	if got := fileAnswer(h, q, contentTerms(q)); got != "The file `"+hr+"` on main matches best. Open it from the source below." {
		t.Errorf("kubernetes: %q", got)
	}
	q = "Which chart does cilium use on defiant?"
	if got := fileAnswer(h, q, contentTerms(q)); !strings.HasPrefix(got, "**cilium** is a HelmRelease on defiant: chart cilium **1.18.2**") {
		t.Errorf("cilium: %q", got)
	}
	const tc = "control/talos/defiant/talconfig.yaml"
	got, ok := fileSummary(tc, seedState(t, tc), nil)
	if !ok || got != "`talconfig.yaml` pins Talos **v1.10.6** and Kubernetes **v1.33.4** for the cluster **defiant** (`talosVersion`, `kubernetesVersion`)." {
		t.Errorf("talconfig: %v %q", ok, got)
	}
}

func TestWhyInsideTheQuestion(t *testing.T) {
	s, _, _ := newSvc(t, newFake(t), llm.NewMock(nil))
	for _, q := range []string{
		"Can you tell me why cn-003 uses port 37 on data-a?",
		"Do you know why cn-003 is on data-a 37?",
		"A slow question: why does cn-003 use port 37?",
		"How come cn-003 is on data-a 37?",
	} {
		a, _ := s.Ask(ctxAs("dennis"), ask(q))
		if !strings.HasPrefix(a.Text, "c-2hd7vw moved cn-003 p2") {
			t.Errorf("%q: %q", q, a.Text)
		}
	}
}

func TestExplainNeedsAChange(t *testing.T) {
	for q, want := range map[string]string{
		"Explain BGP in one sentence":  "",
		"Explain how Flux works":       "",
		"Explain this change":          advisor.ExplainDiff,
		"Explain my change":            advisor.ExplainDiff,
		"Can you explain c-6m1pqd?":    advisor.ExplainDiff,
		"What does the rendered diff?": advisor.ExplainDiff,
	} {
		if got := recipeFor(q); got != want {
			t.Errorf("%q: got %q, want %q", q, got, want)
		}
	}
	f := newFake(t)
	f.hits = []llm.Hit{{Document: llm.Document{Kind: "file", Ref: bgpPath, Title: "rack-6z", Text: seedBGP(t)}}}
	s, _, _ := newSvc(t, f, llm.NewMock(nil))
	a, _ := s.Ask(ctxAs("dennis"), ask("Explain BGP in one sentence"))
	if !strings.HasPrefix(a.Text, "**BGP** (Border Gateway Protocol) is") || !strings.Contains(a.Text, "`bgp-rack-6z.yaml`") {
		t.Errorf("%q", a.Text)
	}
	for _, st := range a.Steps {
		if st.Tool == "advise" {
			t.Errorf("explain-diff ran: %+v", a.Steps)
		}
	}
	// A question about the project is no glossary question.
	if s.glossaryQuestion(ctxAs("dennis"), &progress{now: time.Now, sleep: sleepCtx}, Request{}, "What is the Ceph OSD layout on pegasus?", &Answer{}) {
		t.Error("glossary answered a project question")
	}
}

func TestModelAdviseTakesTheIDOfTheQuestion(t *testing.T) {
	m := &script{steps: []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("1", "advise", map[string]string{"recipe": "risks"})}},
		{Role: llm.RoleAssistant, Content: "c-p4x8ma has a failed lab."},
	}}
	dir := t.TempDir()
	adv := points{pts: []advisor.Point{{Title: "Lab failed", Text: "run-1 failed.", Severity: advisor.SevRisk}}}
	s, err := New(Options{Tools: newFake(t), Advisor: adv, LLM: m, Path: filepath.Join(dir, "s.json")})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Ask(ctxAs("dennis"), Request{Messages: []Message{{Role: "user", Content: "Is it safe to merge c-p4x8ma?"}}})
	if err != nil {
		t.Fatal(err)
	}
	noFailedStep(t, a)
	if len(a.Steps) == 0 || a.Steps[0].Label != "advise risks c-p4x8ma" {
		t.Errorf("steps %+v", a.Steps)
	}
	if len(a.Sources) == 0 || a.Sources[0].Ref != "c-p4x8ma" {
		t.Errorf("sources %+v", a.Sources)
	}
}

type storyFake struct{ *fake }

func (f storyFake) Change(ctx context.Context, id string) (*api.Change, error) {
	switch id {
	case "c-p4x8ma":
		return &api.Change{Id: id, Title: "BGP peers for org-a and org-b", Author: "anna", State: api.ChangeStateLab, LabState: api.LabStateFail,
			LabRun: "c-p4x8ma/run-1", Intent: "Peer rack 6Z with the org-a and org-b routers"}, nil
	case "c-2hd7vw":
		ch := &api.Change{Id: id, Title: "Move cn-003 to data-a 37", Author: "jonas", State: api.ChangeStateVerified,
			MergedAt: time.Date(2026, 9, 12, 10, 15, 0, 0, time.UTC)}
		ch.Mr.Number = 2
		ch.ApprovalsBy = []api.Approval{{User: "anna", Counts: true}, {User: "dennis", Counts: true}}
		return ch, nil
	}
	return f.fake.Change(ctx, id)
}

func (f storyFake) LabRuns(ctx context.Context, id string) ([]api.LabRun, error) {
	if id == "c-p4x8ma" {
		return []api.LabRun{{Id: "c-p4x8ma/run-1", Failures: []string{"session to 10.6.0.3 not established"}}}, nil
	}
	return nil, nil
}

func storySvc(t *testing.T) *Svc {
	dir := t.TempDir()
	s, err := New(Options{Tools: storyFake{newFake(t)}, LLM: llm.NewMock(nil), Path: filepath.Join(dir, "s.json")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFollowUpsAnswerTheNarrowerQuestion(t *testing.T) {
	s := storySvc(t)
	ctx := ctxAs("dennis")
	first, _ := s.Ask(ctx, Request{Messages: conv("What is c-p4x8ma?")})
	lab, _ := s.Ask(ctx, Request{Messages: conv("What is c-p4x8ma?", first.Text, "Why did its lab fail?")})
	if lab.Text != "Lab run-1 of **c-p4x8ma** failed: session to 10.6.0.3 not established." || lab.Text == first.Text {
		t.Errorf("lab %q", lab.Text)
	}
	who, _ := s.Ask(ctx, Request{Messages: conv("Who approved c-2hd7vw?")})
	when, _ := s.Ask(ctx, Request{Messages: conv("Who approved c-2hd7vw?", who.Text, "And when was it merged?")})
	if !strings.HasPrefix(who.Text, "anna and dennis approved **c-2hd7vw**") {
		t.Errorf("who %q", who.Text)
	}
	if when.Text != "**c-2hd7vw** (*Move cn-003 to data-a 37*) was merged on **2026-09-12** in MR !2; jonas wrote it." {
		t.Errorf("when %q", when.Text)
	}
}
