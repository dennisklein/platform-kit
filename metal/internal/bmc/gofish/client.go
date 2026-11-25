// Package gofish is the production bmc.Client: stmcginnis/gofish for the typed Redfish
// surface, raw HTTP through the same authenticated client where gofish keeps a link
// private or a vendor leaves the schema (the Huawei OEM #VirtualMedia.VmmControl,
// Supermicro's Oem board serial). Dialects are discovered per endpoint and dispatched
// as data — never assumed from the vendor string.
//
// The standard shape is always a candidate, and where an endpoint offers both a
// standard and a vendor route for the same thing, the vendor route wins: it is the
// one proven on that hardware. An endpoint with no vendor routes — a BMC that just
// follows the specification — is served by the standard ones alone.
//
// The files split by resource: this one holds the connection, the shared request
// helpers and the inventory reads; media.go, firmware.go, bios.go, accounts.go,
// identity.go and ntp.go hold one concern each.
package gofish

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gf "github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
)

type Config struct {
	Endpoint string // https://[2001:db8:0:100::14]:443 or http://127.0.0.1:8000 (sim)
	Username string
	Password string
	Insecure bool // skip TLS verification (lab BMCs with self-signed certs)
	// RootCAs pins the server-certificate trust anchors (the site's internal CA
	// on real BMCs). Wins over PinnedKeys and Insecure when set.
	RootCAs *x509.CertPool
	// PinnedKeys are the BMC keys the session accepts, in the form of
	// PublicKeyPin, when no CA checks the certificate. A BMC that presents
	// another key fails the handshake, before any credential is sent. Wins
	// over Insecure when set.
	PinnedKeys []string
	// ReadOnly refuses every mutating call at this layer — the read-only rail: pointing the
	// client at real hardware is safe by construction until explicitly unlocked.
	ReadOnly bool
	// RequestTimeout bounds one request, from the send to the last byte of
	// the answer. Zero is DefaultRequestTimeout. A request that uploads a
	// large body (a firmware image) gets UploadTimeout instead.
	RequestTimeout time.Duration
}

// DefaultRequestTimeout bounds one Redfish request. A BMC that completes
// the TLS handshake and then stops answering (a wedged web server, a
// manager in the middle of a reboot) must not hold the caller forever.
const DefaultRequestTimeout = 30 * time.Second

// UploadTimeout bounds one request whose body is larger than bulkBody: a
// firmware push over a slow management link takes minutes.
const UploadTimeout = 15 * time.Minute

// bulkBody is the request body size from which UploadTimeout applies.
const bulkBody = 1 << 20

type client struct {
	api      *gf.APIClient
	svc      *gf.Service
	readOnly bool
	// auth is how the requests authenticate: AuthSession or AuthBasic.
	auth string

	// Where the manager, the chassis and the services live does not change
	// while a session lasts, so each is looked up once per session. The
	// resources themselves are read fresh on each call.
	memo struct {
		sync.Mutex
		root       *rootLinks
		managerURI string
		managerDoc []byte // the manager as first read: links only are taken from it
		chassisURI string
		update     *updateService
		mediaColl  *string
	}
}

// rootLinks are the service root's links this package follows.
type rootLinks struct {
	Chassis            ref `json:"Chassis"`
	Managers           ref `json:"Managers"`
	Systems            ref `json:"Systems"`
	CertificateService ref `json:"CertificateService"`
}

// root reads the service root's links, once per session.
func (c *client) root() (rootLinks, error) {
	c.memo.Lock()
	r := c.memo.root
	c.memo.Unlock()
	if r != nil {
		return *r, nil
	}
	r = &rootLinks{}
	if _, err := c.getJSON(c.svc.ODataID, r); err != nil {
		return rootLinks{}, fmt.Errorf("bmc: service root: %w", err)
	}
	c.memo.Lock()
	c.memo.root = r
	c.memo.Unlock()
	return *r, nil
}

var errReadOnly = fmt.Errorf("bmc: mutating call refused (read-only client)")

