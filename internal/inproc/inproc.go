// Package inproc calls the bench API in-process: a request goes through the
// same HTTP handler as a browser or CLI request, with the identity of the
// caller in its context, so the same rights checks, masking and events
// apply. MCP tool calls, advisors and chat use it.
package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/dennisklein/platform-kit/internal/identity"
)

// Ref holds a handler that is set later (the bench builds its API router
// after the services are wired).
type Ref struct {
	h atomic.Pointer[http.Handler]
}

// Set sets the handler.
func (r *Ref) Set(h http.Handler) { r.h.Store(&h) }

// Get returns the handler or nil.
func (r *Ref) Get() http.Handler {
	if p := r.h.Load(); p != nil {
		return *p
	}
	return nil
}

// ErrNoHandler is returned before the handler is set.
var ErrNoHandler = errors.New("bench API is not ready")

// Error is a non-2xx answer of the API.
type Error struct {
	Status int
	Msg    string
	Detail string
	Paths  []string
}

func (e *Error) Error() string {
	s := e.Msg
	if s == "" {
		s = http.StatusText(e.Status)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// StatusOf returns the HTTP status of an inproc error, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Client calls the API under a base path (/api/v1).
type Client struct {
	ref  *Ref
	base string
}

// New returns a client over the handler in ref.
func New(ref *Ref, base string) *Client { return &Client{ref: ref, base: strings.TrimRight(base, "/")} }

// Response is a raw answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do sends one request. ctx must carry the identity (identity.With); path
// is relative to the base (for example /changes/c-k7m2qz). body is
// marshaled to JSON unless it is nil, []byte or an io.Reader.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any) (*Response, error) {
	h := c.ref.Get()
	if h == nil {
		return nil, ErrNoHandler
	}
	if _, ok := identity.From(ctx); !ok {
		return nil, &Error{Status: http.StatusUnauthorized, Msg: "login required"}
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case io.Reader:
		rd = b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req := httptest.NewRequestWithContext(ctx, method, u, rd)
	req.Host = "inproc"
	req.RemoteAddr = "127.0.0.1:0"
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return &Response{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}, nil
}

// JSON sends a request and decodes a 2xx answer into out (when not nil).
// Other answers become *Error.
func (c *Client) JSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.Do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if resp.Status/100 != 2 {
		return ErrorOf(resp)
	}
	if out == nil || len(resp.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

// ErrorOf turns a non-2xx response into *Error.
func ErrorOf(resp *Response) *Error {
	e := &Error{Status: resp.Status}
	var body struct {
		Error  string   `json:"error"`
		Detail string   `json:"detail"`
		Paths  []string `json:"paths"`
	}
	if json.Unmarshal(resp.Body, &body) == nil && body.Error != "" {
		e.Msg, e.Detail, e.Paths = body.Error, body.Detail, body.Paths
	} else {
		e.Msg = strings.TrimSpace(string(resp.Body))
		if len(e.Msg) > 300 {
			e.Msg = e.Msg[:300]
		}
	}
	return e
}

// Seg escapes one path segment (ids with a slash, such as lab runs, stay
// one segment).
func Seg(s string) string { return url.PathEscape(s) }
