package bench

// Advisors, commit message, chat, suggestions, LLM log, search and agent grants (area D).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/chat"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/inproc"
	"github.com/dennisklein/platform-kit/internal/llm"
	"github.com/dennisklein/platform-kit/internal/mcp"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// llmErr maps errors of the LLM layer to HTTP errors.
func llmErr(err error) error {
	var ie *inproc.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ie):
		return &HTTPError{Status: ie.Status, Msg: ie.Msg, Detail: ie.Detail, Paths: ie.Paths}
	case errors.Is(err, advisor.ErrUnknownRecipe), errors.Is(err, chat.ErrNotFound), errors.Is(err, llm.ErrNotFound):
		return &HTTPError{Status: http.StatusNotFound, Msg: err.Error()}
	case errors.Is(err, advisor.ErrNothingToCommit), errors.Is(err, chat.ErrStale), errors.Is(err, chat.ErrDecided):
		return &HTTPError{Status: http.StatusConflict, Msg: err.Error()}
	case errors.Is(err, chat.ErrNoQuestion):
		return &HTTPError{Status: http.StatusBadRequest, Msg: err.Error()}
	case errors.Is(err, inproc.ErrNoHandler):
		return unavailable("the bench API")
	}
	return err
}

func sourcesToAPI(ss []llm.Source) []api.Source { return advisor.APISources(ss) }

func suggestionsToAPI(ss []advisor.Suggestion) []api.Suggestion {
	out := make([]api.Suggestion, 0, len(ss))
	for _, s := range ss {
		out = append(out, chat.ToAPI(s))
	}
	return out
}

// requireHuman answers 403 for agents and bots.
func requireHuman(ctx context.Context) (identity.Identity, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return id, errUnauthorized
	}
	if id.Kind != identity.Human {
		return id, errHumansOnly
	}
	return id, nil
}

// requireRole lets people with one of the roles through. Without a policy
// every person passes.
func (s *Server) requireRole(ctx context.Context, roles ...string) (identity.Identity, error) {
	id, err := requireHuman(ctx)
	if err != nil {
		return id, err
	}
	if s.d.Policy == nil {
		return id, nil
	}
	p, perr := s.d.Policy.Current(ctx)
	if perr != nil || p == nil {
		return id, nil
	}
	for _, pp := range p.People {
		if pp.ID != id.User {
			continue
		}
		for _, r := range pp.Roles {
			if slices.Contains(roles, r) {
				return id, nil
			}
		}
	}
	return id, &HTTPError{Status: http.StatusForbidden, Msg: "only " + strings.Join(roles, " or ") + " can do this"}
}

// ---------------------------------------------------------------- advisors

func (s *Server) ListAdvisors(ctx context.Context, req api.ListAdvisorsRequestObject) (api.ListAdvisorsResponseObject, error) {
	if s.d.Advisors == nil {
		return nil, unavailable("the advisors")
	}
	out := api.ListAdvisors200JSONResponse{}
	for _, r := range s.d.Advisors.Recipes() {
		tools := r.Tools
		if tools == nil {
			tools = []string{}
		}
		out = append(out, api.AdvisorRecipe{Id: r.ID, Title: r.Title, Question: r.Question, Tools: tools,
			Output: api.AdvisorRecipeOutput(r.Output), Scope: api.AdvisorRecipeScope(r.Scope)})
	}
	return out, nil
}

// AdvisorResultToAPI converts a recipe result.
func AdvisorResultToAPI(r *advisor.Result) api.AdvisorResult {
	out := api.AdvisorResult{
		Recipe: r.Recipe, Title: r.Title, ChangeId: r.ChangeID, Note: r.Note, Model: r.Model, Mock: r.Mock, Fallback: r.Fallback,
		LogId: r.LogID, CreatedAt: r.CreatedAt, Points: []api.AdvisorPoint{},
		Reading: sourcesToAPI(r.Reading), Suggestions: suggestionsToAPI(r.Suggestions),
	}
	for _, p := range r.Points {
		out.Points = append(out.Points, api.AdvisorPoint{Title: p.Title, Text: p.Text,
			Severity: api.AdvisorPointSeverity(p.Severity), Sources: sourcesToAPI(p.Sources)})
	}
	return out
}

