package sim

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// State is the sim's mutable world: a Fixture plus everything an operation can change
// (power, boot override, mounted media, injected faults). It implements bmc.Client
// directly — the in-memory backend for unit tests — and server.go serves the same State
// over HTTP for the gofish client and the CLI.
type State struct {
	mu        sync.Mutex
	f         *Fixture
	power     map[string]bmc.PowerState // by system id
	boot      map[string]bootOverride
	media     map[string]*mountedMedia // by media slot id
	faults    map[Fault]bool
	resets    map[string]int        // ForceRestart/On transitions per system — test observability
	lastReset map[string]time.Time  // advances on reset — only for systems whose fixture serves it
	machines  map[string]Machine    // real machines behind systems (machine.go); nil entries = mem-only
	fw        fwState               // manager firmware + staging slot (convergence.go)
	bios      map[string]*biosState // by system id; nil entry = no BIOS resource served
	accounts  []*account            // manager accounts (accounts.go); empty = open sim, no auth
	ntp       bmc.NTPConfig         // time sources (ntp.go); meaningful only where the fixture serves NTP
	identity  identityState         // HTTPS identity (identity.go); served/pending/staged certificates
}

type bootOverride struct {
	target string
	once   bool
}

type mountedMedia struct {
	imageURL string                // as the BMC stores it — userinfo intact on the vmm dialect
	creds    *bmc.MediaCredentials // fetch credentials, whichever way they arrived
	image    []byte                // the full pull a power-on performs — the sim's "boot"
}

// mediaFetcher is the sim's HTTP client for media pulls — skip-verify, mirroring
// the deployed posture (iBMC verifies nothing; the X12 trust store is empty and
// unfeedable).
var mediaFetcher = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // the observed BMC posture
}

// Fault injection — the lying-oracle states real hardware won't produce on demand.
type Fault string

const (
	// FaultUnreachable makes every call fail — the dead-BMC path.
	FaultUnreachable Fault = "unreachable"
	// FaultStalePower freezes reported power state regardless of actions.
	FaultStalePower Fault = "stale-power"
	// FaultFlashFails makes a correct firmware upload answer success while
	// nothing ever flashes — no version advance, no BMC reboot. The recorded
	// X12 pattern generalised: completion must be judged by observables.
	FaultFlashFails Fault = "flash-fails"
	// FaultBiosStuck holds staged BIOS settings across every reset — pending
	// never applies, never clears.
	FaultBiosStuck Fault = "bios-stuck"
	// FaultCertStuck answers success to a certificate import/upload while
	// the listener keeps serving the old identity across every manager
	// reset — the lying oracle for the identity lane.
	FaultCertStuck Fault = "cert-stuck"
)

var errUnreachable = errors.New("sim: endpoint unreachable (injected)")

func NewState(f *Fixture) *State {
	s := &State{
		f:         f,
		power:     map[string]bmc.PowerState{},
		boot:      map[string]bootOverride{},
		media:     map[string]*mountedMedia{},
		faults:    map[Fault]bool{},
		resets:    map[string]int{},
		lastReset: map[string]time.Time{},
		fw:        fwState{version: f.Manager.Firmware, lastReset: f.Manager.LastResetTime},
		bios:      map[string]*biosState{},
		ntp:       bmc.NTPConfig{Enabled: f.NTP.Enabled, Servers: f.NTP.Servers},
	}
	for _, a := range f.Accounts {
		s.accounts = append(s.accounts, &account{
			id: a.ID, username: a.Username, password: a.Password, role: a.Role, enabled: a.Enabled,
		})
	}
	for _, sys := range f.Systems {
		s.power[sys.ID] = bmc.PowerState(sys.Power)
		if !sys.LastResetTime.IsZero() {
			s.lastReset[sys.ID] = sys.LastResetTime
		}
		s.bios[sys.ID] = nil // present-but-nil = existing system, no BIOS resource
		if f.Bios.Attributes != nil {
			s.bios[sys.ID] = &biosState{current: jsonTypes(f.Bios.Attributes)}
		}
	}
	return s
}

