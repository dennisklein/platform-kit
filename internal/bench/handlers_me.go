package bench

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
)

// Version is the bench version, set by cmd/pk.
var Version = "dev"

func (s *Server) GetMe(ctx context.Context, _ api.GetMeRequestObject) (api.GetMeResponseObject, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return nil, errUnauthorized
	}
	me := api.Me{
		Id: id.User, Name: id.Name, Email: id.Email, Kind: api.IdentityKind(id.Kind),
		ActsFor: id.ActsFor, Roles: []string{}, Demo: s.cfg.Demo,
	}
	if me.Name == "" {
		me.Name = id.User
	}
	if s.d.Policy != nil {
		if p, err := s.d.Policy.Current(ctx); err == nil && p != nil {
			for _, pp := range p.People {
				if pp.ID == id.Principal() && id.Kind == identity.Human {
					me.Roles, me.Teams = pp.Roles, pp.Teams
				}
			}
			for _, a := range p.Agents {
				if a.ID == id.User {
					me.Roles = []string{"agent"}
				}
			}
		}
	}
	if id.Kind == identity.Agent {
		if g, ok := s.grants.Get(id.User); ok {
			me.Grant = grantToAPI(g, time.Now())
		}
	}
	me.Initials = initials(me.Name)
	return api.GetMe200JSONResponse(me), nil
}

func initials(name string) string {
	var out []rune
	for _, f := range strings.Fields(name) {
		out = append(out, []rune(strings.ToUpper(f))[0])
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func grantToAPI(g Grant, now time.Time) api.AgentGrant {
	can := g.Can
	if can == nil {
		can = []string{}
	}
	return api.AgentGrant{
		Id: g.ID, ActsFor: g.ActsFor, Can: can, Mode: api.AgentMode(g.Mode),
		Active: g.Active(now), ApprovedBy: g.ApprovedBy, ApprovedAt: g.ApprovedAt,
		ExpiresAt: g.ExpiresAt, LastSeenAt: g.LastSeenAt, LastTool: g.LastTool,
	}
}

// statusCache keeps the bench status for a few seconds, so the top bar can
// poll without pinging the forge on each request.
type statusCache struct {
	mu  sync.Mutex
	at  time.Time
	val api.BenchStatus
}

const statusTTL = 5 * time.Second

func (s *Server) GetStatus(ctx context.Context, _ api.GetStatusRequestObject) (api.GetStatusResponseObject, error) {
	s.status.mu.Lock()
	defer s.status.mu.Unlock()
	if time.Since(s.status.at) < statusTTL {
		return api.GetStatus200JSONResponse(s.status.val), nil
	}
	st := s.benchStatus(ctx)
	s.status.val, s.status.at = st, time.Now()
	return api.GetStatus200JSONResponse(st), nil
}

func (s *Server) benchStatus(ctx context.Context) api.BenchStatus {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st := api.BenchStatus{
		Mode: api.Local, Version: Version, Demo: s.cfg.Demo,
		Forge: api.ServiceStatus{Name: "Gitea", Url: s.cfg.ForgeURL},
		Flux:  api.ServiceStatus{Name: "Flux", Url: s.cfg.FluxURL},
		Llm:   api.LlmStatus{Mock: true, Model: "mock model", Reachable: true},
	}
	var wg sync.WaitGroup
	if s.d.Forge != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.Forge.Name = s.d.Forge.Name()
			if err := s.d.Forge.Ping(ctx); err != nil {
				st.Forge.Detail = err.Error()
			} else {
				st.Forge.Reachable = true
			}
		}()
	} else {
		st.Forge.Detail = "not configured"
	}
	if s.d.Flux != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.d.Flux.Status(ctx); err != nil {
				st.Flux.Detail = err.Error()
			} else {
				st.Flux.Reachable = true
			}
		}()
	} else {
		st.Flux.Detail = "not configured"
	}
	if s.d.LLM != nil {
		st.Llm = api.LlmStatus{Mock: s.d.LLM.Mock(), Model: s.d.LLM.Model(),
			EmbedModel: s.cfg.LLMEmbedModel, BaseUrl: s.cfg.LLMBaseURL, Reachable: true}
		if !s.d.LLM.Mock() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.d.LLM.Ping(ctx); err != nil {
					st.Llm.Reachable, st.Llm.Detail = false, err.Error()
				}
			}()
		}
	}
	if s.d.Changes != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := s.d.Changes.MainDir(ctx)
			if err != nil {
				return
			}
			log, err := gitx.Open(dir).Log(ctx, "HEAD", "-n", "1")
			if err != nil || len(log) == 0 {
				return
			}
			e := log[0]
			st.Main = api.MainRef{Sha: e.SHA, Subject: e.Subject, At: e.CommitDate}
			for _, t := range e.Trailers {
				if t.Key == "Pk-Change" {
					st.Main.Change = t.Value
				}
			}
		}()
	}
	wg.Wait()
	return st
}

