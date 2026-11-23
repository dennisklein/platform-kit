package bench

// Changes: list, create, get, checkout, commits, push, ready, rebase, approve, merge, close (area A).

import (
	"context"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/identity"
)

// changes returns the change service or 503.
func (s *Server) changeSvc() (change.Service, error) {
	if s.d.Changes == nil {
		return nil, unavailable("the change service (forge)")
	}
	return s.d.Changes, nil
}

func (s *Server) ListChanges(ctx context.Context, req api.ListChangesRequestObject) (api.ListChangesResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	list, err := svc.List(ctx, req.Params.Scope == "all")
	if err != nil {
		return nil, err
	}
	out := api.ChangeList{Changes: []api.ChangeSummary{}}
	for _, c := range list {
		out.Changes = append(out.Changes, change.SummaryToAPI(c))
	}
	out.Main = s.mainRef(ctx)
	if f, ok := svc.(interface{ ForgeOffline() bool }); ok {
		out.ForgeOffline = f.ForgeOffline()
	}
	return api.ListChanges200JSONResponse(out), nil
}

// mainRef is the head of main with the change of the last merge.
func (s *Server) mainRef(ctx context.Context) api.MainRef {
	ref := api.MainRef{}
	h, err := s.history(ctx)
	if err != nil {
		return ref
	}
	ref.Sha, ref.At, ref.Subject, ref.Change = h.head, h.headAt, h.headSubject, h.headChange
	return ref
}

func (s *Server) CreateChange(ctx context.Context, req api.CreateChangeRequestObject) (api.CreateChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Title) == "" {
		return nil, Errorf(400, "title is required")
	}
	b := req.Body
	cr := change.CreateRequest{Title: strings.TrimSpace(b.Title), Intent: b.Intent, Target: b.Target, Parent: b.Parent, WorkItems: b.WorkItems}
	if cr.Target == "" && cr.Parent != "" {
		cr.Target = cr.Parent
	}
	ch, err := svc.Create(ctx, cr)
	if err != nil {
		return nil, err
	}
	if full, err := svc.Get(ctx, ch.ID); err == nil {
		ch = full
	}
	return api.CreateChange201JSONResponse(s.changeToAPI(ctx, ch)), nil
}

// changeToAPI converts a change and adds the impact summary when the
// renderer is wired.
func (s *Server) changeToAPI(ctx context.Context, ch *change.Change) api.Change {
	out := change.ChangeToAPI(ch)
	// Merged changes show what their merge changed; closed ones have no impact.
	if ch.State != change.Closed {
		ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if imp, err := s.impactSummary(ictx, ch.ID); err == nil && imp != nil {
			out.Impact = *imp
		}
		cancel()
	}
	return out
}

func (s *Server) GetChange(ctx context.Context, req api.GetChangeRequestObject) (api.GetChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	ch, err := svc.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return api.GetChange200JSONResponse(s.changeToAPI(ctx, ch)), nil
}

