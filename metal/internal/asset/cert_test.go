package asset

import (
	"context"
	"crypto/tls"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelfSignedRenewsBeforeExpiry(t *testing.T) {
	clock := time.Now()
	s := &SelfSigned{now: func() time.Time { return clock }}
	first, err := s.GetCertificate(nil)
	require.NoError(t, err)
	assert.WithinDuration(t, clock.Add(selfSignedLifetime), first.Leaf.NotAfter, time.Second)

	clock = clock.Add(selfSignedLifetime - selfSignedRenewBefore - time.Hour)
	same, err := s.GetCertificate(nil)
	require.NoError(t, err)
	assert.Same(t, first, same, "kept while more than a third of its life is left")

	clock = clock.Add(2 * time.Hour)
	renewed, err := s.GetCertificate(nil)
	require.NoError(t, err)
	assert.NotSame(t, first, renewed)
	assert.True(t, renewed.Leaf.NotAfter.After(first.Leaf.NotAfter))
	assert.True(t, clock.Before(first.Leaf.NotAfter), "replaced while the old one was still valid")
	assert.Equal(t, first.Leaf.Subject, renewed.Leaf.Subject, "same names: nothing outside the manager pins it")
}

func TestReloadingKeepsTheLastGoodCertificate(t *testing.T) {
	a, err := EphemeralCert()
	require.NoError(t, err)
	b, err := EphemeralCert()
	require.NoError(t, err)
	var next atomic.Value
	next.Store(a)
	var fail atomic.Bool
	load := func(context.Context) (tls.Certificate, error) {
		if fail.Load() {
			return tls.Certificate{}, errors.New("secret pk-metal/media-tls: not found")
		}
		return next.Load().(tls.Certificate), nil
	}

	_, err = NewReloading(t.Context(), func(context.Context) (tls.Certificate, error) {
		return tls.Certificate{}, errors.New("secret pk-metal/media-tls: not found")
	}, time.Millisecond)
	assert.Error(t, err, "a named Secret that cannot be read at start-up is a start-up error")

	r, err := NewReloading(t.Context(), load, 5*time.Millisecond)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	serial := func() string {
		c, err := r.GetCertificate(nil)
		if !assert.NoError(t, err) {
			return ""
		}
		return c.Leaf.SerialNumber.String()
	}
	assert.Equal(t, a.Leaf.SerialNumber.String(), serial())

	fail.Store(true)
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, a.Leaf.SerialNumber.String(), serial(), "a failed load keeps the certificate in service")

	next.Store(b)
	fail.Store(false)
	assert.Eventually(t, func() bool { return serial() == b.Leaf.SerialNumber.String() },
		2*time.Second, 5*time.Millisecond, "a renewed Secret is served without a restart")
}

func TestServiceServesTheCurrentCertificate(t *testing.T) {
	a, err := EphemeralCert()
	require.NoError(t, err)
	b, err := EphemeralCert()
	require.NoError(t, err)
	var current atomic.Pointer[tls.Certificate]
	current.Store(&a)

	svc, err := NewService("127.0.0.1:0", "https://media.example.invalid", tls.Certificate{})
	require.NoError(t, err)
	svc.Certs = certFunc(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return current.Load(), nil })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = svc.Start(ctx) }()
	addr, err := svc.Addr(ctx)
	require.NoError(t, err)

	served := func() string {
		conn, err := tls.Dial("tcp", addr.String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the serial is the observable
		require.NoError(t, err)
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	assert.Equal(t, a.Leaf.SerialNumber.String(), served())
	current.Store(&b)
	assert.Equal(t, b.Leaf.SerialNumber.String(), served(), "the next handshake gets the new certificate")
}

type certFunc func(*tls.ClientHelloInfo) (*tls.Certificate, error)

func (f certFunc) GetCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return f(h) }
