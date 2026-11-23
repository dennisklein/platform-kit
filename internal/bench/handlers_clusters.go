package bench

// Clusters, rings (P2) and revert (P2) (area C).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/rings"
)

// FluxClusterToAPI converts a fluxsim cluster status.
func FluxClusterToAPI(c *fluxsim.ClusterStatus) api.Cluster {
	out := api.Cluster{Id: c.ID, Role: c.Role, Ring: c.Ring, Healthy: c.Healthy, Failing: c.Failing, Held: c.Held,
		Objects: c.Objects, Message: c.Message, Reachable: true, Kustomizations: []api.KustomizationStatus{}}
	if strings.HasPrefix(c.Ref, "ring/") {
		out.RingTag = c.Ref
	}
	if c.Applied != nil {
		out.Applied = api.Revision{Sha: c.Applied.SHA, At: c.Applied.At, Change: c.Applied.Change}
	}
	if c.Verified != nil {
		out.Verified = api.Revision{Sha: c.Verified.SHA, At: c.Verified.At, Change: c.Verified.Change}
	}
	if c.Pending != nil {
		out.Pending = api.Revision{Sha: c.Pending.SHA, At: c.Pending.At, Change: c.Pending.Change}
	}
	for _, k := range c.Kustomizations {
		out.Kustomizations = append(out.Kustomizations, api.KustomizationStatus{Name: k.Name, Path: k.Path, Ready: k.Ready,
			Revision: k.Revision, Message: k.Message})
	}
	return out
}

func ringRank(r string) int {
	switch r {
	case "lab":
		return 0
	case "canary":
		return 1
	case "prod":
		return 2
	}
	return 3
}

// clusterList is the clusters of main with their Flux state.
func (s *Server) clusterList(ctx context.Context) ([]api.Cluster, *fluxsim.Status, error) {
	byID := map[string]*api.Cluster{}
	var ids []string
	add := func(c api.Cluster) {
		if _, ok := byID[c.Id]; !ok {
			ids = append(ids, c.Id)
		}
		cc := c
		byID[c.Id] = &cc
	}
	if s.d.Changes != nil && s.d.Renderer != nil {
		if src, err := s.d.Changes.Source(ctx, "main"); err == nil {
			if cs, err := s.d.Renderer.Clusters(ctx, src); err == nil {
				for _, c := range cs {
					add(api.Cluster{Id: c.ID, Role: c.Role, Ring: c.Ring, Path: c.Path, Kustomizations: []api.KustomizationStatus{},
						Message: "no Flux status"})
				}
			}
		}
	}
	var st *fluxsim.Status
	if s.d.Flux != nil {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		var err error
		st, err = s.d.Flux.Status(c)
		cancel()
		if err != nil {
			st = nil
			for _, id := range ids {
				byID[id].Message = "fluxsim unreachable"
			}
		}
	}
	if st != nil {
		for i := range st.Clusters {
			fc := FluxClusterToAPI(&st.Clusters[i])
			if old, ok := byID[fc.Id]; ok {
				fc.Path = old.Path
				if fc.Role == "" {
					fc.Role = old.Role
				}
				if fc.Ring == "" {
					fc.Ring = old.Ring
				}
			}
			add(fc)
		}
	}
	if len(ids) == 0 && st == nil && s.d.Changes == nil {
		return nil, nil, unavailable("clusters")
	}
	out := make([]api.Cluster, 0, len(ids))
	for _, id := range ids {
		c := byID[id]
		if s.d.Rings != nil && c.Ring != "" && s.d.Rings.HeldBy(c.Ring) != "" {
			c.Held = true
		}
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ringRank(out[i].Ring) != ringRank(out[j].Ring) {
			return ringRank(out[i].Ring) < ringRank(out[j].Ring)
		}
		return out[i].Id < out[j].Id
	})
	return out, st, nil
}

func (s *Server) ListClusters(ctx context.Context, req api.ListClustersRequestObject) (api.ListClustersResponseObject, error) {
	out, _, err := s.clusterList(ctx)
	if err != nil {
		return nil, err
	}
	return api.ListClusters200JSONResponse(out), nil
}

