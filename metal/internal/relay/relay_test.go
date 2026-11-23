package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRelay wires a relay over loopback listeners and returns the tunnel
// address plus the (possibly wrapped) front listener's address.
func startRelay(t *testing.T, wrapFront func(net.Listener) net.Listener) (tunnelAddr, frontAddr string) {
	t.Helper()
	tunnel, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	if wrapFront != nil {
		front = wrapFront(front)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, tunnel, front, "") }()
	return tunnel.Addr().String(), front.Addr().String()
}

func TestReverseStream(t *testing.T) {
	tunnelAddr, frontAddr := startRelay(t, nil)

	client, err := Dial(context.Background(), tunnelAddr)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	assert.Equal(t, frontAddr, client.FrontAddr(), "registration must report the front address")

	// A plain HTTP server over the reverse listener — the rendezvous itself.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pong")
	})}
	go func() { _ = srv.Serve(client) }()
	defer func() { _ = srv.Close() }()

	resp, err := http.Get("http://" + frontAddr + "/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, "pong", string(body))
}

// TestFrontSeesOnlyTLS is the relay-sees-only-TLS gate: every byte crossing the
// front is captured, and the plaintext the asset server serves must never
// appear in it — the splice carries ciphertext only.
func TestFrontSeesOnlyTLS(t *testing.T) {
	const canary = "pk-relay-plaintext-canary"
	var reads, writes captureBuf

	tunnelAddr, frontAddr := startRelay(t, func(ln net.Listener) net.Listener {
		return &teeListener{Listener: ln, reads: &reads, writes: &writes}
	})

	client, err := Dial(context.Background(), tunnelAddr)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	// TLS terminates at the operator-side server, exactly like the asset server.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, canary)
	}))
	_ = srv.Listener.Close()
	srv.Listener = client
	srv.StartTLS()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/grants/token/x.iso")
	require.NoError(t, err)
	require.Equal(t, "https://"+frontAddr, srv.URL, "the fetch must have traversed the front")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, canary, string(body), "the payload arrives intact through the tunnel")

	require.NotEmpty(t, reads.Bytes())
	require.NotEmpty(t, writes.Bytes())
	assert.EqualValues(t, 0x16, reads.Bytes()[0], "client's first front byte is a TLS handshake record")
	assert.EqualValues(t, 0x16, writes.Bytes()[0], "server's first front byte is a TLS handshake record")
	assert.NotContains(t, string(reads.Bytes()), canary, "front capture must not contain the plaintext")
	assert.NotContains(t, string(writes.Bytes()), canary, "front capture must not contain the plaintext")
	assert.NotContains(t, string(reads.Bytes()), "/grants/", "even the URL path stays inside TLS")
}

func TestDialOnward(t *testing.T) {
	tunnelAddr, _ := startRelay(t, nil)
	client, err := Dial(context.Background(), tunnelAddr)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	conn, err := client.DialOnward(context.Background(), echo.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "ping")
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))

	_, err = client.DialOnward(context.Background(), "127.0.0.1:1")
	assert.Error(t, err, "an unreachable onward target must surface as an error")
}

func TestOperatorHandover(t *testing.T) {
	tunnelAddr, frontAddr := startRelay(t, nil)

	first, err := Dial(context.Background(), tunnelAddr)
	require.NoError(t, err)
	second, err := Dial(context.Background(), tunnelAddr)
	require.NoError(t, err)
	defer func() { _ = second.Close() }()

	// The relay closed the first control connection; its Accept unblocks.
	_, err = first.Accept()
	require.ErrorIs(t, err, net.ErrClosed, "a displaced operator's listener closes")

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "second")
	})}
	go func() { _ = srv.Serve(second) }()
	defer func() { _ = srv.Close() }()
	resp, err := http.Get("http://" + frontAddr + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "second", string(body), "front traffic follows the latest registration")

	// No operator at all: the front refuses by closing.
	_ = second.Close()
	require.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", frontAddr)
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(make([]byte, 1))
		return err == io.EOF
	}, 5*time.Second, 50*time.Millisecond, "an operatorless front closes inbound connections")
}

func TestAcceptUnknownID(t *testing.T) {
	tunnelAddr, _ := startRelay(t, nil)
	conn, err := net.Dial("tcp", tunnelAddr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "ACCEPT deadbeef\n")
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "an unknown rendezvous id is dropped")
}

// captureBuf is a concurrency-safe byte sink for the tee.
type captureBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *captureBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *captureBuf) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

type teeListener struct {
	net.Listener
	reads, writes *captureBuf
}

func (l *teeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &teeConn{Conn: c, reads: l.reads, writes: l.writes}, nil
}

type teeConn struct {
	net.Conn
	reads, writes *captureBuf
}

func (c *teeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	_, _ = c.reads.Write(p[:n])
	return n, err
}

func (c *teeConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	_, _ = c.writes.Write(p[:n])
	return n, err
}

func (c *teeConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
