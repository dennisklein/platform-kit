package asset

import (
	"bytes"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// Server serves BMC media pulls under grants. It is transport-agnostic — the
// caller terminates TLS in front of it (day-0: the CLI's listener; steady: the
// operator's), never the relay. Ranged GETs are the BMC dialect: the Supermicro
// mount probes with HEADs, 512-byte reads, and a 64 KiB header read before
// streaming, then keeps a 1 Hz HEAD alive while inserted.
type Server struct {
	Grants *Grants
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/grants/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	token, filename, ok := strings.Cut(rest, "/")
	if !ok {
		http.NotFound(w, r)
		return
	}

	// One answer for absent, expired, revoked, and mismatched: 403. The token is
	// the capability — nothing else about the table leaks.
	g := s.Grants.lookup(token)
	if g == nil || filename != g.Filename {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if c := g.Credentials; c != nil {
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="pk-metal"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(c.Password)) == 1
		if !userOK || !passOK {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Explicit content type — ServeContent would otherwise ask the host's mime
	// tables what an .iso is.
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, g.Filename, time.Time{}, bytes.NewReader(g.content))
}
