package qemu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// The firmware-hang guard. OVMF has been seen to stop right after a correct
// power-on: the console showed nothing but the firmware clearing the screen,
// no boot option was tried, and the machine sat there until someone reset it.
// A boot has also been seen to stop one step later: the firmware started the
// installer's boot manager, its countdown ran out, the screen cleared, and no
// kernel ever started. Real hardware does both, rarely, and an admin
// power-cycles it. The guard does the same: when, for HangTimeout, the
// console gains nothing readable and the kernel banner has not shown, the VM
// is power-cycled.
//
// It never touches a VM whose kernel started: the banner disarms it for the
// rest of that power-on, so it cannot reset a running OS. It watches only
// the BMC's power-ons; a reboot the guest does on its own is not watched. Its
// own power cycle is watched again, up to hangTries cycles per power-on, then
// the VM is left for a person to look at. One try was not enough: on a CI
// runner that starts five VMs at once, three hung, and one of them hung again
// after its cycle, failing the round after 30 minutes for want of a second
// cycle.

// hangTries is how many times the guard power-cycles one power-on.
const hangTries = 3

// DefaultHangTimeout is how long a freshly powered-on VM may go without
// readable console output, before its kernel starts, until the guard
// power-cycles it.
const DefaultHangTimeout = 90 * time.Second

// hangCheckEvery is how often the guard reads the console.
const hangCheckEvery = 2 * time.Second

// screenFilter reads console bytes and tells whether anything readable came
// through: it skips ANSI escape sequences (CSI, OSC, the two- and
// three-byte forms), control characters and blanks. It keeps its state
// between calls, as a sequence may be split across two reads.
type screenFilter struct {
	state filterState
}

type filterState int

const (
	plain   filterState = iota
	escape              // after ESC
	csi                 // in ESC [ … final byte
	osc                 // in ESC ] … BEL or ESC \
	oscEsc              // ESC inside an OSC: the start of its ST
	charset             // after ESC ( and the like: one more byte follows
)

// progress reports whether data holds a readable character.
func (f *screenFilter) progress(data []byte) bool {
	for _, b := range data {
		switch f.state {
		case plain:
			switch {
			case b == 0x1b:
				f.state = escape
			case b <= ' ' || b == 0x7f:
				// Control characters and blanks draw nothing.
			default:
				return true
			}
		case escape:
			switch b {
			case '[':
				f.state = csi
			case ']':
				f.state = osc
			case '(', ')', '*', '+', '#', '%':
				f.state = charset
			default:
				f.state = plain
			}
		case csi:
			// Parameter and intermediate bytes are 0x20–0x3f; anything
			// else ends the sequence.
			if b < 0x20 || b > 0x3f {
				f.state = plain
			}
		case osc:
			switch b {
			case 0x07:
				f.state = plain
			case 0x1b:
				f.state = oscEsc
			}
		case oscEsc:
			f.state = plain
		case charset:
			f.state = plain
		}
	}
	return false
}

// consoleTail reads what a console capture gained since the last read.
type consoleTail struct {
	path   string
	offset int64
}

