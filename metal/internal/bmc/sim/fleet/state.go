package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/machine/docker"
	"github.com/dennisklein/platform-kit/metal/internal/machine/qemu"
)

// The state endpoint is the fleet seen from outside Redfish: what each BMC
// holds and what the machine behind it does, in one JSON document. A
// dashboard reads it instead of logging in to every BMC and asking the QEMU
// processes. It is read-only, served over plain HTTP on its own address
// (loopback unless the caller says otherwise) and carries no secrets: no
// passwords, no userinfo in media URLs, no process ids.

// StatePath is where the state endpoint answers.
const StatePath = "/state"

// DefaultRequestHistory is how many Redfish requests each machine remembers
// for the state endpoint when Options.RequestHistory is zero.
const DefaultRequestHistory = 20

// State is the document the state endpoint serves (version 1). New keys may
// be added; existing keys keep their meaning while version is 1.
type State struct {
	Version  int            `json:"version"`
	MediaURL string         `json:"mediaURL"`
	Machines []MachineState `json:"machines"`
}

// MachineState is one BMC and the machine behind it.
type MachineState struct {
	Name        string `json:"name"`
	Personality string `json:"personality"`
	// BMC is the Redfish endpoint, https://host:port.
	BMC string `json:"bmc"`
	// Serial and BMCMAC are the identity facts a cluster object carries too,
	// so that a dashboard can join the two views.
	Serial string `json:"serial"`
	BMCMAC string `json:"bmcMAC"`
	// Power, BootProgress and BootOverride are those of the machine's first
	// system, the one a backend is attached to.
	Power        string                   `json:"power"`
	BootProgress string                   `json:"bootProgress"`
	BootOverride sim.BootOverrideSnapshot `json:"bootOverride"`
	Media        []sim.MediaSnapshot      `json:"media"`
	// Requests are the last Redfish requests the BMC answered, oldest first.
	Requests []Request   `json:"requests"`
	Backend  BackendInfo `json:"backend"`
	// ConsoleLog is the path of the VM's serial console capture; empty for a
	// backend without one.
	ConsoleLog string `json:"consoleLog"`
}

// Request is one answered Redfish request. The path carries no query: a
// query is where a client could put a token.
type Request struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
}

// BackendInfo is the machine behind the BMC, as far as an observer needs it.
type BackendInfo struct {
	Kind         string `json:"kind"` // none | qemu | docker
	Running      bool   `json:"running"`
	BootProgress string `json:"bootProgress"`
	// HangCycles counts the power cycles the firmware-hang guard did: a VM
	// that stayed in its firmware after a power-on (qemu only).
	HangCycles int `json:"hangCycles"`
}

// requestLog keeps the last answered requests of one BMC.
type requestLog struct {
	mu   sync.Mutex
	size int
	reqs []Request
}

func newRequestLog(size int) *requestLog {
	if size <= 0 {
		size = DefaultRequestHistory
	}
	return &requestLog{size: size}
}

func (l *requestLog) add(r Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, r)
	if len(l.reqs) > l.size {
		l.reqs = append(l.reqs[:0], l.reqs[len(l.reqs)-l.size:]...)
	}
}

func (l *requestLog) list() []Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Request{}, l.reqs...)
}

// wrap records every request the handler answers.
func (l *requestLog) wrap(next http.Handler, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		l.add(Request{Time: now(), Method: r.Method, Path: r.URL.Path, Status: rec.code})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// member is what the state endpoint reads of one machine.
type member struct {
	ep      Endpoint
	serial  string
	bmcMAC  string
	backend sim.Machine // nil for the none backend
	log     *requestLog
}

// State reads the whole fleet. Each machine's backend is asked in turn; a
// probe that fails leaves its part of the answer at the zero value.
func (f *Fleet) State(ctx context.Context) State {
	st := State{Version: 1, MediaURL: f.mediaURL, Machines: []MachineState{}}
	for _, m := range f.members {
		snap := m.ep.State.Snapshot(ctx)
		ms := MachineState{
			Name: m.ep.Name, Personality: m.ep.Personality, BMC: m.ep.URL,
			Serial: m.serial, BMCMAC: m.bmcMAC,
			Media:    snap.Media,
			Requests: m.log.list(),
			Backend:  BackendInfo{Kind: m.ep.Backend},
		}
		if len(snap.Systems) > 0 {
			sys := snap.Systems[0]
			ms.Power, ms.BootProgress, ms.BootOverride = sys.Power, sys.BootProgress, sys.BootOverride
		}
		switch b := m.backend.(type) {
		case *qemu.Machine:
			s := b.Status(ctx)
			ms.Backend.Running, ms.Backend.BootProgress = s.Running, s.BootProgress
			ms.Backend.HangCycles = s.HangCycles
			ms.ConsoleLog = s.ConsoleLog
		case *docker.Machine:
			ms.Backend.Running, _ = b.Running(ctx)
		case nil:
			// The sim's own power map is the whole machine.
			ms.Backend.Running = ms.Power == "On"
		}
		st.Machines = append(st.Machines, ms)
	}
	return st
}

// StateHandler serves State as JSON at StatePath, for GET and HEAD only.
func (f *Fleet) StateHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(StatePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the state endpoint is read-only", http.StatusMethodNotAllowed)
			return
		}
		body, err := json.MarshalIndent(f.State(r.Context()), "", "  ")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append(body, '\n'))
	})
	return mux
}
