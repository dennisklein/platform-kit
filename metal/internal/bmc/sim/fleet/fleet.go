package fleet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/machine/docker"
	"github.com/dennisklein/platform-kit/metal/internal/machine/qemu"
)

// Options are the caller's hooks into a running fleet.
type Options struct {
	// Wrap decorates a machine's Redfish handler — request logging, mostly.
	// Nil serves the handler as it is.
	Wrap func(machine string, h http.Handler) http.Handler
	// StateListen is the address the read-only state endpoint answers on,
	// over plain HTTP (see StateHandler). Empty serves none. Keep it on
	// loopback: it tells anyone who connects what every machine is doing.
	StateListen string
	// RequestHistory is how many Redfish requests per machine the state
	// endpoint shows; zero is DefaultRequestHistory.
	RequestHistory int
	// HangTimeout is the qemu backend's firmware-hang guard: how long a VM
	// may show only screen control sequences on its console after a
	// power-on before it is power-cycled, once. Zero is
	// qemu.DefaultHangTimeout; negative turns the guard off.
	HangTimeout time.Duration
}

// Endpoint is one BMC of a running fleet.
type Endpoint struct {
	Name        string
	URL         string // https://host:port — the Redfish service root is below /redfish/v1
	Personality string
	Backend     string
	// Username and Password are the account the BMC was given, or the
	// fixture's factory account; both empty on a fixture without accounts.
	Username, Password string
	// State is the BMC's state, for callers that script it in-process.
	State *sim.State
}

// Fleet is a set of BMCs being served.
type Fleet struct {
	endpoints []Endpoint
	members   []member
	mediaURL  string
	stateURL  string
	servers   []*http.Server
	backends  []sim.Machine
	failed    chan error
}

// Start brings a fleet up: every address is bound and every backend prepared
// before the first request is served, so an error here means nothing runs. No
// machine is powered on — that is the BMC's job, when asked.
func Start(ctx context.Context, cfg *Config, o Options) (_ *Fleet, err error) {
	f := &Fleet{failed: make(chan error, len(cfg.Machines)+1)}
	var listeners []net.Listener
	defer func() {
		if err != nil {
			for _, ln := range listeners {
				_ = ln.Close()
			}
		}
	}()
	// listen binds an address and wraps it in TLS; the certificate comes from
	// the callback so that a BMC can swap its own.
	listen := func(addr string, cert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (net.Listener, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, ln)
		return tls.NewListener(ln, &tls.Config{GetCertificate: cert, MinVersion: tls.VersionTLS12}), nil
	}
	serve := func(name string, ln net.Listener, h http.Handler) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		f.servers = append(f.servers, srv)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				f.failed <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	var cache *sim.MediaCache
	if cfg.MediaCache != "" {
		cache = sim.NewMediaCache(cfg.MediaCache)
	}

	type pending struct {
		name string
		ln   net.Listener
		h    http.Handler
	}
	var ready []pending
	for _, m := range cfg.Machines {
		fx, err := sim.LoadFixture(m.Personality)
		if err != nil {
			return nil, fmt.Errorf("machine %q: %w", m.Name, err)
		}
		id := sim.Identity{
			Serial: m.Identity.Serial, UUID: m.Identity.UUID,
			BMCMAC: m.Identity.BMCMAC, NICMACs: m.Identity.NICMACs,
		}
		if m.Qemu != nil {
			id.DiskSerial = m.Qemu.DiskSerial
		}
		fx.Personalize(id)
		if m.Credentials.Username != "" {
			fx.SetCredentials(m.Credentials.Username, m.Credentials.Password)
		}
		st := sim.NewState(fx)
		st.SetMediaCache(cache)

		backend, err := newBackend(ctx, cfg, m, o)
		if err != nil {
			return nil, fmt.Errorf("machine %q: %w", m.Name, err)
		}
		if backend != nil {
			st.AttachMachine(fx.Systems[0].ID, backend)
			f.backends = append(f.backends, backend)
		}

		cert, err := asset.EphemeralCert()
		if err != nil {
			return nil, err
		}
		st.SetServedCertificate(cert)
		ln, err := listen(m.Listen, st.TLSCertificate)
		if err != nil {
			return nil, fmt.Errorf("machine %q: %w", m.Name, err)
		}
		reqs := newRequestLog(o.RequestHistory)
		h := reqs.wrap(sim.NewServer(st), time.Now)
		if o.Wrap != nil {
			h = o.Wrap(m.Name, h)
		}
		ready = append(ready, pending{m.Name, ln, h})
		ep := Endpoint{
			Name: m.Name, URL: "https://" + ln.Addr().String(),
			Personality: fx.Name, Backend: m.Backend, State: st,
		}
		if len(fx.Accounts) > 0 {
			ep.Username, ep.Password = fx.Accounts[0].Username, fx.Accounts[0].Password
		}
		f.endpoints = append(f.endpoints, ep)
		// The configured identity first: a fixture may hide the serial from
		// Redfish (the X12 blind spot), the fleet file never does.
		mem := member{ep: ep, serial: m.Identity.Serial, bmcMAC: m.Identity.BMCMAC, backend: backend, log: reqs}
		if mem.serial == "" {
			mem.serial = fx.Systems[0].Serial
		}
		if mem.bmcMAC == "" {
			mem.bmcMAC = fx.Manager.MAC
		}
		f.members = append(f.members, mem)
	}

	if cfg.Media != nil {
		cert, err := asset.EphemeralCert()
		if err != nil {
			return nil, err
		}
		ln, err := listen(cfg.Media.Listen, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil })
		if err != nil {
			return nil, fmt.Errorf("media: %w", err)
		}
		f.mediaURL = "https://" + ln.Addr().String()
		ready = append(ready, pending{"media", ln, http.FileServer(http.Dir(cfg.Media.Dir))})
	}

	if o.StateListen != "" {
		ln, err := net.Listen("tcp", o.StateListen)
		if err != nil {
			return nil, fmt.Errorf("state endpoint: %w", err)
		}
		listeners = append(listeners, ln)
		f.stateURL = "http://" + ln.Addr().String() + StatePath
		ready = append(ready, pending{"state", ln, f.StateHandler()})
	}

	for _, p := range ready {
		serve(p.name, p.ln, p.h)
	}
	return f, nil
}

