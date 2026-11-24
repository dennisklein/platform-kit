package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// Authentication of the tunnel port. Every connection to it, the control
// connection, each ACCEPT and each DIAL, proves that its opener holds the
// secret the relay was started with, before the relay does anything for it:
//
//	relay:  PK-RELAY/1 <nonce>                 a fresh 128-bit nonce per connection
//	peer:   <VERB>[ <arg>] mac=<hmac>          HMAC-SHA256(secret, nonce, verb, arg)
//
// The MAC binds the verb and its argument to this connection's nonce, so a
// line seen on the wire can be replayed neither on another connection nor
// with another target. The secret itself never crosses the wire. A line
// without a valid MAC is answered "ERR unauthenticated" and the connection is
// closed; nothing was registered, accepted or dialled for it.
//
// This authenticates who opens a connection. It does not encrypt the tunnel:
// the payloads it carries are TLS of their own (the media server's, the
// BMCs' Redfish), and at day 0 the tunnel port is reached over ssh -L.

const (
	greeting = "PK-RELAY/1"
	macField = " mac="
	// minSecret is the shortest secret accepted, in bytes: 128 bits as hex.
	minSecret = 32
	// SecretEnv names the environment variable a secret may be given in.
	SecretEnv = "PK_METAL_RELAY_SECRET"
)

// errUnauthenticated: the peer's line carried no valid MAC.
var errUnauthenticated = errors.New("unauthenticated")

// newNonce draws a connection's challenge.
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// lineMAC is the MAC a peer sends for verb and arg on the connection whose
// greeting carried nonce. Each part is length-prefixed by its separator: none
// of them can hold a newline, since the protocol is line-based.
func lineMAC(secret []byte, nonce, verb, arg string) string {
	m := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(m, greeting+"\n"+nonce+"\n"+verb+"\n"+arg)
	return hex.EncodeToString(m.Sum(nil))
}

// signedLine is the line a peer sends: the verb, its argument if any, and
// the MAC.
func signedLine(secret []byte, nonce, verb, arg string) string {
	line := verb
	if arg != "" {
		line += " " + arg
	}
	return line + macField + lineMAC(secret, nonce, verb, arg) + "\n"
}

// verifyLine checks a received line against the connection's nonce and
// returns its verb and argument.
func verifyLine(secret []byte, nonce, line string) (verb, arg string, err error) {
	i := strings.LastIndex(line, macField)
	if i < 0 {
		return "", "", errUnauthenticated
	}
	body, mac := line[:i], line[i+len(macField):]
	verb, arg, _ = strings.Cut(body, " ")
	want := lineMAC(secret, nonce, verb, arg)
	if !hmac.Equal([]byte(mac), []byte(want)) {
		return "", "", errUnauthenticated
	}
	return verb, arg, nil
}

// greet reads the relay's greeting off a fresh tunnel connection and sends
// the signed line for verb and arg.
func greet(c net.Conn, secret []byte, verb, arg string) error {
	line, err := readLine(c)
	if err != nil {
		return err
	}
	nonce, ok := strings.CutPrefix(line, greeting+" ")
	if !ok {
		return fmt.Errorf("not a pk-metal relay, or another version of it (greeting %q)", line)
	}
	_, err = io.WriteString(c, signedLine(secret, nonce, verb, arg))
	return err
}

// ParseSecret checks a shared secret as read from a file or the environment.
// Surrounding white space (a trailing newline) is not part of it.
func ParseSecret(raw []byte) ([]byte, error) {
	s := []byte(strings.TrimSpace(string(raw)))
	if len(s) < minSecret {
		return nil, fmt.Errorf("the relay secret is %d characters long; it needs at least %d "+
			"(pk-metal relay --secret-file writes one when the file does not exist)", len(s), minSecret)
	}
	return s, nil
}

// LoadSecret reads the shared secret from path. When the file does not
// exist and create is set, a new secret is generated and written there,
// readable by its owner only; an existing file is never replaced, so a secret
// already handed to the operator side keeps working. created reports a new
// secret.
func LoadSecret(path string, create bool) (secret []byte, created bool, err error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		s, err := ParseSecret(raw)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		return s, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, false, err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, false, err
	}
	s := []byte(hex.EncodeToString(b[:]))
	// O_EXCL: two relays started at once on the same path must not each
	// write a secret and keep a different one.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadSecret(path, false)
		}
		return nil, false, err
	}
	if _, err := f.Write(append(s, '\n')); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	if err := f.Close(); err != nil {
		return nil, false, err
	}
	return s, true, nil
}
