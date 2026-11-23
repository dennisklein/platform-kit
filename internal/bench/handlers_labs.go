package bench

// Change labs, lab definitions and topology (area C).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/lab"
)

// labError maps lab service errors to HTTP errors.
func labError(err error) error {
	var busy *lab.BusyError
	switch {
	case errors.As(err, &busy):
		return &HTTPError{Status: http.StatusConflict, Msg: "a lab run is still running", Detail: busy.Run}
	case errors.Is(err, lab.ErrNoRight):
		return &HTTPError{Status: http.StatusForbidden, Msg: err.Error()}
	case errors.Is(err, lab.ErrNoWorktree):
		return &HTTPError{Status: http.StatusConflict, Msg: err.Error()}
	case errors.Is(err, lab.ErrScenario):
		return &HTTPError{Status: http.StatusBadRequest, Msg: err.Error()}
	case errors.Is(err, lab.ErrNotFound):
		return &HTTPError{Status: http.StatusNotFound, Msg: err.Error()}
	}
	return err
}

func (s *Server) RunLab(ctx context.Context, req api.RunLabRequestObject) (api.RunLabResponseObject, error) {
	if s.d.Labs == nil {
		return nil, unavailable("labs")
	}
	scenario := ""
	if req.Body != nil {
		scenario = strings.TrimSpace(req.Body.Scenario)
	}
	run, err := s.d.Labs.RunChange(ctx, string(req.Id), scenario)
	if err != nil {
		return nil, labError(err)
	}
	return api.RunLab202JSONResponse(lab.RunToAPI(run)), nil
}

func (s *Server) ListLabs(ctx context.Context, req api.ListLabsRequestObject) (api.ListLabsResponseObject, error) {
	if s.d.Labs == nil {
		return nil, unavailable("labs")
	}
	out := api.LabList{Definitions: []api.LabDefinition{}, Runs: []api.LabRun{}, Scenarios: s.d.Labs.Scenarios()}
	defs, err := s.d.Labs.Definitions(ctx)
	if err != nil {
		s.log.Warn("lab definitions", "err", err)
	}
	var twins map[string]api.Cluster
	if s.d.Flux != nil && len(defs) > 0 {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		if st, err := s.d.Flux.Status(c); err == nil {
			twins = map[string]api.Cluster{}
			for _, t := range st.Twins {
				twins[t.ID] = FluxClusterToAPI(&t)
			}
		}
		cancel()
	}
	for _, d := range defs {
		a := lab.DefinitionToAPI(d)
		if st, ok := twins[a.Name]; ok {
			a.Status = st
		}
		out.Definitions = append(out.Definitions, a)
	}
	runs, err := s.d.Labs.Runs(ctx, req.Params.ChangeId)
	if err != nil {
		return nil, labError(err)
	}
	if req.Params.ChangeId == "" && len(runs) > 50 {
		runs = runs[:50]
	}
	for i := range runs {
		out.Runs = append(out.Runs, lab.RunToAPI(&runs[i]))
	}
	return api.ListLabs200JSONResponse(out), nil
}

func (s *Server) GetLabRun(ctx context.Context, req api.GetLabRunRequestObject) (api.GetLabRunResponseObject, error) {
	if s.d.Labs == nil {
		return nil, unavailable("labs")
	}
	id := req.Run
	if strings.Contains(id, "%") {
		if u, err := url.PathUnescape(id); err == nil {
			id = u
		}
	}
	run, err := s.d.Labs.Get(ctx, id)
	if err != nil {
		return nil, labError(err)
	}
	return api.GetLabRun200JSONResponse(lab.RunToAPI(run)), nil
}

func (s *Server) GetTopology(ctx context.Context, req api.GetTopologyRequestObject) (api.GetTopologyResponseObject, error) {
	if s.d.Labs == nil {
		return nil, unavailable("labs")
	}
	t, err := s.d.Labs.Topology(ctx, string(req.Params.Ref), req.Params.Cluster)
	if err != nil {
		return nil, labError(err)
	}
	return api.GetTopology200JSONResponse(lab.TopologyToAPI(t)), nil
}
