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

// ManagementEndpointSpec is written by the operator when it materializes an Asset —
// pure infrastructure, no claim (adoption is implied by a HostClaim on the Asset).
type ManagementEndpointSpec struct {
	// AssetRef names the Asset this endpoint materializes and verifies against.
	AssetRef Reference `json:"assetRef"`

	// Address is the endpoint's base URL — an access path, never an identity
	// (the multipath discipline: identity lives on Host.status.identity).
	// +kubebuilder:validation:Pattern=`^https://`
	Address string `json:"address"`

	// CredentialsRef names the Secret (operator namespace) used to connect,
	// defaulted from the Asset at materialization — the factory credentials,
	// kept as standing break-glass. Once an EndpointProfile rotates the
	// endpoint onto a managed account, every connect prefers the managed
	// Secret and this ref is the repair fallback.
	// +optional
	CredentialsRef *corev1.LocalObjectReference `json:"credentialsRef,omitempty"`

	// TLS pins how the endpoint's server certificate is verified. The hardware-PoC verdict:
	// both vendors serve internal-ca identities (CN=bmc-<node>.metal.internal),
	// so live endpoints pin that CA; unset falls back to the system trust store.
	// +optional
	TLS *EndpointTLS `json:"tls,omitempty"`
}

// EndpointTLS is the endpoint's server-certificate verification policy.
type EndpointTLS struct {
	// CASecretRef names a Secret (operator namespace, key ca.crt) whose bundle
	// verifies the endpoint's certificate. Wins over insecureSkipVerify.
	// +optional
	CASecretRef *corev1.LocalObjectReference `json:"caSecretRef,omitempty"`

	// InsecureSkipVerify accepts any certificate — the lab/sim fallback, never
	// the live default (identity is solved: internal-ca on every BMC).
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// EndpointCapabilities is what the endpoint can DO — vendor asymmetries live in data,
// not types (the HOW-is-data rule). Values mirror internal/bmc, grounded in the hardware PoC.
type EndpointCapabilities struct {
	// DualMedia: two virtual media slots hold inserts concurrently (X12 yes,
	// iBMC no) — gates the `secondary` config-delivery strategy.
	// +optional
	DualMedia bool `json:"dualMedia,omitempty"`

	// MediaDialects maps media slot id → insert dialect
	// ("insert-media" | "vmm-control" | "none").
	// +optional
	MediaDialects map[string]string `json:"mediaDialects,omitempty"`

	// UpdateDialect is the firmware-update transport the endpoint advertises
	// ("multipart-push" | "simple-update" | "none"). Only multipart-push is
	// proven on our hardware — the convergence reconciler proposes a flash
	// nowhere else.
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

// ManagementEndpointStatus is discovered truth (assertion lives on the Asset).
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

	// Systems lists the ComputerSystems this endpoint fronts (1 today, N-ready) —
	// cached discovery like the rest of status: an unreachable endpoint keeps its
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

// ManagementEndpoint is one realized Redfish manager (any managerType — BMC was too
// narrow). Cluster-scoped; owned by the operator; verification = assertion vs discovery.
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
