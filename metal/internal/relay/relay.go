// Package relay is the dumb L4 reverse tunnel between the operator and the BMC
// network: it holds nothing and terminates nothing. The operator dials OUT to
// the tunnel port and registers;
// inbound BMC fetches on the front port are handed back over the tunnel, and the
// operator's outbound Redfish dials are passed onward. TLS terminates at the
// asset server, never here — the relay splices ciphertext, which is what makes it
// acceptable to run on a box we don't own.
//
// Bindings are just addresses: loopback (the sim, every CI run) or an org Linux
// box at day-0 (front on the BMC-reachable interface, tunnel kept on 127.0.0.1
// and reached over `ssh -L`, so the onward-dial path is never an open proxy into
// the BMC island). The relay serves day-0 only: pk-metal boot uses it, the
// operator does not.
//
// The protocol is three line-prefixed TCP flavors on the tunnel port, each
// opened by the relay's greeting and authenticated by the shared secret (see
// auth.go):
//
//	REGISTER          -> OK front=<addr>    the control connection (last wins)
//	ACCEPT <id>       picks up the front stream announced as "STREAM <id>"
//	DIAL <host:port>  -> OK | ERR <why>     the operator's Redfish path onward,
//	                                        to targets in the allow list only
//
// Everything after a connection's first line is spliced payload. Without the
// secret, a connection to the tunnel port can neither take the control slot
// (and with it the BMCs' media fetches, whose certificate the BMCs do not
// check) nor dial onward into the BMC network.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	handshakeTimeout  = 5 * time.Second
	rendezvousTimeout = 5 * time.Second
	maxLine           = 256
)

// Serve runs the relay over the two listeners until ctx is cancelled or a
// listener dies. advertise overrides the front address reported to the
// registering operator (the address as BMCs reach it); empty means front.Addr().
// secret is the shared secret every tunnel connection must prove it holds
// (ParseSecret, LoadSecret); the relay refuses to run without one. Without
// WithAllow, a DIAL may name any target.
func Serve(ctx context.Context, tunnel, front net.Listener, advertise string, secret []byte, opts ...Option) error {
	if len(secret) < minSecret {
		return fmt.Errorf("relay: a secret of at least %d characters is required", minSecret)
	}
	if advertise == "" {
		advertise = front.Addr().String()
	}
	r := newRelay(advertise, secret)
	for _, opt := range opts {
		opt(r)
	}
	return r.serve(ctx, tunnel, front)
}

func newRelay(advertise string, secret []byte) *relay {
	return &relay{
		advertise:         advertise,
		secret:            secret,
		pending:           map[string]chan net.Conn{},
		dial:              net.DialTimeout,
		lookup:            lookupHost,
		handshakeTimeout:  handshakeTimeout,
		rendezvousTimeout: rendezvousTimeout,
	}
}

func (r *relay) serve(ctx context.Context, tunnel, front net.Listener) error {
	stop := context.AfterFunc(ctx, func() {
		_ = tunnel.Close()
		_ = front.Close()
	})
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- acceptLoop(tunnel, r.handleTunnel) }()
	go func() { errc <- acceptLoop(front, r.handleFront) }()
	err := <-errc
	_ = tunnel.Close()
	_ = front.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type relay struct {
	advertise string
	secret    []byte
	// allow limits the onward dials; nil allows every target.
	allow Allow
	// dial, lookup and the two timeouts are fields so that tests can record onward
	// dials and wait milliseconds, not seconds.
	dial              func(network, address string, timeout time.Duration) (net.Conn, error)
	lookup            resolver
	handshakeTimeout  time.Duration
	rendezvousTimeout time.Duration

	mu      sync.Mutex
	control net.Conn
	pending map[string]chan net.Conn // rendezvous id -> the waiting front handler
}

func acceptLoop(ln net.Listener, handle func(net.Conn)) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go handle(c)
	}
}

func (r *relay) handleTunnel(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(r.handshakeTimeout))
	nonce, err := newNonce()
	if err != nil {
		_ = c.Close()
		return
	}
	if _, err := io.WriteString(c, greeting+" "+nonce+"\n"); err != nil {
		_ = c.Close()
		return
	}
	line, err := readLine(c)
	if err != nil {
		_ = c.Close()
		return
	}
	verb, arg, err := verifyLine(r.secret, nonce, line)
	if err != nil {
		// Nothing was registered, accepted or dialled for this connection.
		_, _ = fmt.Fprintf(c, "ERR %v\n", err)
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	switch verb {
	case "REGISTER":
		r.register(c)
	case "ACCEPT":
		r.accept(c, arg)
	case "DIAL":
		r.dialOnward(c, arg)
	default:
		_ = c.Close()
	}
}

