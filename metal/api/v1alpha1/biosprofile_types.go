package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BiosProfileSpec is one layer of desired BIOS configuration for the Hosts it
// matches. Profiles are git-authored data — no controller reconciles a profile;
// the convergence reconciler materializes the merged result per host into a
// BiosSettings object.
type BiosProfileSpec struct {
	// Match selects Hosts by label. Two label axes exist by construction: intent
	// labels stamped from the bound claim (role: worker — absent on unclaimed
	// machines) and discovered facts (metal.pk.io/vendor, …/model —
	// present regardless of claim state, the axis for hardware baselines).
	Match metav1.LabelSelector `json:"match"`

	// Layer orders overlapping profiles: materialization merges matched profiles
	// by (layer, name) and a later layer deliberately overrides an earlier one —
	// a role profile (layer 10) over a hardware baseline (layer 0). Two profiles
	// on the SAME layer setting the same attribute to different values are a
	// conflict, and conflict holds the whole materialization.
	// +kubebuilder:default=0
	// +optional
	Layer int32 `json:"layer,omitempty"`

	// Attributes are the BIOS attributes this profile manages, values as the
	// vendor's attribute registry types them (enum string, boolean, integer).
	// An attribute omitted here is unmanaged — a higher layer can override a
	// value but not retract management.
	// +kubebuilder:validation:MinProperties=1
	Attributes map[string]apiextensionsv1.JSON `json:"attributes"`
}

// BiosProfile is a set of BIOS settings that some group of machines should share
// — authored in git, applying to whichever Hosts match its selector. Several may
// match one machine; they are layered, and the result of that layering is the
// machine's BiosSettings, which records which profile each value came from.
//
// Nothing reconciles a BiosProfile directly. It states intent; the settings it
// produces are what a machine is held against.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Layer",type=integer,JSONPath=`.spec.layer`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BiosProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BiosProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type BiosProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BiosProfile `json:"items"`
}