// Connect dials the endpoint and returns a bmc.Client.
//
// The client logs in with a Redfish session where the service offers one:
// the password crosses the wire once, in the login, and every request after
// it carries the session token (X-Auth-Token) instead. Close deletes the
// session. A service that links no Sessions collection, or that refuses the
// login for another reason than the credentials, gets HTTP Basic on every
// request instead; Authentication reports which one the client uses, so that
// the caller can say so. A login that the service refuses for the
// credentials (401) is returned as such: Basic would only send the same
// wrong password again.
//
// gofish binds ctx to every request of the session, so cancelling ctx ends
// the request in flight. Each request is also bounded on its own (see
// Config.RequestTimeout). The session keeps its connections open between
// requests: a TLS handshake with a BMC costs up to a second, and the small
// connection tables of some BMCs fill up when every request opens a new one.
func Connect(ctx context.Context, cfg Config) (bmc.Client, error) {
	// The transport is built here, not left to gofish, so that every
	// response passes the guard below, gofish's own reads included. It is
	// cloned from the default transport, as gofish does for its own client,
	// so a proxy and the dial and handshake timeouts are kept. Each session
	// gets its own clone: gofish's own default shares the default transport's
	// TLS config by pointer and sets InsecureSkipVerify on it, which would
	// turn off certificate checks process-wide and race between sessions.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	tlsConfig, err := clientTLS(cfg)
	if err != nil {
		return nil, err
	}
	t.TLSClientConfig = tlsConfig
	timeout := cmp.Or(cfg.RequestTimeout, DefaultRequestTimeout)
	// The header timeout starts after the request body is sent, so it also
	// holds for an upload.
	t.ResponseHeaderTimeout = timeout
	hc := &http.Client{Transport: guard{next: t, timeout: timeout}}
	fail := func(err error) (bmc.Client, error) {
		t.CloseIdleConnections()
		return nil, fmt.Errorf("bmc: connect %s: %w", cfg.Endpoint, err)
	}

	// The service root is public: it is read before any credential is sent,
	// to learn whether the service offers sessions.
	root, err := readServiceRoot(ctx, hc, cfg.Endpoint)
	if err != nil {
		return fail(err)
	}
	gcfg := gf.ClientConfig{
		Endpoint:         cfg.Endpoint,
		Username:         cfg.Username,
		Password:         cfg.Password,
		HTTPClient:       hc,
		ReuseConnections: true,
	}
	auth := AuthNone
	var api *gf.APIClient
	if cfg.Username != "" && root.Links.Sessions.ODataID != "" {
		api, err = gf.ConnectContext(ctx, gcfg)
		if err == nil {
			if s, serr := api.GetSession(); serr != nil || s.Token == "" {
				// A login that answers without a token leaves gofish with
				// no credential at all: Basic takes over.
				api.Logout()
				api = nil
			} else {
				auth = AuthSession
			}
		} else if LoginRefused(err) || statusOf(err) == 0 {
			// Refused credentials, or no answer at all: Basic would fare
			// no better.
			return fail(err)
		}
	}
	if api == nil {
		gcfg.BasicAuth = cfg.Username != ""
		if gcfg.BasicAuth {
			auth = AuthBasic
		}
		if api, err = gf.ConnectContext(ctx, gcfg); err != nil {
			return fail(err)
		}
	}
	c := &client{api: api, svc: api.GetService(), readOnly: cfg.ReadOnly, auth: auth}
	c.memo.root = &root.rootLinks
	return c, nil
}

// clientTLS is the TLS policy of a session: the pinned CAs, else the pinned
// keys, else no check at all where the caller asked for that, else the
// system's CAs (nil).
func clientTLS(cfg Config) (*tls.Config, error) {
	switch {
	case cfg.RootCAs != nil:
		return &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}, nil
	case len(cfg.PinnedKeys) > 0:
		pins := slices.Clone(cfg.PinnedKeys)
		return &tls.Config{
			// The chain is not checked: a pinned BMC certificate is usually
			// self-signed. VerifyConnection checks the key instead, and runs
			// on every handshake, a resumed one included.
			InsecureSkipVerify: true, //nolint:gosec // the key is checked below
			MinVersion:         tls.VersionTLS12,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("bmc: the BMC presents no certificate")
				}
				if got := PublicKeyPin(cs.PeerCertificates[0]); !slices.Contains(pins, got) {
					return &KeyMismatchError{Presented: got}
				}
				return nil
			},
		}, nil
	case cfg.Insecure:
		// Lab BMCs with self-signed certs, asked for by the caller.
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, nil //nolint:gosec
	}
	return nil, nil
}