func (s *Server) RunAdvisor(ctx context.Context, req api.RunAdvisorRequestObject) (api.RunAdvisorResponseObject, error) {
	if s.d.Advisors == nil {
		return nil, unavailable("the advisors")
	}
	res, err := s.d.Advisors.Run(ctx, req.Recipe, req.Id)
	if err != nil {
		return nil, llmErr(err)
	}
	return api.RunAdvisor200JSONResponse(AdvisorResultToAPI(res)), nil
}

func (s *Server) SuggestCommitMessage(ctx context.Context, req api.SuggestCommitMessageRequestObject) (api.SuggestCommitMessageResponseObject, error) {
	if s.d.Advisors == nil {
		return nil, unavailable("the advisors")
	}
	var paths []string
	if req.Body != nil {
		paths = req.Body.Paths
	}
	res, err := s.d.Advisors.CommitMessage(ctx, req.Id, paths)
	if err != nil {
		return nil, llmErr(err)
	}
	return api.SuggestCommitMessage200JSONResponse(api.CommitMessageSuggestion{
		Message: res.Message, Subject: res.Subject, Body: res.Body, Model: res.Model, Mock: res.Mock, Fallback: res.Fallback,
		LogId: res.LogID, Sources: sourcesToAPI(res.Sources),
	}), nil
}

// ---------------------------------------------------------------- chat and suggestions

func (s *Server) Chat(ctx context.Context, req api.ChatRequestObject) (api.ChatResponseObject, error) {
	if s.d.Chat == nil {
		return nil, unavailable("the chat")
	}
	if id, ok := identity.From(ctx); ok && id.Kind != identity.Human {
		return nil, &HTTPError{Status: http.StatusForbidden, Msg: "the chat is for people; agents use the MCP tools"}
	}
	if req.Body == nil {
		return nil, Errorf(http.StatusBadRequest, "messages are required")
	}
	cr := chat.Request{ChangeID: req.Body.ChangeId, Path: req.Body.Path, RequestID: req.Body.RequestId}
	for _, m := range req.Body.Messages {
		cr.Messages = append(cr.Messages, chat.Message{Role: string(m.Role), Content: m.Content})
	}
	a, err := s.d.Chat.Ask(ctx, cr)
	if err != nil {
		return nil, llmErr(err)
	}
	return api.Chat200JSONResponse(api.ChatAnswer{
		Text: a.Text, Sources: sourcesToAPI(a.Sources), Suggestions: suggestionsToAPI(a.Suggestions),
		Refused: a.Refused, Model: a.Model, Mock: a.Mock, Fallback: a.Fallback, LogId: a.LogID,
		FallbackReason: a.FallbackReason, Steps: chat.StepsToAPI(a.Steps),
	}), nil
}

func (s *Server) ListSuggestions(ctx context.Context, req api.ListSuggestionsRequestObject) (api.ListSuggestionsResponseObject, error) {
	if s.d.Chat == nil {
		return nil, unavailable("the suggestions")
	}
	list, err := s.d.Chat.Suggestions(ctx, req.Id)
	if err != nil {
		return nil, llmErr(err)
	}
	return api.ListSuggestions200JSONResponse(suggestionsToAPI(list)), nil
}

func (s *Server) AcceptSuggestion(ctx context.Context, req api.AcceptSuggestionRequestObject) (api.AcceptSuggestionResponseObject, error) {
	if s.d.Chat == nil {
		return nil, unavailable("the suggestions")
	}
	if _, err := requireHuman(ctx); err != nil {
		return nil, err
	}
	sg, err := s.d.Chat.Accept(ctx, req.Sid)
	if err != nil {
		return nil, llmErr(err)
	}
	return api.AcceptSuggestion200JSONResponse(chat.ToAPI(*sg)), nil
}