func (c *consoleTail) read() ([]byte, error) {
	f, err := os.Open(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // nothing written yet
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < c.offset {
		c.offset = 0 // a new capture replaced the one being followed
	}
	buf := make([]byte, min(st.Size()-c.offset, consoleChunk))
	n, err := f.ReadAt(buf, c.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	c.offset += int64(n)
	return buf[:n], nil
}

// hangGuard watches one power-on.
type hangGuard struct {
	timeout time.Duration
	start   time.Time              // the power-on
	read    func() ([]byte, error) // console bytes written since the last call
	cycle   func() error
	logf    func(format string, args ...any)
	name    string
	try     int // which of the power-on's hangTries this cycle would be
}

// watch reads the console at every tick (a tick carries the time) until the
// kernel banner shows, ctx ends, or the console stays without progress for
// the timeout; then it power-cycles once. It reports whether it did. Two
// hangs are told apart: the firmware's (nothing readable since the power-on)
// and the loader's (the firmware or the boot manager wrote, then nothing for
// the timeout and no kernel started). A console it cannot read is no
// evidence of a hang: the guard then gives up rather than reset a machine it
// cannot see.
func (g *hangGuard) watch(ctx context.Context, tick <-chan time.Time) bool {
	var now time.Time
	var f screenFilter
	var k bannerScan
	last, loader := g.start, false
	for {
		select {
		case <-ctx.Done():
			return false
		case now = <-tick:
		}
		data, err := g.read()
		if err != nil {
			g.logf("qemu %s: firmware-hang guard: cannot read the console, not watching this boot: %v", g.name, err)
			return false
		}
		if k.found(data) {
			return false
		}
		if f.progress(data) {
			last, loader = now, true
			continue
		}
		if waited := now.Sub(last); waited >= g.timeout {
			// A power action of the BMC may have replaced this power-on
			// while the console was read.
			if ctx.Err() != nil {
				return false
			}
			if loader {
				g.logf("qemu %s: LOADER HANG: the boot manager ran but no kernel started, nothing new on the console for %s; "+
					"power-cycling the VM (try %d of %d)", g.name, waited.Round(time.Second), g.try, hangTries)
			} else {
				g.logf("qemu %s: FIRMWARE HANG: no boot output on the console %s after power-on "+
					"(only screen control sequences); power-cycling the VM (try %d of %d)", g.name, waited.Round(time.Second),
					g.try, hangTries)
			}
			if err := g.cycle(); err != nil {
				g.logf("qemu %s: firmware-hang guard: the power cycle failed: %v", g.name, err)
			}
			return true
		}
	}
}

// kernelBanner is the kernel's first console line; once it shows, the boot
// is the operating system's and the guard stands down.
var kernelBanner = []byte("Linux version ")

// bannerScan looks for the kernel banner across reads: it keeps the tail of
// the last read, as the banner may be split between two.
type bannerScan struct{ tail []byte }

func (b *bannerScan) found(data []byte) bool {
	buf := append(b.tail, data...)
	if bytes.Contains(buf, kernelBanner) {
		return true
	}
	if keep := len(kernelBanner) - 1; len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	b.tail = bytes.Clone(buf)
	return false
}

// armGuard starts watching the power-on that just happened; offset is where
// its console output begins. A guard already running for an earlier
// power-on is replaced. The guard's own power cycle is watched too, until
// that power-on has had hangTries of them.
func (m *Machine) armGuard(offset int64) {
	timeout := m.o.HangTimeout
	if timeout == 0 {
		timeout = DefaultHangTimeout
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disarmLocked()
	if timeout < 0 {
		return
	}
	if !m.cycling {
		m.guardTries = 0 // a power-on of the BMC's own: a fresh count
	} else if m.guardTries >= hangTries {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	gen := m.guardGen
	m.guardCancel = cancel
	tick, stop := m.ticker()
	tail := &consoleTail{path: m.ConsoleLog(), offset: offset}
	intent := m.intent
	g := &hangGuard{
		timeout: timeout, start: m.now(), read: tail.read, logf: m.logf, name: m.o.Name, try: m.guardTries + 1,
		cycle: func() error { return m.hangCycle(gen, intent) },
	}
	go func() {
		defer stop()
		g.watch(ctx, tick)
	}()
}

// disarmLocked stops the running guard, if any, and makes sure a power cycle
// it already decided on is skipped. Caller holds mu.
func (m *Machine) disarmLocked() {
	m.guardGen++
	if m.guardCancel != nil {
		m.guardCancel()
		m.guardCancel = nil
	}
}

// hangCycle is the guard's power cycle: off, then on, as a BMC's power cycle
// does. The new boot takes the boot source the hung one was given: a
// one-time override was used up by the boot that hung, but that boot never
// got to use it, so the retry does what the power-on asked for. It is
// skipped when another power action replaced the guard's power-on in the
// meantime.
func (m *Machine) hangCycle(gen int, intent bootIntent) error {
	m.mu.Lock()
	if gen != m.guardGen || m.cycling {
		m.mu.Unlock()
		return nil
	}
	m.cycling = true
	m.hangCycles++
	m.guardTries++
	m.source, m.once = intent.source, intent.once
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.cycling = false
		m.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return m.powerCycle(ctx)
}

// coldCycle stops and starts the VM.
func (m *Machine) coldCycle(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
}

// realTicker ticks every hangCheckEvery.
func realTicker() (<-chan time.Time, func()) {
	t := time.NewTicker(hangCheckEvery)
	return t.C, t.Stop
}

// bootIntent is the boot source a power-on was given.
type bootIntent struct {
	source string
	once   bool
}
