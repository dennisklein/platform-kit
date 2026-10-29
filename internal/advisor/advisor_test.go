package advisor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/llm"
)

func testCtx() context.Context {
	return identity.With(context.Background(), identity.Identity{User: "dennis", Kind: identity.Human})
}

func newTestEngine(t *testing.T, ft *fakeTools, client llm.Client) (*Engine, *llm.FileLog, *memStore) {
	t.Helper()
	log := llm.NewFileLog(filepath.Join(t.TempDir(), "log.jsonl"))
	store := &memStore{}
	return New(Options{Tools: ft, LLM: client, Log: log, Store: store, Kinds: all.Registry()}), log, store
}

func pointTexts(ps []Point) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(string(p.Severity) + " " + p.Title + ": " + p.Text + "\n")
	}
	return b.String()
}

func TestRisksMock(t *testing.T) {
	e, log, _ := newTestEngine(t, newFakeTools(), llm.NewMock(nil))
	res, err := e.Run(testCtx(), Risks, demoID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Mock || !res.Fallback {
		t.Errorf("mock answer: mock %v fallback %v", res.Mock, res.Fallback)
	}
	all := pointTexts(res.Points)
	t.Log("\n" + all + res.Note)
	for _, want := range []string{
		"c-p4x8ma (anna) also changes CiliumBGPPeeringPolicy/rack-6z: 2 new BGP peers.",
		"Until c-p4x8ma merges, cn-035 and cn-036 peer only with 10.6.0.1/32.",
		"Its last lab run failed: BGP session to 10.6.0.3 not established.",
		"cn-036 is in the inventory of this change, in GC5A-6Z, but not in the BGP policy rack-6z.",
		"c-2hd7vw (jonas, 2026-09-12) changed cn-003 p2: data-b 37 to data-a 37, to free data-b ports for rack 6Z. This change uses data-b 37 (freed then) and data-a 38 (next to data-a 37).",
		"run-1 tested one data link down, not both.",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(all, "c-a5e3vk") {
		t.Error("serial sync is not port history")
	}
	if res.Note != `Advisory only. The rating stays "high" from the policy.` {
		t.Errorf("note %q", res.Note)
	}
	if res.Model != "mock" || !res.Mock || res.LogID == "" {
		t.Errorf("model %q mock %v log %q", res.Model, res.Mock, res.LogID)
	}
	for _, p := range res.Points {
		if len(p.Sources) == 0 {
			t.Errorf("point without sources: %s", p.Text)
		}
	}
	e2, _ := log.Get(context.Background(), res.LogID)
	if e2.Surface != llm.SurfaceAdvisor || e2.User != "dennis" || e2.Recipe != Risks || !strings.Contains(e2.Prompt, "<data>") {
		t.Errorf("log %+v", e2)
	}
}

func TestCheckCommitSuggestsBGPFix(t *testing.T) {
	ft := newFakeTools()
	e, _, store := newTestEngine(t, ft, llm.NewMock(nil))
	res, err := e.Run(testCtx(), CheckCommit, demoID)
	if err != nil {
		t.Fatal(err)
	}
	all := pointTexts(res.Points)
	if !strings.Contains(all, "cn-036 is in the inventory of this change, in GC5A-6Z, but not in the BGP policy rack-6z") {
		t.Fatalf("points:\n%s", all)
	}
	if len(res.Suggestions) != 1 || len(store.list) != 1 {
		t.Fatalf("suggestions %+v", res.Suggestions)
	}
	s := res.Suggestions[0]
	if s.Path != bgpPath || s.By != "advisor:check-commit" || s.ChangeID != demoID || s.LogID != res.LogID {
		t.Errorf("suggestion %+v", s)
	}
	if !strings.Contains(s.After, "          - cn-035\n          - cn-036\n") || strings.Count(s.After, "cn-036") != 1 {
		t.Errorf("after:\n%s", s.After)
	}
	if res.Points[0].Severity != SevRisk {
		t.Errorf("first point %+v", res.Points[0])
	}
}

func TestCommitMessageNodes(t *testing.T) {
	ft := newFakeTools()
	ft.status = []api.FileStatus{
		{Path: "control/inventory/cn-035.yaml", Status: api.FileChangeUntracked},
		{Path: "control/inventory/cn-036.yaml", Status: api.FileChangeUntracked},
	}
	e, log, _ := newTestEngine(t, ft, llm.NewMock(nil))
	res, err := e.CommitMessage(testCtx(), demoID, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "feat(inventory): add cn-035 and cn-036 in GC5A-6Z\n\n- 2 compute nodes for defiant, U6 and U8\n- bmc and data ports on mgmt, data-a and data-b"
	if res.Message != want {
		t.Fatalf("got\n%s\nwant\n%s", res.Message, want)
	}
	entry, _ := log.Get(context.Background(), res.LogID)
	if entry.Surface != llm.SurfaceCommitMessage {
		t.Errorf("surface %s", entry.Surface)
	}
}

// Edits of files that an earlier commit added: the message names the edit,
// not "new compute node" (the impact compares with the base of the change).
func TestCommitMessageEditOfCommittedNodes(t *testing.T) {
	ft := newFakeTools()
	for _, n := range []string{"cn-035", "cn-036"} {
		p := "control/inventory/" + n + ".yaml"
		ft.files["headsha\x00"+p] = ft.files[demoID+"\x00"+p]
		ft.files[demoID+"\x00"+p] = strings.Replace(ft.files[demoID+"\x00"+p], "  role: worker\n", "  role: worker\n  model: Supermicro SYS-220U-TNR\n", 1)
	}
	ft.status = []api.FileStatus{
		{Path: "control/inventory/cn-035.yaml", Status: api.FileChangeModified},
		{Path: "control/inventory/cn-036.yaml", Status: api.FileChangeModified},
	}
	e, _, _ := newTestEngine(t, ft, llm.NewMock(nil))
	res, err := e.CommitMessage(testCtx(), demoID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Message, "new compute node") || strings.Contains(res.Subject, "add ") {
		t.Fatalf("message names the earlier commit:\n%s", res.Message)
	}
	if res.Subject != "chore(inventory): update cn-035 and cn-036" || !strings.Contains(res.Body, "- cn-036: model") {
		t.Fatalf("message:\n%s", res.Message)
	}
}

func TestCommitMessageBGP(t *testing.T) {
	ft := newFakeTools()
	e, _, _ := newTestEngine(t, ft, llm.NewMock(nil))
	res, err := e.CommitMessage(testCtx(), demoID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject != "feat(cilium): peer cn-035 in rack-6z" {
		t.Fatalf("subject %q\n%s", res.Subject, res.Message)
	}
	ft.status = nil
	if _, err := e.CommitMessage(testCtx(), demoID, nil); err != ErrNothingToCommit {
		t.Fatalf("empty: %v", err)
	}
}

func TestOtherRecipesMock(t *testing.T) {
	e, _, _ := newTestEngine(t, newFakeTools(), llm.NewMock(nil))
	for _, r := range []string{MergeOrder, LabGaps, ExplainDiff, CommitMessage} {
		res, err := e.Run(testCtx(), r, demoID)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		all := pointTexts(res.Points)
		t.Logf("%s:\n%s", r, all)
		if len(res.Points) == 0 {
			t.Errorf("%s: no points", r)
		}
		switch r {
		case MergeOrder:
			for _, want := range []string{"Merge c-31d0aa (dennis, approved 2 of 2) first.", "c-k7m2qz is behind main by 1: rebase, render and run the lab again",
				"Both edit bgp-rack-6z.yaml", "c-31d0aa → c-k7m2qz → c-p4x8ma"} {
				if !strings.Contains(all, want) {
					t.Errorf("merge-order: missing %q", want)
				}
			}
		case LabGaps:
			if !strings.Contains(all, "c-k7m2qz/run-1 pass, 12 of 12 checks pass") || !strings.Contains(all, "covers cn-036, not cn-035") {
				t.Errorf("lab-gaps:\n%s", all)
			}
		case ExplainDiff:
			for _, want := range []string{"On defiant (tenants cluster, canary ring): Flux creates the Machines cn-035 and cn-036",
				"The BGP policy rack-6z also selects cn-035, so it gets a BGP session.", "from 40 to 42 objects",
				"Check that CiliumBGPPeeringPolicy/rack-6z gains cn-035"} {
				if !strings.Contains(all, want) {
					t.Errorf("explain-diff: missing %q in\n%s", want, all)
				}
			}
		}
	}
	if _, err := e.Run(testCtx(), "nope", demoID); err == nil {
		t.Fatal("unknown recipe ran")
	}
}

// scripted is a model that answers with a fixed text.
type scripted struct {
	answer string
	got    []llm.Request
}

func (s *scripted) Mock() bool { return false }
func (s *scripted) Embed(context.Context, []string) ([][]float32, error) {
	return nil, llm.ErrNoEmbeddings
}
func (s *scripted) Ping(context.Context) error { return nil }
func (s *scripted) Model() string              { return "qwen3" }
func (s *scripted) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	s.got = append(s.got, req)
	return &llm.Response{Model: "qwen3", Message: llm.Message{Role: llm.RoleAssistant, Content: s.answer}}, nil
}

func TestLabGapsPlanned(t *testing.T) {
	ft := newFakeTools()
	ft.runs[demoID] = nil
	e, _, _ := newTestEngine(t, ft, llm.NewMock(nil))
	res, err := e.Run(testCtx(), LabGaps, demoID)
	if err != nil {
		t.Fatal(err)
	}
	all := pointTexts(res.Points)
	for _, want := range []string{"What the lab will test: The lab runs the metal scenario.", "with Talos through the emulated BMC",
		"What it leaves out:", "real hardware"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	var reads []string
	for _, s := range res.Reading {
		reads = append(reads, s.Label)
	}
	if !strings.Contains(strings.Join(reads, "|"), "emulation.yaml") {
		t.Errorf("reading %v", reads)
	}
}

func TestMergeOrderReadsTheOtherChanges(t *testing.T) {
	ft := newFakeTools()
	e, _, _ := newTestEngine(t, ft, llm.NewMock(nil))
	res, err := e.Run(testCtx(), MergeOrder, demoID)
	if err != nil {
		t.Fatal(err)
	}
	var reads []string
	for _, s := range res.Reading {
		reads = append(reads, s.Kind+":"+s.Label)
	}
	got := strings.Join(reads, "|")
	for _, want := range []string{"c-31d0aa", "c-p4x8ma", "policy:"} {
		if !strings.Contains(got, want) {
			t.Errorf("reading lacks %q: %s", want, got)
		}
	}
}

func TestRisksRealModel(t *testing.T) {
	answer, _ := json.Marshal(map[string]any{"points": []map[string]any{
		{"title": "BGP order", "text": "cn-035 and cn-036 peer only with 10.6.0.1 until c-p4x8ma merges.", "severity": "warn", "findings": []string{"f1"}},
		{"title": "Made up", "text": "A point with an unknown finding.", "findings": []string{"f99"}},
	}})
	m := &scripted{answer: "```json\n" + string(answer) + "\n```"}
	e, log, _ := newTestEngine(t, newFakeTools(), m)
	res, err := e.Run(testCtx(), Risks, demoID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mock || res.Fallback || res.Model != "qwen3" || len(res.Points) != 2 || res.Points[0].Title != "BGP order" {
		t.Fatalf("%+v", res)
	}
	if len(res.Points[0].Sources) == 0 || res.Points[0].Sources[0].Label != "bgp-rack-6z.yaml" {
		t.Errorf("sources come from findings: %+v", res.Points[0].Sources)
	}
	if len(res.Points[1].Sources) != 0 || res.Points[1].Severity != SevInfo {
		t.Errorf("unknown finding: %+v", res.Points[1])
	}
	req := m.got[0]
	if req.Messages[0].Role != llm.RoleSystem || !strings.Contains(req.Messages[1].Content, `"id": "f1"`) {
		t.Errorf("prompt %+v", req.Messages)
	}
	entry, _ := log.Get(context.Background(), res.LogID)
	if entry.Model != "qwen3" || entry.Mock {
		t.Errorf("log %+v", entry)
	}

	// A broken answer falls back to the findings.
	m.answer = "I think it is fine."
	res, err = e.Run(testCtx(), Risks, demoID)
	if err != nil || len(res.Points) < 3 || !res.Fallback {
		t.Fatalf("fallback %v %+v", err, res)
	}
	entry, _ = log.Get(context.Background(), res.LogID)
	if entry.Error == "" {
		t.Error("no error logged for the fallback")
	}
}

func TestCommitMessageRealModel(t *testing.T) {
	ft := newFakeTools()
	m := &scripted{answer: "feat(cilium): select cn-035 in rack-6z\n\n* BGP policy rack-6z selects cn-035"}
	e, _, _ := newTestEngine(t, ft, m)
	res, err := e.CommitMessage(testCtx(), demoID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message != "feat(cilium): select cn-035 in rack-6z\n\n- BGP policy rack-6z selects cn-035" || res.Model != "qwen3" {
		t.Fatalf("%q", res.Message)
	}
	if res.Fallback {
		t.Error("model answer marked as fallback")
	}
	m.answer = "Sure! Here is a message: added nodes"
	res, _ = e.CommitMessage(testCtx(), demoID, nil)
	if res.Subject != "feat(cilium): peer cn-035 in rack-6z" || !res.Fallback {
		t.Fatalf("fallback %q %v", res.Subject, res.Fallback)
	}
	// Through the recipe too.
	rr, err := e.Run(testCtx(), CommitMessage, demoID)
	if err != nil || !rr.Fallback {
		t.Fatalf("recipe fallback %v %+v", err, rr)
	}
	if !strings.Contains(m.got[0].Messages[1].Content, "+          - cn-035") {
		t.Errorf("diff not in prompt:\n%s", m.got[0].Messages[1].Content)
	}
}

func TestAgentCallsLogAsMCP(t *testing.T) {
	e, log, _ := newTestEngine(t, newFakeTools(), llm.NewMock(nil))
	ctx := identity.With(context.Background(), identity.Identity{User: "dennis-agent", Kind: identity.Agent, ActsFor: "dennis"})
	res, err := e.Run(ctx, ExplainDiff, demoID)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := log.Get(context.Background(), res.LogID)
	if entry.Surface != llm.SurfaceMCP || entry.User != "dennis-agent" {
		t.Errorf("%+v", entry)
	}
}
