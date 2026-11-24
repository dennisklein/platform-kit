// Package boot runs the media boot ladder — the day-0 sequence the CLI's boot
// verb fronts — and holds the rungs the in-cluster executor shares with it
// (slot and target choice, eject-and-verify, slot inspection): pack a rendered
// machine config into its ISO, serve it under a fresh grant (directly or through
// a relay), mount it on the BMC with the grant's credentials, point the boot
// override at removable media, power the node, and withdraw grant and medium
// once the image has been read or the grant expired.
package boot

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

type Options struct {
	Client bmc.Client
	// NodeName names the granted .iso and the revocation handle.
	NodeName string
	// Config is the rendered machine config to pack and serve.
	Config []byte
	// MediaID is the slot to mount on; empty picks the first insertable
	// optical slot, or the first insertable slot when none is optical.
	MediaID string
	// SystemID is the system to boot; empty requires the endpoint to front
	// exactly one.
	SystemID string
	// TTL bounds the grant — and the wait for the node's pull.
	TTL time.Duration
	// Listen is the asset server's own address (must be BMC-reachable).
	// Ignored when Relay is set.
	Listen string
	// Relay serves through a relay front instead of a local listener. The
	// ladder consumes the client: it is closed when serving stops.
	Relay *relay.Client
	// Hold keeps serving with the medium mounted after the pull, until ctx is
	// cancelled — for watching a node that needs the medium across re-reads.
	Hold bool
	// AllowRestart lets the ladder hard-reset a machine that is powered on.
	// Without it, Run refuses a running machine before it changes anything:
	// a restart kills whatever the machine runs.
	AllowRestart bool
	// NoPower mounts the medium and sets the boot override, but leaves power
	// alone: the person restarts the machine (and Run serves until the image
	// is read or the grant ends).
	NoPower bool
	// Logf reports each rung; nil is silent.
	Logf func(format string, args ...any)
}

// Result is what the ladder can observe from the serving side. ReadComplete is
// a heuristic — every byte of the image was served after the machine was
// told to start; the node's own check-in (the bootstrap flow) is the
// authoritative signal.
type Result struct {
	ImageURL     string // the granted URL as mounted (credentials travel separately)
	Size         int    // bytes of the packed config ISO
	ServedBytes  int64
	ReadComplete bool
}

// ErrMachineOn: the machine is powered on and the caller did not allow a
// restart. Run returns it before it changes anything.
var ErrMachineOn = errors.New("boot: the machine is powered on, and booting it means a hard reset")

