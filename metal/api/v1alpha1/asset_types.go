package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AssetSpec is the org's assertion about one physical machine — what the CMDB *claims*,
// never what Redfish discovered (the two must stay independent origins or Verified
// compares Redfish to Redfish and can catch nothing). Filled today by Flux from git
// YAML; filled later by fnt-sync from FNT Command — same CR downstream.
type AssetSpec struct {
	// CID is the org's durable identity for this machine (FNT Configuration Item ID).
	// +kubebuilder:validation:MinLength=1
	CID string `json:"cid"`

	// Serial is the org-asserted chassis/board serial — the Verify join key, checked
	// against discovery (assertion vs Redfish, two origins).
	// +kubebuilder:validation:MinLength=1
	Serial string `json:"serial"`

	// BMCMac is the management controller's own NIC MAC (the dnsmasq-ledger identity
	// on the 1034 island), lowercase colon-separated.
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`
	// +optional
	BMCMac string `json:"bmcMac,omitempty"`

	// VLAN is the management network the endpoint lives on (1034 servers, 1035 routers).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	// +optional
	VLAN int `json:"vlan,omitempty"`

	// CredentialsRef names a Secret in the operator's namespace holding the endpoint
	// credentials — factory creds at adoption, rotated by EndpointProfile.
	CredentialsRef corev1.LocalObjectReference `json:"credentialsRef"`
}

// Asset is what the organization asserts about one machine: the serial it was
// bought under, the MAC of its management controller, where it was installed. It
// is the entry point — creating an Asset is how a machine becomes known to the
// cluster at all.
//
// It has no status, because nothing about it is discovered. Everything the cluster
// learns by looking lands on the ManagementEndpoint and Host it materializes into.
// Those kinds keep a copy of the Asset spec they last resolved, so an outage of
// whatever authors Assets degrades to the last known values instead of stopping
// work.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="CID",type=string,JSONPath=`.spec.cid`
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=`.spec.serial`
// +kubebuilder:printcolumn:name="VLAN",type=integer,JSONPath=`.spec.vlan`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Asset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AssetSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type AssetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Asset `json:"items"`
}

// AssetSnapshot is the consumer-side cache of the last-resolved Asset spec.
type AssetSnapshot struct {
	CID    string `json:"cid,omitempty"`
	Serial string `json:"serial,omitempty"`
	BMCMac string `json:"bmcMac,omitempty"`
	VLAN   int    `json:"vlan,omitempty"`
	// ObservedGeneration is the Asset generation this snapshot was resolved at.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
