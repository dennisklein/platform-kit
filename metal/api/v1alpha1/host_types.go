package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// AnnotationMaintenance on a Host takes the machine out of the operator's
// hands, like setting a node offline in a cluster manager: the operator
// proposes nothing for it (no install, no release, no BIOS or firmware
// change) and starts no operation on it that has not started yet. An
// operation already running finishes. The value is the reason, shown in the
// Host's Maintenance condition and in the messages of claims on it. Remove
// the annotation to give the machine back:
//
//	kubectl annotate host <host> metal.pk.io/maintenance="replacing DIMM B2"
//	kubectl annotate host <host> metal.pk.io/maintenance-
const AnnotationMaintenance = "metal.pk.io/maintenance"

// AnnotationAllowSelfDisruption on a Host lets the operator reboot or power
// off the machine even though the operator runs on it, or the machine runs a
// control plane of the cluster the operator runs in. Without it the operator
// proposes no install, release or BIOS change for such a machine and starts
// no power action on it: the operator would take itself down halfway. With it,
// a control plane is still taken down only one at a time, and only while the
// other control planes keep etcd's majority. Any value counts; remove the
// annotation when the work is done:
//
//	kubectl annotate host <host> metal.pk.io/allow-self-disruption=true
//	kubectl annotate host <host> metal.pk.io/allow-self-disruption-
//
// The safer way is to move the operator to another cluster first
// (pk-metal pivot); the machine is then an ordinary one.
const AnnotationAllowSelfDisruption = "metal.pk.io/allow-self-disruption"

// HostPhase summarizes a machine's conditions together with whether it is
// claimed. Enrolling and Inspecting mean the cluster is still finding out what
// the machine is; Available means it is understood and free; Assigned means
// something claimed it; Provisioning means work is happening to it right now and
// Provisioned that the work finished; Releasing means it is being handed back.
// Failed and Maintenance are the two stops: Failed when an install did not
// work, Maintenance while a person has set the metal.pk.io/maintenance
// annotation.
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

// HostSpec is written by the operator from the HostClaim that binds the
// machine; you do not edit it. Everything else about a Host is what the
// operator found, not what anyone wrote.
type HostSpec struct {
	// ClaimRef names the HostClaim that holds this machine. The first claim
	// to set it wins; any other claim on the same machine waits
	// (HostAlreadyClaimed). The UID pins the exact claim: a claim deleted and
	// created again under the same name is a new claim.
	// +optional
	ClaimRef *ClaimReference `json:"claimRef,omitempty"`

	// OS is the TalosNode this machine should run, copied from the claim and
	// cleared when the claim is released. Until it is set, a bound machine is
	// reserved and nothing is done to it.
	// +optional
	OS *HostOS `json:"os,omitempty"`
}

// HostOS names what the machine runs.
type HostOS struct {
	// NodeRef names the TalosNode (os.pk.io) whose config this machine
	// boots.
	NodeRef Reference `json:"nodeRef"`
}

// ClaimReference points a Host at the namespaced HostClaim holding it.
type ClaimReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// HostIdentity identifies the machine, whichever BMC it is reached through:
// the same machine seen through two BMCs gets the same identity, and so one
// Host. Vendors put the serial number in different places (Huawei on the
// system, Supermicro on the board); the operator reads the right one for each.
type HostIdentity struct {
	// +optional
	UUID string `json:"uuid,omitempty"`
	// Serial is the machine's serial number: the board serial on Supermicro,
	// the system serial on Huawei and other vendors.
	// +optional
	Serial string `json:"serial,omitempty"`
	// +optional
	Vendor string `json:"vendor,omitempty"`
}

// ManagementPath is one way to reach this host: a BMC and the system behind
// it. A machine can have more than one, each reachable or not on its own.
type ManagementPath struct {
	// EndpointRef names the ManagementEndpoint (the BMC).
	EndpointRef Reference `json:"endpointRef"`
	// SystemID is the Redfish ComputerSystem id behind that BMC.
	SystemID string `json:"systemId"`
	// Reachable says whether this BMC answers.
	Reachable bool `json:"reachable"`
}

// HostStatus is what the operator found out about the machine itself.
type HostStatus struct {
	// +optional
	Identity *HostIdentity `json:"identity,omitempty"`

	// ManagedBy lists the ways to reach the machine — usually one BMC.
	// +listType=atomic
	// +optional
	ManagedBy []ManagementPath `json:"managedBy,omitempty"`

	// LastResetTime is when the BMC last saw the machine reset. When it moves
	// and no operation explains it, the Drifted condition says somebody reset
	// the machine another way (ipmitool, the BMC web page, the power button).
	// Not every BMC reports it (Huawei iBMC does not); drift is then not
	// detected.
	// +optional
	LastResetTime *metav1.MicroTime `json:"lastResetTime,omitempty"`

	// FirmwareProposed remembers the last firmware plan proposed for this
	// machine, so the same plan is not proposed twice. Written by the
	// firmware lane only.
	// +optional
	FirmwareProposed *ProposedPlan `json:"firmwareProposed,omitempty"`

	// SystemFirmwareProposed is the system-firmware lane's plan memory, the
	// same guard as firmwareProposed for the components a reset applies.
	// +optional
	SystemFirmwareProposed *ProposedPlan `json:"systemFirmwareProposed,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the conditions below for people and for kubectl columns.
	// It is a summary, not the contract: automation should read conditions.
	// +optional
	Phase HostPhase `json:"phase,omitempty"`

	// BlockedOn says why the host is not moving on (Approval(Provisioning),
	// SerialMismatch, HostInMaintenance); a reason, not a phase, because it
	// can happen in any phase.
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// Host is one physical machine as the operator found it through its BMC. The
// operator creates it and names it after the machine's serial number, in lower
// case; the label metal.pk.io/asset names the Asset it was found by. You read
// it to see the state of the machine: kubectl get hosts shows the asset,
// serial, vendor and phase, and kubectl describe host <name> shows why it
// waits.
//
// A HostClaim binds a Host for use. To take a machine out of the operator's
// hands for hardware work, set the metal.pk.io/maintenance annotation on it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Asset",type=string,JSONPath=`.metadata.labels.metal\.pk\.io/asset`
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
