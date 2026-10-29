package advisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// fakeTools is the demo story in memory: change c-k7m2qz adds cn-035 and
// cn-036 (committed) and selects cn-035 in the BGP policy (saved, not
// committed). c-p4x8ma (anna) also changes the policy; c-31d0aa is approved.
type fakeTools struct {
	files    map[string]string // "ref\x00path"
	status   []api.FileStatus
	changed  []api.FileStatus
	runs     map[string][]api.LabRun
	writes   []string
	problems []api.Problem
}

const demoID = "c-k7m2qz"

func seedFile(p string) string {
	b, err := os.ReadFile(filepath.Join("..", "..", "seed", "state", p))
	if err != nil {
		panic(err)
	}
	return string(b)
}

func newFakeTools() *fakeTools {
	ft := &fakeTools{files: map[string]string{}, runs: map[string][]api.LabRun{}}
	bgp := seedFile(bgpPath)
	ft.files["main\x00"+bgpPath] = bgp
	ft.files["headsha\x00"+bgpPath] = bgp
	withCN035, _ := AddHostnames(bgp, "rack-6z", []string{"cn-035"})
	ft.files[demoID+"\x00"+bgpPath] = withCN035
	ft.files[demoID+"\x00control/inventory/cn-035.yaml"] = cn("cn-035", 6, cn035Ports)
	ft.files[demoID+"\x00control/inventory/cn-036.yaml"] = cn("cn-036", 8, strings.NewReplacer("37}", "39}", "38}", "40}", "MGT-0141", "MGT-0142", "DATA-0201", "DATA-0203", "DATA-0202", "DATA-0204").Replace(cn035Ports))
	ft.status = []api.FileStatus{{Path: bgpPath, Status: api.FileChangeModified}}
	ft.changed = []api.FileStatus{
		{Path: "control/inventory/cn-035.yaml", Status: api.FileChangeAdded, Committed: true},
		{Path: "control/inventory/cn-036.yaml", Status: api.FileChangeAdded, Committed: true},
		{Path: bgpPath, Status: api.FileChangeModified},
	}
	ft.runs[demoID] = []api.LabRun{{Id: demoID + "/run-1", Number: 1, Result: api.LabResultPass, Driver: "sim", Sha: "headsha",
		Summary: "12 of 12 checks pass", Steps: []api.LabStep{
			{Type: api.LabStepTypeCheck, Name: "nodes", Title: "check: nodes Ready", Detail: "2 of 2", Status: api.LabStepStatusPass},
			{Type: api.LabStepTypeBmc, Name: "bmc", Title: "bmc: power cycle cn-036", Status: api.LabStepStatusPass},
			{Type: api.LabStepTypeFault, Name: "fault", Title: "fault: data-b link down", Detail: "traffic via data-a", Status: api.LabStepStatusPass},
		}}}
	ft.runs["c-p4x8ma"] = []api.LabRun{{Id: "c-p4x8ma/run-1", Number: 1, Result: api.LabResultFail, Driver: "sim",
		Steps: []api.LabStep{{Type: api.LabStepTypeCheck, Title: "bgp", Status: api.LabStepStatusFail,
			Checks: []api.LabCheck{{Name: "bgp established", Ok: false, Detail: "BGP session to 10.6.0.3 not established"}}}}}}
	return ft
}

func (f *fakeTools) Change(ctx context.Context, id string) (*api.Change, error) {
	if id != demoID {
		return nil, fmt.Errorf("change %s not found", id)
	}
	ch := &api.Change{Id: demoID, Title: "Add cn-035 and cn-036 to defiant", Intent: "2 compute nodes for tenants, in GC5A-6Z U6 and U8",
		Author: "dennis", State: api.ChangeStateDraft, Target: "main", HeadSha: "headsha", ChangedFiles: f.changed,
		Commits: []api.Commit{{Sha: "a41f2c9aaaa", Subject: "feat(inventory): add cn-035 and cn-036 in GC5A-6Z", Date: time.Now()}},
		OverlapsDetail: []api.Overlap{{Change: "c-p4x8ma", By: "anna", State: api.ChangeStateLab,
			Objects: []api.ObjectRef{{Cluster: "defiant", ApiVersion: "cilium.io/v2alpha1", Kind: "CiliumBGPPeeringPolicy", Name: "rack-6z"}}}},
	}
	ch.Gates.Risk.Level = api.High
	ch.Gates.LabGate.Required = true
	ch.Gates.LabGate.Status = api.LabGateStatusMet
	return ch, nil
}

