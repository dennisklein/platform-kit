package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BiosSettingsSpec is the rendered desired BIOS state for ONE host — the merge
// of every matched BiosProfile, written by the convergence reconciler (its sole
// author). Named after its Host and owned by it.
type BiosSettingsSpec struct {
	// Attributes is the merged desired attribute map. When two profiles
	// conflict, the previous merge stays as it is: the operator alerts and holds,
	// and never applies part of a conflicted state. status.provenance names the
	// profile of each key.
	// +optional
	Attributes map[string]apiextensionsv1.JSON `json:"attributes,omitempty"`
}

// ProposedPlan remembers that an operation was proposed for one exact desired
// state — the one fact that is not re-derivable from the world (the operation
// object may be gone: TTL-GC'd, or deleted by a human as a veto). An unchanged
// planHash never spawns a second operation; everything else about the outcome
// is read live from the operation or its absence.
type ProposedPlan struct {
	// PlanHash is the sha256 of the desired state the operation serves.
	PlanHash string `json:"planHash"`
	// Operation is the proposed HostOperation as namespace/name.
	Operation string `json:"operation"`
}

// BiosSettingsStatus carries the observation side: the managed keys as the
// machine last served them, and the plan memory.
type BiosSettingsStatus struct {
	// Observed caches the managed attributes as last read from the machine —
	// status-as-cache: an unreachable pass keeps the last view.
	// +optional
	Observed map[string]apiextensionsv1.JSON `json:"observed,omitempty"`

	// Pending caches the managed attributes staged in the @Redfish.Settings
	// object — applied at the next system reset.
	// +optional
	Pending map[string]apiextensionsv1.JSON `json:"pending,omitempty"`

	// Provenance names the profile that gave each attribute. Thus the object
	// tells what the host gets and why, and you need not do the merge yourself.
	// +optional
	Provenance map[string]string `json:"provenance,omitempty"`

	// Proposed is the plan memory for the BIOS domain.
	// +optional
	Proposed *ProposedPlan `json:"proposed,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BiosSettings is one machine's BIOS state: what is wanted, what is actually set,
// and where each wanted value came from. It is the merge of the BiosProfiles
// that match the Host. Read it before an apply, to see what changes and why.
//
// Cluster-scoped like its Host, named after it, and owned by it: deleting the
// machine takes its settings with it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Converged",type=string,JSONPath=`.status.conditions[?(@.type=="Converged")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Converged")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BiosSettings struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec BiosSettingsSpec `json:"spec,omitempty"`
	// +optional
	Status BiosSettingsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type BiosSettingsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BiosSettings `json:"items"`
}
