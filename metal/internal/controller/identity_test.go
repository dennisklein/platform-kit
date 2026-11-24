package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func selfSignedPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestFirstCertificate(t *testing.T) {
	leafPEM := selfSignedPEM(t, "bmc-01.example.test")
	caPEM := selfSignedPEM(t, "Example Test CA")

	t.Run("leaf of a chain", func(t *testing.T) {
		leaf, got, err := firstCertificate(append(append([]byte{}, leafPEM...), caPEM...))
		require.NoError(t, err)
		assert.Equal(t, "bmc-01.example.test", leaf.Subject.CommonName)
		assert.Equal(t, leafPEM, got, "the leaf re-encodes to the bytes it was issued as")
	})

	t.Run("broken intermediate refused", func(t *testing.T) {
		broken := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")})
		_, _, err := firstCertificate(append(append([]byte{}, leafPEM...), broken...))
		assert.Error(t, err, "the whole chain is pushed to the manager, so every certificate must parse")
	})

	t.Run("no certificate", func(t *testing.T) {
		_, _, err := firstCertificate([]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"))
		assert.Error(t, err)
	})
}

// leafFor issues a certificate for the given names, valid for the given
// lifetime from now, under a fresh key.
func leafFor(t *testing.T, cn string, dns []string, ips []net.IP, lifetime time.Duration) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dns,
		IPAddresses:  ips,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(lifetime),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return leaf
}

func TestDesiredIdentityIPSANForms(t *testing.T) {
	pol := &metalv1alpha1.IdentityPolicy{Domain: "bmc.example.test"}
	for _, tc := range []struct {
		address string
		want    []string
	}{
		{"https://192.0.2.14", []string{"192.0.2.14"}},
		{"https://192.0.2.14:8443", []string{"192.0.2.14"}},
		{"https://[2001:db8::14]", []string{"2001:db8::14"}},
		// A name is not an IP. It gives no IP SAN, so the drift check never
		// waits for an address that no certificate can carry.
		{"https://bmc-07.example.test", nil},
		{"https://bmc-07.example.test:443", nil},
	} {
		t.Run(tc.address, func(t *testing.T) {
			ep := &metalv1alpha1.ManagementEndpoint{
				ObjectMeta: metav1.ObjectMeta{Name: "ep-07"},
				Spec:       metalv1alpha1.ManagementEndpointSpec{Address: tc.address},
			}
			d := desiredIdentity(ep, pol)
			assert.Equal(t, "bmc-ep-07.bmc.example.test", d.CommonName)
			assert.Equal(t, []string{d.CommonName}, d.DNSNames)
			assert.Equal(t, tc.want, d.IPAddresses)
		})
	}
}

// TestIdentityDriftNamesNotKeys pins what the drift check compares. The
// lane converges on what a client checks: names and lifetime. Leaves with the
// same names and different keys are the same identity, so a manager that
// re-keys is not pushed to again on every pass.
func TestIdentityDriftNamesNotKeys(t *testing.T) {
	desired := identityDesired{
		CommonName:  "bmc-ep-07.bmc.example.test",
		DNSNames:    []string{"bmc-ep-07.bmc.example.test"},
		IPAddresses: []string{"192.0.2.14"},
	}
	ip := []net.IP{net.ParseIP("192.0.2.14")}
	const window, long = 720 * time.Hour, 2160 * time.Hour

	a := leafFor(t, desired.CommonName, desired.DNSNames, ip, long)
	b := leafFor(t, desired.CommonName, desired.DNSNames, ip, long)
	require.NotEqual(t, a.PublicKey, b.PublicKey)
	assert.Empty(t, identityDrift(a, desired, window))
	assert.Empty(t, identityDrift(b, desired, window), "another key under the same names is in sync")

	noDNS := leafFor(t, desired.CommonName, nil, ip, long)
	assert.Contains(t, identityDrift(noDNS, desired, window), "lacks DNS SAN")
	otherIP := leafFor(t, desired.CommonName, desired.DNSNames, []net.IP{net.ParseIP("192.0.2.15")}, long)
	assert.Contains(t, identityDrift(otherIP, desired, window), `lacks IP SAN "192.0.2.14"`)
}

// certificateRequest is a cert-manager CertificateRequest with the given
// status, as the API stores it.
func certificateRequest(name string, status map[string]any) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{Object: map[string]any{"status": status}}
	cr.SetGroupVersionKind(certificateRequestGVK)
	cr.SetNamespace("pk-metal")
	cr.SetName(name)
	return cr
}

func TestCertificateRequestChain(t *testing.T) {
	chainPEM := selfSignedPEM(t, "bmc-ep-07.bmc.example.test")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		certificateRequest("issued", map[string]any{"certificate": base64.StdEncoding.EncodeToString(chainPEM)}),
		certificateRequest("garbled", map[string]any{"certificate": "%%% not base64 %%%"}),
		certificateRequest("pending", map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "Pending", "message": "waiting on the issuer"},
		}}),
	).Build()
	r := &EndpointConvergenceReconciler{Client: cl, CredentialsNamespace: "pk-metal"}

	t.Run("issued", func(t *testing.T) {
		chain, ready, _, err := r.certificateRequestChain(t.Context(), "issued")
		require.NoError(t, err)
		assert.True(t, ready)
		assert.Equal(t, chainPEM, chain)
	})

	t.Run("an undecodable certificate is an error, not a push", func(t *testing.T) {
		chain, ready, _, err := r.certificateRequestChain(t.Context(), "garbled")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "undecodable certificate")
		assert.False(t, ready)
		assert.Nil(t, chain)
	})

	t.Run("pending says why", func(t *testing.T) {
		_, ready, msg, err := r.certificateRequestChain(t.Context(), "pending")
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, msg, "pk-metal/pending awaiting issuance")
		assert.Contains(t, msg, "waiting on the issuer")
	})

	t.Run("missing request", func(t *testing.T) {
		_, _, _, err := r.certificateRequestChain(t.Context(), "absent")
		assert.True(t, apierrors.IsNotFound(err), "got %v", err)
	})
}

func TestPushIdentityRefusesABrokenChainBeforeTheSession(t *testing.T) {
	s := newSimEndpoint(t, "supermicro-x12")
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial}
	broken := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")})
	chain := append(selfSignedPEM(t, "bmc-a.metal.internal"), broken...)

	for _, dialect := range []bmc.IdentityDialect{bmc.DialectCSRNative, bmc.DialectKeyUpload} {
		t.Run(string(dialect), func(t *testing.T) {
			_, err := r.pushIdentity(t.Context(), s.ep, dialect, chain, []byte("key"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "issued chain")
			assert.Empty(t, s.dialed, "nothing reaches the manager: a push restarts it")
		})
	}
}