func (f *fakeTools) Changes(ctx context.Context) ([]api.ChangeSummary, error) {
	return []api.ChangeSummary{
		{Id: demoID, Title: "Add cn-035 and cn-036 to defiant", Author: "dennis", State: api.ChangeStateDraft, Target: "main", LabState: api.LabStatePass},
		{Id: "c-31d0aa", Title: "Cabling for GC5A-6Z", Author: "dennis", State: api.ChangeStateApproved, Target: "main", Approvals: api.ApprovalCount{Have: 2, Need: 2}, Mr: api.MergeRequestRef{Number: 9}},
		{Id: "c-p4x8ma", Title: "BGP peers for org-a and org-b", Author: "anna", State: api.ChangeStateLab, Target: "main", LabState: api.LabStateFail, Mr: api.MergeRequestRef{Number: 10, Url: "http://forge/pr/10"}},
	}, nil
}

func (f *fakeTools) Impact(ctx context.Context, id string) (*api.Impact, error) {
	if id == "c-p4x8ma" {
		return &api.Impact{Semantic: []api.SemanticLine{{Subject: "rack-6z", Text: "2 new BGP peers", Op: api.OpChange, Path: bgpPath}}}, nil
	}
	imp := &api.Impact{ChangeId: id, Clusters: []api.ClusterImpact{{Cluster: "defiant", Role: "tenants", Ring: "canary", ObjectsBase: 40, ObjectsHead: 42,
		Kustomizations: []api.KustomizationImpact{
			{Name: "infra-metal", Objects: []api.ObjectChange{
				{Op: api.OpAdd, Cluster: "defiant", Kind: "Machine", Name: "cn-035", Source: api.SourceRef{Path: "control/inventory/cn-035.yaml", Line: 1}},
				{Op: api.OpAdd, Cluster: "defiant", Kind: "Machine", Name: "cn-036", Source: api.SourceRef{Path: "control/inventory/cn-036.yaml", Line: 1}}}},
			{Name: "cilium", Objects: []api.ObjectChange{{Op: api.OpChange, Cluster: "defiant", Kind: "CiliumBGPPeeringPolicy", Name: "rack-6z",
				Source: api.SourceRef{Path: bgpPath, Line: 1}, Diff: "--- a\n+++ b\n@@ -1,3 +1,4 @@\n       - cn-034\n+      - cn-035\n"}}},
		}}},
		Semantic: []api.SemanticLine{
			{Subject: "cn-035", Text: "new compute node in GC5A-6Z, U6", Op: api.OpAdd, Path: "control/inventory/cn-035.yaml"},
			{Subject: "cn-036", Text: "new compute node in GC5A-6Z, U8", Op: api.OpAdd, Path: "control/inventory/cn-036.yaml"},
			{Subject: "rack-6z", Text: "BGP policy selects 1 more node", Op: api.OpChange, Path: bgpPath},
		},
	}
	imp.Risk.Level = api.High
	imp.Risk.Reasons = []api.RiskReason{{Reason: "a change to a node is high"}}
	return imp, nil
}

func (f *fakeTools) Diff(ctx context.Context, id string) (*api.Diff, error) {
	if id == "c-p4x8ma" {
		return &api.Diff{Files: []api.FileDiff{{Path: bgpPath, Unified: "@@ -26,3 +26,7 @@\n       neighbors:\n         - peerAddress: 10.6.0.1/32\n           peerASN: 65000\n+        - peerAddress: 10.6.0.3/32\n+          peerASN: 65101\n+        - peerAddress: 10.6.0.4/32\n+          peerASN: 65102\n"}}}, nil
	}
	return &api.Diff{Files: []api.FileDiff{{Path: bgpPath, Unified: "@@ -20,6 +20,7 @@\n           - cn-032\n           - cn-033\n           - cn-034\n+          - cn-035\n   virtualRouters:\n     - localASN: 64512\n       exportPodCIDR: true\n"}}}, nil
}

