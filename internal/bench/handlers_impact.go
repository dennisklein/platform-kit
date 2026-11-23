package bench

// Impact, diff, gates and policy (area B).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// bChangeImpact renders the merge base and the head of a change and compares
// them (cached by content in the render engine).
func (s *Server) bChangeImpact(ctx context.Context, id string) (*render.Impact, error) {
	if s.d.Changes == nil {
		return nil, unavailable("the change service")
	}
	base, head, err := s.d.Changes.Sources(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.bRenderer().Impact(ctx, base, head)
}

// bImpactRisk rates an impact with the policy on main (changes never loosen
// their own gates). Without a policy the risk is empty.
func (s *Server) bImpactRisk(ctx context.Context, id string, imp *render.Impact) policy.Risk {
	pol := s.b().pol
	p, err := pol.Current(ctx)
	if err != nil || p == nil {
		return policy.Risk{Reasons: []policy.RiskReason{}}
	}
	g := pol.Evaluate(p, policy.GateInput{ChangeID: id, Paths: imp.Paths(), Objects: imp.Objects(), Target: "main"})
	return g.Risk
}

func bAPIRisk(r policy.Risk) api.Risk {
	out := api.Risk{Level: api.RiskLevel(r.Level), Reasons: []api.RiskReason{}}
	if out.Level == "" {
		out.Level = api.Low
	}
	for _, x := range r.Reasons {
		out.Reasons = append(out.Reasons, api.RiskReason{Reason: x.Reason, Source: api.RiskReasonSource(x.Source), Rule: x.Rule, Object: x.Object})
	}
	return out
}

// bSummaryCluster has the shape of api.ImpactSummary.Clusters elements.
type bSummaryCluster = struct {
	Added   int    `json:"added"`
	Changed int    `json:"changed"`
	Cluster string `json:"cluster"`
	Deleted int    `json:"deleted"`
}

// ImpactSummaryOf returns the counts per cluster and the "defiant +2 ~1" text.
func ImpactSummaryOf(imp *render.Impact, risk policy.RiskLevel) api.ImpactSummary {
	sum := api.ImpactSummary{Clusters: []bSummaryCluster{}, Risk: api.RiskLevel(risk), Text: imp.SummaryText()}
	for _, c := range imp.Clusters {
		a, ch, d := c.Counts()
		sum.Added += a
		sum.Changed += ch
		sum.Deleted += d
		sum.Clusters = append(sum.Clusters, bSummaryCluster{Added: a, Changed: ch, Deleted: d, Cluster: c.Cluster})
	}
	return sum
}

func bAPIObjectChange(cluster string, o render.ObjectChange) api.ObjectChange {
	return api.ObjectChange{Cluster: cluster, ApiVersion: o.Ref.APIVersion, Kind: o.Ref.Kind, Namespace: o.Ref.Namespace, Name: o.Ref.Name,
		Key: o.Ref.Key(), Kustomization: o.Kustomization, Op: api.Op(o.Op), Diff: o.Diff,
		Source: api.SourceRef{Path: o.Source.Path, Line: o.Source.Line}}
}

func bAPIClusters(imp *render.Impact, only string) []api.ClusterImpact {
	out := []api.ClusterImpact{}
	for _, c := range imp.Clusters {
		if only != "" && c.Cluster != only {
			continue
		}
		ci := api.ClusterImpact{Cluster: c.Cluster, Role: c.Role, Ring: c.Ring, ObjectsBase: c.ObjectsBase, ObjectsHead: c.ObjectsHead, Kustomizations: []api.KustomizationImpact{}}
		for _, k := range c.Kustomizations {
			ki := api.KustomizationImpact{Name: k.Name, Path: k.Path, Objects: []api.ObjectChange{}}
			for _, o := range k.Objects {
				ki.Objects = append(ki.Objects, bAPIObjectChange(c.Cluster, o))
			}
			ci.Kustomizations = append(ci.Kustomizations, ki)
		}
		out = append(out, ci)
	}
	return out
}

func bAPIFileDiffs(fs []render.FileDiff) []api.FileDiff {
	out := make([]api.FileDiff, 0, len(fs))
	for _, f := range fs {
		out = append(out, api.FileDiff{Path: f.Path, OldPath: f.OldPath, Status: api.FileChange(f.Status), Unified: f.Unified, Old: f.Old, New: f.New})
	}
	return out
}

func bAPIOpenSteps(ss []render.OpenStep) []api.OpenStep {
	out := make([]api.OpenStep, 0, len(ss))
	for _, st := range ss {
		out = append(out, api.OpenStep{Kind: st.Kind, Name: st.Name, Path: st.Path, ApplyPath: api.ApplyPath(st.ApplyPath), Text: st.Text})
	}
	return out
}

func bRenderRef(src render.Source) api.RenderRef {
	return api.RenderRef{Ref: src.Ref, Sha: src.SHA, Dirty: src.Dirty}
}

// bOverlaps returns the other open changes that touch the same rendered
// objects as imp.
func (s *Server) bOverlaps(ctx context.Context, id string, imp *render.Impact) []api.Overlap {
	out := []api.Overlap{}
	mine := imp.Objects()
	if len(mine) == 0 || s.d.Changes == nil {
		return out
	}
	heads, err := s.d.Changes.Heads(ctx)
	if err != nil {
		return out
	}
	for _, h := range heads {
		if h.ID == id {
			continue
		}
		other, err := s.bChangeImpact(ctx, h.ID)
		if err != nil {
			continue
		}
		shared := render.Overlaps(mine, map[string][]render.ObjectRef{h.ID: other.Objects()})[h.ID]
		if len(shared) == 0 {
			continue
		}
		ov := api.Overlap{Change: h.ID, Title: h.Title, By: h.Author, State: api.ChangeState(h.State), Objects: []api.ObjectRef{}}
		for _, o := range shared {
			ov.Objects = append(ov.Objects, api.ObjectRef{Cluster: o.Cluster, ApiVersion: o.APIVersion, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name})
		}
		out = append(out, ov)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Change < out[j].Change })
	if len(out) > 0 {
		// Heads only knows draft or review; the list has the derived state
		// (lab, approved, ...).
		if list, err := s.d.Changes.List(ctx, false); err == nil {
			for i := range out {
				for _, c := range list {
					if c.ID == out[i].Change && c.State != "" {
						out[i].State = api.ChangeState(c.State)
					}
				}
			}
		}
	}
	return out
}

