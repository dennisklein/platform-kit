package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dialRecorder stands in for the onward dialer: it records every dial and
// connects none, so no test reaches past the relay.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialRecorder) dial(_, address string, _ time.Duration) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrs = append(d.addrs, address)
	return nil, errors.New("onward dial refused under test")
}

func (d *dialRecorder) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.addrs)
}

// testRelay runs a relay on loopback with short timeouts and a recording
// onward dialer.
type testRelay struct {
	*relay
	tunnel, front string
	dials         *dialRecorder
}

func startTestRelay(t testing.TB, rendezvous time.Duration, opts ...Option) *testRelay {
	t.Helper()
	tunnel, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	d := &dialRecorder{}
	r := newRelay(front.Addr().String(), testSecret)
	r.dial = d.dial
	r.handshakeTimeout = 200 * time.Millisecond
	r.rendezvousTimeout = rendezvous
	for _, opt := range opts {
		opt(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	loops := goroutinesIn("relay.acceptLoop(")
	go func() { _ = r.serve(ctx, tunnel, front); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// Wait for both accept loops: a leak check that takes its baseline
	// before they run would count them as leaked.
	deadline := time.Now().Add(5 * time.Second)
	for goroutinesIn("relay.acceptLoop(") < loops+2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return &testRelay{relay: r, tunnel: tunnel.Addr().String(), front: front.Addr().String(), dials: d}
}

func (tr *testRelay) pendingCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.pending)
}

func (tr *testRelay) hasControl() bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.control != nil
}

// register opens a control connection and reads the OK line.
func register(t testing.TB, tunnel string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, br := signedOpen(t, tunnel, testSecret, "REGISTER", "")
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "OK front="), "got %q", line)
	return c, br
}

// FuzzRelayFirstLine sends an arbitrary first line to the tunnel port, after
// the greeting, either as it is or signed with the relay's secret. The relay
// must answer or close within the handshake timeout, act only on a line that
// carries a valid MAC, dial onward only for a signed DIAL, leave no
// rendezvous behind, and stop every goroutine it started for the connection.
func FuzzRelayFirstLine(f *testing.F) {
	for _, seed := range []string{
		"REGISTER\n", "ACCEPT 0011223344556677\n", "ACCEPT\n", "DIAL 10.0.0.21:443\n", "DIAL\n",
		"dial 10.0.0.21:443\n", "DIAL\r\n", "HELLO\n", "\n", "no newline",
		"REGISTER mac=00\n", "DIAL 10.0.0.21:443 mac=" + strings.Repeat("0", 64) + "\n",
		strings.Repeat("A", 300) + "\n", "GET / HTTP/1.1\r\nHost: x\r\n\r\n",
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	tr := startTestRelay(f, time.Second)
	f.Fuzz(func(t *testing.T, line []byte, sign bool) {
		baseline := relayGoroutines()
		before := tr.dials.count()

		c, err := net.Dial("tcp", tr.tunnel)
		require.NoError(t, err)
		br := bufio.NewReader(c)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		greet, err := br.ReadString('\n')
		require.NoError(t, err, "every tunnel connection is greeted")
		nonce := strings.TrimSpace(strings.TrimPrefix(greet, greeting+" "))

		first, rest, hasLine := bytes.Cut(line, []byte("\n"))
		verb, arg, _ := strings.Cut(string(first), " ")
		if sign && hasLine {
			line = append([]byte(strings.TrimSuffix(signedLine(testSecret, nonce, verb, arg), "\n")), '\n')
			line = append(line, rest...)
			first, _, _ = bytes.Cut(line, []byte("\n"))
		}
		authed := sign && hasLine && len(first) < maxLine
		_, _ = c.Write(line)
		// Read whatever the relay answers until it closes the connection or
		// the handshake timeout has clearly passed. A signed REGISTER is the
		// one line that keeps it open, as the control connection.
		var got []byte
		if authed && verb == "REGISTER" {
			got, _ = br.ReadBytes('\n')
			assert.True(t, bytes.HasPrefix(got, []byte("OK front=")), "REGISTER answered %q", got)
		} else {
			got, err = io.ReadAll(br)
			var ne net.Error
			assert.False(t, errors.As(err, &ne) && ne.Timeout(),
				"the relay kept a %q connection open past the handshake timeout", verb)
		}
		_ = c.Close()

		dialed := tr.dials.count() - before
		switch {
		case authed && verb == "DIAL":
			assert.Equal(t, 1, dialed, "a signed DIAL must dial onward once")
			assert.True(t, bytes.HasPrefix(got, []byte("ERR ")), "a refused dial answers ERR, got %q", got)
		case !authed && hasLine && len(first) < maxLine:
			assert.Zero(t, dialed, "first line %q dialed onward without the secret", first)
			assert.Equal(t, "ERR unauthenticated\n", string(got), "first line %q", first)
		default:
			assert.Zero(t, dialed, "first line %q dialed onward", first)
		}
		assert.Zero(t, tr.pendingCount(), "a rendezvous was left behind")
		assert.Eventually(t, func() bool { return !tr.hasControl() }, 2*time.Second, 5*time.Millisecond,
			"a closed control connection must be forgotten")
		// Polled by hand, and counting only goroutines that run relay code:
		// the test runtime, the race detector and the HTTP stack start and
		// stop goroutines of their own, which made a plain count flaky.
		deadline := time.Now().Add(2 * time.Second)
		for relayGoroutines() > baseline && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		assert.LessOrEqual(t, relayGoroutines(), baseline, "goroutines leaked after first line %q", first)
	})
}

// TestFrontWithoutAccept: when the operator never picks a stream up, the
// rendezvous is withdrawn and the front connection closed.
func TestFrontWithoutAccept(t *testing.T) {
	tr := startTestRelay(t, 50*time.Millisecond)
	ctl, br := register(t, tr.tunnel)
	defer ctl.Close()

	front, err := net.Dial("tcp", tr.front)
	require.NoError(t, err)
	defer front.Close()
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "STREAM "))

	_ = front.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := front.Read(make([]byte, 1))
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF, "the front must be closed once the rendezvous times out")
	assert.Zero(t, tr.pendingCount(), "forget must withdraw the id")

	// A late ACCEPT finds nothing and is closed, never spliced.
	late, lateBR := signedOpen(t, tr.tunnel, testSecret, "ACCEPT", strings.TrimSpace(strings.TrimPrefix(line, "STREAM ")))
	defer late.Close()
	_ = late.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = lateBR.ReadByte()
	assert.ErrorIs(t, err, io.EOF)
}

