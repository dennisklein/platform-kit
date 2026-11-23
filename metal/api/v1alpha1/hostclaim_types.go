package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AnnotationHold on a HostClaim stops the operator from proposing anything
// for it: no install, no release. Proposals still waiting for approval are
// withdrawn. The value is an optional reason, shown in the claim's
// Provisioned condition. An operation already approved and running is not
// stopped by it. Remove the annotation to go on.
const AnnotationHold = "metal.pk.io/hold"

// ClaimPhase is derived write-only from the Bound condition (internal/phase).
// ClaimPhase says how many machines the claim's selection resolved to: none yet
// (Pending), exactly one (Bound), or more than one with nothing to choose between
// them (Ambiguous). Ambiguous is a stop — the cluster will not guess.
// +kubebuilder:validation:Enum=Pending;Bound;Ambiguous
type ClaimPhase string

const (
	ClaimPending   ClaimPhase = "Pending"
	ClaimBound     ClaimPhase = "Bound"
	ClaimAmbiguous ClaimPhase = "Ambiguous"
)

// HostClaimSpec selects one machine and binds it exclusively — the system's ONLY
// claim (decisions attach to machines, not to controllers; the endpoint has none).
// Selection goes through the Asset: the org's durable identity is what a human
// commits, the Host name is discovered later.
//
// What the claim binds is fixed for its lifetime: pointing it elsewhere would
// leave the machine it already bound without a release (power off, media
// out) and a boot in flight for the old target. Re-targeting is a new claim.
// +kubebuilder:validation:XValidation:rule="self.assetRef == oldSelf.assetRef",message="assetRef is immutable: delete the claim and create another"
// +kubebuilder:validation:XValidation:rule="has(self.systemRef) == has(oldSelf.systemRef) && (!has(self.systemRef) || self.systemRef == oldSelf.systemRef)",message="systemRef is immutable: delete the claim and create another"
type HostClaimSpec struct {
	// AssetRef names the Asset whose machine this claim binds.
	AssetRef Reference `json:"assetRef"`

	// SystemRef picks one ComputerSystem by its Redfish member id when the
	// endpoint fronts several; optional when it fronts one.
	// +optional
	SystemRef *Reference `json:"systemRef,omitempty"`

	// Labels is the intent axis for profile matching: a free-form label map
	// (role: worker today; rack zone, tenant tier, GPU pool need no schema
	// change) stamped onto the bound Host's metadata labels and cleared on
	// release — an unclaimed machine carries no intent labels by construction.
	// Keys are unprefixed (no "/"), so they can never collide with the
	// discovered-fact labels stamped under metal.pk.io/.
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 63 && k.matches('^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'))",message="label keys must be unprefixed valid label names"
	// +optional
	Labels map[string]LabelValue `json:"labels,omitempty"`

	// OS names the payload the bound machine runs. Without it the claim only
	// reserves the machine (Assigned) — provisioning starts when the intent
	// appears and the reconciler stamps it onto the Host.
	// +optional
	OS *ClaimOS `json:"os,omitempty"`

	// Provisioning tunes how the operator installs the OS onto the machine.
	// +optional
	Provisioning *ClaimProvisioning `json:"provisioning,omitempty"`
}

// ProvisioningApproval says whether the operator's boot operation for this
// claim waits for a person.
// +kubebuilder:validation:Enum=Required;Automatic
type ProvisioningApproval string

const (
	// ProvisioningApprovalRequired: the boot operation waits for a person to
	// approve it — the default, because it powers the machine.
	ProvisioningApprovalRequired ProvisioningApproval = "Required"
	// ProvisioningApprovalAutomatic: the operator creates the boot operation
	// pre-cleared — for cattle, where the claim itself is the decision.
	ProvisioningApprovalAutomatic ProvisioningApproval = "Automatic"
)

// ClaimProvisioning is the claim's say in how its machine is provisioned.
type ClaimProvisioning struct {
	// Approval: Required (default) or Automatic.
	// +kubebuilder:default=Required
	// +optional
	Approval ProvisioningApproval `json:"approval,omitempty"`
}

// LabelValue is one intent label value, constrained to the k8s label-value
// shape at the schema level (a value the Host metadata would reject must
// already fail at claim admission).
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type LabelValue string

// ClaimOS is the claim's OS intent. The node it names is fixed while the
// intent stands: a boot operation and its hand-off record belong to one node,
// and dropping the intent (which stops a boot still running) is the only way
// to another.
// +kubebuilder:validation:XValidation:rule="self.nodeRef == oldSelf.nodeRef",message="os.nodeRef is immutable: drop the OS intent first, or create another claim"
type ClaimOS struct {
	// NodeRef names the TalosNode (os.pk.io, cluster-scoped) whose
	// rendered config the machine boots.
	NodeRef Reference `json:"nodeRef"`
}

// ClaimHandOff is the record of one finished install.
type ClaimHandOff struct {
	// Host is the Host the node was installed on.
	Host string `json:"host"`
	// Node is the TalosNode the machine runs.
	Node string `json:"node"`
	// ConfigDigest is sha256:<hex> of the rendered config the machine booted
	// with; empty when the record was rebuilt from an operation that did not
	// carry it.
	// +optional
	ConfigDigest string `json:"configDigest,omitempty"`
	// Operation is the boot operation (namespace/name) that installed the
	// machine. Empty when the machine already ran the node when the claim
	// found it: the operator never touched it, so a release leaves its
	// power alone.
	// +optional
	Operation string `json:"operation,omitempty"`
	// At is when the operator saw the hand-off.
	At metav1.Time `json:"at"`
}

// HostClaimStatus is the claim's side of the bind (the authoritative pointer is
// Host.spec.claimRef — PV/PVC-style, first writer wins).
type HostClaimStatus struct {
	// HostRef names the bound Host.
	// +optional
	HostRef *Reference `json:"hostRef,omitempty"`

	// HandOff records that the machine was installed: which host runs which
	// TalosNode, from which config, since when. It is written once, when the
	// boot finishes, and is the record the operator reads to know the work is
	// done — a node that is down later is not installed again, and deleting
	// old HostOperations changes nothing. It goes away with the claim.
	// +optional
	HandOff *ClaimHandOff `json:"handOff,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the conditions below for people and for kubectl columns.
	// It is a summary, not the contract: automation should read conditions.
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`

	// BlockedOn is derived — why the claim isn't binding (AssetNotFound,
	// HostAlreadyClaimed) or, once bound, why its machine isn't provisioning
	// (SingleMediumUnsupported, OperationFailed); a reason, not a phase.
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// HostClaim binds one Host exclusively, PVC-style. Namespaced so the door to
// multi-team tenancy stays open (deferred: one core team today); lives in git —
// claiming a machine is a reviewed change, not a live write.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=metal
// +kubebuilder:printcolumn:name="Asset",type=string,JSONPath=`.spec.assetRef.name`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.hostRef.name`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.os.nodeRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Provisioned",type=string,JSONPath=`.status.conditions[?(@.type=="Provisioned")].reason`
// +kubebuilder:printcolumn:name="Blocked",type=string,JSONPath=`.status.blockedOn`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HostClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HostClaimSpec `json:"spec"`
	// +optional
	Status HostClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HostClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostClaim `json:"items"`
}
