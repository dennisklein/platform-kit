package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BiosSchemaSpec is the discovered attribute registry of one host's BIOS. Like
// Asset it is spec-only — the object IS the data; nothing about it is derived
// further. Written by the convergence reconciler from the registry the Bios
// resource advertises (registry names are normalized on the client: the spelled
// identity differs between the Bios resource, the Registries member, and the
// file on both vendors).
type BiosSchemaSpec struct {
	// Registry is the registry identity as the Bios resource advertises it,
	// kept verbatim (e.g. BiosAttributeRegistry.v1_0_0).
	// +optional
	Registry string `json:"registry,omitempty"`

	// Attributes lists the registry's entries, sorted by name. Desired values
	// in a materialized BiosSettings are validated against this before any
	// operation is proposed — the machine would otherwise reject an unknown
	// attribute at PATCH and silently drop an out-of-range value at apply.
	// +listType=atomic
	Attributes []BiosSchemaAttribute `json:"attributes"`
}

// BiosSchemaAttribute is one registry entry, mirroring the seam's shape.
type BiosSchemaAttribute struct {
	Name string `json:"name"`

	// Type is the registry's attribute type: Enumeration, Boolean, Integer or
	// String — kept as served.
	Type string `json:"type"`

	// AllowedValues enumerates the legal values; empty for non-enumerations.
	// +listType=atomic
	// +optional
	AllowedValues []string `json:"allowedValues,omitempty"`
}

// BiosSchema is the BIOS attribute registry discovered on one machine: which
// settings that machine's firmware actually has, what each one accepts, and which
// are read-only. It is written by the cluster, not by hand — it is how you find
// out what a BiosProfile is allowed to ask for.
//
// Cluster-scoped, named after its Host and owned by it.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Registry",type=string,JSONPath=`.spec.registry`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BiosSchema struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BiosSchemaSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type BiosSchemaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BiosSchema `json:"items"`
}