// SetFault toggles an injected fault.
func (s *State) SetFault(f Fault, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[f] = on
}

// BootOverride reports the set boot override (test observability); empty target
// means none.
func (s *State) BootOverride(systemID string) (target string, once bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bo := s.boot[systemID]
	return bo.target, bo.once
}

// Resets reports how many power transitions a system has seen (test observability).
func (s *State) Resets(systemID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets[systemID]
}

func (s *State) reachable() error {
	if s.faults[FaultUnreachable] {
		return errUnreachable
	}
	return nil
}

func (s *State) Manager(context.Context) (bmc.Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.Manager{}, err
	}
	return bmc.Manager{
		Vendor:          s.f.Vendor,
		FirmwareVersion: s.fw.version,
		Hostname:        s.f.Manager.Hostname,
		MAC:             s.f.Manager.MAC,
		LastResetTime:   s.fw.lastReset,
	}, nil
}

func (s *State) Chassis(context.Context) (bmc.Chassis, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.Chassis{}, err
	}
	ch := bmc.Chassis{BoardSerial: s.f.Chassis.BoardSerial, Model: s.f.Chassis.Model}
	for _, a := range s.f.Chassis.Adapters {
		ch.Adapters = append(ch.Adapters, bmc.Adapter{Model: a.Model, PortMACs: a.PortMACs})
	}
	return ch, nil
}

func (s *State) Systems(ctx context.Context) ([]bmc.System, error) {
	s.syncMachines(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	var out []bmc.System
	for _, fs := range s.f.Systems {
		sys := bmc.System{
			ID:            fs.ID,
			UUID:          fs.UUID,
			SerialNumber:  fs.Serial,
			Manufacturer:  fs.Manufacturer,
			Model:         fs.Model,
			PowerState:    s.reportedPower(fs.ID),
			LastResetTime: s.lastReset[fs.ID],
			MemoryGiB:     fs.MemoryGiB,
			CPUModel:      fs.CPUModel,
			CPUCount:      fs.CPUCount,
			BootTargets:   fs.BootTargets,
		}
		for _, n := range fs.NICs {
			sys.NICs = append(sys.NICs, bmc.NIC{ID: n.ID, MAC: n.MAC})
		}
		for _, d := range fs.Drives {
			sys.Drives = append(sys.Drives, bmc.Drive{Model: d.Model, Serial: d.Serial, CapacityGiB: d.CapacityGiB, MediaType: d.Media, Protocol: d.Protocol})
		}
		out = append(out, sys)
	}
	return out, nil
}

func (s *State) Capabilities(context.Context) (bmc.Capabilities, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.Capabilities{}, err
	}
	caps := bmc.Capabilities{
		MediaDialect:    map[string]bmc.MediaDialect{},
		UpdateDialect:   s.f.updateDialect(),
		IdentityDialect: bmc.IdentityDialect(s.f.identityDialect()),
	}
	for i, vm := range s.f.VirtualMedia {
		caps.MediaDialect[vm.ID] = s.f.dialect(i)
	}
	return caps, nil
}

// reportedPower honours the stale-power fault: the world moved, the oracle lies.
func (s *State) reportedPower(systemID string) bmc.PowerState {
	if s.faults[FaultStalePower] {
		return bmc.PowerOn
	}
	return s.power[systemID]
}

func (s *State) Power(ctx context.Context, systemID string) (bmc.PowerState, error) {
	s.syncMachines(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return "", err
	}
	if _, ok := s.power[systemID]; !ok {
		return "", fmt.Errorf("sim: no system %q", systemID)
	}
	return s.reportedPower(systemID), nil
}