// newBackend prepares the machine behind one BMC; nil for the none backend.
func newBackend(ctx context.Context, cfg *Config, m Machine, fo Options) (sim.Machine, error) {
	switch m.Backend {
	case BackendQemu:
		o := qemu.Options{Name: m.Name, Dir: filepath.Join(cfg.StateDir, m.Name), HangTimeout: fo.HangTimeout}
		// The VM's NIC carries the address the BMC's inventory reports.
		if len(m.Identity.NICMACs) > 0 {
			o.MAC = m.Identity.NICMACs[0]
		}
		if q := m.Qemu; q != nil {
			o.Tap, o.MemoryMiB, o.CPUs, o.DiskGiB = q.Tap, q.MemoryMiB, q.CPUs, q.DiskGiB
			o.DiskSerial, o.Accel, o.HostIP, o.ForwardPorts = q.DiskSerial, q.Accel, q.HostIP, q.ForwardPorts
		}
		vm := qemu.New(o)
		if err := vm.Ensure(ctx); err != nil {
			return nil, err
		}
		return vm, nil
	case BackendDocker:
		c := docker.New(m.Name, m.Docker.Image, m.Docker.Network, m.Docker.IP)
		if err := c.Ensure(ctx); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, nil
	}
}

// Endpoints lists the BMCs in configuration order.
func (f *Fleet) Endpoints() []Endpoint { return f.endpoints }

// MediaURL is the base URL of the media directory, empty when none is served.
func (f *Fleet) MediaURL() string { return f.mediaURL }

// StateURL is the URL of the state endpoint, empty when none is served.
func (f *Fleet) StateURL() string { return f.stateURL }

// Failed delivers the error of a listener that stopped serving on its own.
func (f *Fleet) Failed() <-chan error { return f.failed }

// Close stops serving and powers the machines off: a VM or container must not
// outlive the BMC that is its only control. Their disks stay where they are.
func (f *Fleet) Close(ctx context.Context) error {
	var errs []error
	for _, srv := range f.servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, srv.Close())
		}
	}
	for _, b := range f.backends {
		errs = append(errs, b.Stop(ctx))
	}
	return errors.Join(errs...)
}
