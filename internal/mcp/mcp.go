// Package mcp is the MCP endpoint of the bench (Streamable HTTP on /mcp). Its
// tools are generated from the operations with x-mcp-tool in the embedded
// OpenAPI spec. A tool call dispatches in-process through the same HTTP
// handler with the agent identity, so the same rights checks and SSE events
// happen. Each call emits a presence event.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/inproc"
)

// Options of the MCP handler.
type Options struct {
	// Spec is the OpenAPI document (api.Spec).
	Spec []byte
	// API is the bench handler for /api/v1 (behind auth); tool calls go through it.
	API http.Handler
	// BasePath is /api/v1.
	BasePath string
	Events   events.Publisher
	// Version is the server version shown to clients.
	Version string
	Logger  *slog.Logger
	// LabWait is how long pk_lab_status waits for a running lab run to end
	// before it answers (default 25 s; negative: answer at once). Agents
	// then need a few calls per run instead of dozens of polls.
	LabWait time.Duration
}

// Tool is one MCP tool derived from an operation.
type Tool struct {
	Name        string
	Description string
	OperationID string
	Method      string
	Path        string
	// InputSchema is path and query params plus the request body.
	InputSchema map[string]any
}

// Instructions tell the agent how the bench works.
const Instructions = `platform-kit bench: change the desired state of the platform one change at a time.
Flow: pk_change_new (title, intent) → pk_kind_schema / pk_file_read → pk_file_write (validated through the schema; problems come back) → pk_render_impact → pk_commit (pk_commit_message suggests one) → pk_lab_run, then pk_lab_status (it waits while the run is running) → pk_change_push → pk_change_ready.
Your commits carry a Pk-Agent trailer. Agents never approve or merge: people do. The admin sees each tool call live in the web UI.`

const maxResult = 256 << 10

// Activity records the last tool call of each agent.
type Activity struct {
	mu   sync.Mutex
	last map[string]Call
}

// Call is one tool call.
type Call struct {
	Tool string
	At   time.Time
}

// Last returns the last call of an agent.
func (a *Activity) Last(agent string) (Call, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.last[agent]
	return c, ok
}

func (a *Activity) record(agent, tool string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		a.last = map[string]Call{}
	}
	a.last[agent] = Call{Tool: tool, At: time.Now().UTC()}
}

// Recent is the activity of all MCP handlers in this process.
var Recent = &Activity{}

// Handler is the MCP endpoint.
type Handler struct {
	o     Options
	api   *inproc.Client
	tools map[string]toolPlan
	h     http.Handler
	log   *slog.Logger
}

// New builds the handler. The request context must carry the identity of
// an agent (the bench mounts it behind requireAgent).
func New(o Options) (*Handler, error) {
	if o.BasePath == "" {
		o.BasePath = "/api/v1"
	}
	if o.Events == nil {
		o.Events = events.Nop{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.LabWait == 0 {
		o.LabWait = 25 * time.Second
	}
	plans, err := plan(o.Spec)
	if err != nil {
		return nil, err
	}
	ref := &inproc.Ref{}
	ref.Set(o.API)
	h := &Handler{o: o, api: inproc.New(ref, o.BasePath), tools: map[string]toolPlan{}, log: o.Logger.With("component", "mcp")}
	version := o.Version
	if version == "" {
		version = "dev"
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "platform-kit", Title: "platform-kit bench", Version: version},
		&sdk.ServerOptions{Instructions: Instructions})
	for _, p := range plans {
		if p.OperationID == "getLabRun" && o.LabWait > 0 {
			p.Description += fmt.Sprintf("\n\nWhile the run is running, the call waits up to %s for it to end.", o.LabWait)
		}
		h.tools[p.Name] = p
		tp := p
		srv.AddTool(&sdk.Tool{Name: p.Name, Description: p.Description, InputSchema: p.InputSchema,
			Annotations: annotations(p)}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return h.call(ctx, tp, req)
		})
	}
	stream := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: o.Logger.With("component", "mcp-transport")})
	// The bench authenticated the request; hand its identity to the tool
	// handlers as token info.
	verify := func(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
		id, ok := identity.From(r.Context())
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: id.User, Expiration: time.Now().Add(time.Hour), Extra: map[string]any{"identity": id}}, nil
	}
	h.h = auth.RequireBearerToken(verify, nil)(stream)
	return h, nil
}

