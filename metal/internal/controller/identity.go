package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/cert"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
)

// The identity lane: a cert-manager-signed HTTPS identity for
// bmc-<endpoint>.<domain>, pushed over the vendor's discovered dialect. The
// served certificate is the ONLY convergence observable — everything
// re-derives from it except one thing: whether the current desired state was
// already pushed (a manager can answer success and keep serving the old
// certificate), which status.identityPushed guards. A transient in-flight
// Secret carries the CSR (and, on the key-upload dialect, the private key)
// across the passes between generation and issuance; it is deleted the
// moment the identity verifies, so a key never outlives its push.

const (
	planHashAnnotation = labelDomain + "plan-hash"
	crNameAnnotation   = labelDomain + "certificate-request"

	identityDefaultDuration    = 2160 * time.Hour // 90 days
	identityDefaultRenewBefore = 720 * time.Hour  // 30 days
)

var certificateRequestGVK = schema.GroupVersionKind{
	Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequest",
}

// identitySecretName is the in-flight CSR/key Secret (operator namespace,
// owned by the endpoint).
func identitySecretName(epName string) string { return "endpoint-identity-" + epName }

// identityDesired is the hashed desired state — CN, SANs, issuer, subject.
// Lifetime is deliberately NOT part of it: renewal re-issues under the same
// hash (the transport is proven by the previous push having landed).
type identityDesired struct {
	CommonName  string                            `json:"commonName"`
	DNSNames    []string                          `json:"dnsNames"`
	IPAddresses []string                          `json:"ipAddresses,omitempty"`
	Issuer      metalv1alpha1.IssuerRef           `json:"issuer"`
	Subject     *metalv1alpha1.CertificateSubject `json:"subject,omitempty"`
}

func desiredIdentity(ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.IdentityPolicy) identityDesired {
	domain := pol.Domain
	if domain == "" {
		domain = "metal.internal"
	}
	cn := "bmc-" + ep.Name + "." + domain
	d := identityDesired{
		CommonName: cn,
		DNSNames:   []string{cn},
		Issuer:     pol.IssuerRef,
		Subject:    pol.Subject,
	}
	if u, err := url.Parse(ep.Spec.Address); err == nil {
		if host := u.Hostname(); net.ParseIP(host) != nil {
			// Clients dial IP literals on the island — the address rides
			// along as IP SAN.
			d.IPAddresses = []string{host}
		}
	}
	return d
}

// identity converges the manager's HTTPS identity against the profile.
func (r *EndpointConvergenceReconciler) identity(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.IdentityPolicy) (metav1.ConditionStatus, string, string, error) {
	leaf, err := servedLeaf(ctx, ep.Spec.Address)
	if err != nil {
		return metav1.ConditionUnknown, metalv1alpha1.ReasonNoReachablePath,
			"cannot observe the served certificate: " + err.Error(), nil
	}
	return r.identityWith(ctx, ep, pol, leaf)
}