// Run climbs the ladder. On return the grant is revoked and the medium ejected
// (best-effort on error paths), unless Hold kept them until ctx cancellation.
func Run(ctx context.Context, o Options) (Result, error) {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.NodeName == "" {
		return Result{}, fmt.Errorf("boot: node name required")
	}
	if o.TTL <= 0 {
		return Result{}, fmt.Errorf("boot: grant ttl must be positive")
	}

	slot, dialect, err := pickSlot(ctx, o.Client, o.MediaID)
	if err != nil {
		return Result{}, err
	}
	system, err := pickSystem(ctx, o.Client, o.SystemID)
	if err != nil {
		return Result{}, err
	}
	target, err := PickTarget(system.BootTargets)
	if err != nil {
		return Result{}, err
	}
	// Read the power state before anything changes, so that a refusal leaves
	// the machine as it was. An unreadable state counts as on: the reset is
	// what needs consent.
	state, perr := o.Client.Power(ctx, system.ID)
	running := perr != nil || state != bmc.PowerOff
	power := "power the machine on"
	switch {
	case o.NoPower:
		power = "leave power alone: restart the machine yourself to boot from the medium"
	case running:
		power = fmt.Sprintf("HARD-RESET the machine (it is %s now; the running OS is not shut down)", powerWord(state, perr))
	}
	logf("boot: plan for system %s (media slot %s, %s):", system.ID, slot, dialect)
	logf("boot:   1. eject whatever is mounted on slot %s", slot)
	logf("boot:   2. serve the config ISO for node %s for up to %s, under a one-time user and password", o.NodeName, o.TTL)
	logf("boot:   3. mount it on slot %s", slot)
	logf("boot:   4. boot from %s at the next start only", target)
	logf("boot:   5. %s", power)
	logf("boot:   6. wait until the machine has read the image, then eject it and stop serving")
	if running && !o.NoPower && !o.AllowRestart {
		return Result{}, ErrMachineOn
	}

	// The X12 lesson: a stale mount refuses InsertMedia — eject and verify
	// Inserted=false before mounting.
	if err := EjectAndVerify(ctx, o.Client, slot, logf); err != nil {
		return Result{}, err
	}

	iso, err := asset.BuildConfigISO(o.Config)
	if err != nil {
		return Result{}, err
	}

	grants := asset.NewGrants()
	creds, err := asset.MintCredentials()
	if err != nil {
		return Result{}, err
	}
	grant, err := grants.Issue(o.NodeName, o.NodeName+".iso", iso, o.TTL, creds)
	if err != nil {
		return Result{}, err
	}

	host, ln, err := listener(o)
	if err != nil {
		return Result{}, err
	}
	cert, err := asset.EphemeralCert()
	if err != nil {
		_ = ln.Close()
		return Result{}, err
	}
	m := newMeter(&asset.Server{Grants: grants}, int64(len(iso)))
	srv := &http.Server{Handler: m}
	go func() {
		_ = srv.Serve(tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}))
	}()
	defer func() { _ = srv.Close() }()

	imageURL := "https://" + host + grant.Path()
	res := Result{ImageURL: imageURL, Size: len(iso)}
	// The token in the URL is half of the grant: the log shows its start only.
	logf("boot: serving %d B under a %s grant at https://%s%s", len(iso), o.TTL, host,
		strings.Replace(grant.Path(), grant.Token, shortToken(grant.Token), 1))

	if err := o.Client.InsertMedia(ctx, slot, imageURL,
		&bmc.MediaCredentials{Username: creds.Username, Password: creds.Password}); err != nil {
		grants.Revoke(grant.Token)
		return res, fmt.Errorf("boot: insert: %w", err)
	}
	logf("boot: media mounted")
	if o.NoPower {
		// The person restarts the machine from now on. The reads the BMC
		// made to mount the medium are not the machine booting from it.
		m.arm()
	}

	override := bmc.BootOverride{Target: target, Enabled: bmc.OverrideOnce}
	// The medium boots UEFI. A system that serves another mode would take the
	// override as a legacy boot (the X12 reports Legacy out of the box), so
	// the mode goes along; one that serves none is not asked for it.
	if mode := system.BootOverride.Mode; mode != "" && mode != "UEFI" {
		override.Mode = "UEFI"
	}
	if err := o.Client.SetBootOverride(ctx, system.ID, override); err != nil {
		return res, cleanupAfter(ctx, o, grants, grant.Token, slot, err)
	}
	logf("boot: boot override %s (once)", target)

	if o.NoPower {
		logf("boot: power left alone; restart the machine to boot from the medium")
	} else {
		// Read again: the state may have changed while the medium went in,
		// and a machine somebody switched on meanwhile needs consent too.
		action := bmc.ActionOn
		if state, err := o.Client.Power(ctx, system.ID); err != nil || state != bmc.PowerOff {
			if !o.AllowRestart {
				return res, cleanupAfter(ctx, o, grants, grant.Token, slot, ErrMachineOn)
			}
			action = bmc.ActionForceRestart
		}
		// Only reads from here on count: a BMC reads the header of the
		// medium when it mounts it, and with a small config that probe
		// alone is as large as the image.
		m.arm()
		if err := o.Client.SetPower(ctx, system.ID, action); err != nil {
			return res, cleanupAfter(ctx, o, grants, grant.Token, slot, err)
		}
		logf("boot: power %s", action)
	}

	select {
	case <-m.done:
		res.ReadComplete = true
		logf("boot: image read complete")
	case <-time.After(o.TTL):
		logf("boot: grant expired before a full read")
	case <-ctx.Done():
		res.ServedBytes = m.total()
		return res, cleanupAfter(ctx, o, grants, grant.Token, slot, ctx.Err())
	}
	res.ServedBytes = m.total()

	if o.Hold {
		logf("boot: holding mount and grant until interrupted")
		<-ctx.Done()
	}
	err = cleanupAfter(ctx, o, grants, grant.Token, slot, nil)
	res.ServedBytes = m.total()
	return res, err
}

