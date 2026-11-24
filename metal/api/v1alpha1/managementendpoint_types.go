package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EndpointPhase summarizes an endpoint's conditions, in the order an endpoint
// normally moves through them: Pending until it is reachable, then Connecting,
// Inventorying and Verifying as the cluster learns what is there, Configuring
// while settings are applied, and Ready. Unreachable and Mismatch are the two
// stops — the first means nobody answered, the second that what answered was not
// the machine the Asset described.
// +kubebuilder:validation:Enum=Pending;Connecting;Inventorying;Verifying;Configuring;Ready;Unreachable;Mismatch
type EndpointPhase string

const (
	EndpointPending      EndpointPhase = "Pending"
	EndpointConnecting   EndpointPhase = "Connecting"
	EndpointInventorying EndpointPhase = "Inventorying"
	EndpointVerifying    EndpointPhase = "Verifying"
	EndpointConfiguring  EndpointPhase = "Configuring"
	EndpointReady        EndpointPhase = "Ready"
	EndpointUnreachable  EndpointPhase = "Unreachable"
	EndpointMismatch     EndpointPhase = "Mismatch"
)

// ManagementEndpointSpec is written by the operator from the Asset and the
// ledger; you do not edit it.
type ManagementEndpointSpec struct {
	// AssetRef names the Asset this endpoint was made from, and whose serial
	// and MAC it is checked against.
	AssetRef Reference `json:"assetRef"`

	// Address is the BMC's https URL, from the ledger. It is how the operator
	// reaches the machine, not what identifies it: a Host is identified by
	// the serial number the BMC reports.
	// +kubebuilder:validation:Pattern=`^https://`
	Address string `json:"address"`

	// CredentialsRef names the Secret (operator namespace) used to connect,
	// defaulted from the Asset at materialization — the factory credentials,
	// kept as standing break-glass. Once an EndpointProfile rotates the
	// endpoint onto a managed account, every connect prefers the managed
	// Secret and this ref is the repair fallback.
	// +optional
	CredentialsRef *corev1.LocalObjectReference `json:"credentialsRef,omitempty"`

	// TLS says how the BMC's HTTPS certificate is checked, from the manager's
	// --bmc-ca-secret or --bmc-insecure. Unset uses the system's trusted CAs.
	// +optional
	TLS *EndpointTLS `json:"tls,omitempty"`
}

