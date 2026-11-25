package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What OVMF wrote on the console of the VM that hung: screen clears and cursor
// moves, nothing to read.
const hungScreen = "\x1b[2J\x1b[01;01H\x1b[=3h\x1b[2J\x1b[01;01H\x1b[0m\x1b[35m\x1b[40m"

func TestScreenFilter(t *testing.T) {
	for name, tc := range map[string]struct {
		chunks []string
		want   []bool
	}{
		"screen clears only":        {[]string{hungScreen, "\r\n\r\n"}, []bool{false, false}},
		"the boot manager speaks":   {[]string{hungScreen, "BdsDxe: loading Boot0001"}, []bool{false, true}},
		"a kernel line":             {[]string{"[    0.000000] Linux version 6.12"}, []bool{true}},
		"a sequence split in two":   {[]string{"\x1b[", "2J", "\x1b[0;", "1H"}, []bool{false, false, false, false}},
		"a window title (OSC, BEL)": {[]string{"\x1b]0;OVMF\x07"}, []bool{false}},
		"an OSC ended by ST":        {[]string{"\x1b]0;OVMF\x1b\\"}, []bool{false}},
		"a charset switch":          {[]string{"\x1b(B\x1b)0"}, []bool{false}},
		"text after a clear":        {[]string{"\x1b[2Jx"}, []bool{true}},
		"a UTF-8 character":         {[]string{"\x1b[2J\xe2\x94\x80"}, []bool{true}},
	} {
		var f screenFilter
		for i, c := range tc.chunks {
			assert.Equal(t, tc.want[i], f.progress([]byte(c)), "%s: chunk %d %q", name, i, c)
		}
	}
}

// fakeClock is a clock the test moves by hand.
type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// fakeConsole hands the guard what the test writes, as a console capture
// would grow.
type fakeConsole struct {
	mu      sync.Mutex
	pending []byte
	err     error
}

func (c *fakeConsole) write(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, s...)
}

func (c *fakeConsole) read() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.pending
	c.pending = nil
	return out, c.err
}

// logRecorder collects log lines.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// guardRun runs one guard against a fake console and clock; the test drives
// it tick by tick. A send on tick returns once the guard took the tick, and
// the guard reads the console and the clock right after, so the test writes
// both before it ticks.
type guardRun struct {
	clock   *fakeClock
	console *fakeConsole
	log     *logRecorder
	tick    chan time.Time
	cycles  atomic.Int32
	done    chan bool
	exited  chan struct{}
	cancel  context.CancelFunc
}

