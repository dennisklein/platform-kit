package change

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// SummaryToAPI converts a summary to the API schema (required arrays non-nil).
func SummaryToAPI(s Summary) api.ChangeSummary {
	out := api.ChangeSummary{
		Id: s.ID, Title: s.Title, Intent: s.Intent, Branch: s.Branch, Target: s.Target, Parent: s.Parent,
		Author: s.Author, Agent: s.Agent, AuthorKind: api.IdentityKind(s.AuthorKind), State: api.ChangeState(s.State),
		LabState: api.LabState(s.LabState), LabRun: s.LabRun, Group: api.ChangeGroup(s.Group),
		Local: s.Local, Dirty: s.Dirty, Pushed: s.Pushed,
		AheadTarget: s.AheadTarget, BehindTarget: s.BehindTarget, AheadRemote: s.AheadRemote, BehindRemote: s.BehindRemote,
		Files: s.Files, Commits: s.Commits, WorkItems: s.WorkItems,
		Approvals: api.ApprovalCount{Have: s.ApprovalsHave, Need: s.ApprovalsNeed},
		Risk:      api.RiskLevel(s.Risk), Overlaps: s.Overlaps,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, MergedAt: s.MergedAt, MergeSha: s.MergeSHA,
	}
	if out.LabState == "" {
		out.LabState = "none"
	}
	if out.AuthorKind == "" {
		out.AuthorKind = api.IdentityKind("human")
	}
	if s.MR != nil {
		out.Mr = MRToAPI(s.MR)
	}
	for _, r := range s.Rollout {
		out.Rollout = append(out.Rollout, api.ClusterRollout{Cluster: r.Cluster, Ring: r.Ring, Deployed: r.Deployed,
			Verified: r.Verified, Sha: r.SHA, At: r.At})
	}
	return out
}

// MRToAPI converts a merge request reference.
func MRToAPI(m *MR) api.MergeRequestRef {
	if m == nil {
		return api.MergeRequestRef{}
	}
	st := m.State
	if st == "" {
		st = "open"
	}
	return api.MergeRequestRef{Number: m.Number, Url: m.URL, Draft: m.Draft, State: api.MergeRequestRefState(st), Title: m.Title}
}

// CommitToAPI converts a commit.
func CommitToAPI(c Commit) api.Commit {
	out := api.Commit{
		Sha: c.SHA, Short: short(c.SHA), Subject: c.Subject, Body: c.Body, Author: c.Author,
		AuthorName: c.AuthorName, AuthorEmail: c.AuthorEmail, Date: c.Date, Agent: c.Agent, OnBehalfOf: c.OnBehalfOf,
		Files: c.Files, Pushed: c.Pushed,
	}
	for _, t := range c.Trailers {
		out.Trailers = append(out.Trailers, api.Trailer{Key: t.Key, Value: t.Value})
	}
	return out
}

// LabRunToAPI converts a lab run reference.
func LabRunToAPI(r LabRunRef) api.LabRunRef {
	return api.LabRunRef{Run: r.Run, Sha: r.SHA, Result: api.LabResult(r.Result), Report: r.Report}
}