func (f *fakeTools) Status(ctx context.Context, id string) (*api.WorktreeStatus, error) {
	return &api.WorktreeStatus{Clean: len(f.status) == 0, Files: f.status}, nil
}

func (f *fakeTools) Problems(ctx context.Context, id string) ([]api.Problem, error) {
	return f.problems, nil
}

func (f *fakeTools) Tree(ctx context.Context, ref string) ([]api.FileNode, error) {
	var paths []string
	for k := range f.files {
		r, p, _ := strings.Cut(k, "\x00")
		if r == ref {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []api.FileNode
	for _, p := range paths {
		n := api.FileNode{Path: p, Name: filepath.Base(p), Type: api.FileNodeTypeFile}
		if p == bgpPath {
			n.Kind = "CiliumBGPPeeringPolicy"
		}
		out = append(out, n)
	}
	return []api.FileNode{{Path: "", Type: api.FileNodeTypeDir, Children: out}}, nil
}

func (f *fakeTools) ReadFile(ctx context.Context, ref, p string) (*api.FileContent, error) {
	c, ok := f.files[ref+"\x00"+p]
	if !ok {
		return nil, fmt.Errorf("%s: not found", p)
	}
	return &api.FileContent{Path: p, Content: c, Exists: true}, nil
}

func (f *fakeTools) WriteFile(ctx context.Context, changeID, p, content string) (*api.WriteFileResult, error) {
	f.files[changeID+"\x00"+p] = content
	f.writes = append(f.writes, p)
	return &api.WriteFileResult{File: api.FileContent{Path: p, Content: content, Exists: true}}, nil
}

func (f *fakeTools) History(ctx context.Context, p api.ListHistoryParams) ([]api.HistoryEntry, error) {
	return []api.HistoryEntry{
		{Sha: "sha-c-a5e3vk", Change: "c-a5e3vk", Title: "Sync serials from asset DB", Author: "sync-bot", Date: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)},
		{Sha: "sha-c-2hd7vw", Change: "c-2hd7vw", Title: "Move cn-003 to data-a 37", Intent: "free data-b ports for rack 6Z", Author: "jonas", Mr: "!2",
			ApprovedBy: []string{"anna", "dennis"}, Date: time.Date(2026, 9, 12, 10, 15, 0, 0, time.UTC)},
	}, nil
}

func (f *fakeTools) HistoryEntry(ctx context.Context, sha string) (*api.HistoryDetail, error) {
	switch sha {
	case "sha-c-2hd7vw":
		return &api.HistoryDetail{Semantic: []api.SemanticLine{{Subject: "cn-003 p2", Text: "data-b 37 to data-a 37", Op: api.OpChange, Path: "control/inventory/cn-003.yaml"}}}, nil
	case "sha-c-a5e3vk":
		return &api.HistoryDetail{Semantic: []api.SemanticLine{{Subject: "cn-025", Text: "serial S4R7K2J0025 from the asset DB"}}}, nil
	}
	return nil, fmt.Errorf("no entry %s", sha)
}

func (f *fakeTools) LabRuns(ctx context.Context, changeID string) ([]api.LabRun, error) {
	return f.runs[changeID], nil
}

func (f *fakeTools) Entity(ctx context.Context, kind, name, ref string) (*api.Entity, error) {
	return nil, fmt.Errorf("no entity")
}

func (f *fakeTools) Search(ctx context.Context, q string, limit int) ([]llm.Hit, error) {
	return nil, nil
}

// memStore is a SuggestionStore for tests.
type memStore struct{ list []Suggestion }

func (m *memStore) AddSuggestion(ctx context.Context, s Suggestion) (*Suggestion, error) {
	s.ID = fmt.Sprintf("s-%d", len(m.list)+1)
	s.Status = StatusPending
	m.list = append(m.list, s)
	return &s, nil
}