// identityWith converges the identity against the certificate the manager
// serves now.
func (r *EndpointConvergenceReconciler) identityWith(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, pol *metalv1alpha1.IdentityPolicy, leaf *x509.Certificate) (metav1.ConditionStatus, string, string, error) {
	desired := desiredIdentity(ep, pol)
	hash := planHash(desired)
	renewBefore := identityDefaultRenewBefore
	if pol.RenewBefore != nil {
		renewBefore = pol.RenewBefore.Duration
	}
	// The names and dates are claims anybody can put in a certificate. The
	// identity is in sync only when the served certificate is the very one
	// the operator pushed for this desired state: issued by the named
	// issuer for the operator's own request, so its issuer and its key are
	// the expected ones. The handshake proved that the BMC holds that key.
	p := ep.Status.IdentityPushed
	pushedForThis := p != nil && p.PlanHash == hash
	servedIsPushed := pushedForThis && certFingerprint(leaf) == p.Fingerprint
	drift := identityDrift(leaf, desired, renewBefore)
	switch {
	case drift == "" && servedIsPushed:
		if err := r.cleanupIdentityInFlight(ctx, ep); err != nil {
			return "", "", "", err
		}
		return metav1.ConditionTrue, metalv1alpha1.ReasonInSync,
			fmt.Sprintf("identity %s, renews after %s", desired.CommonName,
				leaf.NotAfter.Add(-renewBefore).Format(time.RFC3339)), nil
	case pushedForThis && !servedIsPushed:
		// Pushed for exactly this desired state, yet something else is being
		// served — the push did not take (the lying-oracle shape). Held per
		// hash: re-pushing forever would reboot the manager forever.
		if drift == "" {
			drift = "the served certificate is not the pushed one"
		}
		return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushFailed,
			fmt.Sprintf("%s; pushed certificate %.8s is not being served — change the profile to retry",
				drift, p.Fingerprint), nil
	}

	dialect := bmc.DialectNoIdentity
	if caps := ep.Status.Capabilities; caps != nil {
		dialect = bmc.IdentityDialect(caps.IdentityDialect)
	}
	if dialect != bmc.DialectCSRNative && dialect != bmc.DialectKeyUpload {
		return metav1.ConditionFalse, metalv1alpha1.ReasonNoIdentityTransport,
			"endpoint advertises no certificate-replacement surface", nil
	}
	if dialect == bmc.DialectKeyUpload && !tlsVerified(ep) {
		return metav1.ConditionFalse, metalv1alpha1.ReasonTLSNotVerified,
			"the key-upload dialect sends the private key to the BMC, and spec.tls.insecureSkipVerify " +
				"checks nothing about who receives it; the key is not made. Give the endpoints a CA " +
				"(--bmc-ca-secret), or remove --bmc-insecure so that the operator pins each BMC's key", nil
	}

	sec, csrPEM, err := r.ensureIdentityInFlight(ctx, ep, desired, hash, dialect)
	if err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushFailed, err.Error(), nil
	}

	crName := ep.Name + "-identity-" + hash8("sha256:"+csrHex(csrPEM))
	if err := r.ensureCertificateRequest(ctx, ep, crName, csrPEM, pol); err != nil {
		return "", "", "", err
	}
	if sec.Annotations[crNameAnnotation] != crName {
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[crNameAnnotation] = crName
		if err := r.Update(ctx, sec); err != nil {
			return "", "", "", err
		}
	}
	chain, ready, msg, err := r.certificateRequestChain(ctx, crName)
	if err != nil {
		return "", "", "", err
	}
	if !ready {
		return metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingIssuance, msg, nil
	}

	// Pushing restarts the manager. Under a running operation that drops
	// the media it mounted (most managers do not keep them over a restart)
	// and the step in flight; the push waits for the operation instead.
	if busy, err := r.operationBehind(ctx, ep); err != nil {
		return "", "", "", err
	} else if busy != "" {
		return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushHeld,
			"the signed certificate is ready, but pushing it restarts the manager, and " + busy +
				"; the push follows when it has finished", nil
	}
	if err := issuedForRequest(chain, csrPEM); err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushFailed, err.Error(), nil
	}
	pushedLeaf, err := r.pushIdentity(ctx, ep, dialect, chain, sec.Data["key.pem"])
	if err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushFailed, err.Error(), nil
	}
	ep.Status.IdentityPushed = &metalv1alpha1.IdentityPush{
		PlanHash: hash, Fingerprint: certFingerprint(pushedLeaf),
		PublicKey: gofishclient.PublicKeyPin(pushedLeaf),
	}
	logf.FromContext(ctx).Info("pushed identity", "endpoint", ep.Name, "cn", desired.CommonName)
	return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushed,
		"identity pushed and manager restarted — the next observation verifies", nil
}

