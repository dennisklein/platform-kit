package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
)

// HTTPError is an error with a status code. Handlers return it (or a domain
// error that statusOf maps) instead of writing responses by hand.
type HTTPError struct {
	Status int
	Msg    string
	Detail string
	// Paths are offending paths, for example rebase conflicts.
	Paths []string
}

func (e *HTTPError) Error() string {
	if e.Detail != "" {
		return e.Msg + ": " + e.Detail
	}
	return e.Msg
}

// Errorf returns an HTTPError.
func Errorf(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// Common errors.
var (
	errUnauthorized = &HTTPError{Status: http.StatusUnauthorized, Msg: "login required"}
	// ErrAgentsNever is the answer for agents on approve and merge.
	ErrAgentsNever = &HTTPError{Status: http.StatusForbidden, Msg: "agents never approve or merge"}
	errHumansOnly  = &HTTPError{Status: http.StatusForbidden, Msg: "only people can do this"}
	errNotDemo     = &HTTPError{Status: http.StatusNotFound, Msg: "demo endpoints need PK_DEMO=1"}
)

// notImplemented answers operations that are not built yet.
func notImplemented(op string) error {
	return &HTTPError{Status: http.StatusNotImplemented, Msg: "not implemented", Detail: op}
}

// unavailable answers when a dependency is not wired.
func unavailable(what string) error {
	return &HTTPError{Status: http.StatusServiceUnavailable, Msg: what + " is not available"}
}

// statusOf maps errors to HTTP errors.
func statusOf(err error) *HTTPError {
	var he *HTTPError
	if errors.As(err, &he) {
		return he
	}
	var conflict *gitx.ErrConflict
	switch {
	case errors.As(err, &conflict):
		return &HTTPError{Status: http.StatusConflict, Msg: "rebase conflict, resolve by hand", Paths: conflict.Paths}
	case errors.Is(err, change.ErrAgentDeny):
		return ErrAgentsNever
	case errors.Is(err, change.ErrNotFound), errors.Is(err, forge.ErrNotFound):
		return &HTTPError{Status: http.StatusNotFound, Msg: err.Error()}
	case errors.Is(err, change.ErrForbidden), errors.Is(err, forge.ErrForbidden):
		return &HTTPError{Status: http.StatusForbidden, Msg: err.Error()}
	case errors.Is(err, change.ErrBehind), errors.Is(err, change.ErrConflict), errors.Is(err, change.ErrGates),
		errors.Is(err, change.ErrNoWorktree), errors.Is(err, change.ErrDirty), errors.Is(err, forge.ErrConflict):
		return &HTTPError{Status: http.StatusConflict, Msg: err.Error()}
	case errors.Is(err, forge.ErrUnreachable):
		return &HTTPError{Status: http.StatusBadGateway, Msg: MsgForgeUnreachable, Detail: forgeCause(err)}
	}
	return &HTTPError{Status: http.StatusInternalServerError, Msg: "internal error", Detail: err.Error()}
}

// MsgForgeUnreachable is the answer while Gitea is down; the git or HTTP
// detail goes to the log and the detail field.
const MsgForgeUnreachable = "Gitea is not reachable. Push and merge wait for the forge. Edit, commit and labs still work."

// forgeCause is the short cause of a forge error, without the git command
// line ("Could not resolve host: gitea"); the full error is in the log.
func forgeCause(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, "fatal: "); i >= 0 {
		msg = msg[i+len("fatal: "):]
	} else if i := strings.LastIndex(msg, "unreachable: "); i >= 0 {
		msg = msg[i+len("unreachable: "):]
	}
	if j := strings.LastIndex(msg, "': "); j >= 0 && strings.Contains(msg[:j], "unable to access '") {
		msg = msg[j+3:]
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

func writeError(w http.ResponseWriter, status int, msg, detail string) {
	writeJSON(w, status, api.Error{Error: msg, Detail: detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requestError answers malformed requests (bad params or body).
func (s *Server) requestError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, http.StatusBadRequest, "bad request", err.Error())
}

// responseError answers errors that handlers return.
func (s *Server) responseError(w http.ResponseWriter, r *http.Request, err error) {
	he := statusOf(err)
	if he.Status >= 500 && he.Status != http.StatusNotImplemented && he.Status != http.StatusServiceUnavailable {
		s.log.Error("handler", "path", r.URL.Path, "err", err)
	}
	writeJSON(w, he.Status, api.Error{Error: he.Msg, Detail: he.Detail, Paths: he.Paths})
}
