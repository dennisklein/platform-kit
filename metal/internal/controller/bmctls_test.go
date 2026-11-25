package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/testpki"
)

// newLeaf makes a certificate with a key of its own.
func newLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	c, err := asset.EphemeralCert()
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	require.NoError(t, err)
	return leaf
}

// The observing pass trusts the key a BMC presents first, before it sends
// a credential, and from then on refuses any other key until a person
// approves it. The key of a certificate the operator pushed moves the pin
// without a person.
func TestTLSKeyPinnedOnFirstUse(t *testing.T) {
	s := newSimEndpoint(t, "generic")
	s.ep.Status.TLSPin = nil
	s.ep.Status.Asset = &metalv1alpha1.AssetSnapshot{Serial: s.fixture.Chassis.BoardSerial, BMCMac: s.fixture.Manager.MAC}
	presented := s.leaf
	r := &ManagementEndpointReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial,
		Probe: func(context.Context, string) (*x509.Certificate, error) { return presented, nil }}
	reachable := func() *metav1.Condition {
		return meta.FindStatusCondition(s.ep.Status.Conditions, metalv1alpha1.ConditionReachable)
	}

	// First contact: the key is pinned, then the session accepts it alone.
	r.observe(t.Context(), s.ep)
	first := gofishclient.PublicKeyPin(s.leaf)
	require.Equal(t, &metalv1alpha1.TLSPin{PublicKey: first, Source: metalv1alpha1.TLSPinFirstUse}, s.ep.Status.TLSPin)
	require.Len(t, s.dialed, 1)
	assert.Equal(t, []string{first}, s.dialed[0].PinnedKeys)
	assert.Equal(t, metav1.ConditionTrue, reachable().Status)

	// Another key: no session at all, so no password goes to it.
	presented = newLeaf(t)
	other := gofishclient.PublicKeyPin(presented)
	r.observe(t.Context(), s.ep)
	assert.Len(t, s.dialed, 1, "nothing is sent to a BMC that presents another key")
	assert.Equal(t, metav1.ConditionFalse, reachable().Status)
	assert.Equal(t, metalv1alpha1.ReasonTLSKeyChanged, reachable().Reason)
	assert.Contains(t, reachable().Message, metalv1alpha1.AnnotationApproveTLSKey+"="+other,
		"the condition says how to approve exactly the presented key")
	assert.Equal(t, first, s.ep.Status.TLSPin.PublicKey, "the pin stays")

	// An approval of some other key approves nothing.
	s.ep.Annotations = map[string]string{metalv1alpha1.AnnotationApproveTLSKey: first}
	r.observe(t.Context(), s.ep)
	assert.Len(t, s.dialed, 1)

	// The approval of the presented key moves the pin.
	s.ep.Annotations[metalv1alpha1.AnnotationApproveTLSKey] = other
	assert.False(t, approvalDone(s.ep), "the approval is not used up before its pin is stored")
	r.observe(t.Context(), s.ep)
	assert.Equal(t, &metalv1alpha1.TLSPin{PublicKey: other, Source: metalv1alpha1.TLSPinApproved}, s.ep.Status.TLSPin)
	require.Len(t, s.dialed, 2)
	assert.Equal(t, metav1.ConditionTrue, reachable().Status)
	assert.True(t, approvalDone(s.ep), "the stored pin uses the approval up")

	// The key of a pushed certificate moves the pin by itself.
	pushed := newLeaf(t)
	s.ep.Status.IdentityPushed = &metalv1alpha1.IdentityPush{
		PlanHash: "h", Fingerprint: certFingerprint(pushed), PublicKey: gofishclient.PublicKeyPin(pushed),
	}
	presented = pushed
	r.observe(t.Context(), s.ep)
	assert.Equal(t, &metalv1alpha1.TLSPin{PublicKey: gofishclient.PublicKeyPin(pushed), Source: metalv1alpha1.TLSPinPushed},
		s.ep.Status.TLSPin)
	assert.Equal(t, metav1.ConditionTrue, reachable().Status)
}

// No lane sends a credential to an endpoint whose key nobody pinned yet;
// a CA or the explicit opt-out need no pin.
func TestSessionsNeedAPinWithoutACA(t *testing.T) {
	s := newSimEndpoint(t, "generic")
	s.ep.Status.TLSPin = nil
	_, reason, err := endpointClientConfig(t.Context(), s.cl, "pk-metal", s.ep, true)
	require.Error(t, err)
	assert.Equal(t, metalv1alpha1.ReasonTLSKeyNotPinned, reason)

	s.ep.Spec.TLS = &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true}
	cfg, _, err := endpointClientConfig(t.Context(), s.cl, "pk-metal", s.ep, true)
	require.NoError(t, err)
	assert.True(t, cfg.Insecure)
	assert.Empty(t, cfg.PinnedKeys)
}