// ensureIdentityInFlight returns the in-flight Secret for this desired
// state, generating the CSR (on the manager or locally, per dialect) when
// none matches.
func (r *EndpointConvergenceReconciler) ensureIdentityInFlight(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, desired identityDesired, hash string, dialect bmc.IdentityDialect) (*corev1.Secret, []byte, error) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: identitySecretName(ep.Name), Namespace: r.CredentialsNamespace,
	}}
	err := r.Get(ctx, client.ObjectKeyFromObject(sec), sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	if err == nil && sec.Annotations[planHashAnnotation] == hash {
		return sec, sec.Data["csr.pem"], nil
	}
	// Desired state moved (or first pass): withdraw the stale request and
	// start fresh.
	if err == nil {
		if stale := sec.Annotations[crNameAnnotation]; stale != "" {
			if err := r.deleteCertificateRequest(ctx, stale); err != nil {
				return nil, nil, err
			}
		}
	}

	var csrPEM []byte
	data := map[string][]byte{}
	switch dialect {
	case bmc.DialectCSRNative:
		csrPEM, err = r.managerCSR(ctx, ep, desired)
		if err != nil {
			return nil, nil, err
		}
	case bmc.DialectKeyUpload:
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, err
		}
		var ips []net.IP
		for _, raw := range desired.IPAddresses {
			ips = append(ips, net.ParseIP(raw))
		}
		name := pkix.Name{CommonName: desired.CommonName}
		if s := desired.Subject; s != nil {
			name.Country = nonEmpty(s.Country)
			name.Province = nonEmpty(s.State)
			name.Locality = nonEmpty(s.Locality)
			name.Organization = nonEmpty(s.Organization)
			name.OrganizationalUnit = nonEmpty(s.OrganizationalUnit)
		}
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: name, DNSNames: desired.DNSNames, IPAddresses: ips,
		}, key)
		if err != nil {
			return nil, nil, err
		}
		csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
		data["key.pem"] = pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
	}
	data["csr.pem"] = csrPEM

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sec, func() error {
		if err := controllerutil.SetControllerReference(ep, sec, r.Scheme()); err != nil {
			return err
		}
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[planHashAnnotation] = hash
		delete(sec.Annotations, crNameAnnotation)
		sec.Data = data
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("in-flight identity %s: %w", sec.Name, err)
	}
	return sec, csrPEM, nil
}

// csrTimeout bounds a pass that has the manager make a key and a request.
// An on-manager 2048-bit key takes a BMC up to a minute or more; a pass
// that gives up early and starts again only restarts the generation.
const csrTimeout = 5 * time.Minute

// managerCSR has a csr-native manager build the request — the key never
// leaves it.
func (r *EndpointConvergenceReconciler) managerCSR(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, desired identityDesired) ([]byte, error) {
	cfg, _, err := endpointClientConfig(ctx, r.Client, r.CredentialsNamespace, ep, false)
	if err != nil {
		return nil, err
	}
	subject := bmc.CSRSubject{
		CommonName:  desired.CommonName,
		DNSNames:    desired.DNSNames,
		IPAddresses: desired.IPAddresses,
	}
	if s := desired.Subject; s != nil {
		subject.Country, subject.State, subject.Locality = s.Country, s.State, s.Locality
		subject.Organization, subject.OrganizationalUnit = s.Organization, s.OrganizationalUnit
	}
	var csr []byte
	err = withSessionFor(ctx, csrTimeout, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
		csr, err = cl.GenerateCSR(ctx, subject)
		return err
	})
	return csr, err
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts;hostoperations,verbs=get;list;watch

// operationBehind names an operation that runs on a machine this endpoint
// manages, or returns "". Running means approved and not finished: one that
// has started, and one about to start. An operation still waiting for a
// person can wait for days and holds nothing.
//
// It reads the cache, and an operation can start between this read and the
// restart. That leaves a window of the push itself, seconds, instead of
// every running operation.
func (r *EndpointConvergenceReconciler) operationBehind(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) (string, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return "", err
	}
	behind := map[string]bool{}
	for i := range hosts.Items {
		for _, path := range hosts.Items[i].Status.ManagedBy {
			if path.EndpointRef.Name == ep.Name {
				behind[hosts.Items[i].Name] = true
			}
		}
	}
	if len(behind) == 0 {
		return "", nil
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops); err != nil {
		return "", err
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if behind[op.Spec.HostRef.Name] && opActive(op) && op.DeletionTimestamp.IsZero() {
			return fmt.Sprintf("operation %s/%s runs on host %s", op.Namespace, op.Name, op.Spec.HostRef.Name), nil
		}
	}
	return "", nil
}

