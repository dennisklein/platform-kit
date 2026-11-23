// Package boot runs the media boot ladder — the day-0 sequence the CLI's boot
// verb fronts and the provisioning flow will drive in-cluster: pack a rendered
// machine config into its ISO, serve it under a fresh grant (directly or through
// a relay), mount it on the BMC with the grant's credentials, point the boot
// override at removable media, power the node, and withdraw grant and medium
// once the image has been read or the grant expired.
package boot

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

type Options struct {
	Client bmc.Client
	// NodeName names the granted .iso and the revocation handle.
	NodeName string
	// Config is the rendered machine config to pack and serve.
	Config []byte
	// MediaID is the slot to mount on; empty picks the first insertable one.
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
	// Logf reports each rung; nil is silent.
	Logf func(format string, args ...any)
}

// Result is what the ladder can observe from the serving side. ReadComplete is
// a heuristic — cumulative bytes served reached the image size; the node's own
// check-in (the bootstrap flow) is the authoritative signal.
type Result struct {
	ImageURL     string // the granted URL as mounted (credentials travel separately)
	Size         int    // bytes of the packed config ISO
	ServedBytes  int64
	ReadComplete bool
}

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
	target, err := pickTarget(system.BootTargets)
	if err != nil {
		return Result{}, err
	}
	logf("boot: media %s (%s), system %s, boot target %s", slot, dialect, system.ID, target)

	// The X12 lesson: a stale mount refuses InsertMedia — eject and verify
	// Inserted=false before mounting.
	if err := ejectStale(ctx, o.Client, slot, logf); err != nil {
		return Result{}, err
	}

	iso, err := asset.BuildConfigISO(o.Config)
	if err != nil {
		return Result{}, err
	}

	grants := asset.NewGrants()
	creds, err := mintCredentials()
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
	m := &meter{next: &asset.Server{Grants: grants}, size: int64(len(iso)), done: make(chan struct{})}
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
	logf("boot: serving %d B under a %s grant at %s", len(iso), o.TTL, imageURL)

	if err := o.Client.InsertMedia(ctx, slot, imageURL,
		&bmc.MediaCredentials{Username: creds.Username, Password: creds.Password}); err != nil {
		grants.Revoke(grant.Token)
		return res, fmt.Errorf("boot: insert: %w", err)
	}
	logf("boot: media mounted")

	if err := o.Client.SetBootOverride(ctx, system.ID, target, true); err != nil {
		return res, cleanupAfter(ctx, o, grants, grant.Token, slot, err)
	}
	logf("boot: boot override %s (once)", target)

	action := bmc.ActionOn
	if state, err := o.Client.Power(ctx, system.ID); err == nil && state == bmc.PowerOn {
		action = bmc.ActionForceRestart
	}
	if err := o.Client.SetPower(ctx, system.ID, action); err != nil {
		return res, cleanupAfter(ctx, o, grants, grant.Token, slot, err)
	}
	logf("boot: power %s", action)

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

// cleanupAfter withdraws the medium and the grant, keeping cause as the error.
func cleanupAfter(ctx context.Context, o Options, grants *asset.Grants, token, slot string, cause error) error {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// The BMC may be mid-pull; eject first so the mount drops before its URL dies.
	if err := ejectStale(context.WithoutCancel(ctx), o.Client, slot, logf); err != nil {
		logf("boot: eject on cleanup: %v", err)
	} else {
		logf("boot: media ejected")
	}
	if grants.Revoke(token) {
		logf("boot: grant revoked")
	}
	return cause
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
	for _, m := range media { // slot order is the endpoint's — deterministic
		if d := caps.MediaDialect[m.ID]; d != "" && d != bmc.DialectNone {
			return m.ID, d, nil
		}
	}
	return "", "", fmt.Errorf("boot: endpoint advertises no insertable media slot")
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

// pickTarget prefers UsbCd where offered — the X12 boots nothing from Cd —
// and never strays outside AllowableValues.
func pickTarget(targets []string) (string, error) {
	for _, want := range []string{"UsbCd", "Cd"} {
		if slices.Contains(targets, want) {
			return want, nil
		}
	}
	return "", fmt.Errorf("boot: no removable-media boot target in %v", targets)
}

// ejectStale clears the slot if occupied and verifies Inserted=false — InsertMedia
// on a stale X12 mount answers "action not supported".
func ejectStale(ctx context.Context, c bmc.Client, slot string, logf func(string, ...any)) error {
	inserted, err := slotInserted(ctx, c, slot)
	if err != nil || !inserted {
		return err
	}
	logf("boot: ejecting stale mount on %s", slot)
	if err := c.EjectMedia(ctx, slot); err != nil {
		return fmt.Errorf("boot: eject %s: %w", slot, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		inserted, err := slotInserted(ctx, c, slot)
		if err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("boot: media %s still inserted after eject", slot)
		}
		time.Sleep(200 * time.Millisecond)
	}
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

func mintCredentials() (*asset.Credentials, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return &asset.Credentials{Username: "media", Password: hex.EncodeToString(raw)}, nil
}

// meter watches the serving side for the node's pull: cumulative GET bytes
// reaching the image size closes done. Overlapping ranges double-count, which
// only ever fires the signal early — the check-in is the authoritative gate.
type meter struct {
	next http.Handler
	size int64

	mu     sync.Mutex
	served int64
	once   sync.Once
	done   chan struct{}
}

func (m *meter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cw := &countingWriter{ResponseWriter: w}
	m.next.ServeHTTP(cw, r)
	if r.Method != http.MethodGet {
		return
	}
	m.mu.Lock()
	m.served += cw.n
	total := m.served
	m.mu.Unlock()
	if total >= m.size {
		m.once.Do(func() { close(m.done) })
	}
}

func (m *meter) total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.served
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}