func (s *Server) GetImpact(ctx context.Context, req api.GetImpactRequestObject) (api.GetImpactResponseObject, error) {
	imp, err := s.bChangeImpact(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	risk := s.bImpactRisk(ctx, req.Id, imp)
	out := api.Impact{
		ChangeId:   req.Id,
		Base:       bRenderRef(imp.Base),
		Head:       bRenderRef(imp.Head),
		Clusters:   bAPIClusters(imp, ""),
		Semantic:   bAPISemantic(imp.Semantic),
		OpenSteps:  bAPIOpenSteps(imp.OpenSteps),
		Problems:   bAPIProblems(imp.Problems),
		Overlaps:   s.bOverlaps(ctx, req.Id, imp),
		Risk:       bAPIRisk(risk),
		Summary:    ImpactSummaryOf(imp, risk.Level),
		RenderedAt: imp.RenderedAt,
	}
	return api.GetImpact200JSONResponse(out), nil
}

func (s *Server) GetDiff(ctx context.Context, req api.GetDiffRequestObject) (api.GetDiffResponseObject, error) {
	imp, err := s.bChangeImpact(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	view := req.Params.View
	if view == "" {
		view = api.DiffViewKindSemantic
	}
	out := api.Diff{View: view}
	switch view {
	case api.DiffViewKindSemantic:
		out.Semantic = bAPISemantic(imp.Semantic)
	case api.DiffViewKindSource:
		out.Files = bAPIFileDiffs(imp.Files)
	case api.DiffViewKindRendered:
		out.Objects = []api.ObjectChange{}
		if req.Params.Cluster != "" && !slices.ContainsFunc(imp.Clusters, func(c render.ClusterImpact) bool { return c.Cluster == req.Params.Cluster }) {
			return nil, bNotFound("no cluster %s", req.Params.Cluster)
		}
		for _, c := range bAPIClusters(imp, req.Params.Cluster) {
			for _, k := range c.Kustomizations {
				out.Objects = append(out.Objects, k.Objects...)
			}
		}
	default:
		return nil, bBadRequest("unknown view %s (semantic, source or rendered)", view)
	}
	return api.GetDiff200JSONResponse(out), nil
}

func (s *Server) GetGates(ctx context.Context, req api.GetGatesRequestObject) (api.GetGatesResponseObject, error) {
	if s.d.Changes == nil {
		return nil, unavailable("the change service")
	}
	g, err := s.d.Changes.Gates(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	var out api.Gates
	if err := bConvertJSON(g, &out); err != nil {
		return nil, err
	}
	bFixGates(&out)
	return api.GetGates200JSONResponse(out), nil
}

// bFixGates makes required arrays non-nil.
func bFixGates(g *api.Gates) {
	g.Approvals.By = bNonNil(g.Approvals.By)
	g.Blockers = bNonNil(g.Blockers)
	g.MatchedRules = bNonNil(g.MatchedRules)
	g.PathRights.Details = bNonNil(g.PathRights.Details)
	g.PathRights.Writers = bNonNil(g.PathRights.Writers)
	g.Risk.Reasons = bNonNil(g.Risk.Reasons)
	for i := range g.MatchedRules {
		g.MatchedRules[i].Paths = bNonNil(g.MatchedRules[i].Paths)
	}
	if g.LabGate.Status == "" {
		g.LabGate.Status = api.LabGateStatusNa
	}
	if g.Risk.Level == "" {
		g.Risk.Level = api.Low
	}
}

// GatesToAPI converts policy gates to the API shape.
func GatesToAPI(g policy.Gates) api.Gates {
	var out api.Gates
	_ = bConvertJSON(g, &out)
	bFixGates(&out)
	return out
}

func (s *Server) GetPolicySummary(ctx context.Context, req api.GetPolicySummaryRequestObject) (api.GetPolicySummaryResponseObject, error) {
	if s.d.Changes == nil {
		return nil, unavailable("the change service")
	}
	dir, err := s.d.Changes.MainDir(ctx)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, policy.Path))
	if err != nil {
		return nil, bNotFound("no policy on main: %v", err)
	}
	p, probs, err := s.b().pol.Load(data)
	out := api.PolicySummary{Path: policy.Path, People: []api.Person{}, Agents: []api.PolicyAgent{}, Bots: []api.PolicyAgent{},
		Rules: []api.PolicyRule{}, RiskRules: []api.RiskRule{}, Problems: bAPIProblems(probs), SchemaUrl: BasePath + "/kinds/Policy/schema"}
	for i := range out.Problems {
		if out.Problems[i].Path == "" {
			out.Problems[i].Path = policy.Path
		}
	}
	if src, err := s.d.Changes.Source(ctx, "main"); err == nil {
		out.Sha = src.SHA
	}
	if err != nil || p == nil {
		return api.GetPolicySummary200JSONResponse(out), nil
	}
	for _, x := range p.People {
		out.People = append(out.People, api.Person{Id: x.ID, Name: x.Name, Email: x.Email, Roles: x.Roles, Teams: x.Teams})
	}
	for _, x := range p.Agents {
		out.Agents = append(out.Agents, api.PolicyAgent{Id: x.ID, ActsFor: x.ActsFor, Can: bRights(x.Can)})
	}
	for _, x := range p.Bots {
		out.Bots = append(out.Bots, api.PolicyAgent{Id: x.ID, Can: bRights(x.Can)})
	}
	for _, r := range p.Rules {
		out.Rules = append(out.Rules, api.PolicyRule{Name: r.Name, Paths: bNonNil(r.Paths), Writers: bNonNil(r.Writers), Approvers: bNonNil(r.Approvers),
			Approvals: r.Approvals, LabGate: r.LabGate, Risk: api.RiskLevel(r.Risk)})
	}
	for _, r := range p.RiskRules {
		m := map[string]string{}
		for k, v := range map[string]string{"cluster": r.Match.Cluster, "apiVersion": r.Match.APIVersion, "kind": r.Match.Kind, "namespace": r.Match.Namespace, "name": r.Match.Name} {
			if v != "" {
				m[k] = v
			}
		}
		out.RiskRules = append(out.RiskRules, api.RiskRule{Match: m, Risk: api.RiskLevel(r.Risk), Reason: r.Reason})
	}
	if p.FastPath != nil {
		out.FastPath.Approvals = p.FastPath.Approvals
		out.FastPath.LabGate = p.FastPath.LabGate
	}
	return api.GetPolicySummary200JSONResponse(out), nil
}

func bRights(rs []policy.Right) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}