// RingToAPI converts a ring state.
func RingToAPI(r *rings.Ring) api.RingStatus {
	out := api.RingStatus{Ring: r.Ring, Tag: r.Tag, Sha: r.SHA, Change: r.Change, Held: r.Held, HeldBy: r.HeldBy,
		Clusters: append([]string{}, r.Clusters...), Following: r.Following, Candidate: r.Candidate,
		CandidateChange: r.CandidateChange, Gate: api.RingStatusGate(r.Gate), Reason: r.Reason, SoakUntil: r.SoakUntil,
		Verified: r.Verified}
	return out
}

func (s *Server) ringSvc() (rings.Service, error) {
	if s.d.Rings == nil {
		return nil, unavailable("rings")
	}
	return s.d.Rings, nil
}

func ringError(err error) error {
	switch {
	case errors.Is(err, rings.ErrNotFound):
		return &HTTPError{Status: http.StatusNotFound, Msg: err.Error()}
	case errors.Is(err, rings.ErrForbidden):
		return &HTTPError{Status: http.StatusForbidden, Msg: err.Error()}
	case errors.Is(err, rings.ErrConflict):
		return &HTTPError{Status: http.StatusConflict, Msg: err.Error()}
	}
	return &HTTPError{Status: http.StatusBadGateway, Msg: "rings", Detail: err.Error()}
}

func (s *Server) ListRings(ctx context.Context, req api.ListRingsRequestObject) (api.ListRingsResponseObject, error) {
	svc, err := s.ringSvc()
	if err != nil {
		return nil, err
	}
	rs, err := svc.List(ctx)
	if err != nil {
		return nil, ringError(err)
	}
	out := make([]api.RingStatus, 0, len(rs))
	for i := range rs {
		out = append(out, RingToAPI(&rs[i]))
	}
	return api.ListRings200JSONResponse(out), nil
}

// ringAction runs a ring action as the request identity and audits it.
func (s *Server) ringAction(ctx context.Context, action, ring, detail string,
	f func(svc rings.Service, who identity.Identity) (*rings.Ring, error)) (api.RingStatus, error) {
	svc, err := s.ringSvc()
	if err != nil {
		return api.RingStatus{}, err
	}
	who, ok := identity.From(ctx)
	if !ok {
		return api.RingStatus{}, errUnauthorized
	}
	if !who.IsHuman() {
		return api.RingStatus{}, errHumansOnly
	}
	r, err := f(svc, who)
	if err != nil {
		return api.RingStatus{}, ringError(err)
	}
	if detail == "" {
		detail = short(r.SHA)
	}
	_ = s.d.Audit.Append(ctx, audit.Entry{Actor: who.Principal(), ActorKind: string(who.Kind), Action: action,
		Target: "ring/" + ring, Detail: detail})
	return RingToAPI(r), nil
}

func (s *Server) PromoteRing(ctx context.Context, req api.PromoteRingRequestObject) (api.PromoteRingResponseObject, error) {
	sha := ""
	if req.Body != nil {
		sha = strings.TrimSpace(req.Body.Sha)
	}
	out, err := s.ringAction(ctx, "ring.promote", req.Ring, "", func(svc rings.Service, who identity.Identity) (*rings.Ring, error) {
		return svc.Promote(ctx, who, req.Ring, sha)
	})
	if err != nil {
		return nil, err
	}
	return api.PromoteRing200JSONResponse(out), nil
}

func (s *Server) HoldRing(ctx context.Context, req api.HoldRingRequestObject) (api.HoldRingResponseObject, error) {
	out, err := s.ringAction(ctx, "ring.hold", req.Ring, "hold", func(svc rings.Service, who identity.Identity) (*rings.Ring, error) {
		return svc.Hold(ctx, who, req.Ring)
	})
	if err != nil {
		return nil, err
	}
	return api.HoldRing200JSONResponse(out), nil
}

func (s *Server) ReleaseRing(ctx context.Context, req api.ReleaseRingRequestObject) (api.ReleaseRingResponseObject, error) {
	out, err := s.ringAction(ctx, "ring.release", req.Ring, "release", func(svc rings.Service, who identity.Identity) (*rings.Ring, error) {
		return svc.Release(ctx, who, req.Ring)
	})
	if err != nil {
		return nil, err
	}
	return api.ReleaseRing200JSONResponse(out), nil
}

