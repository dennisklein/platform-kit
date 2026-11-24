package asset

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Service is the operator's media server: the grant table plus the HTTPS
// listener BMCs pull granted media from. It runs inside the manager process
// for as long as the manager does; the HostOperation executor issues and
// revokes grants on it.
//
// Listen and BaseURL differ on purpose. The listener binds inside the pod;
// BMCs reach it through whatever fronts the pod (a NodePort in the virtual
// lab, a load balancer address in a rack), and only the advertised BaseURL
// ever goes into a mount.
type Service struct {
	Grants *Grants
	// Listen is the address to bind, e.g. ":8443".
	Listen string
	// BaseURL is the https:// origin BMCs use to reach Listen.
	BaseURL string
	// Cert serves TLS when Certs is nil. BMCs do not verify media
	// certificates, so an ephemeral self-signed one is fine
	// where nothing else is configured.
	Cert tls.Certificate
	// Certs, when set, is asked for the certificate at every handshake
	// instead of serving Cert: a certificate renewed while the manager runs
	// is served without a restart.
	Certs CertSource

	// ready is closed once the listener is bound — tests wait on it.
	ready chan struct{}
	addr  net.Addr
}

// NewService validates the advertised URL and prepares a service; Start runs it.
func NewService(listen, baseURL string, cert tls.Certificate) (*Service, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("media url %q: want https://host[:port] with no path (BMCs reject anything else)", baseURL)
	}
	return &Service{
		Grants:  NewGrants(),
		Listen:  listen,
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Cert:    cert,
		ready:   make(chan struct{}),
	}, nil
}

// Start serves until ctx ends. It satisfies controller-runtime's Runnable.
func (s *Service) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("media: listen %s: %w", s.Listen, err)
	}
	s.addr = ln.Addr()
	close(s.ready)
	srv := &http.Server{
		Handler:           &Server{Grants: s.Grants},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.Certs != nil {
		cfg.GetCertificate = s.Certs.GetCertificate
	} else {
		cfg.Certificates = []tls.Certificate{s.Cert}
	}
	err = srv.Serve(tls.NewListener(ln, cfg))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// NeedLeaderElection is true: the grants an executor issues live in its own
// process, so only the leader's listener can answer for them.
func (s *Service) NeedLeaderElection() bool { return true }

// Addr reports the bound listener address once Start has bound it.
func (s *Service) Addr(ctx context.Context) (net.Addr, error) {
	select {
	case <-s.ready:
		return s.addr, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// URL is the advertised address of a grant.
func (s *Service) URL(g *Grant) string {
	return s.BaseURL + g.Path()
}

// MintCredentials makes fresh per-grant media credentials: a BMC presents them
// as HTTP Basic, so a leaked URL alone fetches nothing.
func MintCredentials() (*Credentials, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return &Credentials{Username: "media", Password: hex.EncodeToString(raw)}, nil
}
