package asset

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"sync"
	"time"
)

const (
	// selfSignedLifetime is how long a self-signed media certificate is valid.
	selfSignedLifetime = 30 * 24 * time.Hour
	// selfSignedRenewBefore is how long before its end a self-signed
	// certificate is replaced: a third of its life, so a BMC firmware that
	// checks the validity period never sees one that is about to expire.
	selfSignedRenewBefore = 10 * 24 * time.Hour
)

// EphemeralCert mints a throwaway self-signed serving certificate. The media
// posture is HTTPS without verification (neither vendor's BMC can
// be fed a CA), so a serving cert only needs to exist, not chain — the channel
// gives privacy, the grant gives access control.
func EphemeralCert() (tls.Certificate, error) {
	return mintSelfSigned(time.Now())
}

func mintSelfSigned(now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "pk-metal"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(selfSignedLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// CertSource hands the media server the certificate to serve at each TLS
// handshake, so a certificate can be replaced while the server runs.
type CertSource interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// SelfSigned serves a self-signed certificate of its own making and makes a
// new one when the current one enters its last third. It is the source when
// no certificate Secret is named: nothing outside the manager knows or pins
// this certificate, so replacing it changes nothing anyone relies on.
type SelfSigned struct {
	mu   sync.Mutex
	cert *tls.Certificate
	// now is the clock; nil is time.Now.
	now func() time.Time
}

// NewSelfSigned mints the first certificate at once, so a manager that cannot
// make one fails at start-up rather than at the first BMC's handshake.
func NewSelfSigned() (*SelfSigned, error) {
	s := &SelfSigned{}
	if _, err := s.GetCertificate(nil); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SelfSigned) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if s.cert == nil || now.After(s.cert.Leaf.NotAfter.Add(-selfSignedRenewBefore)) {
		c, err := mintSelfSigned(now)
		if err != nil {
			if s.cert != nil {
				return s.cert, nil // the old one is still valid for days
			}
			return nil, err
		}
		s.cert = &c
	}
	return s.cert, nil
}

// Reloading serves the certificate a load function returns (the manager reads
// a Secret that cert-manager renews) and calls it again every Interval while
// Start runs. A load that fails, or returns a certificate that does not
// parse, keeps the last good certificate in service: the source never falls
// back to a certificate of its own making when one was named.
type Reloading struct {
	Load     func(ctx context.Context) (tls.Certificate, error)
	Interval time.Duration
	// Logf reports reloads and failed loads; nil is silent.
	Logf func(format string, args ...any)

	mu   sync.RWMutex
	cert *tls.Certificate
}

// NewReloading loads the certificate once; an error here is a start-up error.
func NewReloading(ctx context.Context, load func(ctx context.Context) (tls.Certificate, error), interval time.Duration) (*Reloading, error) {
	r := &Reloading{Load: load, Interval: interval}
	if err := r.reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reloading) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cert == nil {
		return nil, errors.New("media: no certificate loaded")
	}
	return r.cert, nil
}

// reload calls Load and swaps the result in when it differs from what is
// served.
func (r *Reloading) reload(ctx context.Context) error {
	c, err := r.Load(ctx)
	if err != nil {
		return err
	}
	if len(c.Certificate) == 0 {
		return errors.New("media: the certificate is empty")
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil && r.cert.Leaf.Equal(c.Leaf) {
		return nil
	}
	if r.cert != nil && r.Logf != nil {
		r.Logf("media certificate reloaded: serial %s, valid until %s", c.Leaf.SerialNumber, c.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	r.cert = &c
	return nil
}

// Start reloads every Interval until ctx ends. It satisfies
// controller-runtime's Runnable.
func (r *Reloading) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.reload(ctx); err != nil && r.Logf != nil {
				r.Logf("media certificate not reloaded, the one in service stays: %v", err)
			}
		}
	}
}

// NeedLeaderElection is false: a standby manager keeps its certificate fresh
// too, so it serves a current one the moment it leads.
func (r *Reloading) NeedLeaderElection() bool { return false }