// EndpointTLS says how a BMC's HTTPS certificate is checked.
type EndpointTLS struct {
	// CASecretRef names a Secret (operator namespace, key ca.crt) with the
	// CA that signed the BMC's certificate. Wins over insecureSkipVerify.
	// +optional
	CASecretRef *corev1.LocalObjectReference `json:"caSecretRef,omitempty"`

	// InsecureSkipVerify accepts any certificate. For simulated BMCs in the
	// lab only.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// EndpointCapabilities is what this BMC can do, as discovered. Vendors differ
// here, and the operator records the difference instead of assuming it.
type EndpointCapabilities struct {
	// DualMedia: two virtual media slots can hold an image at the same time
	// (Supermicro X12 yes, Huawei iBMC no) — needed to mount the installer
	// and the node's config together.
	// +optional
	DualMedia bool `json:"dualMedia,omitempty"`

	// MediaDialects maps media slot id → insert dialect
	// ("insert-media" | "vmm-control" | "none").
	// +optional
	MediaDialects map[string]string `json:"mediaDialects,omitempty"`

	// UpdateDialect is the firmware-update method the BMC offers
	// ("multipart-push" | "simple-update" | "none"). The operator proposes a
	// firmware flash only over multipart-push, the one it has been tested
	// with.
	// +optional
	UpdateDialect string `json:"updateDialect,omitempty"`

	// IdentityDialect is how the manager's HTTPS certificate is replaced
	// ("csr-native" | "key-upload" | "none"): the iBMC generates its CSR on
	// the manager — the key never leaves it — while the X12 takes a
	// locally-generated leaf and key over an OEM upload.
	// +optional
	IdentityDialect string `json:"identityDialect,omitempty"`
}

// IdentityPush records what was last pushed to a management controller, keyed by
// the hash of what was wanted at the time. Everything else about identity is
// re-derived from the certificate the controller actually serves; this exists for
// the one thing that cannot be observed — a push the controller reported success
// for but never began serving — so that it is not pushed again forever.
type IdentityPush struct {
	// PlanHash of the desired identity the push belonged to.
	PlanHash string `json:"planHash"`
	// Fingerprint (sha256, hex) of the pushed leaf certificate.
	Fingerprint string `json:"fingerprint"`
}

// FirmwareInventoryItem is one UpdateService SoftwareInventory member as
// discovered. IDs are vendor-shaped (X12: BMC/Backup_BMC/…, iBMC:
// ActiveBMC/…) and kept opaque.
type FirmwareInventoryItem struct {
	ID string `json:"id"`
	// +optional
	Version string `json:"version,omitempty"`
	// +optional
	Updateable bool `json:"updateable,omitempty"`
}

// DiscoveredSystem is one ComputerSystem as seen through this endpoint — the
// projection the HostReconciler joins into identity-keyed Hosts. ID is a path
// coordinate (an opaque Redfish member id), never an identity; identity is
// normalized in internal/identity over the serial anchors.
type DiscoveredSystem struct {
	// ID is the Redfish ComputerSystem member id behind this endpoint.
	ID string `json:"id"`

	// +optional
	UUID string `json:"uuid,omitempty"`

	// SerialNumber as served by the ComputerSystem — the Huawei anchor; empty on
	// Supermicro X12 (there the chassis board serial anchors).
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`

	// Manufacturer as served by the ComputerSystem — the machine's vendor, which
	// on an aggregator path differs from the endpoint's vendor.
	// +optional
	Manufacturer string `json:"manufacturer,omitempty"`

	// +optional
	Model string `json:"model,omitempty"`

	// LastResetTime as served by the ComputerSystem — the baseline-and-advance
	// observable (X12 serves it, iBMC doesn't). Micro precision so back-to-back
	// resets stay distinguishable.
	// +optional
	LastResetTime *metav1.MicroTime `json:"lastResetTime,omitempty"`

	// BootTargets are the boot override targets the system allows
	// (BootSourceOverrideTarget@Redfish.AllowableValues) — what a proposed
	// boot override picks from, never assumed.
	// +listType=atomic
	// +optional
	BootTargets []string `json:"bootTargets,omitempty"`

	// BootOverrideMode is the BootSourceOverrideMode the system serves
	// ("UEFI" | "Legacy"); empty when it serves none. A boot override asks for
	// UEFI only where this says otherwise — the X12 reports Legacy on a
	// machine that boots UEFI, and a system that serves no mode may refuse one.
	// +optional
	BootOverrideMode string `json:"bootOverrideMode,omitempty"`
}

// DiscoveredMedia is one virtual media slot as the endpoint lists it.
type DiscoveredMedia struct {
	// ID is the slot's Redfish member id ("CD", "VirtualMedia1", …).
	ID string `json:"id"`

	// MediaTypes the slot advertises ("CD", "DVD", "USBStick", …). Empty says
	// nothing — the iBMC's CD slot serves none.
	// +listType=atomic
	// +optional
	MediaTypes []string `json:"mediaTypes,omitempty"`
}

// ManagementEndpointStatus is what the BMC reports; what your records say is
// on the Asset.
type ManagementEndpointStatus struct {
	// ManagerType is the discovered Redfish Manager type, kept opaque
	// (BMC | EnclosureManager | RackManager | …).
	// +optional
	ManagerType string `json:"managerType,omitempty"`

	// Vendor is the discovered manufacturer ("Huawei" | "Supermicro" | …).
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// +optional
	Model string `json:"model,omitempty"`

	// FirmwareVersion is the manager's firmware as served.
	// +optional
	FirmwareVersion string `json:"firmwareVersion,omitempty"`

	// BoardSerial is the chassis board serial — the Supermicro identity anchor
	// (its ComputerSystem serves none; Huawei's matches the system serial).
	// +optional
	BoardSerial string `json:"boardSerial,omitempty"`

	// Firmware lists the UpdateService inventory — the firmware-convergence
	// observable (desired version vs the entries here), small and bounded
	// (~5 members per vendor). Sorted by id.
	// +listType=atomic
	// +optional
	Firmware []FirmwareInventoryItem `json:"firmware,omitempty"`

	// Systems lists the machines (Redfish ComputerSystems) behind this BMC —
	// usually one, several for a chassis with more than one node. It is
	// kept like the rest of status: an unreachable endpoint keeps its
	// last view.
	// +listType=atomic
	// +optional
	Systems []DiscoveredSystem `json:"systems,omitempty"`

	// +optional
	Capabilities *EndpointCapabilities `json:"capabilities,omitempty"`

	// VirtualMedia lists the media slots in the endpoint's collection order,
	// with the media types each advertises — what a boot picks its slots by
	// (optical first, in this order), next to capabilities.mediaDialects,
	// which says how each is inserted.
	// +listType=atomic
	// +optional
	VirtualMedia []DiscoveredMedia `json:"virtualMedia,omitempty"`

	// Asset caches the last-resolved assertion, so an asset-source outage
	// degrades to the snapshot rather than failing resolution.
	// +optional
	Asset *AssetSnapshot `json:"asset,omitempty"`

	// IdentityPushed is written by the endpoint convergence reconciler
	// alone; the observing endpoint reconciler leaves it untouched — the
	// two share this status with disjoint fields.
	// +optional
	IdentityPushed *IdentityPush `json:"identityPushed,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the conditions below for people and for kubectl columns.
	// It is a summary, not the contract: automation should read conditions.
	// +optional
	Phase EndpointPhase `json:"phase,omitempty"`

	// BlockedOn is derived — why the endpoint isn't advancing (e.g. SerialMismatch).
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// ManagementEndpoint is one BMC — iDRAC, iLO, XCC, iBMC or any other Redfish
// controller — that the operator talks to. The operator creates it from an
// Asset; you read it to see whether the BMC answers and what it reports
// (kubectl get mep).
//
// Its status holds what the BMC reports: vendor, model, serial numbers, the
// machines behind it, virtual media slots and firmware. The Verified
// condition compares that with the Asset. Phase Mismatch means the BMC at
// this address is not the machine the Asset describes, and the operator does
// nothing to it; Unreachable means nothing answered at the address.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mep,categories=metal
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Vendor",type=string,JSONPath=`.status.vendor`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Blocked",type=string,JSONPath=`.status.blockedOn`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ManagementEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ManagementEndpointSpec `json:"spec"`
	// +optional
	Status ManagementEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ManagementEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagementEndpoint `json:"items"`
}
