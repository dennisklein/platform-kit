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

// HostClaimSpec says which machine you want and what it should run. You pick
// the machine by its Asset, because you know the Asset name before the
// operator has found the machine and named its Host.
//
// Pointing a claim at another machine (pk-metal replace) moves it: the
// operator first releases the machine the claim holds, the way deleting the
// claim would (its node leaves the cluster, the machine is reset or marked
// not wiped, with approval where that turns a node off), and only then binds
// the new one, so two machines never run the same node. Only an admin may do
// it (config/policy/hostclaim-provisioning.yaml): it moves a node's config,
// with the cluster's keys on a control plane, to another machine.
type HostClaimSpec struct {
	// AssetRef names the Asset whose machine this claim binds.
	AssetRef Reference `json:"assetRef"`

	// SystemRef picks one machine by its Redfish system id when the BMC
	// manages more than one (a multi-node chassis). Leave it out when the BMC
	// manages one machine.
	// +optional
	SystemRef *Reference `json:"systemRef,omitempty"`

	// Labels are copied onto the bound Host and removed when the claim goes
	// away, for example role: worker. EndpointProfiles and firmware plans
	// select machines by them. Keys have no prefix (no "/"), so they never
	// clash with the labels the operator sets under metal.pk.io/.
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 63 && k.matches('^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'))",message="label keys must be unprefixed valid label names"
	// +optional
	Labels map[string]LabelValue `json:"labels,omitempty"`

	// OS names the TalosNode the machine should run. Setting it proposes an
	// install that erases the disk chosen by the TalosNode's
	// installDiskSelector. Without it the claim only reserves the machine and
	// nothing is done to it.
	// +optional
	OS *ClaimOS `json:"os,omitempty"`

	// Provisioning tunes how the operator installs the OS onto the machine.
	// +optional
	Provisioning *ClaimProvisioning `json:"provisioning,omitempty"`
}

// ProvisioningApproval says whether the operations the operator proposes for
// this claim (install, and shutting down a running node on release) wait for a
// person.
// +kubebuilder:validation:Enum=Required;Automatic
type ProvisioningApproval string

const (
	// ProvisioningApprovalRequired: each operation waits for a person to
	// approve it (pk-metal approve). The default, because these operations
	// erase disks and power machines off.
	ProvisioningApprovalRequired ProvisioningApproval = "Required"
	// ProvisioningApprovalAutomatic: the operations run without approval.
	// Use it for machines you can reinstall at any time, where merging the
	// claim is the decision.
	ProvisioningApprovalAutomatic ProvisioningApproval = "Automatic"
)

// ClaimProvisioning is the claim's say in how its machine is provisioned.
type ClaimProvisioning struct {
	// Approval: Required (default) or Automatic.
	// +kubebuilder:default=Required
	// +optional
	Approval ProvisioningApproval `json:"approval,omitempty"`
}

// LabelValue is one label value. It must be a valid Kubernetes label value,
// so that a value the Host would reject is refused when you apply the claim.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type LabelValue string

// ClaimOS names what the machine should run. The node cannot be changed in
// place: an install belongs to one node. To switch, remove spec.os (which
// stops an install still running) and set it again.
// +kubebuilder:validation:XValidation:rule="self.nodeRef == oldSelf.nodeRef",message="os.nodeRef is immutable: drop the OS intent first, or create another claim"
type ClaimOS struct {
	// NodeRef names the TalosNode (os.pk.io) whose config the machine boots.
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

// HostClaimStatus says which machine the claim holds and how far its install
// got. The Host's spec.claimRef is what actually holds the machine; the first
// claim to set it wins.
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

	// BlockedOn says why the claim is not moving on: why it cannot bind
	// (AssetNotFound, AssetNotMaterialized, HostAlreadyClaimed) or, once
	// bound, why the install waits (Approval(Provisioning), HeldByUser,
	// HostInMaintenance, OperationFailed). kubectl describe shows the message
	// with the command to run.
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// HostClaim takes one machine for your use. What it does to the machine:
//
//   - Creating it with spec.os proposes installing that TalosNode. Once the
//     install is approved, the disk picked by the TalosNode's
//     installDiskSelector is erased and overwritten.
//   - Deleting it releases the machine. If the operator installed the machine,
//     it is shut down: a graceful shutdown first, then forced off after 5
//     minutes (the manager's --release-shutdown-timeout). If the machine runs
//     a node, the shutdown waits for approval unless
//     spec.provisioning.approval is Automatic. A machine the claim never
//     installed is left running.
//   - Without spec.os the claim only reserves the machine; nothing is done to
//     it.
//
// Each proposed step is a HostOperation; with approval Required (the
// default) it waits until you run pk-metal approve <namespace>/<operation>.
// To stop all proposals for a claim, set the metal.pk.io/hold annotation on
// it. After a finished install, status.handOff records which machine runs
// which node; the operator does not install it again.
//
// A claim binds exactly one Host, and only one claim can hold a Host. Keep
// claims in git, so that taking a machine is a reviewed change.
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
