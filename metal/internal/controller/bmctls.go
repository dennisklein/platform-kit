package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
)

// How the operator trusts a BMC's TLS key when no CA checks it: on first
// use. The observing pass reads the certificate the BMC presents, with no
// credential sent, and the first key it sees becomes status.tlsPin. Every
// session after that accepts only the pinned key (and the key of a
// certificate the operator itself pushed), so a device that poses as the BMC
// later gets no password. A BMC that presents another key holds the endpoint
// Unreachable (reason TLSKeyChanged) until a person approves that key with
// the annotation metal.pk.io/approve-tls-key.

// TLSProbe reads the certificate an address presents, without a credential
// and without checking it.
type TLSProbe func(ctx context.Context, address string) (*x509.Certificate, error)

func (p TLSProbe) orDefault() TLSProbe {
	if p == nil {
		return servedLeaf
	}
	return p
}

// pinsKeys reports whether the endpoint's TLS policy is the key pin: no CA
// checks the BMC and the check is not skipped.
func pinsKeys(ep *metalv1alpha1.ManagementEndpoint) bool {
	t := ep.Spec.TLS
	return t == nil || (t.CASecretRef == nil && !t.InsecureSkipVerify)
}

// endpointPins are the keys a session to the endpoint accepts: the pinned
// one, and the key of the certificate the operator pushed. The second lets
// the convergence lane reach the BMC right after the push, before the
// observing pass has moved the pin.
func endpointPins(ep *metalv1alpha1.ManagementEndpoint) []string {
	var pins []string
	if p := ep.Status.TLSPin; p != nil && p.PublicKey != "" {
		pins = append(pins, p.PublicKey)
	}
	if p := ep.Status.IdentityPushed; p != nil && p.PublicKey != "" {
		pins = append(pins, p.PublicKey)
	}
	return pins
}

// errNotPinned: a session would need a pinned key, and none is pinned yet.
func errNotPinned(ep *metalv1alpha1.ManagementEndpoint) error {
	return fmt.Errorf("no TLS key of endpoint %s is pinned yet, and no CA checks it; "+
		"the observing pass pins the key the BMC presents first", ep.Name)
}

// checkPin decides, before any credential goes to the BMC, whether the key
// it presents is trusted, and moves the pin where that is allowed. It
// returns false, with the Reachable condition set, when the pass must not
// dial.
func (r *ManagementEndpointReconciler) checkPin(ctx context.Context, ep *metalv1alpha1.ManagementEndpoint) bool {
	unreachable := func(reason, msg string) bool {
		setCondition(&ep.Status.Conditions, ep.Generation, metalv1alpha1.ConditionReachable,
			metav1.ConditionFalse, reason, msg)
		return false
	}
	leaf, err := r.Probe.orDefault()(ctx, ep.Spec.Address)
	if err != nil {
		return unreachable("DialError", "cannot read the BMC's certificate: "+err.Error())
	}
	presented := gofishclient.PublicKeyPin(leaf)
	pin := func(source string) {
		ep.Status.TLSPin = &metalv1alpha1.TLSPin{PublicKey: presented, Source: source}
		logf.FromContext(ctx).Info("pinned the BMC's TLS key", "endpoint", ep.Name, "key", presented, "source", source)
	}
	cur := ep.Status.TLSPin
	switch {
	case cur != nil && cur.PublicKey == presented:
	case cur == nil || cur.PublicKey == "":
		pin(metalv1alpha1.TLSPinFirstUse)
	case ep.Status.IdentityPushed != nil && ep.Status.IdentityPushed.PublicKey == presented:
		pin(metalv1alpha1.TLSPinPushed)
	case ep.Annotations[metalv1alpha1.AnnotationApproveTLSKey] == presented:
		pin(metalv1alpha1.TLSPinApproved)
	default:
		return unreachable(metalv1alpha1.ReasonTLSKeyChanged, fmt.Sprintf(
			"the BMC presents TLS key %s, but key %s is pinned (%s); the operator sends it nothing. "+
				"Check the BMC. If the change is expected (a new certificate, a replaced board), approve the key: "+
				"kubectl annotate managementendpoint %s %s=%s",
			presented, cur.PublicKey, cur.Source, ep.Name, metalv1alpha1.AnnotationApproveTLSKey, presented))
	}
	return true
}

// approvalDone reports whether the approve annotation names the key that is
// pinned already, as stored before this pass: the approval is used up, and
// the annotation can go.
func approvalDone(orig *metalv1alpha1.ManagementEndpoint) bool {
	v, ok := orig.Annotations[metalv1alpha1.AnnotationApproveTLSKey]
	return ok && orig.Status.TLSPin != nil && orig.Status.TLSPin.PublicKey == v
}

// servedLeaf grabs the certificate the endpoint currently presents. It
// sends no credential, and it checks nothing: the caller compares what it
// read with what it trusts.
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