func (s *Server) GetForgeRules(ctx context.Context, req api.GetForgeRulesRequestObject) (api.GetForgeRulesResponseObject, error) {
	pol := s.b().pol
	p, err := pol.Current(ctx)
	if err != nil {
		return nil, err
	}
	fr := pol.ForgeRules(p)
	var out api.ForgeRules
	if err := bConvertJSON(fr, &out); err != nil {
		return nil, err
	}
	out.BranchProtection.MergeWhitelist = bNonNil(out.BranchProtection.MergeWhitelist)
	out.BranchProtection.RequiredChecks = bNonNil(out.BranchProtection.RequiredChecks)
	if adm, ok := s.d.Forge.(forge.Admin); ok {
		bp, err := adm.GetBranchProtection(ctx, fr.BranchProtection.Branch)
		switch {
		case err != nil:
			// Not checked (for example no admin credentials): no drift claim.
			s.log.Debug("forge rules: branch protection", "err", err)
		case bp == nil:
			out.Drift = []string{"no branch protection on " + fr.BranchProtection.Branch}
		default:
			out.Drift = ForgeDrift(fr.BranchProtection, *bp)
			out.InSync = len(out.Drift) == 0
		}
	}
	return api.GetForgeRules200JSONResponse(out), nil
}

// ForgeDrift compares the generated branch protection with the forge.
func ForgeDrift(want policy.BranchProtection, have forge.BranchProtection) []string {
	var d []string
	set := func(s []string) string {
		c := slices.Clone(s)
		sort.Strings(c)
		return strings.Join(c, ", ")
	}
	cmp := func(name, w, h string) {
		if w != h {
			d = append(d, fmt.Sprintf("%s: forge has %q, policy wants %q", name, h, w))
		}
	}
	cmp("required checks", set(want.RequiredChecks), set(have.RequiredChecks))
	cmp("merge whitelist", set(want.MergeWhitelist), set(have.MergeWhitelist))
	cmp("approvals whitelist", set(want.ApprovalsWhitelist), set(have.ApprovalsWhitelist))
	cmp("required approvals", fmt.Sprint(want.RequiredApprovals), fmt.Sprint(have.RequiredApprovals))
	cmp("block on outdated branch", fmt.Sprint(want.BlockOnOutdated), fmt.Sprint(have.BlockOnOutdated))
	cmp("direct push", fmt.Sprint(want.EnablePush), fmt.Sprint(have.EnablePush))
	return d
}