// pushIdentity delivers the signed chain per dialect and restarts the
// manager — the apply mechanism on both vendors (graceful first, force as
// the recorded fallback). Returns the pushed leaf.
func (r *EndpointConvergenceReconciler) pushIdentity(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, dialect bmc.IdentityDialect, chain, keyPEM []byte) (*x509.Certificate, error) {
	leaf, leafPEM, err := firstCertificate(chain)
	if err != nil {
		return nil, err
	}
	cfg, _, err := endpointClientConfig(ctx, r.Client, r.CredentialsNamespace, ep, false)
	if err != nil {
		return nil, err
	}
	// A private key goes only over a connection whose peer is checked: by
	// a CA or by the pinned key.
	if dialect == bmc.DialectKeyUpload && cfg.RootCAs == nil && len(cfg.PinnedKeys) == 0 {
		return nil, fmt.Errorf("refused to upload a private key over a connection that checks no certificate")
	}
	err = withSession(ctx, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
		switch dialect {
		case bmc.DialectCSRNative:
			// The full chain imports — "less than 10 levels" allowed, the
			// manager then serves it whole.
			if err := cl.ImportServerCertificate(ctx, chain); err != nil {
				return err
			}
		case bmc.DialectKeyUpload:
			// Leaf only — both X12 field implementations strip the chain.
			if keyPEM == nil {
				return fmt.Errorf("no in-flight key for the upload dialect")
			}
			if err := cl.UploadServerIdentity(ctx, leafPEM, keyPEM); err != nil {
				return err
			}
		}
		// The certificate is on the manager from here on, whether or not
		// the restart that puts it in service succeeds.
		r.Audit.BMCIdentityPushed(endpointObject(ep), certFingerprint(leaf), leaf.Subject.CommonName)
		action := bmc.ActionGracefulRestart
		if err := cl.ResetManager(ctx, action); err != nil {
			action = bmc.ActionForceRestart
			if err := cl.ResetManager(ctx, action); err != nil {
				return fmt.Errorf("manager reset refused: %w", err)
			}
		}
		r.Audit.BMCRestarted(endpointObject(ep), string(action), metalv1alpha1.ReasonIdentityPushed)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return leaf, nil
}

// cleanupIdentityInFlight withdraws the in-flight Secret and its
// CertificateRequest — called the moment the identity verifies (a key must
// not outlive its push) and when the identity stanza leaves the profile.
func (r *EndpointConvergenceReconciler) cleanupIdentityInFlight(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) error {
	sec := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.CredentialsNamespace, Name: identitySecretName(ep.Name)}
	switch err := r.Get(ctx, key, sec); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if crName := sec.Annotations[crNameAnnotation]; crName != "" {
		if err := r.deleteCertificateRequest(ctx, crName); err != nil {
			return err
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, sec))
}

// ensureCertificateRequest creates the cert-manager request when absent —
// unstructured, so the operator carries no cert-manager module for one
// object shape.
func (r *EndpointConvergenceReconciler) ensureCertificateRequest(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint, name string, csrPEM []byte, pol *metalv1alpha1.IdentityPolicy) error {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(certificateRequestGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: r.CredentialsNamespace, Name: name}, cr)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	duration := identityDefaultDuration
	if pol.Duration != nil {
		duration = pol.Duration.Duration
	}
	kind := pol.IssuerRef.Kind
	if kind == "" {
		kind = "ClusterIssuer"
	}
	cr = &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"request":  base64.StdEncoding.EncodeToString(csrPEM),
			"duration": duration.String(),
			"usages":   []any{"digital signature", "key encipherment", "server auth"},
			"issuerRef": map[string]any{
				"name": pol.IssuerRef.Name, "kind": kind, "group": "cert-manager.io",
			},
		},
	}}
	cr.SetGroupVersionKind(certificateRequestGVK)
	cr.SetNamespace(r.CredentialsNamespace)
	cr.SetName(name)
	if err := controllerutil.SetControllerReference(ep, cr, r.Scheme()); err != nil {
		return err
	}
	if err := r.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("certificaterequest %s: %w", name, err)
	}
	logf.FromContext(ctx).Info("requested identity certificate", "endpoint", ep.Name, "certificaterequest", name)
	return nil
}

