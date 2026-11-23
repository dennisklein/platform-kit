package bench

// Demo helpers, only with PK_DEMO=1 (area A).

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// approverAs is implemented by the change service: approve in the forge
// with the token of another person (demo only).
type approverAs interface {
	ApproveAs(ctx context.Context, user, id, comment string) (*policy.Approval, error)
}

func (s *Server) DemoApprove(ctx context.Context, req api.DemoApproveRequestObject) (api.DemoApproveResponseObject, error) {
	if !s.cfg.Demo {
		return nil, errNotDemo
	}
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	if req.Body == nil || req.Body.As == "" || req.Body.ChangeId == "" {
		return nil, Errorf(http.StatusBadRequest, "as and changeId are required")
	}
	if id, ok := identity.From(ctx); ok && id.Kind != identity.Human {
		return nil, ErrAgentsNever
	}
	as := strings.TrimSpace(req.Body.As)
	if strings.HasSuffix(as, "-agent") || strings.HasSuffix(as, "-bot") || as == "pk-ci" {
		return nil, ErrAgentsNever
	}
	ap, ok := svc.(approverAs)
	if !ok {
		return nil, unavailable("demo approve")
	}
	a, err := ap.ApproveAs(ctx, as, req.Body.ChangeId, "Approved in the demo")
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		actor := ""
		if id, ok := identity.From(ctx); ok {
			actor = id.Principal()
		}
		_ = s.d.Audit.Append(ctx, audit.Entry{Actor: actor, ActorKind: "human", Action: audit.DemoApprove,
			Target: req.Body.ChangeId, Detail: "approved as " + as})
	}
	out := change.ApprovalToAPI(*a)
	out.At = time.Now().UTC()
	return api.DemoApprove200JSONResponse(out), nil
}

func (s *Server) DemoFlux(ctx context.Context, req api.DemoFluxRequestObject) (api.DemoFluxResponseObject, error) {
	if !s.cfg.Demo {
		return nil, errNotDemo
	}
	if s.d.Flux == nil {
		return nil, unavailable("fluxsim")
	}
	if req.Body == nil || req.Body.Cluster == "" {
		return nil, Errorf(http.StatusBadRequest, "cluster is required")
	}
	st, err := s.d.Flux.SetFailing(ctx, req.Body.Cluster, req.Body.Fail)
	if err != nil {
		return nil, err
	}
	return api.DemoFlux200JSONResponse(demoClusterToAPI(st)), nil
}

// demoClusterToAPI converts a fluxsim cluster status.
func demoClusterToAPI(c *fluxsim.ClusterStatus) api.Cluster {
	out := api.Cluster{Id: c.ID, Role: c.Role, Ring: c.Ring, Healthy: c.Healthy, Failing: c.Failing, Held: c.Held,
		Objects: c.Objects, Message: c.Message}
	if strings.HasPrefix(c.Ref, "ring/") {
		out.RingTag = c.Ref
	}
	if c.Applied != nil {
		out.Applied = api.Revision{Sha: c.Applied.SHA, At: c.Applied.At, Change: c.Applied.Change}
	}
	if c.Verified != nil {
		out.Verified = api.Revision{Sha: c.Verified.SHA, At: c.Verified.At, Change: c.Verified.Change}
	}
	for _, k := range c.Kustomizations {
		out.Kustomizations = append(out.Kustomizations, api.KustomizationStatus{Name: k.Name, Path: k.Path, Ready: k.Ready,
			Revision: k.Revision, Message: k.Message})
	}
	return out
}