// bRenderEvent is the render.updated payload (api RenderEvent).
type bRenderEvent struct {
	ChangeID string            `json:"changeId"`
	Summary  api.ImpactSummary `json:"summary"`
}

// renderAfterSave renders the change after a save, publishes render.updated
// and returns the impact summary (nil when the render fails).
func (s *Server) renderAfterSave(ctx context.Context, changeID string) *api.ImpactSummary {
	sum, err := s.impactSummary(ctx, changeID)
	if err != nil {
		s.log.Warn("render after save", "change", changeID, "err", err)
		return nil
	}
	if s.hub != nil {
		s.hub.Publish(events.RenderUpdated, bRenderEvent{ChangeID: changeID, Summary: *sum})
	}
	return sum
}

// impactSummary returns the impact summary of a change.
func (s *Server) impactSummary(ctx context.Context, changeID string) (*api.ImpactSummary, error) {
	imp, err := s.bChangeImpact(ctx, changeID)
	if err != nil {
		return nil, err
	}
	sum := ImpactSummaryOf(imp, s.bImpactRisk(ctx, changeID, imp).Level)
	return &sum, nil
}

// changeGates evaluates the policy gates of a change.
func (s *Server) changeGates(ctx context.Context, ch *change.Change) (policy.Gates, error) {
	if ch != nil && ch.Gates != nil {
		return *ch.Gates, nil
	}
	if s.d.Changes == nil {
		return policy.Gates{}, unavailable("the change service")
	}
	id := ""
	if ch != nil {
		id = ch.ID
	}
	g, err := s.d.Changes.Gates(ctx, id)
	if err != nil {
		return policy.Gates{}, err
	}
	if g == nil {
		return policy.Gates{}, fmt.Errorf("no gates for %s", id)
	}
	return *g, nil
}
