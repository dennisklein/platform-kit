package render

import (
	"bytes"
	stdx509 "crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/siderolabs/crypto/x509"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/role"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The admin credentials the operator publishes are minted from the bundle's
// CAs. Minting is not deterministic (every call draws a new key), so callers
// keep a published credential while it is still Current and mint a new one
// only when it is not: the credential is re-derivable from the bundle at any
// time, but it is not re-derived on every reconcile.

// MaxCredentialTTL bounds the lifetime of a minted admin credential. The
// certificates cannot be revoked short of a CA rotation, so a copy that
// leaks stays good until it expires. TalosCluster.spec.credentials.ttl has
// the same bound in its CEL.
const MaxCredentialTTL = 720 * time.Hour

// Talosconfig mints an admin talosconfig for the cluster: a client
// certificate signed by the bundle's Talos CA with the os:admin role, valid
// for ttl, and the given endpoints.
func Talosconfig(bundle *secrets.Bundle, clusterName string, endpoints []string, ttl time.Duration) ([]byte, error) {
	return TalosconfigWithRole(bundle, clusterName, endpoints, ttl, role.Admin)
}

// TalosconfigWithRole is Talosconfig with another role: os:reader can read
// every machine resource and change none, which is what monitoring needs.
func TalosconfigWithRole(bundle *secrets.Bundle, clusterName string, endpoints []string, ttl time.Duration, r role.Role) ([]byte, error) {
	cert, err := bundle.GenerateTalosAPIClientCertificateWithTTL(role.MakeSet(r), ttl)
	if err != nil {
		return nil, fmt.Errorf("mint talos client certificate: %w", err)
	}
	cfg := clientconfig.NewConfig(clusterName, endpoints, bundle.Certs.OS.Crt, cert)
	return cfg.Bytes()
}

// Kubeconfig mints an admin kubeconfig for the cluster: a client certificate
// signed by the bundle's Kubernetes CA with the subject Talos gives its own
// admin kubeconfigs (CN admin, O system:masters), valid for ttl, pointing at
// endpoint.
func Kubeconfig(bundle *secrets.Bundle, clusterName, endpoint string, ttl time.Duration) ([]byte, error) {
	ca, err := x509.NewCertificateAuthorityFromCertificateAndKey(bundle.Certs.K8s)
	if err != nil {
		return nil, fmt.Errorf("kubernetes CA: %w", err)
	}
	now := bundle.Clock.Now()
	kp, err := x509.NewKeyPair(ca,
		x509.CommonName(constants.KubernetesAdminCertCommonName),
		x509.Organization(constants.KubernetesAdminCertOrganization),
		x509.NotBefore(now),
		x509.NotAfter(now.Add(ttl)),
		x509.KeyUsage(stdx509.KeyUsageDigitalSignature|stdx509.KeyUsageKeyEncipherment),
		x509.ExtKeyUsage([]stdx509.ExtKeyUsage{stdx509.ExtKeyUsageClientAuth}),
	)
	if err != nil {
		return nil, fmt.Errorf("mint kubernetes admin certificate: %w", err)
	}

	user := "admin@" + clusterName
	cfg := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{clusterName: {
			Server:                   endpoint,
			CertificateAuthorityData: bundle.Certs.K8s.Crt,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{user: {
			ClientCertificateData: kp.CrtPEM,
			ClientKeyData:         kp.KeyPEM,
		}},
		Contexts: map[string]*clientcmdapi.Context{user: {
			Cluster: clusterName, AuthInfo: user,
		}},
		CurrentContext: user,
	}
	return clientcmd.Write(cfg)
}

// TalosconfigCurrent reports whether a published talosconfig still serves:
// it trusts the bundle's Talos CA, names exactly the wanted endpoints, and its
// certificate is signed by that CA and valid for at least minValidity more.
func TalosconfigCurrent(data []byte, bundle *secrets.Bundle, endpoints []string, minValidity time.Duration) bool {
	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		return false
	}
	ctx := cfg.Contexts[cfg.Context]
	if ctx == nil || !slices.Equal(ctx.Endpoints, endpoints) {
		return false
	}
	return certCurrent(ctx.CA, ctx.Crt, bundle.Certs.OS.Crt, minValidity, bundle.Clock.Now())
}

// KubeconfigCurrent is TalosconfigCurrent for the admin kubeconfig.
func KubeconfigCurrent(data []byte, bundle *secrets.Bundle, endpoint string, minValidity time.Duration) bool {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return false
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil {
		return false
	}
	cl, user := cfg.Clusters[ctx.Cluster], cfg.AuthInfos[ctx.AuthInfo]
	if cl == nil || user == nil || cl.Server != endpoint {
		return false
	}
	return certCurrent(string(cl.CertificateAuthorityData), string(user.ClientCertificateData),
		bundle.Certs.K8s.Crt, minValidity, bundle.Clock.Now())
}

// certCurrent checks that the credential trusts wantCA (talosconfig stores
// PEM base64-encoded, kubeconfig stores it raw, so both forms are accepted),
// and that its certificate chains to it and outlives now+minValidity.
func certCurrent(gotCA, gotCrt string, wantCA []byte, minValidity time.Duration, now time.Time) bool {
	if !samePEM(gotCA, wantCA) {
		return false
	}
	crt, err := parseCert(gotCrt)
	if err != nil {
		return false
	}
	ca, err := parseCert(string(wantCA))
	if err != nil {
		return false
	}
	if crt.CheckSignatureFrom(ca) != nil {
		return false
	}
	return crt.NotAfter.After(now.Add(minValidity))
}

func samePEM(got string, want []byte) bool {
	if dec, err := decodeMaybeBase64(got); err == nil {
		return bytes.Equal(bytes.TrimSpace(dec), bytes.TrimSpace(want))
	}
	return false
}

// decodeMaybeBase64 accepts PEM text as is and base64 of PEM decoded, the two
// forms the two client config formats store certificates in.
func decodeMaybeBase64(s string) ([]byte, error) {
	if bytes.HasPrefix(bytes.TrimSpace([]byte(s)), []byte("-----BEGIN")) {
		return []byte(s), nil
	}
	return base64.StdEncoding.DecodeString(s)
}

func parseCert(s string) (*stdx509.Certificate, error) {
	raw, err := decodeMaybeBase64(s)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return stdx509.ParseCertificate(block.Bytes)
}

// TalosconfigNotAfter is when a talosconfig's client certificate expires.
func TalosconfigNotAfter(data []byte) (time.Time, error) {
	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		return time.Time{}, err
	}
	ctx := cfg.Contexts[cfg.Context]
	if ctx == nil {
		return time.Time{}, errors.New("talosconfig has no current context")
	}
	crt, err := parseCert(ctx.Crt)
	if err != nil {
		return time.Time{}, err
	}
	return crt.NotAfter, nil
}

// KubeconfigNotAfter is when a kubeconfig's client certificate expires.
func KubeconfigNotAfter(data []byte) (time.Time, error) {
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return time.Time{}, err
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil || cfg.AuthInfos[ctx.AuthInfo] == nil {
		return time.Time{}, errors.New("kubeconfig has no current user")
	}
	crt, err := parseCert(string(cfg.AuthInfos[ctx.AuthInfo].ClientCertificateData))
	if err != nil {
		return time.Time{}, err
	}
	return crt.NotAfter, nil
}

// TalosconfigRoles are the roles a talosconfig's client certificate carries
// (Talos puts them in the certificate's organizations).
func TalosconfigRoles(data []byte) ([]string, error) {
	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		return nil, err
	}
	ctx := cfg.Contexts[cfg.Context]
	if ctx == nil {
		return nil, errors.New("talosconfig has no current context")
	}
	crt, err := parseCert(ctx.Crt)
	if err != nil {
		return nil, err
	}
	return crt.Subject.Organization, nil
}