// PublicKeyPin is how a pin names a certificate's key: "sha256:" and the hex
// SHA-256 of its SubjectPublicKeyInfo. It is what
//
//	openssl x509 -pubkey -noout | openssl pkey -pubin -outform der | sha256sum
//
// prints for the certificate.
func PublicKeyPin(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// KeyMismatchError is a BMC that presents a key the session does not pin.
type KeyMismatchError struct {
	// Presented is the key the BMC presented, as PublicKeyPin names it.
	Presented string
}

func (e *KeyMismatchError) Error() string {
	return "bmc: the BMC presents key " + e.Presented + ", which is not pinned"
}

// Authentication values: how a client authenticates its requests.
const (
	// AuthSession: a Redfish session; each request carries its token.
	AuthSession = "Session"
	// AuthBasic: HTTP Basic; each request carries the password.
	AuthBasic = "Basic"
	// AuthNone: no credential was given.
	AuthNone = ""
)

// Authentication reports how a client from Connect authenticates its
// requests: AuthSession or AuthBasic. It is AuthNone for a client that
// Connect did not make, or that got no credential.
func Authentication(cl bmc.Client) string {
	if c, ok := cl.(*client); ok {
		return c.auth
	}
	return AuthNone
}

// serviceRoot is the part of the service root Connect reads before it logs
// in.
type serviceRoot struct {
	rootLinks
	Links struct {
		Sessions ref `json:"Sessions"`
	} `json:"Links"`
}

// readServiceRoot reads the public service root, without credentials.
func readServiceRoot(ctx context.Context, hc *http.Client, endpoint string) (serviceRoot, error) {
	var root serviceRoot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/redfish/v1/", nil)
	if err != nil {
		return root, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return root, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return root, err
	}
	if resp.StatusCode/100 != 2 {
		return root, fmt.Errorf("service root: %s", resp.Status)
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return root, fmt.Errorf("service root: %w", err)
	}
	return root, nil
}

// Close logs out and closes the connections the session kept open. Each
// session has a transport of its own, so nothing else would close them.
func (c *client) Close() error {
	if c.auth == AuthSession {
		c.api.Logout()
	}
	c.api.HTTPClient.CloseIdleConnections()
	return nil
}

// ref is a Redfish link: {"@odata.id": "..."}.
type ref struct {
	ODataID string `json:"@odata.id"`
}

// getRaw reads a resource and reports its ETag — the response header where
// the service sends one, the @odata.etag property otherwise.
func (c *client) getRaw(uri string) (body []byte, etag string, err error) {
	resp, err := c.api.Get(uri)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if body, err = readBody(resp.Body); err != nil {
		return nil, "", fmt.Errorf("%s: %w", uri, err)
	}
	if etag = resp.Header.Get("ETag"); etag == "" {
		var tagged struct {
			ETag string `json:"@odata.etag"`
		}
		_ = json.Unmarshal(body, &tagged)
		etag = tagged.ETag
	}
	return body, stripGzipSuffix(etag), nil
}

// maxBodyBytes caps a response body read here. The largest resource read
// raw is a few hundred KiB; a BMC that sends more is broken, and must not
// make the operator hold an unbounded body in memory.
const maxBodyBytes = 32 << 20

// readBody reads a response body of at most maxBodyBytes.
func readBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("response body over %d MiB", maxBodyBytes>>20)
	}
	return body, nil
}

// guard checks every response before gofish or this package decodes it.
// The body is read in full, up to maxBodyBytes, so no read holds an unbounded
// body. A successful GET must answer with a JSON object, as every Redfish
// resource is one: gofish decodes a JSON null into a nil collection and then
// dereferences it in a goroutine of its own, which no recover in the caller
// can catch, so such an answer must never reach it.
//
// The guard also bounds each request, the body read included: gofish sets no
// deadline of its own.
type guard struct {
	next    http.RoundTripper
	timeout time.Duration
}

func (g guard) RoundTrip(req *http.Request) (*http.Response, error) {
	timeout := g.timeout
	if req.ContentLength > bulkBody {
		timeout = max(timeout, UploadTimeout)
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()
	resp, err := g.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		return resp, g.explain(ctx, req, timeout, err)
	}
	if resp.Body == nil {
		return resp, nil
	}
	body, err := readBody(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, g.explain(ctx, req, timeout, err))
	}
	if req.Method == http.MethodGet && resp.StatusCode/100 == 2 &&
		resp.StatusCode != http.StatusAccepted && !jsonObjectOrEmpty(body) {
		return nil, fmt.Errorf("GET %s: answer is not a JSON object", req.URL.Path)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// explain names the request timeout when it, and not the caller, ended the
// request.
func (guard) explain(ctx context.Context, req *http.Request, timeout time.Duration, err error) error {
	var ne net.Error
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	if timedOut && req.Context().Err() == nil {
		return fmt.Errorf("no answer within %s: %w", timeout, err)
	}
	return err
}

// CloseIdleConnections closes the kept-open connections of the transport
// underneath, so that http.Client.CloseIdleConnections reaches them.
func (g guard) CloseIdleConnections() {
	if c, ok := g.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// jsonObjectOrEmpty reports whether body is empty or one valid JSON object.
func jsonObjectOrEmpty(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) == 0 || (trimmed[0] == '{' && json.Valid(trimmed))
}

// stripGzipSuffix removes the "-gzip" that Apache mod_deflate appends inside
// the ETag of a compressed response. The backend compares If-Match against
// the plain tag, so echoing the suffixed one is a 412 on every attempt. This
// is the trim gofish applies to typed entities (sanitizeETag).
func stripGzipSuffix(etag string) string {
	if strings.HasSuffix(etag, `-gzip"`) {
		return strings.TrimSuffix(etag, `-gzip"`) + `"`
	}
	return etag
}

// getJSON reads a resource into v (nil to read only the ETag).
func (c *client) getJSON(uri string, v any) (etag string, err error) {
	body, etag, err := c.getRaw(uri)
	if err != nil {
		return "", err
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			return "", fmt.Errorf("%s: %w", uri, err)
		}
	}
	return etag, nil
}