// certificateRequestChain reads issuance state: the signed chain when Ready,
// otherwise a human-facing wait message.
func (r *EndpointConvergenceReconciler) certificateRequestChain(ctx context.Context, name string) ([]byte, bool, string, error) {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(certificateRequestGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.CredentialsNamespace, Name: name}, cr); err != nil {
		return nil, false, "", err
	}
	b64, _, _ := unstructured.NestedString(cr.Object, "status", "certificate")
	if b64 == "" {
		msg := fmt.Sprintf("certificaterequest %s/%s awaiting issuance", r.CredentialsNamespace, name)
		conds, _, _ := unstructured.NestedSlice(cr.Object, "status", "conditions")
		for _, c := range conds {
			cond, ok := c.(map[string]any)
			if ok && cond["type"] == "Ready" && cond["status"] != "True" {
				msg = fmt.Sprintf("%s: %v: %v", msg, cond["reason"], cond["message"])
			}
		}
		return nil, false, msg, nil
	}
	chain, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false, "", fmt.Errorf("certificaterequest %s: undecodable certificate: %w", name, err)
	}
	return chain, true, "", nil
}

func (r *EndpointConvergenceReconciler) deleteCertificateRequest(ctx context.Context, name string) error {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(certificateRequestGVK)
	cr.SetNamespace(r.CredentialsNamespace)
	cr.SetName(name)
	return client.IgnoreNotFound(r.Delete(ctx, cr))
}

// identityDrift compares the served leaf against the desired identity.
// Empty means in sync; otherwise the first drift, human-readable.
func identityDrift(leaf *x509.Certificate, desired identityDesired, renewBefore time.Duration) string {
	if leaf.Subject.CommonName != desired.CommonName {
		return fmt.Sprintf("served CN %q, want %q", leaf.Subject.CommonName, desired.CommonName)
	}
	for _, dns := range desired.DNSNames {
		if !slices.Contains(leaf.DNSNames, dns) {
			return fmt.Sprintf("served certificate lacks DNS SAN %q", dns)
		}
	}
	for _, raw := range desired.IPAddresses {
		want := net.ParseIP(raw)
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("served certificate lacks IP SAN %q", raw)
		}
	}
	if remaining := time.Until(leaf.NotAfter); remaining < renewBefore {
		return fmt.Sprintf("served certificate expires %s — inside the renewal window", leaf.NotAfter.Format(time.RFC3339))
	}
	return ""
}

// tlsVerified reports whether sessions to the endpoint check the BMC: by a
// CA or by the pinned key. Only insecureSkipVerify checks nothing.
func tlsVerified(ep *metalv1alpha1.ManagementEndpoint) bool {
	t := ep.Spec.TLS
	return t == nil || t.CASecretRef != nil || !t.InsecureSkipVerify
}

// issuedForRequest checks the issued chain before it goes to the manager:
// the leaf carries the key of the operator's request, and where the chain
// holds the issuer's certificate, the issuer's signature verifies.
func issuedForRequest(chainPEM, csrPEM []byte) error {
	certs, err := cert.ParseCertsPEM(chainPEM)
	if err != nil {
		return fmt.Errorf("issued chain: %w", err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return fmt.Errorf("in-flight request: no PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("in-flight request: %w", err)
	}
	if !bytes.Equal(certs[0].RawSubjectPublicKeyInfo, csr.RawSubjectPublicKeyInfo) {
		return fmt.Errorf("the issued certificate does not carry the key of the request; it is not pushed")
	}
	if len(certs) > 1 {
		if err := certs[0].CheckSignatureFrom(certs[1]); err != nil {
			return fmt.Errorf("the issued certificate is not signed by the next certificate of its chain: %w", err)
		}
	}
	return nil
}

func certFingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func csrHex(csrPEM []byte) string {
	sum := sha256.Sum256(csrPEM)
	return hex.EncodeToString(sum[:])
}

// firstCertificate extracts the chain's leaf, parsed and re-encoded. Every
// certificate in the chain must parse: the whole chain goes to the manager,
// so a broken intermediate is refused here rather than pushed.
func firstCertificate(chainPEM []byte) (*x509.Certificate, []byte, error) {
	certs, err := cert.ParseCertsPEM(chainPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("issued chain: %w", err)
	}
	leafPEM, err := cert.EncodeCertificates(certs[0])
	if err != nil {
		return nil, nil, err
	}
	return certs[0], leafPEM, nil
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