func (s *Server) GetFocus(ctx context.Context, _ api.GetFocusRequestObject) (api.GetFocusResponseObject, error) {
	id := identity.MustFrom(ctx)
	return api.GetFocus200JSONResponse(s.focus.Get(id.Principal())), nil
}

// SetFocus moves the focus of the caller's person. An agent in suggest mode,
// or any agent while the person pinned the view, only sends a presence event
// with a "look here" suggestion.
func (s *Server) SetFocus(ctx context.Context, req api.SetFocusRequestObject) (api.SetFocusResponseObject, error) {
	if req.Body == nil {
		return nil, Errorf(400, "missing body")
	}
	id := identity.MustFrom(ctx)
	principal := id.Principal()
	if id.Kind == identity.Agent {
		cur := s.focus.Get(principal)
		g, _ := s.grants.Get(id.User)
		upd := *req.Body
		upd.Pinned = false
		// An MCP tool call sends its own presence event; only a "look
		// here" card is sent from here, with the tool, in its place.
		tc := events.ToolCallOf(ctx)
		if g.Mode == ModeSuggest || cur.Pinned {
			p := api.Presence{
				Actor: id.User, Kind: api.IdentityKind(id.Kind), ActsFor: id.ActsFor,
				ChangeId: upd.ChangeId, Path: upd.Path, Suggest: upd,
				Text: "look here", At: time.Now().UTC(),
			}
			if tc != nil {
				p.Tool, p.Text, tc.Sent = tc.Tool, tc.Tool, true
				switch {
				case upd.Path != "":
					p.Text += " " + upd.Path
				case upd.ChangeId != "":
					p.Text += " " + upd.ChangeId
				}
				p.Text += " (look here)"
			}
			s.hub.PublishTo(principal, events.Presence, p)
			return api.SetFocus200JSONResponse(cur), nil
		}
		fo := s.focus.Set(principal, id.User, upd)
		if tc == nil {
			s.hub.PublishTo(principal, events.Presence, api.Presence{
				Actor: id.User, Kind: api.IdentityKind(id.Kind), ActsFor: id.ActsFor,
				ChangeId: upd.ChangeId, Path: upd.Path, At: time.Now().UTC(),
			})
		}
		return api.SetFocus200JSONResponse(fo), nil
	}
	return api.SetFocus200JSONResponse(s.focus.Set(principal, id.User, *req.Body)), nil
}

func (s *Server) ListAudit(ctx context.Context, req api.ListAuditRequestObject) (api.ListAuditResponseObject, error) {
	limit := 200
	if req.Params.Limit > 0 {
		limit = req.Params.Limit
	}
	entries, err := s.d.Audit.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make(api.ListAudit200JSONResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.AuditEntry{
			Id: e.ID, Time: e.Time, Actor: e.Actor, ActorKind: api.IdentityKind(e.ActorKind),
			Action: e.Action, Target: e.Target, Detail: e.Detail,
		})
	}
	return out, nil
}