// Tools lists the tools of the handler.
func (h *Handler) Tools() []Tool {
	out := make([]Tool, 0, len(h.tools))
	for _, t := range h.tools {
		out = append(out, t.Tool)
	}
	return out
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Bearer middleware needs the header; in-process tests may set only the context.
	if r.Header.Get("Authorization") == "" {
		if _, ok := identity.From(r.Context()); ok {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer from-context")
		}
	}
	h.h.ServeHTTP(w, r)
}

func annotations(p toolPlan) *sdk.ToolAnnotations {
	ro := p.Method == http.MethodGet
	a := &sdk.ToolAnnotations{ReadOnlyHint: ro}
	if !ro {
		f := false
		a.DestructiveHint = &f
	}
	return a
}

// call dispatches one tool call through the API as the agent.
func (h *Handler) call(ctx context.Context, tp toolPlan, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	var id identity.Identity
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		id, _ = req.Extra.TokenInfo.Extra["identity"].(identity.Identity)
	}
	if id.IsZero() {
		return errorResult("unauthorized: no agent identity"), nil
	}
	ctx = identity.With(ctx, id)
	ctx, tc := events.WithToolCall(ctx, tp.Name)
	args := map[string]any{}
	if req.Params != nil && len(req.Params.Arguments) > 0 && string(req.Params.Arguments) != "null" {
		dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return errorResult("bad arguments: " + err.Error()), nil
		}
	}
	path, query, body, err := build(tp, args)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	resp, err := h.api.Do(ctx, tp.Method, path, query, body)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	if tp.OperationID == "getLabRun" {
		resp = h.waitLab(ctx, tp, path, query, resp)
	}
	Recent.record(id.User, tp.Name)
	ok := resp.Status/100 == 2
	if ok {
		h.follow(ctx, id, tp, args, resp.Body)
	}
	fail := ""
	if !ok {
		fail = errorText(inproc.ErrorOf(resp))
	}
	if !tc.Sent {
		h.presence(id, tp, args, resp.Body, ok, fail)
	}
	if !ok {
		return errorResult(fail), nil
	}
	text := strings.TrimSpace(string(resp.Body))
	if text == "" {
		text = `{"ok":true}`
	}
	if len(text) > maxResult {
		text = text[:maxResult] + "\n…(cut: the answer is longer than 256 KiB)"
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil
}

// waitLab polls a running lab run until it ends or LabWait passes.
func (h *Handler) waitLab(ctx context.Context, tp toolPlan, path string, query url.Values, resp *inproc.Response) *inproc.Response {
	running := func(r *inproc.Response) bool {
		var run struct {
			Result string `json:"result"`
		}
		return r.Status/100 == 2 && json.Unmarshal(r.Body, &run) == nil && run.Result == string(api.LabResultRunning)
	}
	if h.o.LabWait <= 0 || !running(resp) {
		return resp
	}
	deadline := time.NewTimer(h.o.LabWait)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return resp
		case <-deadline.C:
			return resp
		case <-tick.C:
		}
		next, err := h.api.Do(ctx, tp.Method, path, query, nil)
		if err != nil || next.Status/100 != 2 {
			return resp
		}
		resp = next
		if !running(resp) {
			return resp
		}
	}
}

func errorResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

// errorText is the tool error for an API error, for example
// "denied: agents never approve or merge".
func errorText(e *inproc.Error) string {
	msg := e.Msg
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if len(e.Paths) > 0 {
		msg += ": " + strings.Join(e.Paths, ", ")
	}
	switch e.Status {
	case http.StatusForbidden:
		return "denied: " + msg
	case http.StatusUnauthorized:
		return "unauthorized: " + msg
	case http.StatusNotFound:
		return "not found: " + msg
	case http.StatusConflict:
		return "conflict: " + msg
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid: " + msg
	}
	return fmt.Sprintf("error %d: %s", e.Status, msg)
}

