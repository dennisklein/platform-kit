package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FirmwareProfileSpec is one layer of desired firmware for the Hosts it matches.
// As a BiosProfile, it is data without a controller. The convergence
// reconciler compares the desired version with the version that the
// endpoints report, and proposes a flash operation on drift. Firmware
// converges before BIOS, because the BIOS schema depends on the firmware.
//
// +kubebuilder:validation:XValidation:rule="has(self.manager) || (has(self.system) && size(self.system) > 0)",message="give manager, system, or both"
type FirmwareProfileSpec struct {
	// Match selects Hosts by label (same two axes as BiosProfile.spec.match).
	Match metav1.LabelSelector `json:"match"`

	// Layer orders overlapping profiles exactly as on BiosProfile: (layer, name)
	// merge order, later overrides, same-layer disagreement is a conflict.
	// +kubebuilder:default=0
	// +optional
	Layer int32 `json:"layer,omitempty"`

	// Manager is the desired management-controller firmware. It is flashed
	// over the multipart push the endpoint advertises; endpoints that
	// advertise only SimpleUpdate hold at NoUpdateTransport rather than
	// flashing over an unproven path. Flashing the manager does not stop the
	// machine it manages.
	// +optional
	Manager *FirmwareTarget `json:"manager,omitempty"`

	// System is the desired system firmware: BIOS/UEFI, network cards, RAID
	// controllers, whatever the machine's UpdateService lists in its
	// FirmwareInventory. Each image applies at the next reset of the machine.
	// Thus, on a machine that runs a node, the operation drains the node first
	// and waits for the node after the reset. System firmware
	// converges after the manager's and before BIOS settings (a BIOS update
	// can change the settings it offers).
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=component
	// +optional
	System []SystemFirmwareTarget `json:"system,omitempty"`
}

// SystemFirmwareTarget is the desired version of one system component.
type SystemFirmwareTarget struct {
	// Component matches FirmwareInventory member ids, as a regular expression
	// over the whole id: BIOS, NIC\.Slot\.[0-9]+, RAID.*. Ids are vendor
	// shaped; kubectl describe managementendpoint lists the ones a machine
	// serves. Every matching entry is brought to the version; a pattern that
	// matches nothing holds the profile at NoMatchingComponent.
	// +kubebuilder:validation:MinLength=1
	Component string `json:"component"`

	// Version is the desired version, compared verbatim against the version
	// the inventory entry reports.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Image is the digest-pinned OCI reference of the vendor's update
	// package for this component.
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`
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

// FirmwareProfile is the firmware that a group of machines should run. You keep
// it in git, and it applies to the Hosts that match its selector. As a
// BiosProfile, it states intent, and no controller reconciles it directly. For
// a machine that drifts from it, the operator proposes a HostOperation. A flash
// can stop the machine, so the operation waits for approval.
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