// TestAcceptRacingTimeout: an ACCEPT sent as the rendezvous times out is
// either spliced to the front or closed together with it, exactly once:
// never a front closed while the operator's stream is left open.
func TestAcceptRacingTimeout(t *testing.T) {
	tr := startTestRelay(t, time.Millisecond)
	ctl, br := register(t, tr.tunnel)
	defer ctl.Close()

	var spliced, closed int
	for range 100 {
		front, err := net.Dial("tcp", tr.front)
		require.NoError(t, err)
		line, err := br.ReadString('\n')
		require.NoError(t, err)
		id := strings.TrimSpace(strings.TrimPrefix(line, "STREAM "))

		op, opBR := signedOpen(t, tr.tunnel, testSecret, "ACCEPT", id)
		_, err = io.WriteString(op, "ping")
		require.NoError(t, err)

		// The front sees either the operator's bytes (spliced) or EOF.
		_ = front.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4)
		n, ferr := io.ReadFull(front, buf)
		_ = op.SetReadDeadline(time.Now().Add(5 * time.Second))
		if ferr == nil {
			require.Equal(t, "ping", string(buf[:n]))
			_, err = io.WriteString(front, "pong")
			require.NoError(t, err)
			_, err = io.ReadFull(opBR, buf)
			require.NoError(t, err, "a spliced stream must carry both ways")
			assert.Equal(t, "pong", string(buf))
			spliced++
		} else {
			require.ErrorIs(t, ferr, io.EOF)
			_, err = opBR.Read(buf)
			var ne net.Error
			require.False(t, errors.As(err, &ne) && ne.Timeout(),
				"the front was closed but the accepted stream was left open")
			closed++
		}
		_ = front.Close()
		_ = op.Close()
	}
	assert.Zero(t, tr.pendingCount())
	t.Logf("spliced %d, closed %d", spliced, closed)
}

// TestConcurrentFrontsKeepStreamLinesIntact: fifty fronts at once announce
// fifty whole, distinct STREAM lines on the control connection.
func TestConcurrentFrontsKeepStreamLinesIntact(t *testing.T) {
	tr := startTestRelay(t, 5*time.Second)
	ctl, br := register(t, tr.tunnel)
	defer ctl.Close()

	const n = 50
	var wg sync.WaitGroup
	var dialErrs atomic.Int32
	fronts := make([]net.Conn, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", tr.front)
			if err != nil {
				dialErrs.Add(1)
				return
			}
			fronts[i] = c
		}()
	}
	wg.Wait()
	require.Zero(t, dialErrs.Load())
	defer func() {
		for _, c := range fronts {
			_ = c.Close()
		}
	}()

	form := regexp.MustCompile(`^STREAM [0-9a-f]{16}\n$`)
	seen := map[string]bool{}
	_ = ctl.SetReadDeadline(time.Now().Add(5 * time.Second))
	for range n {
		line, err := br.ReadString('\n')
		require.NoError(t, err)
		assert.Regexp(t, form, line)
		assert.False(t, seen[line], "STREAM line %q twice", line)
		seen[line] = true
	}
}

// TestSecondRegisterClosesFirst: the last registration wins, and the
// control connection it replaces is closed.
func TestSecondRegisterClosesFirst(t *testing.T) {
	tr := startTestRelay(t, time.Second)
	first, _ := register(t, tr.tunnel)
	defer first.Close()
	second, _ := register(t, tr.tunnel)
	defer second.Close()

	_ = first.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := first.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the first control connection must be closed")
	assert.True(t, tr.hasControl())
}

// relayGoroutines counts the goroutines whose stack runs code of the relay
// package, the test's own included (that one is the same before and after).
func relayGoroutines() int {
	return goroutinesIn("metal/internal/relay.")
}

// goroutinesIn counts the goroutines whose stack mentions frame.
func goroutinesIn(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) {
			count++
		}
	}
	return count
}