// The key-upload dialect makes no private key for an endpoint whose
// sessions check nothing, and pushIdentity refuses the upload as well.
func TestKeyUploadRefusedWithoutVerification(t *testing.T) {
	s := newSimEndpoint(t, "supermicro-x12")
	s.ep.Spec.TLS = &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true}
	s.ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{IdentityDialect: string(bmc.DialectKeyUpload)}
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial}
	pol := &metalv1alpha1.IdentityPolicy{IssuerRef: metalv1alpha1.IssuerRef{Name: "ca"}}

	// A served certificate that the operator never pushed.
	status, reason, msg, err := r.identityWith(t.Context(), s.ep, pol, s.leaf)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, metalv1alpha1.ReasonTLSNotVerified, reason, msg)

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.leaf.Raw})
	_, err = r.pushIdentity(t.Context(), s.ep, bmc.DialectKeyUpload, leafPEM, []byte("key"))
	require.ErrorContains(t, err, "refused to upload a private key")
	assert.Empty(t, s.dialed, "no session is opened for the key")
}

// The names and dates of a served certificate are not the identity: only
// the certificate the operator pushed for this desired state is in sync.
func TestIdentityInSyncOnlyForThePushedCertificate(t *testing.T) {
	s := newSimEndpoint(t, "huawei-2288h-v5")
	s.ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{IdentityDialect: string(bmc.DialectNoIdentity)}
	r := &EndpointConvergenceReconciler{Client: s.cl, CredentialsNamespace: "pk-metal", Dial: s.dial}
	pol := &metalv1alpha1.IdentityPolicy{IssuerRef: metalv1alpha1.IssuerRef{Name: "ca"}, RenewBefore: &metav1.Duration{}}
	desired := desiredIdentity(s.ep, pol)
	claims := newLeafNamed(t, desired.CommonName)

	// Right names, never pushed: not in sync (the lane would push).
	status, reason, _, err := r.identityWith(t.Context(), s.ep, pol, claims)
	require.NoError(t, err)
	assert.NotEqual(t, metav1.ConditionTrue, status, reason)

	// Pushed for this desired state and served: in sync.
	s.ep.Status.IdentityPushed = &metalv1alpha1.IdentityPush{
		PlanHash: planHash(desired), Fingerprint: certFingerprint(claims), PublicKey: gofishclient.PublicKeyPin(claims),
	}
	status, reason, msg, err := r.identityWith(t.Context(), s.ep, pol, claims)
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionTrue, status, "%s: %s", reason, msg)

	// Another certificate with the same names, after the push: held.
	status, reason, msg, err = r.identityWith(t.Context(), s.ep, pol, newLeafNamed(t, desired.CommonName))
	require.NoError(t, err)
	assert.Equal(t, metav1.ConditionFalse, status)
	assert.Equal(t, metalv1alpha1.ReasonIdentityPushFailed, reason)
	assert.Contains(t, msg, "not the pushed one")
}

// newLeafNamed makes a self-signed certificate for a name, with a key of its
// own.
func newLeafNamed(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	certPEM, _ := testpki.SelfSigned(t, cn, []string{cn}, nil)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	c, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return c
}

// testCSR makes a request for a name, with a key of its own.
func testCSR(t *testing.T, cn string) ([]byte, *x509.CertificateRequest) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
	}, key)
	require.NoError(t, err)
	csr, err := x509.ParseCertificateRequest(der)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), csr
}

// An issued certificate goes to the manager only when it carries the key of
// the operator's request and its issuer's signature verifies.
func TestIssuedForRequest(t *testing.T) {
	ca := testpki.NewCA(t, "issuer")
	csrPEM, csr := testCSR(t, "bmc-a.metal.internal")
	otherPEM, _ := testCSR(t, "bmc-a.metal.internal")

	chain, err := ca.Sign(csr, time.Hour)
	require.NoError(t, err)
	require.NoError(t, issuedForRequest(chain, csrPEM))
	assert.ErrorContains(t, issuedForRequest(chain, otherPEM), "does not carry the key of the request")

	block, _ := pem.Decode(chain)
	require.NotNil(t, block)
	stranger := testpki.NewCA(t, "stranger")
	wrongIssuer := append(pem.EncodeToMemory(block),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: stranger.Cert.Raw})...)
	assert.ErrorContains(t, issuedForRequest(wrongIssuer, csrPEM), "not signed by")
}
