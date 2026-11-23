package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// HostPhase summarizes a machine's conditions together with whether it is
// claimed. Enrolling and Inspecting mean the cluster is still finding out what
// the machine is; Available means it is understood and free; Assigned means
// something claimed it; Provisioning means work is happening to it right now and
// Provisioned that the work finished; Releasing means it is being handed back.
// Failed and Maintenance are the two stops.
//
// Assigned and Provisioning are deliberately separate. A machine can be spoken
// for without anything disruptive happening to it, and conflating the two leaves
// no way to say so.
// +kubebuilder:validation:Enum=Enrolling;Inspecting;Available;Assigned;Provisioning;Provisioned;Releasing;Failed;Maintenance
type HostPhase string

const (
	HostEnrolling    HostPhase = "Enrolling"
	HostInspecting   HostPhase = "Inspecting"
	HostAvailable    HostPhase = "Available"
	HostAssigned     HostPhase = "Assigned"
	HostProvisioning HostPhase = "Provisioning"
	HostProvisioned  HostPhase = "Provisioned"
	HostReleasing    HostPhase = "Releasing"
	HostFailed       HostPhase = "Failed"
	HostMaintenance  HostPhase = "Maintenance"
)

// HostSpec carries the claim bind and the OS payload hand-off. Both are stamped
// by the HostClaimReconciler (single writer); everything else about a Host is
// discovered, not authored.
type HostSpec struct {
	// ClaimRef is the authoritative bind pointer (PV-style): set by the
	// HostClaimReconciler, first writer wins via apiserver optimistic concurrency.
	// The UID pins the exact claim object — a deleted-and-recreated claim of the
	// same name is a different claimant.
	// +optional
	ClaimRef *ClaimReference `json:"claimRef,omitempty"`

	// OS is the operating system this machine should be running, copied here
	// from the claim that bound it and cleared when the claim is released.
	// Setting it is what starts provisioning: until it is present, a bound
	// machine is reserved and idle.
	// +optional
	OS *HostOS `json:"os,omitempty"`
}

// HostOS is the osRef edge into the os API group.
type HostOS struct {
	// NodeRef points at the TalosNode (os.pk.io, cluster-scoped like
	// the Host) whose rendered config this machine boots.
	NodeRef Reference `json:"nodeRef"`
}

// ClaimReference points a Host at the namespaced HostClaim holding it.
type ClaimReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// HostIdentity is the vendor-normalized anchor — independent of access path (the
// multipath discipline). Computed by a per-vendor strategy over bmc.Client: Huawei's
// ComputerSystem carries the chassis serial, Supermicro's carries none (the board
// serial lives on Chassis); the normalizer must return byte-identical identity for a
// system reached via two managers (the identity-invariant test).
type HostIdentity struct {
	// +optional
	UUID string `json:"uuid,omitempty"`
	// Serial is the normalized serial anchor (chassis board serial on Supermicro,
	// system serial on Huawei).
	// +optional
	Serial string `json:"serial,omitempty"`
	// +optional
	Vendor string `json:"vendor,omitempty"`
}

// ManagementPath is one discovered way to reach this host (from Redfish
// Links.ManagedBy) — N fungible paths with independent health, never an identity.
type ManagementPath struct {
	// EndpointRef names the ManagementEndpoint fronting this system.
	EndpointRef Reference `json:"endpointRef"`
	// SystemID is the Redfish ComputerSystem member id behind that endpoint, opaque.
	SystemID string `json:"systemId"`
	// Reachable is this path's independent health.
	Reachable bool `json:"reachable"`
}

// HostStatus is discovered truth about the machine itself.
type HostStatus struct {
	// +optional
	Identity *HostIdentity `json:"identity,omitempty"`

	// ManagedBy lists the discovered access paths — a list, length 1 today, N-ready
	// (multiple managers, moved boards, Redfish aggregators). Atomic: one writer
	// (the HostReconciler) replaces it whole.
	// +listType=atomic
	// +optional
	ManagedBy []ManagementPath `json:"managedBy,omitempty"`

	// LastResetTime is the newest reset-time observation across paths — the drift
	// memory: an advance with no explaining HostOperation raises Drifted. Absent
	// where the vendor serves no such observable (iBMC) — drift detection then
	// degrades openly rather than being simulated.
	// +optional
	LastResetTime *metav1.MicroTime `json:"lastResetTime,omitempty"`

	// FirmwareProposed is the firmware domain's plan memory (the BIOS domain's
	// lives on the materialized BiosSettings). Written by the convergence
	// reconciler alone; the HostReconciler leaves it untouched — the two share
	// this status with disjoint fields.
	// +optional
	FirmwareProposed *ProposedPlan `json:"firmwareProposed,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the conditions below for people and for kubectl columns.
	// It is a summary, not the contract: automation should read conditions.
	// +optional
	Phase HostPhase `json:"phase,omitempty"`

	// BlockedOn is derived — why the host isn't advancing (Approval(bios),
	// SerialMismatch); a reason, not a phase, because it recurs across phases.
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// Host is one realized machine (Redfish ComputerSystem), keyed by identity, reached
// via managedBy paths. Cluster-scoped; bound by a namespaced HostClaim.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=`.status.identity.serial`
// +kubebuilder:printcolumn:name="Vendor",type=string,JSONPath=`.status.identity.vendor`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Blocked",type=string,JSONPath=`.status.blockedOn`,priority=1
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.claimRef.name`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Host struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec HostSpec `json:"spec,omitempty"`
	// +optional
	Status HostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Host `json:"items"`
}