// shortToken is the start of a grant token, enough to tell two grants apart
// in a log, too short to use.
func shortToken(token string) string {
	if len(token) <= 6 {
		return "…"
	}
	return token[:6] + "…"
}

// cleanupAfter withdraws the medium and the grant, keeping cause as the error.
func cleanupAfter(ctx context.Context, o Options, grants *asset.Grants, token, slot string, cause error) error {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// The BMC may be mid-pull; eject first so the mount drops before its URL dies.
	if err := EjectAndVerify(context.WithoutCancel(ctx), o.Client, slot, logf); err != nil {
		logf("boot: eject on cleanup: %v", err)
	} else {
		logf("boot: media ejected")
	}
	if grants.Revoke(token) {
		logf("boot: grant revoked")
	}
	return cause
}

func powerWord(s bmc.PowerState, err error) string {
	if err != nil {
		return "in an unknown power state"
	}
	return strings.ToLower(string(s))
}

func listener(o Options) (host string, ln net.Listener, err error) {
	if o.Relay != nil {
		return o.Relay.FrontAddr(), o.Relay, nil
	}
	ln, err = net.Listen("tcp", o.Listen)
	if err != nil {
		return "", nil, fmt.Errorf("boot: listen: %w", err)
	}
	return ln.Addr().String(), ln, nil
}

func pickSlot(ctx context.Context, c bmc.Client, want string) (string, bmc.MediaDialect, error) {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return "", "", fmt.Errorf("boot: capabilities: %w", err)
	}
	if want != "" {
		if d, ok := caps.MediaDialect[want]; ok && d != bmc.DialectNone {
			return want, d, nil
		}
		return "", "", fmt.Errorf("boot: media %q advertises no insert mechanism", want)
	}
	media, err := c.Media(ctx)
	if err != nil {
		return "", "", fmt.Errorf("boot: media: %w", err)
	}
	if slots := InsertableSlots(caps.MediaDialect, media); len(slots) > 0 {
		return slots[0], caps.MediaDialect[slots[0]], nil
	}
	return "", "", fmt.Errorf("boot: endpoint advertises no insertable media slot")
}

// InsertableSlots lists the slots that advertise an insert mechanism, the best
// boot slot first: optical slots lead, wherever they sit in the endpoint's
// collection, the others follow in the endpoint's order (deterministic). Its
// length is what decides whether an installer and a config medium can be
// mounted side by side.
func InsertableSlots(dialects map[string]bmc.MediaDialect, media []bmc.VirtualMedia) []string {
	return bmc.InsertableSlots(media, bmc.Capabilities{MediaDialect: dialects})
}

func pickSystem(ctx context.Context, c bmc.Client, want string) (bmc.System, error) {
	systems, err := c.Systems(ctx)
	if err != nil {
		return bmc.System{}, fmt.Errorf("boot: systems: %w", err)
	}
	if want == "" {
		if len(systems) != 1 {
			return bmc.System{}, fmt.Errorf("boot: endpoint fronts %d systems, pick one", len(systems))
		}
		return systems[0], nil
	}
	for _, s := range systems {
		if s.ID == want {
			return s, nil
		}
	}
	return bmc.System{}, fmt.Errorf("boot: no system %q", want)
}

// PickTarget prefers UsbCd where offered — the X12 boots nothing from Cd —
// and never strays outside AllowableValues.
func PickTarget(targets []string) (string, error) {
	for _, want := range []string{"UsbCd", "Cd"} {
		if slices.Contains(targets, want) {
			return want, nil
		}
	}
	return "", fmt.Errorf("boot: no removable-media boot target in %v", targets)
}

