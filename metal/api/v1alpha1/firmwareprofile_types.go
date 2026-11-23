package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FirmwareProfileSpec is one layer of desired firmware for the Hosts it matches.
// Like BiosProfile it is controller-less data; the convergence reconciler
// compares the desired version against what the fronting endpoints serve and
// proposes a flash operation on drift. Firmware converges before BIOS — the
// BIOS schema depends on firmware.
type FirmwareProfileSpec struct {
	// Match selects Hosts by label (same two axes as BiosProfile.spec.match).
	Match metav1.LabelSelector `json:"match"`

	// Layer orders overlapping profiles exactly as on BiosProfile: (layer, name)
	// merge order, later overrides, same-layer disagreement is a conflict.
	// +kubebuilder:default=0
	// +optional
	Layer int32 `json:"layer,omitempty"`

	// Manager is the desired management-controller firmware. The only firmware
	// target so far — the flash transport is proven for the manager alone
	// (multipart push; endpoints advertising only SimpleUpdate hold at
	// NoUpdateTransport rather than flashing over an unproven path).
	Manager FirmwareTarget `json:"manager"`
}

// FirmwareTarget pairs a desired version with the image that realizes it.
type FirmwareTarget struct {
	// Version is the desired firmware version, compared verbatim against the
	// version the endpoint serves — the convergence observable.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Image is a digest-pinned OCI reference to the firmware image (a single-
	// layer artifact in the internal registry, e.g.
	// registry.pk.internal:5000/firmware/x12-bmc@sha256:…). The digest is
	// required: the executor refuses to flash bytes it cannot verify.
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`
}

// FirmwareProfile is the firmware some group of machines should be running —
// authored in git, applying to whichever Hosts match its selector. Like
// BiosProfile it states intent and is never reconciled directly: a machine that
// drifts from it gets a HostOperation proposed against it, and flashing firmware
// is disruptive enough that the operation waits for approval.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=metal
// +kubebuilder:printcolumn:name="Layer",type=integer,JSONPath=`.spec.layer`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.manager.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type FirmwareProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec FirmwareProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type FirmwareProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FirmwareProfile `json:"items"`
}