// patch updates a resource the way the specification asks for it: with the
// resource's current ETag as If-Match wherever the resource has one. Some
// services refuse a PATCH without it (428). A tag that went stale between the
// read and the write (412) is re-read once.
func (c *client) patch(uri string, body any) error {
	for attempt := 0; ; attempt++ {
		etag, err := c.getJSON(uri, nil)
		if err != nil {
			return err
		}
		var headers map[string]string
		if etag != "" {
			headers = map[string]string{"If-Match": etag}
		}
		resp, err := c.api.PatchWithHeaders(uri, body, headers)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		if attempt > 0 || statusOf(err) != http.StatusPreconditionFailed {
			return err
		}
	}
}

// statusOf is the HTTP status behind a gofish error; 0 when the request never
// got an answer.
func statusOf(err error) int {
	var re *schemas.Error
	if errors.As(err, &re) {
		return re.HTTPReturnedStatusCode
	}
	return 0
}

// LoginRefused reports whether err is a manager refusing the credentials of
// a request (401). Every refused login counts towards the manager's lockout,
// so a caller backs off a credential that got this answer.
func LoginRefused(err error) bool {
	return statusOf(err) == http.StatusUnauthorized
}

// members lists a collection's member URIs in the order the service serves
// them. gofish's own collection walks fetch concurrently and hand members back
// in completion order, so wherever order carries meaning the walk happens here.
func (c *client) members(uri string) ([]string, error) {
	var doc struct {
		Members []ref `json:"Members"`
	}
	if _, err := c.getJSON(uri, &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Members))
	for _, m := range doc.Members {
		out = append(out, m.ODataID)
	}
	return out, nil
}

// errTaskLost is a task monitor that stopped answering: gone, refused or
// unreachable. For most operations that is a failure; for one that reboots
// the manager it is the expected end.
var errTaskLost = errors.New("bmc: task monitor lost")

// pollTask follows a task monitor to the end of the operation.
func (c *client) pollTask(ctx context.Context, uri string) error {
	return poll.Until(ctx, taskBackoff, func(context.Context) (bool, error) {
		resp, err := c.api.Get(uri)
		if err != nil {
			return false, fmt.Errorf("%w: %s: %v", errTaskLost, uri, err)
		}
		body, err := readBody(resp.Body)
		resp.Body.Close()
		if err != nil {
			return false, fmt.Errorf("bmc: task %s: %w", uri, err)
		}
		task, err := decodeTask(body)
		if err != nil {
			if resp.StatusCode == http.StatusAccepted {
				// Still running; a monitor that answers 202 has not ended.
				return false, nil
			}
			return false, fmt.Errorf("bmc: task %s: %w", uri, err)
		}
		switch task.TaskState {
		case "Completed":
			return true, nil
		case "Exception", "Killed", "Cancelled":
			var why []string
			for _, m := range task.Messages {
				why = append(why, m.Message)
			}
			return false, fmt.Errorf("bmc: task %s: %s %s", uri, task.TaskState, strings.Join(why, "; "))
		case "":
			// A task monitor answers 202 while the operation runs and with the
			// operation's own response once it is done — which is no Task.
			return resp.StatusCode != http.StatusAccepted, nil
		}
		return false, nil
	})
}

// taskAnswer is what pollTask reads from a task monitor.
type taskAnswer struct {
	TaskState string
	Messages  []struct{ Message string }
}

// decodeTask reads a task monitor's answer. An empty body or a JSON object
// with no TaskState is the operation's own response. Anything else that
// cannot be read for sure is an error, never a finished task: a cut-off
// {"TaskState":"Exception" must not read as success. encoding/json matches
// keys without case, so every spelling of TaskState is checked, and two that
// disagree are refused.
func decodeTask(body []byte) (taskAnswer, error) {
	var task taskAnswer
	if len(bytes.TrimSpace(body)) == 0 {
		return task, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return task, fmt.Errorf("unreadable task monitor answer: %w", err)
	}
	state, seen := "", false
	for k, v := range doc {
		if !strings.EqualFold(k, "TaskState") {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return task, fmt.Errorf("TaskState is not a string: %s", v)
		}
		if seen && s != state {
			return task, fmt.Errorf("TaskState given twice: %q and %q", state, s)
		}
		if s == "" {
			return task, errors.New("TaskState is empty")
		}
		state, seen = s, true
	}
	// Messages are only the reason text; a body whose Messages do not decode
	// still has a readable TaskState.
	_ = json.Unmarshal(body, &task)
	task.TaskState = state
	return task, nil
}