// recordPrefixes are paths a revert keeps: lab reports, work items and
// change metadata are records, not configuration.
var recordPrefixes = []string{"kb/labs/", "work/", change.MetaDir + "/"}

// RevertChange opens a change that reverts the merge of a merged change:
// git revert of the merge commit (first parent) without the records.
func (s *Server) RevertChange(ctx context.Context, req api.RevertChangeRequestObject) (api.RevertChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	who, ok := identity.From(ctx)
	if !ok {
		return nil, errUnauthorized
	}
	orig, err := svc.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if orig.MergeSHA == "" {
		return nil, Errorf(http.StatusConflict, "%s is not merged", req.Id)
	}
	title := orig.Title
	if len(title) > 60 {
		title = title[:60]
	}
	nc, err := svc.Create(ctx, change.CreateRequest{Title: "Revert \"" + title + "\"",
		Intent: fmt.Sprintf("Revert %s (merge %s): %s", orig.ID, short(orig.MergeSHA), orig.Title)})
	if err != nil {
		return nil, err
	}
	var r *gitx.Repo
	fail := func(e error) (api.RevertChangeResponseObject, error) {
		c := context.WithoutCancel(ctx)
		if r != nil {
			// A clean worktree lets Close remove the unpushed change.
			_, _ = r.Run(c, "reset", "-q", "--hard", "HEAD")
			_, _ = r.Run(c, "clean", "-q", "-fd")
		}
		_, _ = svc.Close(c, nc.ID)
		return nil, e
	}
	wt, err := svc.Worktree(ctx, nc.ID)
	if err != nil {
		return fail(err)
	}
	r = gitx.Open(wt)
	args := []string{"revert", "--no-commit"}
	if out, err := r.Run(ctx, "rev-list", "--parents", "-n", "1", orig.MergeSHA); err == nil && len(strings.Fields(out)) > 2 {
		args = append(args, "-m", "1")
	}
	if _, err := r.Run(ctx, append(args, orig.MergeSHA)...); err != nil {
		_, _ = r.Run(ctx, "revert", "--abort")
		return fail(Errorf(http.StatusConflict, "the revert of %s conflicts with main; revert by hand", orig.ID))
	}
	kept, err := keepRecords(ctx, r, wt)
	if err != nil {
		return fail(err)
	}
	if kept == 0 {
		_, _ = r.Run(ctx, "revert", "--quit")
		return fail(Errorf(http.StatusConflict, "%s changed only records (reports, work items); nothing to revert", orig.ID))
	}
	_, _ = r.Run(ctx, "revert", "--quit")
	msg := fmt.Sprintf("Revert %q\n\nThis reverts merge %s of %s.", orig.Title, short(orig.MergeSHA), orig.ID)
	if _, err := svc.Commit(ctx, nc.ID, msg, nil); err != nil {
		return fail(err)
	}
	_ = s.d.Audit.Append(ctx, audit.Entry{Actor: who.Principal(), ActorKind: string(who.Kind), Action: "change.revert",
		Target: orig.ID, Detail: nc.ID})
	ch, err := svc.Get(ctx, nc.ID)
	if err != nil {
		return nil, err
	}
	return api.RevertChange201JSONResponse(s.changeToAPI(ctx, ch)), nil
}

// keepRecords undoes the revert of record paths and returns how many paths
// the revert still changes.
func keepRecords(ctx context.Context, r *gitx.Repo, wt string) (int, error) {
	out, err := r.Run(ctx, "diff", "--cached", "--name-only", "--no-renames", "HEAD")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range strings.Split(out, "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		record := false
		for _, pre := range recordPrefixes {
			if strings.HasPrefix(p, pre) {
				record = true
			}
		}
		if !record {
			n++
			continue
		}
		if _, err := r.Run(ctx, "cat-file", "-e", "HEAD:"+p); err == nil {
			if _, err := r.Run(ctx, "checkout", "HEAD", "--", p); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := r.Run(ctx, "rm", "-q", "-f", "--cached", "--", p); err != nil {
			return 0, err
		}
		_ = os.Remove(filepath.Join(wt, filepath.FromSlash(p)))
	}
	return n, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
