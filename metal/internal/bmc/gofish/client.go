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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	gf "github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
)

type Config struct {
	Endpoint string // https://[fd6b:…]:443 or http://127.0.0.1:8000 (sim)
	Username string
	Password string
	Insecure bool // skip TLS verification (lab BMCs with self-signed certs)
	// RootCAs pins the server-certificate trust anchors (internal-ca on the
	// live BMCs — the PoC identity verdict). Wins over Insecure when set.
	RootCAs *x509.CertPool
	// ReadOnly refuses every mutating call at this layer — the read-only rail: pointing the
	// client at real hardware is safe by construction until explicitly unlocked.
	ReadOnly bool
}

type client struct {
	api      *gf.APIClient
	svc      *gf.Service
	readOnly bool
}

var errReadOnly = fmt.Errorf("bmc: mutating call refused (read-only client)")

// Connect dials the endpoint and returns a bmc.Client. Basic auth — no Redfish
// sessions, so an aborted run leaks nothing to clean up.
func Connect(ctx context.Context, cfg Config) (bmc.Client, error) {
	gcfg := gf.ClientConfig{
		Endpoint:  cfg.Endpoint,
		Username:  cfg.Username,
		Password:  cfg.Password,
		Insecure:  cfg.Insecure,
		BasicAuth: true,
	}
	// The transport is built here, not left to gofish, so that every
	// response passes the guard below, gofish's own reads included. It is
	// cloned from the default transport, as gofish does for its own client,
	// so a proxy and the dial and handshake timeouts are kept.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	switch {
	case cfg.RootCAs != nil:
		t.TLSClientConfig = &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
	case cfg.Insecure:
		// Lab BMCs with self-signed certs, asked for by the caller.
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec
	}
	gcfg.Insecure = false
	gcfg.HTTPClient = &http.Client{Transport: guard{next: t}}
	api, err := gf.ConnectContext(ctx, gcfg)
	if err != nil {
		return nil, fmt.Errorf("bmc: connect %s: %w", cfg.Endpoint, err)
	}
	return &client{api: api, svc: api.GetService(), readOnly: cfg.ReadOnly}, nil
}

func (c *client) Close() error {
	c.api.Logout()
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
type guard struct{ next http.RoundTripper }

func (g guard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	body, err := readBody(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	if req.Method == http.MethodGet && resp.StatusCode/100 == 2 &&
		resp.StatusCode != http.StatusAccepted && !jsonObjectOrEmpty(body) {
		return nil, fmt.Errorf("GET %s: answer is not a JSON object", req.URL.Path)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
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
// 202 naming a monitor. Some firmwares answer the same action synchronously.
func (c *client) pollAccepted(ctx context.Context, resp *http.Response) error {
	if resp.StatusCode != http.StatusAccepted {
		return nil
	}
	if uri := resp.Header.Get("Location"); uri != "" {
		return c.pollTask(ctx, uri)
	}
	return nil
}

// followTask polls the monitor of an asynchronous answer, if the answer was one.
func (c *client) followTask(ctx context.Context, info *schemas.TaskMonitorInfo) error {
	if info == nil || info.TaskMonitor == "" {
		return nil
	}
	return c.pollTask(ctx, info.TaskMonitor)
}

func (c *client) manager() (*schemas.Manager, error) {
	managers, err := c.svc.Managers()
	if err != nil {
		return nil, fmt.Errorf("bmc: managers: %w", err)
	}
	if len(managers) == 0 {
		return nil, fmt.Errorf("bmc: endpoint exposes no managers")
	}
	return managers[0], nil
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

func (c *client) chassis() (*schemas.Chassis, error) {
	chs, err := c.svc.Chassis()
	if err != nil {
		return nil, fmt.Errorf("bmc: chassis: %w", err)
	}
	if len(chs) == 0 {
		return nil, fmt.Errorf("bmc: endpoint exposes no chassis")
	}
	return chs[0], nil
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
// captured vendors keep direct-attach NVMe), deduped by serial.
func (c *client) sweepDrives(s *schemas.ComputerSystem) []bmc.Drive {
	seen := map[string]bool{}
	var out []bmc.Drive
	add := func(d *schemas.Drive) {
		if d == nil || seen[d.SerialNumber] {
			return
		}
		seen[d.SerialNumber] = true
		out = append(out, bmc.Drive{
			Model:       d.Model,
			Serial:      d.SerialNumber,
			CapacityGiB: derefI(d.CapacityBytes) >> 30,
			MediaType:   string(d.MediaType),
			Protocol:    string(d.Protocol),
		})
	}
	if storage, err := s.Storage(); err == nil {
		for _, st := range storage {
			if drives, err := st.Drives(); err == nil {
				for _, d := range drives {
					add(d)
				}
			}
		}
	}
	if ch, err := c.chassis(); err == nil {
		if drives, err := ch.Drives(); err == nil {
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

func (c *client) systemByID(id string) (*schemas.ComputerSystem, error) {
	systems, err := c.svc.Systems()
	if err != nil {
		return nil, fmt.Errorf("bmc: systems: %w", err)
	}
	for _, s := range systems {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("bmc: no system %q", id)
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
