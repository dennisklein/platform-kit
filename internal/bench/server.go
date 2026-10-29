// Package bench is the bench service (local mode): one REST API (OpenAPI,
// strict server), server-sent events, the MCP endpoint and the embedded web
// UI on one port.
//
// Handlers are split per area (handlers_<area>.go). Each area's services are
// interfaces in the domain packages; cmd/pk wires the implementations into
// Deps. A nil dependency makes its handlers answer 503.
package bench

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/audit"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/chat"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/llm"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
	"github.com/dennisklein/platform-kit/internal/rings"
)

// BasePath is the prefix of the REST API.
const BasePath = "/api/v1"

// Deps are the services of the bench. cmd/pk fills them (wire_*.go).
type Deps struct {
	// Events is the event bus; New creates one when nil.
	Events *events.Hub

	// Changes and forge (area A).
	Changes change.Service
	Forge   forge.Forge

	// Kinds, render and policy (area B).
	Kinds    kinds.Registry
	Renderer render.Renderer
	Policy   policy.Engine

	// Labs and Flux (area C).
	Labs lab.Service
	Flux fluxsim.Client
	// Rings moves ring tags (P2); nil answers 503.
	Rings rings.Service

	// LLM (area D).
	LLM      llm.Client
	LLMLog   llm.Log
	Search   llm.Index
	Redactor llm.Redactor
	Advisors advisor.Runner
	Chat     chat.Service
	// MCP builds the /mcp handler. api is the bench API router without
	// authentication: the MCP handler must put the agent identity into each
	// request context (identity.With) before it dispatches a tool call.
	MCP func(api http.Handler) http.Handler

	// Audit is the audit trail; New uses <data>/state/audit.jsonl when nil.
	Audit audit.Log
}

// Server is the bench.
type Server struct {
	cfg      config.Config
	d        Deps
	log      *slog.Logger
	hub      *events.Hub
	focus    *FocusStore
	grants   *GrantStore
	sessions *Sessions
	status   *statusCache

	adminToken string
	hosts      map[string]bool
	// api is the API router (no auth); handler is the full stack.
	api     *http.ServeMux
	handler http.Handler
	spa     fs.FS
}

// Ensure Server implements the generated strict interface.
var _ api.StrictServerInterface = (*Server)(nil)

// New builds the server. spa is the built web UI (web.Dist); nil or an
// empty tree serves a fallback page.
func New(cfg config.Config, d Deps, spa fs.FS) (*Server, error) {
	if d.Events == nil {
		d.Events = events.NewHub()
	}
	stateDir := cfg.StateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if d.Audit == nil {
		d.Audit = audit.NewFileLog(filepath.Join(stateDir, "audit.jsonl"))
	}
	sessions, err := NewSessions(filepath.Join(stateDir, "session.key"))
	if err != nil {
		return nil, err
	}
	grants, err := NewGrantStore(filepath.Join(stateDir, "agents.json"))
	if err != nil {
		return nil, err
	}
	token, err := adminToken(cfg, filepath.Join(stateDir, "admin-token"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		d:          d,
		log:        slog.Default().With("component", "bench"),
		hub:        d.Events,
		focus:      NewFocusStore(d.Events),
		grants:     grants,
		sessions:   sessions,
		status:     &statusCache{},
		adminToken: token,
		hosts:      map[string]bool{},
		spa:        spa,
	}
	for _, h := range cfg.Hosts() {
		s.hosts[normHost(h)] = true
	}
	s.routes()
	return s, nil
}

// Events returns the event bus.
func (s *Server) Events() *events.Hub { return s.hub }

// Grants returns the agent grant store.
func (s *Server) Grants() *GrantStore { return s.grants }

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// AdminToken returns the token that logs a browser in (/?token=) and
// authorizes the CLI as the bench user.
func (s *Server) AdminToken() string { return s.adminToken }

func (s *Server) routes() {
	s.api = http.NewServeMux()
	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  s.requestError,
		ResponseErrorHandlerFunc: s.responseError,
	})
	api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseURL:          BasePath,
		BaseRouter:       s.api,
		ErrorHandlerFunc: s.requestError,
	})
	s.api.HandleFunc("GET "+BasePath+"/events", s.handleEvents)
	s.api.HandleFunc(BasePath+"/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint", r.Method+" "+r.URL.Path)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle(BasePath+"/", s.requireIdentity(limitBody(s.api)))
	var mcp http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "MCP is not available", "")
	})
	if s.d.MCP != nil {
		mcp = s.d.MCP(s.api)
	}
	mux.Handle("/mcp", s.requireAgent(mcp))
	mux.Handle("/mcp/", s.requireAgent(mcp))
	mux.Handle("/", s.spaHandler())

	s.handler = s.logRequests(s.checkHost(s.authenticate(mux)))
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the SSE stream is long-lived.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.log.Info("bench listening", "addr", s.cfg.Addr, "url", s.cfg.PublicURL, "demo", s.cfg.Demo, "user", s.cfg.User)
	// The login link works in demo mode too (a browser that sends no fetch
	// metadata headers).
	s.log.Info("open the bench", "url", fmt.Sprintf("%s/?token=%s", s.cfg.PublicURL, s.adminToken))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelInfo
		}
		s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond), "user", rec.user)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	user   string
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush keeps SSE working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

const maxBody = 8 << 20

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}
