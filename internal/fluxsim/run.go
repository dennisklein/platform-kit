package fluxsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/render"
)

// Run serves the fluxsim API and follows the state repo until ctx ends.
func Run(ctx context.Context, o Options) error {
	log := slog.Default().With("component", "fluxsim")
	if o.Interval <= 0 {
		o.Interval = 3 * time.Second
	}
	src, err := waitSource(ctx, o, log)
	if err != nil {
		return err
	}
	reg := all.Registry()
	e := NewEngine(EngineOptions{Source: src, Registry: reg, Renderer: render.New(reg), ApplyDelay: o.ApplyDelay,
		VerifyDelay: o.VerifyDelay, StatePath: filepath.Join(o.DataDir, "fluxsim", "state.json"), Log: log})

	srv := &http.Server{Addr: o.Addr, Handler: Handler(e), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return err
	}
	log.Info("fluxsim listening", "addr", o.Addr, "repo", redactURL(src.URL), "apply", o.ApplyDelay, "verify", o.VerifyDelay)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	go e.Loop(ctx, o.Interval)
	select {
	case <-ctx.Done():
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Loop fetches every interval and steps the state machines twice a second.
func (e *Engine) Loop(ctx context.Context, interval time.Duration) {
	var lastErr string
	sync := func() {
		c, cancel := context.WithTimeout(ctx, 2*interval+10*time.Second)
		defer cancel()
		if err := e.Sync(c); err != nil {
			if err.Error() != lastErr {
				e.log.Warn("fetch state repo", "err", err)
			}
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
	}
	sync()
	fetch := time.NewTicker(interval)
	step := time.NewTicker(500 * time.Millisecond)
	defer fetch.Stop()
	defer step.Stop()
	for {
		e.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-fetch.C:
			sync()
		case <-step.C:
		}
	}
}

// waitSource builds the git source; it waits for the credentials file of
// pk forge-init when the remote is not given.
func waitSource(ctx context.Context, o Options, log *slog.Logger) (*GitSource, error) {
	src := &GitSource{URL: o.RemoteURL, Dir: filepath.Join(o.DataDir, "fluxsim"), User: forge.CIUser}
	if src.URL != "" {
		return src, nil
	}
	creds := func() (string, string, error) {
		cr, err := forge.LoadCredentials(o.CredentialsPath)
		if err != nil {
			return "", "", err
		}
		base, owner, repo := o.ForgeURL, o.Owner, o.Repo
		if !o.ForgeURLSet && cr.URL != "" {
			base = cr.URL
		}
		if cr.Owner != "" {
			owner, repo = cr.Owner, cr.Repo
		}
		tok, _ := cr.Token(forge.CIUser)
		return strings.TrimRight(base, "/") + "/" + owner + "/" + repo + ".git", tok, nil
	}
	logged := false
	for {
		url, tok, err := creds()
		if err == nil {
			src.URL, src.Token, src.Creds = url, tok, creds
			return src, nil
		}
		if !logged {
			log.Info("waiting for forge credentials", "path", o.CredentialsPath)
			logged = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// Handler serves GET /status, GET /clusters/{id}/objects and
// POST /clusters/{id}/fail ({"fail": true|false}, default true).
func Handler(e *Engine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("GET /clusters/{id}/objects", func(w http.ResponseWriter, r *http.Request) {
		objs, err := e.Objects(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, objs)
	})
	mux.HandleFunc("POST /clusters/{id}/fail", func(w http.ResponseWriter, r *http.Request) {
		body := struct {
			Fail *bool `json:"fail"`
		}{}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if len(bytes.TrimSpace(b)) > 0 {
			if err := json.Unmarshal(b, &body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		fail := body.Fail == nil || *body.Fail
		st, err := e.SetFailing(r.PathValue("id"), fail)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// NewClient returns a Client for the fluxsim at baseURL.
func NewClient(baseURL string) Client {
	return &client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{Timeout: 3 * time.Second}}
}

type client struct {
	base string
	hc   *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fluxsim unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, e.Error)
		}
		return fmt.Errorf("fluxsim %s %s: %d %s", method, path, resp.StatusCode, e.Error)
	}
	return json.Unmarshal(b, out)
}

// ErrNotFound is returned for unknown clusters.
var ErrNotFound = errors.New("fluxsim: not found")

func (c *client) Status(ctx context.Context) (*Status, error) {
	var st Status
	if err := c.do(ctx, http.MethodGet, "/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *client) Objects(ctx context.Context, cluster string) ([]Object, error) {
	var objs []Object
	if err := c.do(ctx, http.MethodGet, "/clusters/"+url.PathEscape(cluster)+"/objects", nil, &objs); err != nil {
		return nil, err
	}
	return objs, nil
}

func (c *client) SetFailing(ctx context.Context, cluster string, failing bool) (*ClusterStatus, error) {
	var st ClusterStatus
	if err := c.do(ctx, http.MethodPost, "/clusters/"+url.PathEscape(cluster)+"/fail", map[string]bool{"fail": failing}, &st); err != nil {
		return nil, err
	}
	return &st, nil
}