func startGuard(t *testing.T, timeout time.Duration) *guardRun {
	t.Helper()
	r := &guardRun{clock: &fakeClock{}, console: &fakeConsole{}, log: &logRecorder{},
		tick: make(chan time.Time), done: make(chan bool, 1), exited: make(chan struct{})}
	r.clock.advance(time.Hour) // any start will do
	g := &hangGuard{timeout: timeout, start: r.clock.now(), read: r.console.read, logf: r.log.logf, name: "node-1",
		cycle: func() error { r.cycles.Add(1); return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	go func() {
		defer close(r.exited)
		r.done <- g.watch(ctx, r.tick)
	}()
	return r
}

// step writes to the console, moves the clock and ticks. The guard reads
// the console after it took the previous tick, so it can see this write
// early and finish; the tick is then not taken.
func (r *guardRun) step(console string, d time.Duration) {
	r.console.write(console)
	r.clock.advance(d)
	select {
	case r.tick <- r.clock.now():
	case <-r.exited:
	}
}

func (r *guardRun) result(t *testing.T) bool {
	t.Helper()
	select {
	case cycled := <-r.done:
		return cycled
	case <-time.After(5 * time.Second):
		t.Fatal("the guard did not finish")
		return false
	}
}

func TestHangGuardCyclesOnceAfterTheTimeout(t *testing.T) {
	r := startGuard(t, 90*time.Second)
	r.step(hungScreen, 30*time.Second)
	r.step("\x1b[2J", 30*time.Second)
	assert.Zero(t, r.cycles.Load(), "60 s of screen clears is not yet a hang")
	r.step("", 30*time.Second)
	assert.True(t, r.result(t), "90 s without readable output is a hang")
	assert.Equal(t, int32(1), r.cycles.Load())
	assert.Contains(t, r.log.all(), "FIRMWARE HANG")
	assert.Contains(t, r.log.all(), "node-1")
	assert.Contains(t, r.log.all(), "1m30s")
}

func TestHangGuardLeavesABootingMachineAlone(t *testing.T) {
	r := startGuard(t, 90*time.Second)
	r.step(hungScreen, 30*time.Second)
	r.step("BdsDxe: loading Boot0002 \"UEFI QEMU DVD-ROM\"\r\n", 30*time.Second)
	r.step("Boot in 5s.\r\n", 60*time.Second)
	assert.Zero(t, r.cycles.Load(), "readable output restarts the wait")
	r.step("\x1b[2J[    0.000000] Linu", 60*time.Second)
	r.step("x version 6.12.40-talos", 60*time.Second)
	assert.False(t, r.result(t), "the kernel banner ends the watch, even split across reads")
	assert.Zero(t, r.cycles.Load())
}

func TestHangGuardCyclesALoaderThatStartsNoKernel(t *testing.T) {
	r := startGuard(t, 90*time.Second)
	r.step("BdsDxe: starting Boot0003 \"UEFI QEMU DVD-ROM QM00003 \"\r\n", 10*time.Second)
	r.step("    Reboot Into Firmware Interface      Boot in 5s.", 5*time.Second)
	r.step("\x1b[2J", 60*time.Second)
	assert.Zero(t, r.cycles.Load(), "60 s after the boot manager is not yet a hang")
	r.step("", 30*time.Second)
	assert.True(t, r.result(t), "90 s without a kernel after the boot manager is a hang")
	assert.Equal(t, int32(1), r.cycles.Load())
	assert.Contains(t, r.log.all(), "LOADER HANG")
}

func TestHangGuardLateOutputStillCounts(t *testing.T) {
	// Output that arrives in the same read as the deadline is progress: the
	// machine is not cycled in the middle of its boot.
	r := startGuard(t, 90*time.Second)
	r.step("\x1b[2J", 60*time.Second)
	r.step("[    0.000000] Linux version 6.12.40-talos", 60*time.Second)
	assert.False(t, r.result(t))
	assert.Zero(t, r.cycles.Load())
}

func TestHangGuardStopsWithItsPowerOn(t *testing.T) {
	r := startGuard(t, 90*time.Second)
	r.step(hungScreen, 30*time.Second)
	r.cancel() // the BMC powered the machine off or reset it
	assert.False(t, r.result(t))
	assert.Zero(t, r.cycles.Load())
}

func TestHangGuardGivesUpOnAnUnreadableConsole(t *testing.T) {
	r := startGuard(t, 90*time.Second)
	r.console.err = errors.New("permission denied")
	r.step("", 120*time.Second)
	assert.False(t, r.result(t), "no evidence of a hang is no hang")
	assert.Zero(t, r.cycles.Load())
	assert.Contains(t, r.log.all(), "cannot read the console")
}

// newGuardedMachine is a Machine whose guard runs on the test's clock and
// ticks, and whose power cycle only counts; no qemu is started.
func newGuardedMachine(t *testing.T, timeout time.Duration) (*Machine, *fakeClock, chan time.Time, chan struct{}) {
	t.Helper()
	log := &logRecorder{}
	m := New(Options{Name: "node-1", Dir: t.TempDir(), HangTimeout: timeout, Logf: log.logf})
	clock, tick, cycled := &fakeClock{}, make(chan time.Time), make(chan struct{}, 4)
	m.now = clock.now
	m.ticker = func() (<-chan time.Time, func()) { return tick, func() {} }
	m.powerCycle = func(context.Context) error {
		// What Start does after the cycle: it arms a guard, which watches
		// the guard's own cycle until the power-on has had its tries.
		m.armGuard(0)
		cycled <- struct{}{}
		return nil
	}
	t.Cleanup(func() {
		m.mu.Lock()
		m.disarmLocked()
		m.mu.Unlock()
	})
	return m, clock, tick, cycled
}

func waitCycle(t *testing.T, cycled chan struct{}) {
	t.Helper()
	select {
	case <-cycled:
	case <-time.After(5 * time.Second):
		t.Fatal("no power cycle")
	}
}

func TestMachineGuardTriesPerPowerOn(t *testing.T) {
	m, clock, tick, cycled := newGuardedMachine(t, 90*time.Second)
	require.NoError(t, os.WriteFile(m.ConsoleLog(), []byte(hungScreen), 0o600))
	var retried bootIntent
	cycle := m.powerCycle
	m.powerCycle = func(ctx context.Context) error {
		m.mu.Lock()
		retried = bootIntent{m.source, m.once}
		m.mu.Unlock()
		return cycle(ctx)
	}

	// A power-on with a one-time CD boot, which the boot then used up.
	m.mu.Lock()
	m.intent = bootIntent{"Cd", true}
	m.mu.Unlock()
	m.armGuard(0)
	clock.advance(91 * time.Second)
	tick <- clock.now()
	waitCycle(t, cycled)
	m.mu.Lock()
	assert.Equal(t, bootIntent{"Cd", true}, retried, "the retry boots what the hung power-on was asked to")
	m.mu.Unlock()
	assert.Equal(t, 1, m.Status(context.Background()).HangCycles, "the state endpoint shows the cycle")

	// The boot after the guard's cycle hangs too: it is watched and cycled
	// again, until the power-on has had its tries.
	notCycling := func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.cycling
	}
	for try := 2; try <= hangTries; try++ {
		require.Eventually(t, notCycling, 5*time.Second, time.Millisecond)
		clock.advance(91 * time.Second)
		tick <- clock.now()
		waitCycle(t, cycled)
		assert.Equal(t, try, m.Status(context.Background()).HangCycles)
	}
	require.Eventually(t, notCycling, 5*time.Second, time.Millisecond)
	m.mu.Lock()
	assert.Nil(t, m.guardCancel, "after its last try the guard leaves the VM for a person")
	m.mu.Unlock()

	// The next power-on by the BMC is watched again, with tries of its own.
	m.armGuard(0)
	clock.advance(91 * time.Second)
	tick <- clock.now()
	waitCycle(t, cycled)
	assert.Equal(t, hangTries+1, m.Status(context.Background()).HangCycles)
}

func TestMachineGuardReadsOnlyItsPowerOn(t *testing.T) {
	m, clock, tick, cycled := newGuardedMachine(t, 90*time.Second)
	// An in-place reset: what the console held before belongs to the boot
	// before, and readable text there says nothing about this one.
	before := "[  OK  ] Reached target multi-user.target\r\n"
	require.NoError(t, os.WriteFile(m.ConsoleLog(), []byte(before+hungScreen), 0o600))

	m.armGuard(int64(len(before)))
	clock.advance(91 * time.Second)
	tick <- clock.now()
	waitCycle(t, cycled)
	assert.Equal(t, 1, m.Status(context.Background()).HangCycles)
}

func TestMachineGuardOffAndStale(t *testing.T) {
	m, _, _, _ := newGuardedMachine(t, -1)
	m.armGuard(0)
	m.mu.Lock()
	assert.Nil(t, m.guardCancel, "a negative timeout turns the guard off")
	m.mu.Unlock()

	// A guard whose power-on was replaced (by a stop, a reset, a new
	// power-on) does not cycle the machine, even if it decided to.
	m, _, _, cycled := newGuardedMachine(t, 90*time.Second)
	m.armGuard(0)
	m.mu.Lock()
	stale := m.guardGen
	m.mu.Unlock()
	require.NoError(t, m.Stop(context.Background()))
	require.NoError(t, m.hangCycle(stale, bootIntent{}))
	assert.Empty(t, cycled)
	assert.Zero(t, m.Status(context.Background()).HangCycles)
}

func TestConsoleTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	c := &consoleTail{path: path}
	got, err := c.read()
	require.NoError(t, err)
	assert.Empty(t, got, "no capture yet")

	require.NoError(t, os.WriteFile(path, []byte("abc"), 0o600))
	got, err = c.read()
	require.NoError(t, err)
	assert.Equal(t, "abc", string(got))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("def")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got, err = c.read()
	require.NoError(t, err)
	assert.Equal(t, "def", string(got), "only what is new")

	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	got, err = c.read()
	require.NoError(t, err)
	assert.Equal(t, "x", string(got), "a new, shorter capture is read from its start")
}