// taskBackoff paces task and CSR polling. It starts short and backs off: a
// task on a real manager runs for minutes, one on the sim for milliseconds.
var taskBackoff = poll.Backoff{Initial: 50 * time.Millisecond, Max: 2 * time.Second, Factor: 2}

// pollAccepted follows the task behind a raw response, if the response was a
// 202. The monitor is the Location header, as the specification asks; a
// service that leaves the header out names the Task in the body instead.
// Some firmwares answer the same action synchronously.
func (c *client) pollAccepted(ctx context.Context, resp *http.Response) error {
	if resp.StatusCode != http.StatusAccepted {
		return nil
	}
	if uri := resp.Header.Get("Location"); uri != "" {
		return c.pollTask(ctx, uri)
	}
	body, err := readBody(resp.Body)
	if err != nil {
		return fmt.Errorf("bmc: accepted answer: %w", err)
	}
	if uri := taskInBody(body); uri != "" {
		return c.pollTask(ctx, uri)
	}
	return nil
}

// taskInBody is the URI of the Task a 202 carries in its body: its
// TaskMonitor, else its own @odata.id. Empty when the body is no Task.
func taskInBody(body []byte) string {
	var doc struct {
		ODataID     string `json:"@odata.id"`
		ODataType   string `json:"@odata.type"`
		TaskMonitor string `json:"TaskMonitor"`
		TaskState   string `json:"TaskState"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	if !strings.HasPrefix(doc.ODataType, "#Task.") && doc.TaskState == "" {
		return ""
	}
	return cmp.Or(doc.TaskMonitor, doc.ODataID)
}

// followTask polls the monitor of an asynchronous answer, if the answer was
// one: the Location header, else the Task the body carried.
func (c *client) followTask(ctx context.Context, info *schemas.TaskMonitorInfo) error {
	if info == nil {
		return nil
	}
	uri := info.TaskMonitor
	if uri == "" && info.Task != nil {
		uri = cmp.Or(info.Task.TaskMonitor, info.Task.ODataID)
	}
	if uri == "" {
		return nil
	}
	return c.pollTask(ctx, uri)
}

// firstReadable walks the candidates and then the members of a collection,
// in the order the service lists them, and returns the first one that read
// accepts. A member that fails to read is skipped, so one broken member does
// not hide a good one; the first failure is the error when none reads.
func (c *client) firstReadable(what, coll string, prefer []string, read func(uri string) error) (string, error) {
	uris := slices.Clone(prefer)
	if coll != "" {
		members, err := c.members(coll)
		if err != nil && len(uris) == 0 {
			return "", fmt.Errorf("bmc: %s: %w", what, err)
		}
		uris = append(uris, members...)
	}
	var first error
	for _, uri := range uris {
		if uri == "" {
			continue
		}
		err := read(uri)
		if err == nil {
			return uri, nil
		}
		if first == nil {
			first = err
		}
	}
	if first != nil {
		return "", fmt.Errorf("bmc: %s: %w", what, first)
	}
	return "", fmt.Errorf("bmc: endpoint exposes no %s", what)
}

// managerURI finds the manager once per session: the first member of the
// Managers collection that reads.
func (c *client) managerURI() (string, error) {
	c.memo.Lock()
	uri := c.memo.managerURI
	c.memo.Unlock()
	if uri != "" {
		return uri, nil
	}
	root, err := c.root()
	if err != nil {
		return "", err
	}
	var body []byte
	uri, err = c.firstReadable("managers", root.Managers.ODataID, nil, func(uri string) (err error) {
		body, _, err = c.getRaw(uri)
		return err
	})
	if err != nil {
		return "", err
	}
	c.memo.Lock()
	c.memo.managerURI, c.memo.managerDoc = uri, body
	c.memo.Unlock()
	return uri, nil
}

// manager reads the manager, fresh.
func (c *client) manager() (*schemas.Manager, error) {
	uri, err := c.managerURI()
	if err != nil {
		return nil, err
	}
	m, err := schemas.GetManager(c.api, uri)
	if err != nil {
		return nil, fmt.Errorf("bmc: manager: %w", err)
	}
	return m, nil
}

// managerDoc decodes the manager resource, as read once per session, into
// v. It is for the links of the manager (to the media collection, the
// network protocol, the OEM services), which do not change while a session
// lasts; a property that does is read with manager.
func (c *client) managerDoc(v any) error {
	if _, err := c.managerURI(); err != nil {
		return err
	}
	c.memo.Lock()
	body := c.memo.managerDoc
	c.memo.Unlock()
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("bmc: manager: %w", err)
	}
	return nil
}

func (c *client) Manager(ctx context.Context) (bmc.Manager, error) {
	m, err := c.manager()
	if err != nil {
		return bmc.Manager{}, err
	}
	out := bmc.Manager{
		FirmwareVersion: m.FirmwareVersion,
		Vendor:          c.vendor(m),
		Hostname:        c.managerHostname(m),
		LastResetTime:   parseResetTime(m.LastResetTime),
	}
	if nics, err := m.EthernetInterfaces(); err == nil {
		slices.SortFunc(nics, func(a, b *schemas.EthernetInterface) int { return compareIDs(a.ID, b.ID) })
		for _, n := range nics {
			if n.MACAddress != "" {
				out.MAC = strings.ToLower(n.MACAddress)
				break
			}
		}
	}
	return out, nil
}

// vendor names the endpoint's maker: ServiceRoot.Vendor where the service
// states it, the key of the Manager's Oem block otherwise (the iBMC), the
// Manager's Manufacturer as the last resort.
func (c *client) vendor(m *schemas.Manager) string {
	if c.svc.Vendor != "" {
		return c.svc.Vendor
	}
	if v := oemVendor(m.OEM); v != "" {
		return v
	}
	return m.Manufacturer
}

// managerHostname reads the manager's host name: HostName on the raw Manager
// resource (where the iBMC puts it, stamped with the chassis serial — not in
// the schema), the NetworkProtocol resource's HostName otherwise (where the
// schema puts it).
func (c *client) managerHostname(m *schemas.Manager) string {
	var doc struct{ HostName string }
	if _, err := c.getJSON(m.ODataID, &doc); err == nil && doc.HostName != "" {
		return doc.HostName
	}
	if np, err := m.NetworkProtocol(); err == nil && np != nil {
		return np.HostName
	}
	return ""
}

// chassisURI finds the chassis once per session. A BMC can serve more than
// one (a board and a storage enclosure), and their order in the collection
// is not a fact about the machine, so the chassis the first system links
// comes first. The members of the Chassis collection, in the order the
// service lists them, follow.
func (c *client) chassisURI() (string, error) {
	c.memo.Lock()
	uri := c.memo.chassisURI
	c.memo.Unlock()
	if uri != "" {
		return uri, nil
	}
	root, err := c.root()
	if err != nil {
		return "", err
	}
	uri, err = c.firstReadable("chassis", root.Chassis.ODataID, c.systemChassisLinks(root), func(uri string) error {
		_, err := c.getJSON(uri, nil)
		return err
	})
	if err != nil {
		return "", err
	}
	c.memo.Lock()
	c.memo.chassisURI = uri
	c.memo.Unlock()
	return uri, nil
}

// systemChassisLinks lists the chassis the first system names in
// Links.Chassis; none when that cannot be read.
func (c *client) systemChassisLinks(root rootLinks) []string {
	if root.Systems.ODataID == "" {
		return nil
	}
	systems, err := c.members(root.Systems.ODataID)
	if err != nil || len(systems) == 0 {
		return nil
	}
	var doc struct {
		Links struct {
			Chassis []ref `json:"Chassis"`
		} `json:"Links"`
	}
	if _, err := c.getJSON(systems[0], &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Links.Chassis))
	for _, r := range doc.Links.Chassis {
		out = append(out, r.ODataID)
	}
	return out
}

// chassis reads the chassis, fresh.
func (c *client) chassis() (*schemas.Chassis, error) {
	uri, err := c.chassisURI()
	if err != nil {
		return nil, err
	}
	ch, err := schemas.GetChassis(c.api, uri)
	if err != nil {
		return nil, fmt.Errorf("bmc: chassis: %w", err)
	}
	return ch, nil
}

func (c *client) Chassis(ctx context.Context) (bmc.Chassis, error) {
	ch, err := c.chassis()
	if err != nil {
		return bmc.Chassis{}, err
	}
	out := bmc.Chassis{Model: ch.Model, BoardSerial: ch.SerialNumber}
	// The X12 sweep order: Oem.Supermicro.BoardSerialNumber wins, plain
	// SerialNumber is the fallback.
	var oem struct {
		Supermicro struct {
			BoardSerialNumber string
		}
	}
	if len(ch.OEM) > 0 && json.Unmarshal(ch.OEM, &oem) == nil && oem.Supermicro.BoardSerialNumber != "" {
		out.BoardSerial = oem.Supermicro.BoardSerialNumber
	}
	if adapters, err := ch.NetworkAdapters(); err == nil {
		slices.SortFunc(adapters, func(a, b *schemas.NetworkAdapter) int { return compareIDs(a.ID, b.ID) })
		for _, a := range adapters {
			out.Adapters = append(out.Adapters, bmc.Adapter{
				Model:    a.Model,
				PortMACs: adapterPortMACs(a),
			})
		}
	}
	return out, nil
}

// adapterPortMACs reads an adapter's port MACs from where the specification
// puts them: the Ethernet block of each NetworkDeviceFunction. An adapter that
// serves no functions is asked for the deprecated NetworkPort resources
// instead, whose AssociatedNetworkAddresses carry the same fact (the iBMC).
// Best effort — an adapter that serves neither has no MACs to report.
func adapterPortMACs(a *schemas.NetworkAdapter) []string {
	var macs []string
	if fns, err := a.NetworkDeviceFunctions(); err == nil {
		slices.SortFunc(fns, func(x, y *schemas.NetworkDeviceFunction) int { return compareIDs(x.ID, y.ID) })
		for _, fn := range fns {
			if mac := cmpOr(fn.Ethernet.MACAddress, fn.Ethernet.PermanentMACAddress); mac != "" {
				macs = append(macs, strings.ToLower(mac))
			}
		}
	}
	if len(macs) > 0 {
		return macs
	}
	if ports, err := a.NetworkPorts(); err == nil {
		slices.SortFunc(ports, func(x, y *schemas.NetworkPort) int { return compareIDs(x.ID, y.ID) })
		for _, p := range ports {
			for _, addr := range p.AssociatedNetworkAddresses {
				macs = append(macs, strings.ToLower(addr))
			}
		}
	}
	return macs
}

func (c *client) Systems(ctx context.Context) ([]bmc.System, error) {
	systems, err := c.svc.Systems()
	if err != nil {
		return nil, fmt.Errorf("bmc: systems: %w", err)
	}
	slices.SortFunc(systems, func(a, b *schemas.ComputerSystem) int { return compareIDs(a.ID, b.ID) })
	var out []bmc.System
	for _, s := range systems {
		sys := bmc.System{
			ID:            s.ID,
			UUID:          s.UUID,
			SerialNumber:  s.SerialNumber,
			Manufacturer:  s.Manufacturer,
			Model:         s.Model,
			PowerState:    bmc.PowerState(s.PowerState),
			LastResetTime: parseResetTime(s.LastResetTime),
			MemoryGiB:     int(derefF(s.MemorySummary.TotalSystemMemoryGiB)),
			CPUModel:      s.ProcessorSummary.Model,
			CPUCount:      int(derefU(s.ProcessorSummary.Count)),
			BootProgress:  string(s.BootProgress.LastState),
			BootOverride: bmc.BootOverride{
				Target:  string(s.Boot.BootSourceOverrideTarget),
				Enabled: string(s.Boot.BootSourceOverrideEnabled),
				Mode:    string(s.Boot.BootSourceOverrideMode),
			},
		}
		for _, t := range s.Boot.AllowableBootSourceOverrideTargetValues {
			sys.BootTargets = append(sys.BootTargets, string(t))
		}
		if nics, err := s.EthernetInterfaces(); err == nil {
			slices.SortFunc(nics, func(a, b *schemas.EthernetInterface) int { return compareIDs(a.ID, b.ID) })
			for _, n := range nics {
				sys.NICs = append(sys.NICs, bmc.NIC{ID: n.ID, MAC: strings.ToLower(n.MACAddress)})
			}
		}
		sys.Drives = c.sweepDrives(s)
		out = append(out, sys)
	}
	return out, nil
}

// sweepDrives collects drives from both places they live: the system's Storage
// subsystems first (the specified location), then the chassis (where the
// captured vendors keep direct-attach NVMe). A drive seen in both places is
// kept once: the same resource, or the same serial number. A drive with no
// serial number (a virtual disk, a drive of a machine that is off) is told
// apart by its resource alone, so two such drives stay two.
func (c *client) sweepDrives(s *schemas.ComputerSystem) []bmc.Drive {
	seenID, seenSerial := map[string]bool{}, map[string]bool{}
	var out []bmc.Drive
	add := func(d *schemas.Drive) {
		if d == nil || seenID[d.ODataID] || (d.SerialNumber != "" && seenSerial[d.SerialNumber]) {
			return
		}
		seenID[d.ODataID] = true
		if d.SerialNumber != "" {
			seenSerial[d.SerialNumber] = true
		}
		out = append(out, bmc.Drive{
			Model:       d.Model,
			Serial:      d.SerialNumber,
			CapacityGiB: derefI(d.CapacityBytes) >> 30,
			MediaType:   string(d.MediaType),
			Protocol:    string(d.Protocol),
		})
	}
	// gofish reads collection members in parallel, so the order it returns
	// them in changes from call to call: sort by resource ID first.
	byID := func(drives []*schemas.Drive) {
		slices.SortFunc(drives, func(a, b *schemas.Drive) int { return compareIDs(a.ODataID, b.ODataID) })
	}
	if storage, err := s.Storage(); err == nil {
		slices.SortFunc(storage, func(a, b *schemas.Storage) int { return compareIDs(a.ODataID, b.ODataID) })
		for _, st := range storage {
			if drives, err := st.Drives(); err == nil {
				byID(drives)
				for _, d := range drives {
					add(d)
				}
			}
		}
	}
	if ch, err := c.chassis(); err == nil {
		if drives, err := ch.Drives(); err == nil {
			byID(drives)
			for _, d := range drives {
				add(d)
			}
		}
	}
	return out
}

func (c *client) Capabilities(ctx context.Context) (bmc.Capabilities, error) {
	media, err := c.Media(ctx)
	if err != nil {
		return bmc.Capabilities{}, err
	}
	caps := bmc.Capabilities{
		MediaDialect:    map[string]bmc.MediaDialect{},
		UpdateDialect:   c.updateDialect(),
		IdentityDialect: c.identityDialect(),
	}
	for _, m := range media {
		switch {
		case m.InsertStandard:
			caps.MediaDialect[m.ID] = bmc.DialectStandard
		case slices.Contains(m.OemActions, vmmControlAction):
			caps.MediaDialect[m.ID] = bmc.DialectVmmControl
		default:
			caps.MediaDialect[m.ID] = bmc.DialectNone
		}
	}
	return caps, nil
}

// systemByID reads one system. The member whose URI ends in the id is asked
// first, as that is where services put it; the others follow in the order
// the service lists them. A member that fails to read is skipped.
func (c *client) systemByID(id string) (*schemas.ComputerSystem, error) {
	root, err := c.root()
	if err != nil {
		return nil, err
	}
	members, err := c.members(root.Systems.ODataID)
	if err != nil {
		return nil, fmt.Errorf("bmc: systems: %w", err)
	}
	slices.SortStableFunc(members, func(a, b string) int {
		return boolRank(path.Base(b) == id) - boolRank(path.Base(a) == id)
	})
	var first error
	for _, uri := range members {
		s, err := schemas.GetComputerSystem(c.api, uri)
		if err != nil {
			first = cmp.Or(first, err)
			continue
		}
		if s.ID == id {
			return s, nil
		}
	}
	if first != nil {
		return nil, fmt.Errorf("bmc: no system %q: %w", id, first)
	}
	return nil, fmt.Errorf("bmc: no system %q", id)
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (c *client) Power(ctx context.Context, systemID string) (bmc.PowerState, error) {
	s, err := c.systemByID(systemID)
	if err != nil {
		return "", err
	}
	return bmc.PowerState(s.PowerState), nil
}

func (c *client) SetPower(ctx context.Context, systemID string, action bmc.PowerAction) error {
	if c.readOnly {
		return errReadOnly
	}
	s, err := c.systemByID(systemID)
	if err != nil {
		return err
	}
	info, err := s.Reset(schemas.ResetType(action))
	if err != nil {
		return fmt.Errorf("bmc: reset %s: %w", action, err)
	}
	return c.followTask(ctx, info)
}

// SetBootOverride PATCHes the Boot object: the target, the enablement (a
// one-time override when the caller names none) and — only when asked for —
// the mode: firmware that serves no mode property rejects a PATCH naming it.
// A raw PATCH of exactly these properties, with the system's ETag as If-Match.
func (c *client) SetBootOverride(ctx context.Context, systemID string, o bmc.BootOverride) error {
	if c.readOnly {
		return errReadOnly
	}
	s, err := c.systemByID(systemID)
	if err != nil {
		return err
	}
	boot := map[string]any{
		"BootSourceOverrideTarget":  o.Target,
		"BootSourceOverrideEnabled": cmpOr(o.Enabled, bmc.OverrideOnce),
	}
	if o.Mode != "" {
		boot["BootSourceOverrideMode"] = o.Mode
	}
	if err := c.patch(s.ODataID, map[string]any{"Boot": boot}); err != nil {
		return fmt.Errorf("bmc: boot override %s: %w", o.Target, err)
	}
	return nil
}

func (c *client) ResetManager(ctx context.Context, action bmc.PowerAction) error {
	if c.readOnly {
		return errReadOnly
	}
	m, err := c.manager()
	if err != nil {
		return err
	}
	if _, err := m.Reset(schemas.ResetType(action)); err != nil {
		return fmt.Errorf("bmc: manager reset %s: %w", action, err)
	}
	return nil
}

// parseResetTime reads the LastResetTime gofish leaves as a string. RFC3339 with
// optional fractional seconds is what the endpoints serve; anything else (or
// absence — the iBMC) is a zero time, i.e. observable not offered.
func parseResetTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// oemVendor reads the vendor from the Oem key of a resource — the iBMC stamps
// it there; empty when the endpoint doesn't. A block with more than one key
// names no single vendor, so it is empty too rather than a guess.
func oemVendor(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || len(m) != 1 {
		return ""
	}
	for k := range m {
		return k
	}
	return ""
}

// compareIDs orders member ids the way a person would: numbers numerically,
// everything else as text.
func compareIDs(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return an - bn
	}
	return strings.Compare(a, b)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefU(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}

func derefI(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
