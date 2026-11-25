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
// (tested on both), so HTTPS gives privacy against a passive observer only,
// and the grant gives access control. A grant serves the one source address
// that first fetched it, and its fetches and refusals are recorded (Record).
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

	// source is the address the first admitted request came from: the
	// grant serves that address only. requests, bytes and refused count
	// what it served and refused, for the record written when it ends. All
	// four are guarded by the table's lock.
	source   string
	requests int64
	bytes    int64
	refused  int

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

	// Record, when set, is told what happens to a grant: its first fetch,
	// each refusal (up to maxRefusalRecords for one grant), and its end. It
	// is how a media fetch reaches the audit trail. A record never holds the
	// token or the credentials. Set it before the table serves; it must
	// not call back into the table.
	Record func(GrantRecord)

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
	recs := t.sweepLocked()
	t.byToken[g.Token] = g
	t.mu.Unlock()
	t.emit(recs...)
	return g, nil
}

// GrantEvent is what a GrantRecord reports.
type GrantEvent string

const (
	// GrantFetched: the first request the grant admitted. The grant is
	// bound to its source address from now on.
	GrantFetched GrantEvent = "fetched"
	// GrantRefused: a request named the grant and was refused (wrong
	// credentials, or another source address than the bound one).
	GrantRefused GrantEvent = "refused"
	// GrantEnded: the grant was revoked or expired.
	GrantEnded GrantEvent = "ended"
)

// maxRefusalRecords bounds the refusal records of one grant: a client that
// retries without end must not flood the trail. Every refusal still counts
// in the record of the grant's end.
const maxRefusalRecords = 10

// GrantRecord is what happened to one grant. It names the grant by its
// handle and filename, never by its token.
type GrantRecord struct {
	Event GrantEvent
	// Handle is the grant's Asset: the revocation handle, which names the
	// operation that minted it.
	Handle   string
	Filename string
	// Source is the address of the request (fetched, refused), or the
	// address the grant was bound to (ended; empty when never fetched).
	Source string
	// Reason says why a request was refused, or how the grant ended
	// (revoked, expired).
	Reason string
	// Requests and Bytes count what the grant served; Refused counts the
	// requests it refused. Set on ended.
	Requests int64
	Bytes    int64
	Refused  int
}

func (g *Grant) record(ev GrantEvent, source, reason string) GrantRecord {
	return GrantRecord{Event: ev, Handle: g.Asset, Filename: g.Filename, Source: source, Reason: reason,
		Requests: g.requests, Bytes: g.bytes, Refused: g.refused}
}

func (t *Grants) emit(recs ...GrantRecord) {
	if t.Record == nil {
		return
	}
	for _, r := range recs {
		t.Record(r)
	}
}

// admit decides whether the grant serves a request from source, which
// passed the credential check. The first admitted request binds the grant
// to its source address; a request from any other address is refused. A
// grant is made for one BMC, which fetches from one address. A copy of the
// URL and its credentials used from elsewhere is refused while the BMC
// still fetches, and the refusal is recorded.
func (t *Grants) admit(g *Grant, source string) bool {
	t.mu.Lock()
	var rec *GrantRecord
	ok := true
	switch {
	case g.source == "":
		g.source = source
		r := g.record(GrantFetched, source, "")
		rec = &r
	case g.source != source:
		ok = false
		rec = t.refusedLocked(g, source, "the grant is bound to another source address, "+g.source)
	}
	t.mu.Unlock()
	if rec != nil {
		t.emit(*rec)
	}
	return ok
}

// refuse records a refused request of a grant.
func (t *Grants) refuse(g *Grant, source, reason string) {
	t.mu.Lock()
	rec := t.refusedLocked(g, source, reason)
	t.mu.Unlock()
	if rec != nil {
		t.emit(*rec)
	}
}

func (t *Grants) refusedLocked(g *Grant, source, reason string) *GrantRecord {
	g.refused++
	if g.refused > maxRefusalRecords {
		return nil
	}
	r := g.record(GrantRefused, source, reason)
	return &r
}

// served counts one admitted request and the bytes it sent.
func (t *Grants) served(g *Grant, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	g.requests++
	g.bytes += n
}

// Revoke drops one grant; reports whether it existed.
func (t *Grants) Revoke(token string) bool {
	t.mu.Lock()
	g, ok := t.byToken[token]
	var recs []GrantRecord
	if ok {
		recs = append(recs, t.dropLocked(token, g, "revoked"))
	}
	t.mu.Unlock()
	t.emit(recs...)
	return ok
}

// RevokeAsset drops every grant minted from the named BootAsset (eject, check-in,
// artifact rebuild) and reports how many fell.
func (t *Grants) RevokeAsset(asset string) int {
	t.mu.Lock()
	var recs []GrantRecord
	for token, g := range t.byToken {
		if g.Asset == asset {
			recs = append(recs, t.dropLocked(token, g, "revoked"))
		}
	}
	t.mu.Unlock()
	t.emit(recs...)
	return len(recs)
}

// lookup resolves a live grant; expired ones are dropped on sight.
func (t *Grants) lookup(token string) *Grant {
	t.mu.Lock()
	g, ok := t.byToken[token]
	if !ok {
		t.mu.Unlock()
		return nil
	}
	if !t.clock().Before(g.ExpiresAt) {
		rec := t.dropLocked(token, g, "expired")
		t.mu.Unlock()
		t.emit(rec)
		return nil
	}
	t.mu.Unlock()
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
	var recs []GrantRecord
	defer func() { t.emit(recs...) }()
	defer t.mu.Unlock()
	var best *Grant
	for token, g := range t.byToken {
		if g.Asset != asset || g.Filename != filename {
			continue
		}
		if !t.clock().Before(g.ExpiresAt) {
			recs = append(recs, t.dropLocked(token, g, "expired"))
			continue
		}
		if best == nil || g.ExpiresAt.After(best.ExpiresAt) ||
			(g.ExpiresAt.Equal(best.ExpiresAt) && g.Token > best.Token) {
			best = g
		}
	}
	return best, best != nil
}

func (t *Grants) sweepLocked() []GrantRecord {
	var recs []GrantRecord
	for token, g := range t.byToken {
		if !t.clock().Before(g.ExpiresAt) {
			recs = append(recs, t.dropLocked(token, g, "expired"))
		}
	}
	return recs
}

// dropLocked takes a grant out of the table and frees its content. A request
// still reading it then fails mid-transfer, which is what revoking means.
// It returns the record of the grant's end, for the caller to emit once the
// lock is released.
func (t *Grants) dropLocked(token string, g *Grant, how string) GrantRecord {
	delete(t.byToken, token)
	if g.release != nil {
		g.release()
	}
	return g.record(GrantEnded, g.source, how)
}

func (t *Grants) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}