// build makes the request of a tool call from its arguments.
func build(tp toolPlan, args map[string]any) (string, url.Values, any, error) {
	path := tp.Path
	query := url.Values{}
	for _, p := range tp.Params {
		v, ok := args[p.Name]
		if !ok || v == nil {
			if p.Required {
				return "", nil, nil, fmt.Errorf("invalid: %s is required", p.Name)
			}
			continue
		}
		switch p.In {
		case "path":
			s := scalar(v)
			if s == "" {
				return "", nil, nil, fmt.Errorf("invalid: %s is required", p.Name)
			}
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(s))
		case "query":
			if l, ok := v.([]any); ok {
				for _, x := range l {
					query.Add(p.Name, scalar(x))
				}
			} else {
				query.Set(p.Name, scalar(v))
			}
		}
	}
	var body any
	if tp.Body != nil {
		if tp.Body.Flat {
			m := map[string]any{}
			for _, k := range tp.Body.Props {
				if v, ok := args[k]; ok {
					m[k] = v
				}
			}
			if len(m) > 0 || tp.Body.Required {
				body = m
			}
		} else if v, ok := args["body"]; ok {
			body = v
		} else if tp.Body.Required {
			return "", nil, nil, fmt.Errorf("invalid: body is required")
		}
	}
	return path, query, body, nil
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// changeOf returns the change a tool call is about.
func changeOf(tp toolPlan, args map[string]any, result []byte) string {
	for _, k := range []string{"id", "changeId"} {
		if s, ok := args[k].(string); ok && strings.HasPrefix(s, "c-") {
			return s
		}
	}
	if s, ok := args["run"].(string); ok {
		if c, _, ok := strings.Cut(s, "/"); ok && strings.HasPrefix(c, "c-") {
			return c
		}
	}
	if tp.Name == "pk_change_new" || tp.Name == "pk_lab_run" {
		var v struct {
			ID       string `json:"id"`
			ChangeID string `json:"changeId"`
		}
		if json.Unmarshal(result, &v) == nil {
			if strings.HasPrefix(v.ID, "c-") {
				return v.ID
			}
			return v.ChangeID
		}
	}
	return ""
}

func pathOf(tp toolPlan, args map[string]any, result []byte) string {
	if s, ok := args["path"].(string); ok {
		return s
	}
	if tp.Name == "pk_file_new" {
		var v struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(result, &v) == nil {
			return v.Path
		}
	}
	return ""
}

// maxReason caps the error text that a failed call adds to its presence line.
const maxReason = 140

// presence tells the admin's sessions what the agent does. A failed call
// ends in " (failed: <reason>)", so the UI can say why (for example
// "denied: agents never approve or merge").
func (h *Handler) presence(id identity.Identity, tp toolPlan, args map[string]any, result []byte, ok bool, reason string) {
	change := changeOf(tp, args, result)
	path := pathOf(tp, args, result)
	text := tp.Name
	switch {
	case path != "":
		text += " " + path
	case change != "":
		text += " " + change
	}
	if s, ok := args["kind"].(string); ok && path == "" {
		text += " " + s
		if n, ok := args["name"].(string); ok {
			text += "/" + n
		}
	}
	if !ok {
		reason = strings.Join(strings.Fields(reason), " ")
		if r := []rune(reason); len(r) > maxReason {
			reason = string(r[:maxReason-1]) + "…"
		}
		if reason != "" {
			text += " (failed: " + reason + ")"
		} else {
			text += " (failed)"
		}
	}
	h.o.Events.PublishTo(id.Principal(), events.Presence, api.Presence{
		Actor: id.User, Kind: api.IdentityKind(id.Kind), ActsFor: id.ActsFor,
		Tool: tp.Name, ChangeId: change, Path: path, Text: text, At: time.Now().UTC(),
	})
}

