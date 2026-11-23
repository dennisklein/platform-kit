package bench

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/identity"
)

// Auth model:
//   - Every request: the Host header must be in the allow list
//     (127.0.0.1:<port>, localhost:<port>, PK_ALLOWED_HOSTS) and an Origin
//     header, if present, must name an allowed host. No CORS.
//   - People: session cookie pk_session (HttpOnly, SameSite=Strict, HMAC
//     signed). With PK_DEMO=1 the bench sets it for PK_USER on browser
//     page loads (Sec-Fetch-Mode: navigate, Sec-Fetch-Dest: document);
//     GET /?token=<admin token> always sets it.
//   - CLI as the bench user: Authorization: Bearer <admin token>.
//   - Agents: Authorization: Bearer <token> from approveAgent (8 h).
// The identity reaches handlers through identity.From(ctx).

// CookieName is the session cookie.
const CookieName = "pk_session"

const sessionTTL = 7 * 24 * time.Hour

// Sessions signs and verifies session cookies. The key survives restarts.
type Sessions struct {
	key []byte
	now func() time.Time
}

// NewSessions loads or creates the signing key at path.
func NewSessions(path string) (*Sessions, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(key) < 32) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &Sessions{key: key, now: time.Now}, nil
}

// Issue returns a cookie value for user.
func (s *Sessions) Issue(user string) string {
	exp := strconv.FormatInt(s.now().Add(sessionTTL).Unix(), 10)
	payload := base64.RawURLEncoding.EncodeToString([]byte(user + "|" + exp))
	return payload + "." + s.sign(payload)
}

// Verify returns the user of a cookie value.
func (s *Sessions) Verify(v string) (string, bool) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	user, exp, ok := strings.Cut(string(raw), "|")
	if !ok || user == "" {
		return "", false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || s.now().Unix() > n {
		return "", false
	}
	return user, true
}

func (s *Sessions) sign(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) setSession(w http.ResponseWriter, user string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    s.sessions.Issue(user),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   strings.HasPrefix(s.cfg.PublicURL, "https://"),
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// adminToken returns PK_ADMIN_TOKEN or a generated token stored at path.
func adminToken(cfg config.Config, path string) (string, error) {
	if cfg.AdminToken != "" {
		return cfg.AdminToken, nil
	}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "pk_adm_" + hex.EncodeToString(b)
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

func normHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// checkHost rejects requests with a foreign Host (DNS rebinding) or Origin.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hosts[normHost(r.Host)] {
			writeError(w, http.StatusMisdirectedRequest, "host not allowed", r.Host)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "null" {
			u, err := url.Parse(o)
			if err != nil || !s.hosts[normHost(u.Host)] {
				writeError(w, http.StatusForbidden, "origin not allowed", o)
				return
			}
		} else if o == "null" {
			writeError(w, http.StatusForbidden, "origin not allowed", o)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate puts the identity of the request into the context. It also
// logs people in on page loads (demo mode or ?token=).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.identify(r)
		if !ok && isPageLoad(r) {
			switch {
			case r.URL.Query().Get("token") != "" && s.validAdmin(r.URL.Query().Get("token")):
				s.setSession(w, s.cfg.User)
				q := r.URL.Query()
				q.Del("token")
				u := *r.URL
				u.RawQuery = q.Encode()
				http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
				return
			case s.cfg.Demo && isBrowserNavigation(r):
				s.setSession(w, s.cfg.User)
				id, ok = s.person(r.Context(), s.cfg.User), true
			}
		}
		if ok {
			if rec, isRec := w.(*statusRecorder); isRec {
				rec.user = id.User
			}
			r = r.WithContext(identity.With(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) identify(r *http.Request) (identity.Identity, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, found := strings.CutPrefix(h, "Bearer ")
		if !found {
			return identity.Identity{}, false
		}
		tok = strings.TrimSpace(tok)
		if s.validAdmin(tok) {
			return s.person(r.Context(), s.cfg.User), true
		}
		if g, ok := s.grants.Verify(tok); ok {
			s.grants.Seen(g.ID, "")
			return identity.Identity{User: g.ID, Kind: identity.Agent, ActsFor: g.ActsFor,
				Name: g.ID, Email: g.ID + "@pk.local"}, true
		}
		return identity.Identity{}, false
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if user, ok := s.sessions.Verify(c.Value); ok {
			return s.person(r.Context(), user), true
		}
	}
	return identity.Identity{}, false
}

func (s *Server) validAdmin(tok string) bool {
	return s.adminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1
}

// person returns the identity of a person, with name and email from the policy.
func (s *Server) person(ctx context.Context, user string) identity.Identity {
	id := identity.Identity{User: user, Kind: identity.Human, Email: user + "@pk.local"}
	if s.d.Policy != nil {
		if p, err := s.d.Policy.Current(ctx); err == nil && p != nil {
			for _, pp := range p.People {
				if pp.ID == user {
					id.Name, id.Email = pp.Name, pp.Email
				}
			}
		}
	}
	return id
}

func isPageLoad(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, BasePath+"/") || p == "/mcp" || strings.HasPrefix(p, "/mcp/") || strings.HasPrefix(p, "/assets/") {
		return false
	}
	return true
}

// isBrowserNavigation reports a top-level page load by a browser: the
// fetch metadata headers every current browser sends on navigation. Demo
// mode issues the person's session only for these, so a plain HTTP client
// (an agent's curl) does not become PK_USER by loading "/". A local process
// can still forge the headers: demo mode trusts the laptop; without PK_DEMO
// only GET /?token=<admin token> logs in.
func isBrowserNavigation(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
}

// requireIdentity answers 401 without an identity.
func (s *Server) requireIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := identity.From(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, errUnauthorized.Msg, "use the session cookie or a bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAgent lets only agents (bearer token) through.
func (s *Server) requireAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identity.From(r.Context())
		if !ok || id.Kind != identity.Agent {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pk"`)
			writeError(w, http.StatusUnauthorized, "agent token required", "approve the agent in the bench first")
			return
		}
		next.ServeHTTP(w, r)
	})
}
