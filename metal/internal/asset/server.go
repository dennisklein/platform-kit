package asset

import (
	"crypto/subtle"
	"io"
	"net"
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

	// One answer for every refusal: absent, expired, revoked, a filename that
	// does not match, credentials that are missing or wrong, and a source
	// address the grant is not bound to. A caller cannot tell from the
	// answer whether a token is live. The answer is a Basic challenge, so a
	// BMC that sends its credentials only when challenged still does.
	g := s.Grants.lookup(token)
	if g == nil || filename != g.Filename {
		refuse(w)
		return
	}
	source := sourceAddress(r)
	if c := g.Credentials; c != nil {
		user, pass, ok := r.BasicAuth()
		if !ok {
			// The challenge round of a BMC that waits for one: no record.
			refuse(w)
			return
		}
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(c.Password)) == 1
		if !userOK || !passOK {
			s.Grants.refuse(g, source, "wrong credentials")
			refuse(w)
			return
		}
	}
	if !s.Grants.admit(g, source) {
		refuse(w)
		return
	}

	// Explicit content type — ServeContent would otherwise ask the host's mime
	// tables what an .iso is.
	w.Header().Set("Content-Type", "application/octet-stream")
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, g.Filename, time.Time{}, io.NewSectionReader(g.content, 0, g.size))
	s.Grants.served(g, cw.n)
}

// refuse writes the one answer the server gives to every refused grant
// request.
func refuse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="pk-metal"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// sourceAddress is the address the request came from, without its port.
// Behind a Service that rewrites the source (SNAT), it is the address of
// the node that forwarded the request.
func sourceAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// countingWriter counts the body bytes a response sends.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}