// follow moves the admin's focus to where the agent works (navigate mode
// only; in suggest mode the agent calls pk_focus to send a "look here" card).
func (h *Handler) follow(ctx context.Context, id identity.Identity, tp toolPlan, args map[string]any, result []byte) {
	if tp.Name == "pk_focus" || tp.Name == "pk_focus_get" {
		return
	}
	upd, ok := focusFor(tp, args, result)
	if !ok {
		return
	}
	var me api.Me
	if err := h.api.JSON(ctx, http.MethodGet, "/me", nil, nil, &me); err != nil || me.Grant.Mode != api.Navigate {
		return
	}
	if tp.Name == "pk_advise" && strings.Contains(upd.Route, "?advisor=") {
		// The review page tags the answer with the agent that asked.
		upd.Route += "&by=" + url.QueryEscape(agentLabel(id))
	}
	if err := h.api.JSON(ctx, http.MethodPut, "/focus", nil, upd, nil); err != nil {
		h.log.Debug("focus move", "err", err)
	}
}

// agentLabel names an agent as the web UI does ("dennis/agent").
func agentLabel(id identity.Identity) string {
	if id.ActsFor != "" {
		return id.ActsFor + "/agent"
	}
	return id.User
}

// focusFor maps a tool call to a UI route (web/src/routes.ts patterns).
func focusFor(tp toolPlan, args map[string]any, result []byte) (api.FocusUpdate, bool) {
	change := changeOf(tp, args, result)
	path := pathOf(tp, args, result)
	switch tp.Name {
	case "pk_file_read", "pk_file_write", "pk_file_new", "pk_validate", "pk_file_list":
		if change == "" {
			return api.FocusUpdate{}, false
		}
		route := "/changes/" + change + "/files"
		if path != "" {
			route += "?path=" + url.QueryEscape(path)
		}
		return api.FocusUpdate{ChangeId: change, Path: path, View: api.FocusViewEditor, Route: route}, true
	case "pk_render_impact", "pk_diff", "pk_gates", "pk_advise":
		if change == "" {
			return api.FocusUpdate{}, false
		}
		route := "/changes/" + change + "/review"
		if r, _ := args["recipe"].(string); tp.Name == "pk_advise" && r != "" {
			// The review page runs the recipe of ?advisor= once, so the
			// answer shows on the admin's screen too.
			route += "?advisor=" + url.QueryEscape(r)
		}
		return api.FocusUpdate{ChangeId: change, View: api.FocusViewReview, Route: route}, true
	case "pk_lab_run", "pk_lab_status":
		if change == "" {
			return api.FocusUpdate{}, false
		}
		return api.FocusUpdate{ChangeId: change, View: api.FocusViewLab, Route: "/changes/" + change + "/lab"}, true
	case "pk_entity_get":
		kind, _ := args["kind"].(string)
		name, _ := args["name"].(string)
		if kind == "" || name == "" {
			return api.FocusUpdate{}, false
		}
		route := "/assets/" + url.PathEscape(kind) + "/" + url.PathEscape(name)
		if strings.EqualFold(kind, "Rack") {
			route = "/assets/racks/" + url.PathEscape(name)
		} else {
			q := url.Values{}
			for _, k := range []string{"cluster", "path"} {
				if v, _ := args[k].(string); v != "" {
					q.Set(k, v)
				}
			}
			if len(q) > 0 {
				route += "?" + q.Encode()
			}
		}
		return api.FocusUpdate{View: api.FocusViewEntity, Route: route}, true
	case "pk_kb_read":
		if path == "" {
			return api.FocusUpdate{}, false
		}
		return api.FocusUpdate{Path: path, View: api.FocusViewKb, Route: "/kb/article?path=" + url.QueryEscape(path)}, true
	}
	if change != "" && strings.HasPrefix(tp.Name, "pk_change_") || tp.Name == "pk_commit" || tp.Name == "pk_commit_message" {
		if change == "" {
			return api.FocusUpdate{}, false
		}
		return api.FocusUpdate{ChangeId: change, View: api.FocusViewChange, Route: "/changes/" + change}, true
	}
	return api.FocusUpdate{}, false
}
