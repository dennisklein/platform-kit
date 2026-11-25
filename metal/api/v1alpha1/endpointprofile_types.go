package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EndpointProfileSpec is a shared endpoint-configuration class: it selects
// ManagementEndpoints by label and declares how the manager itself is run —
// the operator's own account, the manager's time sources. Overlap follows
// the profile merge rules: profiles order by (layer, name), a later layer
// overrides a whole stanza, same-layer disagreement on a stanza holds the
// endpoint.
type EndpointProfileSpec struct {
	// Match selects ManagementEndpoints by label. Endpoints carry only
	// discovered facts (vendor, model, vlan) — there is no claim, hence no
	// intent axis. An empty selector matches every endpoint.
	Match metav1.LabelSelector `json:"match"`

	// Layer orders overlapping profiles: higher layers override lower ones,
	// stanza by stanza. Profiles on the same layer must agree.
	// +kubebuilder:default=0
	// +optional
	Layer int32 `json:"layer,omitempty"`

	// Credentials makes the operator run its own account on the manager:
	// created through the asset's factory credentials, password kept in a
	// managed Secret, rotated on cadence. A new password is kept beside the
	// working one (keys next-username, next-password) and replaces it only
	// after a login with it succeeds, so a manager that refuses it leaves the
	// working password in place. When the username changes, the account it
	// replaces is disabled once the new one logs in. The factory account is
	// left untouched: it is the way back in after the managed password was
	// changed behind the operator's back, and the way in at adoption.
	// +optional
	Credentials *CredentialsPolicy `json:"credentials,omitempty"`

	// NTP converges the manager's own clock sources. A skewed BMC clock is
	// not cosmetic: it rejects freshly-issued certificates (NotBefore in its
	// future) and mistimes every log line.
	// +optional
	NTP *NTPPolicy `json:"ntp,omitempty"`

	// TLS runs the manager's HTTPS identity: a cert-manager-signed
	// certificate for bmc-<endpoint>.<domain>, pushed over the vendor's
	// dialect and renewed on cadence. The factory certificate is replaced
	// and cannot be restored — a CA-signed identity is strictly better and
	// the intended end state. A push restarts the manager, which drops its
	// virtual media, so it waits while an approved operation on a machine
	// behind the endpoint has not finished (reason IdentityPushHeld).
	// +optional
	TLS *IdentityPolicy `json:"tls,omitempty"`
}

// CredentialsPolicy is the managed-account stanza of an EndpointProfile.
type CredentialsPolicy struct {
	// Username of the operator's managed account (role Administrator — the
	// executor flashes firmware and resets the manager over it).
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,15}$`
	Username string `json:"username"`

	// MaxAge rotates the managed password when the Secret grows older than
	// this; 90 days when unset. A copy of the Secret therefore stops
	// working at the latest after this time. A rotation sets the new
	// password through the managed account's own session first, so it
	// works when the factory login no longer does.
	// +kubebuilder:default="2160h"
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

// NTPPolicy is the time-source stanza of an EndpointProfile. Declaring it
// enables NTP on the manager against exactly these servers.
type NTPPolicy struct {
	// Servers, in preference order. At most three — the iBMC carries exactly
	// three server slots.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:validation:items:MinLength=1
	Servers []string `json:"servers"`
}

// IdentityPolicy is the HTTPS-identity stanza of an EndpointProfile.
type IdentityPolicy struct {
	// IssuerRef names the cert-manager issuer (group cert-manager.io) that
	// signs the identity.
	IssuerRef IssuerRef `json:"issuerRef"`

	// Domain completes the identity: CN and DNS SAN are
	// bmc-<endpoint>.<domain>; the endpoint address, when an IP literal,
	// is added as an IP SAN (clients often reach BMCs by IP address).
	// +kubebuilder:default="metal.internal"
	// +kubebuilder:validation:MinLength=1
	// +optional
	Domain string `json:"domain,omitempty"`

	// Duration of issued certificates.
	// +kubebuilder:default="2160h"
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// RenewBefore re-issues when less than this remains on the served
	// certificate. Must stay below duration.
	// +kubebuilder:default="720h"
	// +optional
	RenewBefore *metav1.Duration `json:"renewBefore,omitempty"`

	// Subject carries the organizational attributes of the identity. The
	// iBMC's on-manager CSR generation requires country; everything else is
	// cosmetic.
	// +optional
	Subject *CertificateSubject `json:"subject,omitempty"`
}

// IssuerRef names a cert-manager issuer.
type IssuerRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +kubebuilder:default=ClusterIssuer
	// +optional
	Kind string `json:"kind,omitempty"`
}

// CertificateSubject is the organizational part of a certificate subject.
type CertificateSubject struct {
	// +optional
	Country string `json:"country,omitempty"`
	// +optional
	State string `json:"state,omitempty"`
	// +optional
	Locality string `json:"locality,omitempty"`
	// +optional
	Organization string `json:"organization,omitempty"`
	// +optional
	OrganizationalUnit string `json:"organizationalUnit,omitempty"`
}

// EndpointProfile is how management controllers themselves should be configured —
// their users, network settings, time sources — authored in git and applied to
// whichever endpoints match its selector.
//
// Unlike BIOS and firmware there is no approval step: nothing here disturbs the
// machine the controller is attached to, so merging the profile is the approval.
// When something cannot be applied the endpoint holds and says so on its
// Configured condition rather than retrying into a broken state.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Layer",type=integer,JSONPath=`.spec.layer`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EndpointProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EndpointProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type EndpointProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EndpointProfile `json:"items"`
}