// ApprovalToAPI converts an approval.
func ApprovalToAPI(a policy.Approval) api.Approval {
	return api.Approval{User: a.User, Sha: a.SHA, Counts: a.Counts, Reason: a.Reason}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// GatesToAPI converts policy gates (same JSON shape; required arrays non-nil).
func GatesToAPI(g *policy.Gates) api.Gates {
	var out api.Gates
	if g != nil {
		if b, err := json.Marshal(g); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	if out.Approvals.By == nil {
		out.Approvals.By = []string{}
	}
	if out.Blockers == nil {
		out.Blockers = []string{}
	}
	if out.MatchedRules == nil {
		out.MatchedRules = []api.MatchedRule{}
	}
	for i := range out.MatchedRules {
		if out.MatchedRules[i].Paths == nil {
			out.MatchedRules[i].Paths = []string{}
		}
	}
	if out.PathRights.Details == nil {
		out.PathRights.Details = []api.PathRight{}
	}
	if out.PathRights.Writers == nil {
		out.PathRights.Writers = []string{}
	}
	if out.Risk.Reasons == nil {
		out.Risk.Reasons = []api.RiskReason{}
	}
	if out.LabGate.Status == "" {
		out.LabGate.Status = "na"
	}
	if out.Risk.Level == "" {
		out.Risk.Level = "low"
	}
	return out
}

// FileStatusToAPI converts a changed file.
func FileStatusToAPI(f FileStatus) api.FileStatus {
	return api.FileStatus{Path: f.Path, OldPath: f.OldPath, Status: api.FileChange(f.Status), Staged: f.Staged, Committed: f.Committed}
}

// OverlapToAPI converts an overlap.
func OverlapToAPI(o Overlap) api.Overlap {
	out := api.Overlap{Change: o.Change, Title: o.Title, By: o.By, State: api.ChangeState(o.State), Objects: []api.ObjectRef{}}
	for _, k := range o.Objects {
		r := ObjectRefOf(k)
		out.Objects = append(out.Objects, api.ObjectRef{Cluster: r.Cluster, ApiVersion: r.APIVersion, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name})
	}
	return out
}

// MetaToAPI converts the metadata.
func MetaToAPI(m Meta) api.ChangeMeta {
	out := api.ChangeMeta{Id: m.ID, Title: m.Title, Intent: m.Intent, Author: m.Author, Agent: m.Agent,
		Target: m.Target, Parent: m.Parent, WorkItems: m.WorkItems}
	for _, r := range m.LabRuns {
		out.LabRuns = append(out.LabRuns, LabRunToAPI(r))
	}
	return out
}

// ChangeToAPI converts a change with details. Impact is left to the caller
// (the renderer lives in the bench).
func ChangeToAPI(ch *Change) api.Change {
	s := SummaryToAPI(ch.Summary)
	out := api.Change{
		Agent: s.Agent, AheadRemote: s.AheadRemote, AheadTarget: s.AheadTarget, Approvals: s.Approvals,
		Author: s.Author, AuthorKind: s.AuthorKind, BaseSha: ch.BaseSHA, BehindRemote: s.BehindRemote,
		BehindTarget: s.BehindTarget, Branch: s.Branch, CreatedAt: s.CreatedAt, Description: ch.Description,
		Dirty: s.Dirty, Files: s.Files, Group: s.Group, HeadSha: ch.HeadSHA, Id: s.Id, Intent: s.Intent,
		LabRun: s.LabRun, LabState: s.LabState, Local: s.Local, MergeSha: s.MergeSha, MergedAt: s.MergedAt,
		Meta: MetaToAPI(ch.Meta), Mr: s.Mr, Overlaps: s.Overlaps, Parent: s.Parent, Pushed: s.Pushed, Risk: s.Risk,
		Rollout: s.Rollout, State: s.State, Target: s.Target, TargetSha: ch.TargetSHA, Title: s.Title,
		UpdatedAt: s.UpdatedAt, WorkItems: s.WorkItems, Worktree: ch.Worktree,
		ApprovalsBy: []api.Approval{}, Children: []string{}, Commits: []api.Commit{}, LabRuns: []api.LabRunRef{},
		OverlapsDetail: []api.Overlap{}, Lifecycle: LifecycleToAPI(ch),
	}
	if ch.Meta.ID == "" {
		out.Meta = api.ChangeMeta{Id: ch.ID, Title: ch.Title, Intent: ch.Intent, Author: ch.Author, Agent: ch.Agent,
			Target: ch.Target, Parent: ch.Parent, WorkItems: ch.WorkItems}
	}
	for _, a := range ch.Approvals {
		out.ApprovalsBy = append(out.ApprovalsBy, ApprovalToAPI(a))
	}
	out.Children = append(out.Children, ch.Children...)
	for _, c := range ch.Commits {
		out.Commits = append(out.Commits, CommitToAPI(c))
	}
	for _, r := range ch.LabRuns {
		out.LabRuns = append(out.LabRuns, LabRunToAPI(r))
	}
	for _, o := range ch.Overlaps {
		out.OverlapsDetail = append(out.OverlapsDetail, OverlapToAPI(o))
	}
	for _, f := range ch.ChangedFiles {
		out.ChangedFiles = append(out.ChangedFiles, FileStatusToAPI(f))
	}
	if ch.Gates != nil {
		out.Gates = GatesToAPI(ch.Gates)
	}
	return out
}

// LifecycleToAPI builds the lifecycle track: draft, lab, review, approved,
// merged, deployed, verified with done/now/todo/fail and a label per dot.
func LifecycleToAPI(ch *Change) []api.LifecycleStep {
	cur := -1
	for i, st := range Lifecycle {
		if st == ch.State {
			cur = i
		}
	}
	labRequired := ch.Gates != nil && ch.Gates.LabGate.Required
	out := make([]api.LifecycleStep, 0, len(Lifecycle))
	for i, st := range Lifecycle {
		step := api.LifecycleStep{State: api.ChangeState(st), Status: api.LifecycleStepStatusTodo}
		switch {
		case ch.State == Closed:
			if i == 0 {
				step.Status = api.LifecycleStepStatusDone
				step.Detail = "closed without merge"
			}
		case i < cur:
			step.Status = api.LifecycleStepStatusDone
		case i == cur:
			step.Status = api.LifecycleStepStatusNow
			if st == Verified {
				step.Status = api.LifecycleStepStatusDone
			}
		}
		switch st {
		case Draft:
			switch {
			case ch.MR != nil:
				step.Label = fmt.Sprintf("!%d", ch.MR.Number)
			case ch.Pushed:
				step.Label = "pushed"
			default:
				step.Label = "local"
			}
		case Lab:
			step.Label = labLabel(ch.LabRun)
			if ch.LabState == string(LabFail) && cur <= i+1 && ch.State != Closed {
				step.Status = api.LifecycleStepStatusFail
				step.Detail = "lab failed on " + step.Label
			}
			// A passing run on the head is done even while the change is
			// still a draft (lab before push).
			if ch.LabState == string(LabPass) && step.Status == api.LifecycleStepStatusTodo && ch.State != Closed {
				step.Status = api.LifecycleStepStatusDone
			}
			// A run that tested an older head (after a rebase or a new
			// commit) no longer counts: the dot is open again.
			if ch.State.Open() && ch.LabState == "none" && ch.LabRun != "" {
				step.Detail = step.Label + " tested an older head"
				if labRequired || ch.Gates == nil {
					step.Status = api.LifecycleStepStatusTodo
					step.Detail += "; run the lab again"
				}
			}
			if step.Label == "" {
				if !labRequired && ch.Gates != nil {
					step.Label = "not required"
				} else {
					step.Label = "no run"
				}
			}
		case Review:
			switch {
			case ch.MR != nil && !ch.MR.Draft, i < cur:
				step.Label = "ready"
			case ch.MR != nil:
				step.Label = "draft MR"
			default:
				step.Label = "no MR"
			}
		case Approved:
			step.Label = fmt.Sprintf("%d of %d", ch.ApprovalsHave, ch.ApprovalsNeed)
		case Merged:
			step.Label = short(ch.MergeSHA)
		case Deployed, Verified:
			n := 0
			for _, r := range ch.Rollout {
				if (st == Deployed && r.Deployed) || (st == Verified && r.Verified) {
					n++
				}
			}
			if len(ch.Rollout) > 0 {
				step.Label = fmt.Sprintf("%d of %d", n, len(ch.Rollout))
			}
		}
		out = append(out, step)
	}
	return out
}

func labLabel(run string) string {
	if i := strings.LastIndex(run, "/"); i >= 0 {
		return run[i+1:]
	}
	return run
}
