package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AssetSpec is what your records say about the machine — never what the
// operator found by looking. Keeping the two apart is what lets the operator
// catch a mistake: it compares what you wrote here with what the BMC reports.
type AssetSpec struct {
	// CID is your asset tag or CMDB id for the machine; any unique string.
	// +kubebuilder:validation:MinLength=1
	CID string `json:"cid"`

	// Serial is the machine's serial number as your records have it: the
	// chassis or board serial on the label. The operator compares it with
	// the serial the BMC reports, and does nothing to the machine if they
	// differ.
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

	// VLAN is the management network the BMC is on. It is used only to pick
	// matching EndpointProfiles; the operator does not configure any network.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	// +optional
	VLAN int `json:"vlan,omitempty"`

	// CredentialsRef names a Secret in the operator's namespace with the
	// BMC's login (keys username and password), usually the factory default.
	// An EndpointProfile can later move the BMC to a managed account; this
	// login then stays as the way back in.
	CredentialsRef corev1.LocalObjectReference `json:"credentialsRef"`
}

// Asset is one physical machine as your records know it: its serial number and
// the MAC address of its BMC. You create one per machine, by hand or from your
// CMDB or inventory export. It is the entry point: the operator does nothing
// with a machine until an Asset names it.
//
// The operator finds the address of the BMC in the ledger from the bmcMac, and
// creates a ManagementEndpoint. Through that BMC it finds the machine and
// creates a Host. It checks that the serial and the MAC that it finds agree
// with this spec. Thus a cabling or ledger mistake stops the work, and the
// operator does not touch the wrong machine.
//
// The status tells one thing only: whether the operator could make a
// ManagementEndpoint from the Asset (condition Materialized). If not, it tells
// why. Usually the bmcMac is not in the ledger. All that the cluster learns
// from the machine goes on the ManagementEndpoint and the Host. These kinds
// keep a copy of the Asset spec that they last read. If the source of the
// Assets is not available, they use the last known values and the work goes on.
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

// AssetSnapshot is the copy of the Asset spec that the endpoint last used. It
// keeps work going on the last known values if the Asset is briefly gone.
type AssetSnapshot struct {
	CID    string `json:"cid,omitempty"`
	Serial string `json:"serial,omitempty"`
	BMCMac string `json:"bmcMac,omitempty"`
	VLAN   int    `json:"vlan,omitempty"`
	// ObservedGeneration is the Asset generation this snapshot was resolved at.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
