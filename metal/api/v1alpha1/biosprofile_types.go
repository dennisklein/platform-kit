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
	// Match selects Hosts by label. There are two types of labels. Intent labels
	// come from the bound claim (role: worker); a machine without a claim has
	// none. Fact labels come from discovery (metal.pk.io/vendor, …/model); every
	// machine has them, so use them for hardware baselines.
	Match metav1.LabelSelector `json:"match"`

	// Layer orders profiles that overlap. The merge sorts the matched profiles
	// by layer and name, and a higher layer overrides a lower one. For example,
	// a role profile (layer 10) overrides a hardware baseline (layer 0). Two
	// profiles on the SAME layer that set one attribute to different values
	// conflict. A conflict holds the whole merge.
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

// BiosProfile is a set of BIOS settings for a group of machines. You keep it in
// git, and it applies to the Hosts that match its selector. Several profiles can
// match one machine. The operator merges them by layer into the BiosSettings of
// the machine, which records the profile of each value.
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
