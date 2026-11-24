package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
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
	desired := desiredIdentity(ep, pol)
	hash := planHash(desired)

	leaf, err := servedLeaf(ctx, ep.Spec.Address)
	if err != nil {
		return metav1.ConditionUnknown, metalv1alpha1.ReasonNoReachablePath,
			"cannot observe the served certificate: " + err.Error(), nil
	}
	renewBefore := identityDefaultRenewBefore
	if pol.RenewBefore != nil {
		renewBefore = pol.RenewBefore.Duration
	}
	if drift := identityDrift(leaf, desired, renewBefore); drift == "" {
		if err := r.cleanupIdentityInFlight(ctx, ep); err != nil {
			return "", "", "", err
		}
		return metav1.ConditionTrue, metalv1alpha1.ReasonInSync,
			fmt.Sprintf("identity %s, renews after %s", desired.CommonName,
				leaf.NotAfter.Add(-renewBefore).Format(time.RFC3339)), nil
	} else if p := ep.Status.IdentityPushed; p != nil && p.PlanHash == hash && certFingerprint(leaf) != p.Fingerprint {
		// Pushed for exactly this desired state, yet something else is being
		// served — the push did not take (the lying-oracle shape). Held per
		// hash: re-pushing forever would reboot the manager forever.
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
	pushedLeaf, err := r.pushIdentity(ctx, ep, dialect, chain, sec.Data["key.pem"])
	if err != nil {
		return metav1.ConditionFalse, metalv1alpha1.ReasonIdentityPushFailed, err.Error(), nil
	}
	ep.Status.IdentityPushed = &metalv1alpha1.IdentityPush{
		PlanHash: hash, Fingerprint: certFingerprint(pushedLeaf),
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
	err = withSession(ctx, r.Dial, cfg, func(ctx context.Context, cl bmc.Client) error {
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
		if err := cl.ResetManager(ctx, bmc.ActionGracefulRestart); err != nil {
			if err := cl.ResetManager(ctx, bmc.ActionForceRestart); err != nil {
				return fmt.Errorf("manager reset refused: %w", err)
			}
		}
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

// servedLeaf grabs the certificate the endpoint currently presents — the
// identity lane's convergence observable. Verification is deliberately off:
// this reads identity claims, it does not trust them.
func servedLeaf(ctx context.Context, address string) (*x509.Certificate, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec // observation, not trust
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck // read-only probe
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no peer certificate presented")
	}
	return certs[0], nil
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