func (s *Server) CheckoutChange(ctx context.Context, req api.CheckoutChangeRequestObject) (api.CheckoutChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	sum, err := svc.Checkout(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return api.CheckoutChange200JSONResponse(change.SummaryToAPI(*sum)), nil
}

func (s *Server) ListCommits(ctx context.Context, req api.ListCommitsRequestObject) (api.ListCommitsResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	cs, err := svc.Commits(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := api.ListCommits200JSONResponse{}
	for _, c := range cs {
		out = append(out, change.CommitToAPI(c))
	}
	return out, nil
}

func (s *Server) Commit(ctx context.Context, req api.CommitRequestObject) (api.CommitResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Message) == "" {
		return nil, Errorf(400, "message is required")
	}
	c, err := svc.Commit(ctx, req.Id, req.Body.Message, req.Body.Paths)
	if err != nil {
		return nil, err
	}
	return api.Commit201JSONResponse(change.CommitToAPI(*c)), nil
}

func (s *Server) PushChange(ctx context.Context, req api.PushChangeRequestObject) (api.PushChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	r, err := svc.Push(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := api.PushResult{Change: change.SummaryToAPI(r.Change), Created: r.Created, RenamedFrom: r.RenamedFrom, Pushed: r.Pushed}
	if r.MR != nil {
		out.Mr = change.MRToAPI(r.MR)
	}
	return api.PushChange200JSONResponse(out), nil
}

func (s *Server) MarkReady(ctx context.Context, req api.MarkReadyRequestObject) (api.MarkReadyResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	ch, err := svc.MarkReady(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if full, err := svc.Get(ctx, req.Id); err == nil {
		ch = full
	}
	return api.MarkReady200JSONResponse(s.changeToAPI(ctx, ch)), nil
}

func (s *Server) RebaseChange(ctx context.Context, req api.RebaseChangeRequestObject) (api.RebaseChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	before, berr := s.impactSummary(ctx, req.Id)
	r, err := svc.Rebase(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := api.RebaseResult{Ok: r.OK, From: r.From, To: r.To, Onto: r.Onto, Conflicts: r.Conflicts, Change: change.SummaryToAPI(r.Change)}
	if berr == nil && before != nil {
		if after, err := s.impactSummary(ctx, req.Id); err == nil && after != nil {
			out.ImpactUnchanged = chSameImpact(before, after)
			out.Change.Impact = *after
		}
	}
	return api.RebaseChange200JSONResponse(out), nil
}

// chSameImpact compares two impact summaries (counts per cluster).
func chSameImpact(a, b *api.ImpactSummary) bool {
	if a.Added != b.Added || a.Changed != b.Changed || a.Deleted != b.Deleted || a.Text != b.Text || len(a.Clusters) != len(b.Clusters) {
		return false
	}
	for i := range a.Clusters {
		if a.Clusters[i] != b.Clusters[i] {
			return false
		}
	}
	return true
}

func (s *Server) ApproveChange(ctx context.Context, req api.ApproveChangeRequestObject) (api.ApproveChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	if id, ok := identity.From(ctx); ok && id.Kind != identity.Human {
		return nil, ErrAgentsNever
	}
	comment := ""
	if req.Body != nil {
		comment = req.Body.Comment
	}
	a, err := svc.Approve(ctx, req.Id, comment)
	if err != nil {
		return nil, err
	}
	out := change.ApprovalToAPI(*a)
	out.At = time.Now().UTC()
	return api.ApproveChange200JSONResponse(out), nil
}

func (s *Server) MergeChange(ctx context.Context, req api.MergeChangeRequestObject) (api.MergeChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	id, ok := identity.From(ctx)
	if ok && id.Kind != identity.Human {
		return nil, ErrAgentsNever
	}
	// Open steps (apply paths that need a manual step) come from the impact
	// of the change; the branch is gone after the merge.
	openSteps := s.openSteps(ctx, svc, req.Id)
	r, err := svc.Merge(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := api.MergeResult{Sha: r.SHA, Message: r.Message, Change: change.SummaryToAPI(r.Change), OpenSteps: openSteps}
	if r.MR != nil {
		out.Mr = change.MRToAPI(r.MR)
	}
	if s.d.Audit != nil {
		_ = s.d.Audit.Append(ctx, audit.Entry{Actor: id.Principal(), ActorKind: string(id.Kind), Action: audit.ChangeMerge,
			Target: req.Id, Detail: chShortSHA(r.SHA) + " " + chFirstLine(r.Message)})
	}
	return api.MergeChange200JSONResponse(out), nil
}

func (s *Server) openSteps(ctx context.Context, svc change.Service, id string) []api.OpenStep {
	if s.d.Renderer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	base, head, err := svc.Sources(ctx, id)
	if err != nil {
		return nil
	}
	imp, err := s.d.Renderer.Impact(ctx, base, head)
	if err != nil {
		return nil
	}
	var out []api.OpenStep
	for _, o := range imp.OpenSteps {
		out = append(out, api.OpenStep{Kind: o.Kind, Name: o.Name, Path: o.Path, ApplyPath: api.ApplyPath(o.ApplyPath), Text: o.Text})
	}
	return out
}

func (s *Server) CloseChange(ctx context.Context, req api.CloseChangeRequestObject) (api.CloseChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	sum, err := svc.Close(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if id, ok := identity.From(ctx); ok {
			_ = s.d.Audit.Append(ctx, audit.Entry{Actor: id.Principal(), ActorKind: string(id.Kind), Action: audit.ChangeClose, Target: req.Id})
		}
	}
	return api.CloseChange200JSONResponse(change.SummaryToAPI(*sum)), nil
}

func chShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func chFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