func (r *relay) register(c net.Conn) {
	// Take the control slot before answering OK, so registrations win in the
	// order their OKs were sent. Answering first let an earlier operator,
	// whose goroutine ran late, displace a later one that already had its OK.
	r.mu.Lock()
	if r.control != nil {
		_ = r.control.Close() // last registration wins — a restarted operator must not strand the relay
	}
	r.control = c
	r.mu.Unlock()
	if _, err := fmt.Fprintf(c, "OK front=%s\n", r.advertise); err != nil {
		r.mu.Lock()
		if r.control == c {
			r.control = nil
		}
		r.mu.Unlock()
		_ = c.Close()
		return
	}

	// The operator sends nothing further on the control connection; this read
	// exists purely to notice it vanishing.
	_, _ = io.Copy(io.Discard, c)
	r.mu.Lock()
	if r.control == c {
		r.control = nil
	}
	r.mu.Unlock()
	_ = c.Close()
}

// accept delivers an operator dial-back to the front handler waiting under the
// id. Lookup, single-use delete, and buffered send happen under one lock, so an
// id is honoured exactly once and never after the handler timed out.
func (r *relay) accept(c net.Conn, id string) {
	r.mu.Lock()
	ch := r.pending[id]
	if ch != nil {
		delete(r.pending, id)
		ch <- c // buffered(1); the sole send for this id
	}
	r.mu.Unlock()
	if ch == nil {
		_ = c.Close()
	}
}

func (r *relay) dialOnward(c net.Conn, hostport string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.handshakeTimeout)
	address, err := r.allow.target(ctx, r.lookup, hostport)
	cancel()
	if err != nil {
		// Refused before any dial: the allow list is what keeps the relay
		// from being a general TCP pivot for a holder of the secret.
		_, _ = fmt.Fprintf(c, "ERR %v\n", err)
		_ = c.Close()
		return
	}
	target, err := r.dial("tcp", address, r.handshakeTimeout)
	if err != nil {
		_, _ = fmt.Fprintf(c, "ERR %v\n", err)
		_ = c.Close()
		return
	}
	if _, err := io.WriteString(c, "OK\n"); err != nil {
		_ = c.Close()
		_ = target.Close()
		return
	}
	splice(c, target)
}

func (r *relay) handleFront(c net.Conn) {
	r.mu.Lock()
	control := r.control
	r.mu.Unlock()
	if control == nil {
		_ = c.Close() // no operator registered — the BMC retries on its own cadence
		return
	}

	id, err := newID()
	if err != nil {
		_ = c.Close()
		return
	}
	ch := make(chan net.Conn, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()

	if _, err := fmt.Fprintf(control, "STREAM %s\n", id); err != nil {
		r.forget(id)
		_ = control.Close() // a dead control write means the operator vanished mid-registration
		_ = c.Close()
		return
	}

	select {
	case op := <-ch:
		splice(c, op)
	case <-time.After(r.rendezvousTimeout):
		if r.forget(id) {
			_ = c.Close()
			return
		}
		// An ACCEPT won the race under the lock — its conn is already in ch.
		splice(c, <-ch)
	}
}

// forget withdraws a rendezvous id; false means an ACCEPT already consumed it.
func (r *relay) forget(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[id]; !ok {
		return false
	}
	delete(r.pending, id)
	return true
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// splice joins two streams — the relay's entire data plane. Each direction
// half-closes as it drains, so an HTTP exchange ends cleanly from either side.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyHalf(a, b) }()
	go func() { defer wg.Done(); copyHalf(b, a) }()
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

func copyHalf(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = dst.Close()
	}
}

// readLine reads a protocol line byte-by-byte — anything after the newline is
// spliced payload and must not be buffered away from it.
func readLine(c net.Conn) (string, error) {
	var line []byte
	var buf [1]byte
	for len(line) < maxLine {
		if _, err := c.Read(buf[:]); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			return string(line), nil
		}
		line = append(line, buf[0])
	}
	return "", fmt.Errorf("relay: protocol line exceeds %d bytes", maxLine)
}
