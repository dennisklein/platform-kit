package asset

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
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
// (tested on both), so HTTPS gives privacy and the grant gives access control.
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

	// content is what the grant serves, size bytes of it. It is read at
	// offsets, so concurrent ranged reads of one grant need no lock.
	content io.ReaderAt
	size    int64
	// release frees what content holds (an open file), once, when the
	// grant leaves the table. Nil for content held in memory.
	release func()
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

// Issue mints a grant for content held in memory: the config media, tens of
// kilobytes each. The grant keeps the slice for its lifetime.
func (t *Grants) Issue(asset, filename string, content []byte, ttl time.Duration, creds *Credentials) (*Grant, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("refusing to grant empty content")
	}
	return t.issue(asset, filename, bytes.NewReader(content), int64(len(content)), nil, ttl, creds)
}

// IssueFile mints a grant for an open file: an installer with an embedded
// config is hundreds of megabytes and is served from the media store, not
// from memory. The grant takes the file over and closes it when the grant is
// revoked or expires, also when IssueFile fails. Serving from the descriptor,
// not the path, means the grant serves the bytes that were verified when it
// was minted, even if the store replaces or removes the file meanwhile.
func (t *Grants) IssueFile(asset, filename string, f *os.File, ttl time.Duration, creds *Credentials) (*Grant, error) {
	release := func() { _ = f.Close() }
	st, err := f.Stat()
	if err != nil {
		release()
		return nil, fmt.Errorf("grant %s: %w", filename, err)
	}
	if st.Size() == 0 {
		release()
		return nil, fmt.Errorf("refusing to grant empty content")
	}
	g, err := t.issue(asset, filename, f, st.Size(), release, ttl, creds)
	if err != nil {
		release()
	}
	return g, err
}

func (t *Grants) issue(asset, filename string, content io.ReaderAt, size int64, release func(), ttl time.Duration, creds *Credentials) (*Grant, error) {
	if !strings.HasSuffix(filename, ".iso") || strings.ContainsAny(filename, "/?#") {
		return nil, fmt.Errorf("filename %q: grant URLs must end in a plain .iso path element (vendor URL rules)", filename)
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
		size:        size,
		release:     release,
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
	g, ok := t.byToken[token]
	if ok {
		t.dropLocked(token, g)
	}
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
			t.dropLocked(token, g)
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
		t.dropLocked(token, g)
		return nil
	}
	return g
}

// Live finds the unexpired grant minted for asset under filename — how a
// resumed mount tells whether the URL it left on a BMC still answers, or died
// with a restart and must be re-minted.
//
// When several grants match, the newest one answers (the latest expiry, then
// the greater token), so the answer does not depend on map order: the last
// mount minted is the one a resumed step compares against.
func (t *Grants) Live(asset, filename string) (*Grant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var best *Grant
	for token, g := range t.byToken {
		if g.Asset != asset || g.Filename != filename {
			continue
		}
		if !t.clock().Before(g.ExpiresAt) {
			t.dropLocked(token, g)
			continue
		}
		if best == nil || g.ExpiresAt.After(best.ExpiresAt) ||
			(g.ExpiresAt.Equal(best.ExpiresAt) && g.Token > best.Token) {
			best = g
		}
	}
	return best, best != nil
}

func (t *Grants) sweepLocked() {
	for token, g := range t.byToken {
		if !t.clock().Before(g.ExpiresAt) {
			t.dropLocked(token, g)
		}
	}
}

// dropLocked takes a grant out of the table and frees its content. A request
// still reading it then fails mid-transfer, which is what revoking means.
func (t *Grants) dropLocked(token string, g *Grant) {
	delete(t.byToken, token)
	if g.release != nil {
		g.release()
	}
}

func (t *Grants) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}
