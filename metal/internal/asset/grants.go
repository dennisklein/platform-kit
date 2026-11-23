package asset

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Credentials are the media credentials a BMC presents while pulling: Supermicro
// sends the InsertMedia UserName/Password action parameters, Huawei the URL
// userinfo — both arrive as HTTP basic auth.
type Credentials struct {
	Username string
	Password string
}

// Grant bounds one artifact fetch. The token is the capability: 128 bits from
// crypto/rand, carried in the URL path because neither vendor accepts query
// strings. Config confidentiality and integrity rest on the grant plus the L2
// island — BMC-verified media TLS is unattainable on either vendor as deployed
// (PoC verdict), so HTTPS gives privacy and the grant gives access control.
type Grant struct {
	Token string
	// Asset is the BootAsset the content came from — the revocation handle.
	Asset string
	// Filename is the URL's last path element. Both vendors require it to end
	// in ".iso".
	Filename string
	// ExpiresAt ends the grant; the Supermicro 1 Hz keep-alive starves a revoked
	// or expired mount naturally.
	ExpiresAt   time.Time
	Credentials *Credentials

	content []byte
}

// Path is the URL path the grant is served under.
func (g *Grant) Path() string {
	return "/grants/" + g.Token + "/" + g.Filename
}

// Grants is the grant table. In-memory by design: grants are short-lived
// capabilities, and a restart forgetting them fails closed — a mount in flight
// re-mints on retry.
type Grants struct {
	mu      sync.Mutex
	byToken map[string]*Grant

	// now is a test seam; nil means time.Now.
	now func() time.Time
}

func NewGrants() *Grants {
	return &Grants{byToken: map[string]*Grant{}}
}

// Issue mints a grant for content. Content is held by the grant for its lifetime —
// config media are tens of kilobytes; when embedded install images (~100 MB,
// per-node) arrive, the content field grows an opener seam instead.
func (t *Grants) Issue(asset, filename string, content []byte, ttl time.Duration, creds *Credentials) (*Grant, error) {
	if !strings.HasSuffix(filename, ".iso") || strings.ContainsAny(filename, "/?#") {
		return nil, fmt.Errorf("filename %q: grant URLs must end in a plain .iso path element (vendor URL rules)", filename)
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("refusing to grant empty content")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("grant ttl must be positive, got %s", ttl)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}
	g := &Grant{
		Token:       hex.EncodeToString(raw),
		Asset:       asset,
		Filename:    filename,
		ExpiresAt:   t.clock().Add(ttl),
		Credentials: creds,
		content:     content,
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	t.byToken[g.Token] = g
	return g, nil
}

// Revoke drops one grant; reports whether it existed.
func (t *Grants) Revoke(token string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.byToken[token]
	delete(t.byToken, token)
	return ok
}

// RevokeAsset drops every grant minted from the named BootAsset (eject, check-in,
// artifact rebuild) and reports how many fell.
func (t *Grants) RevokeAsset(asset string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for token, g := range t.byToken {
		if g.Asset == asset {
			delete(t.byToken, token)
			n++
		}
	}
	return n
}

// lookup resolves a live grant; expired ones are dropped on sight.
func (t *Grants) lookup(token string) *Grant {
	t.mu.Lock()
	defer t.mu.Unlock()
	g, ok := t.byToken[token]
	if !ok {
		return nil
	}
	if !t.clock().Before(g.ExpiresAt) {
		delete(t.byToken, token)
		return nil
	}
	return g
}

// Live finds the unexpired grant minted for asset under filename — how a
// resumed mount tells whether the URL it left on a BMC still answers, or died
// with a restart and must be re-minted.
func (t *Grants) Live(asset, filename string) (*Grant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for token, g := range t.byToken {
		if g.Asset != asset || g.Filename != filename {
			continue
		}
		if !t.clock().Before(g.ExpiresAt) {
			delete(t.byToken, token)
			continue
		}
		return g, true
	}
	return nil, false
}

func (t *Grants) sweepLocked() {
	for token, g := range t.byToken {
		if !t.clock().Before(g.ExpiresAt) {
			delete(t.byToken, token)
		}
	}
}

func (t *Grants) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}