// EjectAndVerify clears the slot if occupied and verifies Inserted=false —
// InsertMedia on a stale X12 mount answers "action not supported". An empty
// slot is success: the goal is an empty slot, not the act of emptying it.
func EjectAndVerify(ctx context.Context, c bmc.Client, slot string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	inserted, err := slotInserted(ctx, c, slot)
	if err != nil || !inserted {
		return err
	}
	logf("boot: ejecting stale mount on %s", slot)
	if err := c.EjectMedia(ctx, slot); err != nil {
		return fmt.Errorf("boot: eject %s: %w", slot, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	return poll.Until(ctx, poll.Every(200*time.Millisecond), func(ctx context.Context) (bool, error) {
		inserted, err := slotInserted(ctx, c, slot)
		switch {
		case err != nil:
			return false, err
		case !inserted:
			return true, nil
		case time.Now().After(deadline):
			return false, fmt.Errorf("boot: media %s still inserted after eject", slot)
		}
		return false, nil
	})
}

func slotInserted(ctx context.Context, c bmc.Client, slot string) (bool, error) {
	media, err := c.Media(ctx)
	if err != nil {
		return false, fmt.Errorf("boot: media: %w", err)
	}
	for _, m := range media {
		if m.ID == slot {
			return m.Inserted, nil
		}
	}
	return false, fmt.Errorf("boot: no media slot %q", slot)
}

// meter watches the serving side for the node's pull. ServedBytes counts
// every byte served. The pull counts only once the meter is armed (at the
// power action), and it counts distinct bytes of the image, so a probe the
// BMC repeats is counted once: done closes when every byte of the image was
// served after the arm. The check-in is the authoritative gate.
type meter struct {
	next http.Handler
	size int64

	mu      sync.Mutex
	served  int64
	armed   bool
	covered spans
	once    sync.Once
	done    chan struct{}
}

func newMeter(next http.Handler, size int64) *meter {
	return &meter{next: next, size: size, done: make(chan struct{})}
}

// arm starts the count of the pull. Reads before it are not the machine
// booting.
func (m *meter) arm() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.armed = true
}

func (m *meter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cw := &countingWriter{ResponseWriter: w}
	m.next.ServeHTTP(cw, r)
	if r.Method != http.MethodGet {
		return
	}
	m.mu.Lock()
	m.served += cw.n
	if m.armed {
		if start, ok := servedFrom(cw); ok {
			m.covered.add(start, start+cw.n)
		}
	}
	complete := m.covered.total() >= m.size
	m.mu.Unlock()
	if complete {
		m.once.Do(func() { close(m.done) })
	}
}

// servedFrom is the image offset of the first byte a response carried: 0
// for a whole answer, the start of the range for a single-range answer. A
// multi-range answer or a range it cannot read counts as no pull.
func servedFrom(cw *countingWriter) (int64, bool) {
	switch cw.code {
	case 0, http.StatusOK:
		return 0, true
	case http.StatusPartialContent:
		var start, end int64
		cr := cw.Header().Get("Content-Range")
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/", &start, &end); err != nil {
			return 0, false
		}
		return start, true
	}
	return 0, false
}

func (m *meter) total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.served
}

// spans is a set of half-open byte ranges, kept sorted and disjoint.
type spans [][2]int64

func (s *spans) add(start, end int64) {
	if end <= start {
		return
	}
	out := make(spans, 0, len(*s)+1)
	for _, sp := range *s {
		switch {
		case sp[1] < start || sp[0] > end:
			out = append(out, sp)
		default:
			start, end = min(start, sp[0]), max(end, sp[1])
		}
	}
	out = append(out, [2]int64{start, end})
	slices.SortFunc(out, func(a, b [2]int64) int { return cmp.Compare(a[0], b[0]) })
	*s = out
}

func (s spans) total() int64 {
	var n int64
	for _, sp := range s {
		n += sp[1] - sp[0]
	}
	return n
}

type countingWriter struct {
	http.ResponseWriter
	n    int64
	code int
}

func (w *countingWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}