func (s *Server) RejectSuggestion(ctx context.Context, req api.RejectSuggestionRequestObject) (api.RejectSuggestionResponseObject, error) {
	if s.d.Chat == nil {
		return nil, unavailable("the suggestions")
	}
	if _, err := requireHuman(ctx); err != nil {
		return nil, err
	}
	sg, err := s.d.Chat.Reject(ctx, req.Sid)
	if err != nil {
		return nil, llmErr(err)
	}
	return api.RejectSuggestion200JSONResponse(chat.ToAPI(*sg)), nil
}

// ---------------------------------------------------------------- LLM log

func llmEntryToAPI(e llm.LogEntry) api.LlmLogEntry {
	out := api.LlmLogEntry{
		Id: e.ID, Time: e.Time, User: e.User, Surface: api.LlmLogEntrySurface(e.Surface), Recipe: e.Recipe,
		ChangeId: e.ChangeID, Model: e.Model, Mock: e.Mock, Prompt: e.Prompt, Answer: e.Answer,
		Redactions: e.Redactions, Rating: api.LlmLogEntryRating(e.Rating), DurationMs: e.DurationMs, Error: e.Error,
	}
	if len(e.Sources) > 0 {
		out.Sources = sourcesToAPI(e.Sources)
	}
	return out
}

// Readers of the LLM log: platform admins, and auditors (read only).
var llmLogReaders = []string{"platform-admin", "auditor"}

func (s *Server) ListLlmLog(ctx context.Context, req api.ListLlmLogRequestObject) (api.ListLlmLogResponseObject, error) {
	if s.d.LLMLog == nil {
		return nil, unavailable("the LLM log")
	}
	if _, err := s.requireRole(ctx, llmLogReaders...); err != nil {
		return nil, err
	}
	limit := req.Params.Limit
	if limit <= 0 {
		limit = 200
	}
	entries, total, err := s.d.LLMLog.List(ctx, req.Params.Q, limit)
	if err != nil {
		return nil, err
	}
	out := api.LlmLogList{Entries: []api.LlmLogEntry{}, Total: total}
	for _, e := range entries {
		out.Entries = append(out.Entries, llmEntryToAPI(e))
	}
	return api.ListLlmLog200JSONResponse(out), nil
}

func (s *Server) ExportLlmLog(ctx context.Context, req api.ExportLlmLogRequestObject) (api.ExportLlmLogResponseObject, error) {
	if s.d.LLMLog == nil {
		return nil, unavailable("the LLM log")
	}
	id, err := s.requireRole(ctx, llmLogReaders...)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := s.d.LLMLog.Export(ctx, &buf); err != nil {
		return nil, err
	}
	_ = s.d.Audit.Append(ctx, audit.Entry{Actor: id.User, ActorKind: string(id.Kind), Action: audit.LLMExport,
		Detail: fmt.Sprintf("%d bytes", buf.Len())})
	return api.ExportLlmLog200ApplicationxNdjsonResponse{Body: &buf, ContentLength: int64(buf.Len())}, nil
}

func (s *Server) DeleteLlmLog(ctx context.Context, req api.DeleteLlmLogRequestObject) (api.DeleteLlmLogResponseObject, error) {
	if s.d.LLMLog == nil {
		return nil, unavailable("the LLM log")
	}
	id, err := s.requireRole(ctx, "platform-admin")
	if err != nil {
		return nil, err
	}
	e, err := s.d.LLMLog.Get(ctx, req.Id)
	if err != nil {
		return nil, llmErr(err)
	}
	if err := s.d.LLMLog.Delete(ctx, req.Id); err != nil {
		return nil, llmErr(err)
	}
	what := string(e.Surface)
	if e.Recipe != "" {
		what += " " + e.Recipe
	}
	_ = s.d.Audit.Append(ctx, audit.Entry{Actor: id.User, ActorKind: string(id.Kind), Action: audit.LLMDelete, Target: req.Id,
		Detail: fmt.Sprintf("%s by %s at %s", what, e.User, e.Time.Format(time.RFC3339))})
	return api.DeleteLlmLog204Response{}, nil
}