func (s *State) SetPower(ctx context.Context, systemID string, action bmc.PowerAction) error {
	s.syncMachines(ctx)
	s.mu.Lock()
	if err := s.reachable(); err != nil {
		s.mu.Unlock()
		return err
	}
	if _, ok := s.power[systemID]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("sim: no system %q", systemID)
	}
	// Decide the transition under the lock, drive the machine outside it: a
	// failing machine op fails the action with the sim's view untouched.
	verb := ""
	switch action {
	case bmc.ActionOn:
		verb = "start"
	case bmc.ActionForceOff, bmc.ActionGracefulShutdown:
		verb = "stop"
	case bmc.ActionForceRestart:
		if s.power[systemID] != bmc.PowerOn {
			s.mu.Unlock()
			return fmt.Errorf("sim: ForceRestart on powered-off system %q", systemID)
		}
		verb = "restart"
	default:
		s.mu.Unlock()
		return fmt.Errorf("sim: unsupported reset type %q", action)
	}
	s.mu.Unlock()
	// The boot-time pull happens first: a starting system reads its mounted
	// media in full, and a media-taking machine gets the images before power
	// is driven. Outside the lock — network I/O against the asset server.
	if verb == "start" || verb == "restart" {
		if err := s.presentMedia(ctx, systemID); err != nil {
			return err
		}
	}
	if err := s.driveMachine(ctx, systemID, verb); err != nil {
		return err
	}

	s.mu.Lock()
	booted := false
	switch action {
	case bmc.ActionOn:
		s.power[systemID] = bmc.PowerOn
		s.reset(systemID)
		booted = true
	case bmc.ActionForceOff, bmc.ActionGracefulShutdown:
		s.power[systemID] = bmc.PowerOff
	case bmc.ActionForceRestart:
		s.reset(systemID)
		booted = true
	}
	if booted {
		// A system coming out of reset applies its staged BIOS settings —
		// the @Redfish.Settings pending→applied edge.
		s.applyBios(systemID)
	}
	s.mu.Unlock()
	return nil
}

// bootFetch pulls a mounted medium's full content and keeps it as the booted
// image. A failed pull simply leaves no image — the node didn't boot from it.
func (s *State) bootFetch(ctx context.Context, mm *mountedMedia) {
	s.mu.Lock()
	raw, creds := mm.imageURL, mm.creds
	s.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	body, err := s.fetch(ctx, http.MethodGet, u, creds, "")
	if err != nil {
		return
	}
	s.mu.Lock()
	mm.image = body
	s.mu.Unlock()
}

// reset records a power transition; the reset-time observable only advances for
// systems whose fixture serves it (the iBMC serves none — the sim never invents
// facts real hardware withholds).
func (s *State) reset(systemID string) {
	s.resets[systemID]++
	if _, served := s.lastReset[systemID]; served {
		s.lastReset[systemID] = time.Now()
	}
}

func (s *State) SetBootOverride(_ context.Context, systemID, target string, once bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	for _, fs := range s.f.Systems {
		if fs.ID != systemID {
			continue
		}
		if !slices.Contains(fs.BootTargets, target) {
			return fmt.Errorf("sim: boot target %q not in AllowableValues %v", target, fs.BootTargets)
		}
		s.boot[systemID] = bootOverride{target: target, once: once}
		return nil
	}
	return fmt.Errorf("sim: no system %q", systemID)
}

func (s *State) Media(context.Context) ([]bmc.VirtualMedia, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	var out []bmc.VirtualMedia
	for _, vm := range s.f.VirtualMedia {
		m := bmc.VirtualMedia{
			ID:             vm.ID,
			MediaTypes:     vm.MediaTypes,
			InsertStandard: vm.InsertStandard,
			OemActions:     vm.OemActions,
		}
		if mm := s.media[vm.ID]; mm != nil {
			m.Inserted, m.ImageURL = true, mm.imageURL
		}
		out = append(out, m)
	}
	return out, nil
}

