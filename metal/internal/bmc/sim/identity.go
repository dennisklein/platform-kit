package sim

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"reflect"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The identity model, capture-true per vendor: the iBMC generates its CSR on
// the manager (HttpsCert.GenerateCSR — the key never leaves it) and takes
// the signed chain back as PEM text; the X12 takes a locally-generated leaf
// and key over the OEM SmcSSLCert.Upload. On BOTH, serving switches only at
// a manager reset (the PoC's explicit apply mechanism) — resetManagerLocked
// applies the staged identity, and the cert-stuck fault models the lying
// oracle that answers success and never serves the pushed certificate.

// identityState is the manager's certificate world.
type identityState struct {
	served     *tls.Certificate // what the HTTPS listener presents
	pendingKey *ecdsa.PrivateKey
	pendingCSR []byte           // PEM, as served in CertificateSigningRequest
	staged     *tls.Certificate // applied at the next manager reset
}

// identityDialect derives the certificate-replacement dialect.
func (f *Fixture) identityDialect() string {
	switch {
	case f.Identity.CSRNative:
		return "csr-native"
	case f.Identity.KeyUpload:
		return "key-upload"
	default:
		return "none"
	}
}

// SetServedCertificate seeds the identity the listener starts with — the
// factory self-signed certificate every BMC ships.
func (s *State) SetServedCertificate(cert tls.Certificate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity.served = &cert
}

// TLSCertificate is the listener's GetCertificate hook — the identity hot
// swap at manager reset is what makes the served certificate an observable.
func (s *State) TLSCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.identity.served == nil {
		return nil, fmt.Errorf("sim: no serving certificate seeded")
	}
	return s.identity.served, nil
}

// ServedLeaf reports the leaf the listener currently presents (test
// observability, and the HttpsCert GET rendering).
func (s *State) ServedLeaf() *x509.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servedLeafLocked()
}

func (s *State) servedLeafLocked() *x509.Certificate {
	if s.identity.served == nil || len(s.identity.served.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(s.identity.served.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// GenerateCSR is the csr-native entrance: the sim mints a key it never hands
// out and builds the request around it.
func (s *State) GenerateCSR(_ context.Context, subject bmc.CSRSubject) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	if !s.f.Identity.CSRNative {
		return nil, fmt.Errorf("sim: fixture models no on-manager CSR generation")
	}
	if subject.CommonName == "" || subject.Country == "" {
		// The captured ActionInfo: Country and CommonName are the required
		// parameters.
		return nil, fmt.Errorf("sim: CommonName and Country required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, raw := range subject.IPAddresses {
		if ip := net.ParseIP(raw); ip != nil {
			ips = append(ips, ip)
		}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:         subject.CommonName,
			Country:            nonEmpty(subject.Country),
			Province:           nonEmpty(subject.State),
			Locality:           nonEmpty(subject.Locality),
			Organization:       nonEmpty(subject.Organization),
			OrganizationalUnit: nonEmpty(subject.OrganizationalUnit),
		},
		DNSNames:    subject.DNSNames,
		IPAddresses: ips,
	}, key)
	if err != nil {
		return nil, err
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	s.identity.pendingKey, s.identity.pendingCSR = key, csr
	return csr, nil
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// ImportServerCertificate takes the signed chain back — csr-native only, and
// only against the pending key this manager generated.
func (s *State) ImportServerCertificate(_ context.Context, chainPEM []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if !s.f.Identity.CSRNative {
		return fmt.Errorf("sim: fixture models no certificate import")
	}
	if s.identity.pendingKey == nil {
		return fmt.Errorf("sim: no pending CSR to match the certificate against")
	}
	leaf, err := firstCertificate(chainPEM)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(leaf.PublicKey, s.identity.pendingKey.Public()) {
		return fmt.Errorf("sim: certificate does not match the pending CSR's key")
	}
	s.identity.staged = &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: s.identity.pendingKey}
	s.identity.pendingKey, s.identity.pendingCSR = nil, nil
	return nil
}

// UploadServerIdentity takes a leaf and key — key-upload only.
func (s *State) UploadServerIdentity(_ context.Context, certPEM, keyPEM []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if !s.f.Identity.KeyUpload {
		return fmt.Errorf("sim: fixture models no identity upload")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("sim: certificate/key pair: %v", err)
	}
	s.identity.staged = &pair
	return nil
}

// applyIdentityLocked is the manager-reset hook: the staged identity goes
// live — unless the cert-stuck fault holds it (success was answered at
// import time; the lie shows only in what the listener keeps serving).
func (s *State) applyIdentityLocked() {
	if s.identity.staged == nil || s.faults[FaultCertStuck] {
		return
	}
	s.identity.served, s.identity.staged = s.identity.staged, nil
}

// PendingCSR reports the CSR awaiting import (test observability and the
// HttpsCert GET rendering); nil when none.
func (s *State) PendingCSR() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity.pendingCSR
}

func firstCertificate(chainPEM []byte) (*x509.Certificate, error) {
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("sim: no certificate in body")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		return x509.ParseCertificate(block.Bytes)
	}
}