func (s *Server) RateLlm(ctx context.Context, req api.RateLlmRequestObject) (api.RateLlmResponseObject, error) {
	if s.d.LLMLog == nil {
		return nil, unavailable("the LLM log")
	}
	if _, err := requireHuman(ctx); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, Errorf(http.StatusBadRequest, "rating is required")
	}
	e, err := s.d.LLMLog.Rate(ctx, req.Id, string(req.Body.Rating))
	if err != nil {
		if errors.Is(err, llm.ErrNotFound) {
			return nil, llmErr(err)
		}
		return nil, &HTTPError{Status: http.StatusBadRequest, Msg: err.Error()}
	}
	return api.RateLlm200JSONResponse(llmEntryToAPI(*e)), nil
}

// ---------------------------------------------------------------- search

func (s *Server) Search(ctx context.Context, req api.SearchRequestObject) (api.SearchResponseObject, error) {
	if s.d.Search == nil {
		return nil, unavailable("the search index")
	}
	limit := req.Params.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	hits, err := s.d.Search.Search(ctx, req.Params.Q, limit)
	if err != nil {
		return nil, err
	}
	out := api.SearchResult{Query: req.Params.Q, Mode: api.SearchResultMode(s.d.Search.Mode()), Hits: []api.SearchHit{}}
	for _, h := range hits {
		out.Hits = append(out.Hits, api.SearchHit{Kind: api.SearchHitKind(h.Kind), Ref: h.Ref, Title: h.Title,
			Snippet: h.Snippet, Score: float32(h.Score), Path: h.Path, Change: h.Change})
	}
	return api.Search200JSONResponse(out), nil
}

// ---------------------------------------------------------------- agents

// policyAgents returns the agents of the policy on main.
func (s *Server) policyAgents(ctx context.Context) []policy.Agent {
	if s.d.Policy == nil {
		return nil
	}
	p, err := s.d.Policy.Current(ctx)
	if err != nil || p == nil {
		return nil
	}
	return p.Agents
}

func (s *Server) grantView(g Grant, now time.Time) api.AgentGrant {
	out := grantToAPI(g, now)
	if c, ok := mcp.Recent.Last(g.ID); ok && c.At.After(out.LastSeenAt) {
		out.LastTool, out.LastSeenAt = c.Tool, c.At
	} else if ok && out.LastTool == "" {
		out.LastTool = c.Tool
	}
	return out
}

func (s *Server) ListAgents(ctx context.Context, req api.ListAgentsRequestObject) (api.ListAgentsResponseObject, error) {
	now := time.Now()
	out := api.ListAgents200JSONResponse{}
	seen := map[string]bool{}
	for _, a := range s.policyAgents(ctx) {
		seen[a.ID] = true
		if g, ok := s.grants.Get(a.ID); ok {
			out = append(out, s.grantView(g, now))
			continue
		}
		can := []string{}
		for _, r := range a.Can {
			can = append(can, string(r))
		}
		out = append(out, api.AgentGrant{Id: a.ID, ActsFor: a.ActsFor, Can: can, Mode: api.Navigate, Active: false})
	}
	for _, g := range s.grants.List() {
		if !seen[g.ID] {
			out = append(out, s.grantView(g, now))
		}
	}
	return out, nil
}

// ClaudeAllowCommand allows the platform-kit MCP tools in the project
// directory of the agent; without it Claude Code asks for permission at
// each new tool. Same line as in the README and docs/demo.html.
const ClaudeAllowCommand = `mkdir -p .claude && echo '{"permissions":{"allow":["mcp__platform-kit"]}}' > .claude/settings.local.json`

// MCPSnippets returns the client configurations for an agent token.
func MCPSnippets(mcpURL, token string) []api.McpSnippet {
	header := "Bearer " + token
	claude, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"platform-kit": map[string]any{
		"type": "http", "url": mcpURL, "headers": map[string]string{"Authorization": header}}}}, "", "  ")
	opencode, _ := json.MarshalIndent(map[string]any{"$schema": "https://opencode.ai/config.json", "mcp": map[string]any{
		"platform-kit": map[string]any{"type": "remote", "url": mcpURL, "enabled": true,
			"headers": map[string]string{"Authorization": header}}}}, "", "  ")
	return []api.McpSnippet{
		{Client: "claude-code", File: ".mcp.json", Content: string(claude),
			Command:   fmt.Sprintf(`claude mcp add --transport http platform-kit %s --header "Authorization: Bearer %s"`, mcpURL, token),
			Allow:     ClaudeAllowCommand,
			AllowFlag: "claude --allowedTools mcp__platform-kit"},
		{Client: "opencode", File: "opencode.json", Content: string(opencode)},
	}
}