// InsertMedia validates the URL the way the vendors do, then performs the
// mount-time fetch a real BMC performs: both dialects HEAD the image, the
// standard (X12) dialect additionally sniffs the first sector. Nobody reads the
// full content before boot — that pull happens at power-on.
func (s *State) InsertMedia(ctx context.Context, mediaID, imageURL string, creds *bmc.MediaCredentials) error {
	s.mu.Lock()
	dialect, err := s.insertable(mediaID)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	u, err := url.Parse(imageURL)
	if err != nil {
		return fmt.Errorf("sim: media %q: unparsable image url", mediaID)
	}
	if u.Scheme != "https" {
		// iBMC rejects plain http before dialing; https-only is the posture.
		return fmt.Errorf("sim: media %q: image url must be https", mediaID)
	}
	if u.RawQuery != "" || !strings.HasSuffix(u.Path, ".iso") {
		return fmt.Errorf("sim: media %q: incorrect file format", mediaID)
	}
	switch {
	case dialect == bmc.DialectStandard && u.User != nil:
		return fmt.Errorf("sim: media %q: userinfo rejected at the Image schema", mediaID)
	case dialect == bmc.DialectVmmControl && u.User != nil:
		pw, _ := u.User.Password()
		creds = &bmc.MediaCredentials{Username: u.User.Username(), Password: pw}
	}

	if err := s.fetchMount(ctx, u, creds, dialect); err != nil {
		// The observed iBMC failure shape for an unfetchable image.
		return fmt.Errorf("sim: media %q: failed to connect to virtual media: %v", mediaID, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.insertable(mediaID); err != nil { // raced with another insert
		return err
	}
	s.media[mediaID] = &mountedMedia{imageURL: imageURL, creds: creds}
	return nil
}

// insertable checks slot existence, dialect, and vacancy under the caller's lock.
func (s *State) insertable(mediaID string) (bmc.MediaDialect, error) {
	if err := s.reachable(); err != nil {
		return "", err
	}
	for i, vm := range s.f.VirtualMedia {
		if vm.ID != mediaID {
			continue
		}
		if d := s.f.dialect(i); d != bmc.DialectNone {
			if s.media[mediaID] != nil {
				return "", fmt.Errorf("sim: media %q already inserted", mediaID)
			}
			return d, nil
		}
		return "", fmt.Errorf("sim: media %q advertises no insert mechanism", mediaID)
	}
	return "", fmt.Errorf("sim: no media slot %q", mediaID)
}

func (s *State) fetchMount(ctx context.Context, u *url.URL, creds *bmc.MediaCredentials, dialect bmc.MediaDialect) error {
	if _, err := s.fetch(ctx, http.MethodHead, u, creds, ""); err != nil {
		return err
	}
	if dialect == bmc.DialectStandard {
		_, err := s.fetch(ctx, http.MethodGet, u, creds, "bytes=0-511")
		return err
	}
	return nil
}

// fetch is one media pull; the userinfo travels as Basic auth (which is how both
// credential forms arrive at the asset server), never in the dialed URL.
func (s *State) fetch(ctx context.Context, method string, u *url.URL, creds *bmc.MediaCredentials, rangeHeader string) ([]byte, error) {
	clean := *u
	clean.User = nil
	req, err := http.NewRequestWithContext(ctx, method, clean.String(), nil)
	if err != nil {
		return nil, err
	}
	if creds != nil {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := mediaFetcher.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// BootImage reports the bytes the last power-on pulled from a mounted medium —
// the sim's evidence of what the node booted. Nil until a boot happened.
func (s *State) BootImage(mediaID string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mm := s.media[mediaID]; mm != nil {
		return mm.image
	}
	return nil
}

func (s *State) EjectMedia(_ context.Context, mediaID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if s.media[mediaID] == nil {
		return fmt.Errorf("sim: media %q not inserted", mediaID)
	}
	delete(s.media, mediaID)
	return nil
}

func (s *State) Close() error { return nil }

var _ bmc.Client = (*State)(nil)
