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

	// BMCMac is the MAC address of the BMC's own network port, as printed on
	// the label or shown by `ipmitool lan print`. Either case, with ':' or '-'
	// between the bytes. The operator looks it up in the ledger to find the
	// BMC's address, and checks that the BMC at that address reports it.
	// +kubebuilder:validation:MaxLength=17
	// +kubebuilder:validation:XValidation:rule="self.matches('^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$') || self.matches('^[0-9A-Fa-f]{2}(-[0-9A-Fa-f]{2}){5}$')",message="bmcMac must be a MAC address such as 0c:c4:7a:12:34:56 (upper case and '-' separators are fine)"
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
// Its status says one thing only: whether the operator could turn it into a
// ManagementEndpoint (condition Materialized), and if not, why — most often a
// bmcMac that is not in the ledger. Everything the cluster learns by looking at
// the machine lands on the ManagementEndpoint and Host. Those kinds keep a copy
// of the Asset spec they last resolved, so an outage of whatever authors Assets
// degrades to the last known values instead of stopping work.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="CID",type=string,JSONPath=`.spec.cid`
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=`.spec.serial`
// +kubebuilder:printcolumn:name="BMC MAC",type=string,JSONPath=`.spec.bmcMac`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.conditions[?(@.type=="Materialized")].reason`
// +kubebuilder:printcolumn:name="VLAN",type=integer,JSONPath=`.spec.vlan`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Asset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AssetSpec `json:"spec"`
	// +optional
	Status AssetStatus `json:"status,omitempty"`
}

// AssetStatus says whether the Asset became a ManagementEndpoint.
type AssetStatus struct {
	// Conditions: Materialized. True names the endpoint; False says what is
	// missing (NoBMCMac, NoLedgerEntry, LedgerEntryInvalid,
	// MaterializeConflict) and how to fix it.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