func (s *Server) ApproveAgent(ctx context.Context, req api.ApproveAgentRequestObject) (api.ApproveAgentResponseObject, error) {
	id, err := requireHuman(ctx)
	if err != nil {
		return nil, err
	}
	actsFor, can := "", []string{}
	known := false
	for _, a := range s.policyAgents(ctx) {
		if a.ID == req.Id {
			known, actsFor = true, a.ActsFor
			for _, r := range a.Can {
				can = append(can, string(r))
			}
		}
	}
	switch {
	case !known && s.d.Policy != nil && len(s.policyAgents(ctx)) > 0:
		return nil, &HTTPError{Status: http.StatusNotFound, Msg: "no agent " + req.Id + " in the policy", Detail: "add it to .pk/policy.yaml (agents) first"}
	case !known:
		// Without a policy an agent <user>-agent acts for its user.
		if req.Id != id.User+"-agent" {
			return nil, &HTTPError{Status: http.StatusNotFound, Msg: "no agent " + req.Id + " in the policy"}
		}
		actsFor = id.User
	}
	if actsFor != id.User {
		return nil, &HTTPError{Status: http.StatusForbidden, Msg: fmt.Sprintf("%s acts for %s; only %s can approve it", req.Id, actsFor, actsFor)}
	}
	hours, mode := 8, ModeNavigate
	if req.Body != nil {
		if req.Body.Hours != 0 {
			hours = req.Body.Hours
		}
		if req.Body.Mode != "" {
			mode = AgentMode(req.Body.Mode)
		}
	}
	if hours < 1 || hours > 12 {
		return nil, Errorf(http.StatusBadRequest, "hours must be 1 to 12")
	}
	if mode != ModeNavigate && mode != ModeSuggest {
		return nil, Errorf(http.StatusBadRequest, "mode must be navigate or suggest")
	}
	token, g, err := s.grants.Approve(req.Id, actsFor, can, mode, time.Duration(hours)*time.Hour, id.User)
	if err != nil {
		return nil, err
	}
	_ = s.d.Audit.Append(ctx, audit.Entry{Actor: id.User, ActorKind: string(id.Kind), Action: audit.AgentApprove, Target: req.Id,
		Detail: fmt.Sprintf("%dh, %s mode, until %s", hours, mode, g.ExpiresAt.Format(time.RFC3339))})
	mcpURL := strings.TrimRight(s.cfg.PublicURL, "/") + "/mcp"
	return api.ApproveAgent200JSONResponse(api.AgentApproval{
		Grant: grantToAPI(g, time.Now()), Token: token, McpUrl: mcpURL, Snippets: MCPSnippets(mcpURL, token),
	}), nil
}

func (s *Server) RevokeAgent(ctx context.Context, req api.RevokeAgentRequestObject) (api.RevokeAgentResponseObject, error) {
	id, err := requireHuman(ctx)
	if err != nil {
		return nil, err
	}
	g, ok := s.grants.Get(req.Id)
	if !ok {
		return nil, &HTTPError{Status: http.StatusNotFound, Msg: "agent " + req.Id + " has no grant"}
	}
	if g.ActsFor != id.User {
		if _, err := s.requireRole(ctx, "owner"); err != nil {
			return nil, &HTTPError{Status: http.StatusForbidden, Msg: fmt.Sprintf("%s acts for %s; only %s or an owner can revoke it", req.Id, g.ActsFor, g.ActsFor)}
		}
	}
	g, _, err = s.grants.Revoke(req.Id)
	if err != nil {
		return nil, err
	}
	_ = s.d.Audit.Append(ctx, audit.Entry{Actor: id.User, ActorKind: string(id.Kind), Action: audit.AgentRevoke, Target: req.Id})
	return api.RevokeAgent200JSONResponse(s.grantView(g, time.Now())), nil
}
