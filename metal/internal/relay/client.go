package relay

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Client is the operator side of the tunnel. It IS a net.Listener: every Accept
// is an inbound front connection delivered over the tunnel, so the asset server
// terminates TLS simply by serving a tls.NewListener wrapped around it.
type Client struct {
	tunnelAddr string
	secret     []byte
	frontAddr  string
	control    net.Conn
	streams    chan net.Conn
	closed     chan struct{}
	closeOnce  sync.Once
}

var _ net.Listener = (*Client)(nil)

// Dial connects to a relay's tunnel port and registers as its operator,
// replacing any previous registration. secret is the one the relay was
// started with.
func Dial(ctx context.Context, tunnelAddr string, secret []byte) (*Client, error) {
	d := net.Dialer{Timeout: handshakeTimeout}
	conn, err := d.DialContext(ctx, "tcp", tunnelAddr)
	if err != nil {
		return nil, fmt.Errorf("relay: dial tunnel: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := greet(conn, secret, "REGISTER", ""); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: register: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: register: %w", err)
	}
	if why, refused := strings.CutPrefix(line, "ERR "); refused {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: register refused: %s (this side and the relay hold different secrets)", why)
	}
	front, ok := strings.CutPrefix(line, "OK front=")
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: unexpected register reply %q", line)
	}
	_ = conn.SetDeadline(time.Time{})

	c := &Client{
		tunnelAddr: tunnelAddr,
		secret:     secret,
		frontAddr:  front,
		control:    conn,
		streams:    make(chan net.Conn),
		closed:     make(chan struct{}),
	}
	go c.run()
	return c, nil
}

// FrontAddr is the address BMCs reach the relay's front on — the host a grant
// URL must name.
func (c *Client) FrontAddr() string { return c.frontAddr }

// run watches the control connection for STREAM announcements; its end — relay
// gone, or a later registration displacing this one — closes the client.
func (c *Client) run() {
	defer func() { _ = c.Close() }()
	for {
		line, err := readLine(c.control)
		if err != nil {
			return
		}
		if id, ok := strings.CutPrefix(line, "STREAM "); ok {
			go c.pickup(id)
		}
	}
}

func (c *Client) pickup(id string) {
	conn, err := net.DialTimeout("tcp", c.tunnelAddr, handshakeTimeout)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := greet(conn, c.secret, "ACCEPT", id); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	select {
	case c.streams <- conn:
	case <-c.closed:
		_ = conn.Close()
	}
}

func (c *Client) Accept() (net.Conn, error) {
	select {
	case conn := <-c.streams:
		return conn, nil
	case <-c.closed:
		return nil, net.ErrClosed
	}
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.control.Close()
	})
	return nil
}

func (c *Client) Addr() net.Addr { return frontTCPAddr(c.frontAddr) }

// frontTCPAddr names the front as an address without resolving it — it may only
// be meaningful from the BMC network.
type frontTCPAddr string

func (a frontTCPAddr) Network() string { return "tcp" }
func (a frontTCPAddr) String() string  { return string(a) }

// DialOnward opens a TCP connection to hostport as dialed FROM the relay — the
// operator's outbound Redfish path into the BMC network.
func (c *Client) DialOnward(ctx context.Context, hostport string) (net.Conn, error) {
	d := net.Dialer{Timeout: handshakeTimeout}
	conn, err := d.DialContext(ctx, "tcp", c.tunnelAddr)
	if err != nil {
		return nil, fmt.Errorf("relay: dial tunnel: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := greet(conn, c.secret, "DIAL", hostport); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: dial onward: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: dial onward: %w", err)
	}
	if line != "OK" {
		_ = conn.Close()
		return nil, fmt.Errorf("relay: dial onward %s: %s", hostport, strings.TrimPrefix(line, "ERR "))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
