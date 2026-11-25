package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BootAssetStrategy selects how the artifact is produced.
// +kubebuilder:validation:Enum=configISO;factory;embeddedInstaller
type BootAssetStrategy string

const (
	// StrategyConfigISO builds the ~10 KB per-node config medium Talos reads when
	// booted with talos.config=metal-iso: an ISO9660 volume labeled metal-iso
	// holding one config.yaml. Mounted as the second virtual medium beside a
	// generic install image — the `secondary` delivery path. Secret-bearing:
	// served only under a grant.
	StrategyConfigISO BootAssetStrategy = "configISO"

	// StrategyFactory is the generic secret-free install ISO of the image
	// factory — content-addressed by schematic, shared across nodes. The
	// operator downloads it into its media store over TLS that it verifies
	// and records its digest. The media service serves it under a grant,
	// pinned to that digest: a BMC never fetches it from the factory.
	StrategyFactory BootAssetStrategy = "factory"

	// StrategyEmbeddedInstaller builds a per-node install ISO with the node's
	// rendered config built in (Talos reads it from the image, no kernel
	// argument and no second medium needed). It is how a machine whose BMC
	// holds one virtual medium gets its config out of band. The Talos imager
	// builds it in a Job in the operator's namespace; the ISO rests in the
	// operator's media store. Secret-bearing: served only under a grant.
	StrategyEmbeddedInstaller BootAssetStrategy = "embeddedInstaller"
)

// BootAssetSpec declares one boot artifact. The strategy decides which of the
// remaining fields apply; the CEL rules on the kind enforce the pairing.
type BootAssetSpec struct {
	// Strategy selects how the artifact is produced.
	Strategy BootAssetStrategy `json:"strategy"`

	// NodeRef names the TalosNode whose rendered config the artifact carries.
	// configISO and embeddedInstaller.
	// +optional
	NodeRef *Reference `json:"nodeRef,omitempty"`

	// TalosVersion pins the release of the install image. factory and
	// embeddedInstaller.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	// +optional
	TalosVersion string `json:"talosVersion,omitempty"`

	// Schematic customizes the install image (system extensions, kernel
	// arguments). factory and embeddedInstaller; omitted means the vanilla
	// image.
	// +optional
	Schematic *Schematic `json:"schematic,omitempty"`
}

// BootAssetStatus reports the built artifact. No artifact has a URL that a BMC
// fetches directly: the fetch URLs are minted per mount as grants of the media
// service and revoked on node check-in, eject, or expiry.
type BootAssetStatus struct {
	// Conditions: Ready.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// URL is where the operator downloads the artifact from. A BMC never
	// fetches it there. factory only.
	// +optional
	URL string `json:"url,omitempty"`

	// Digest is sha256:<hex> of the artifact bytes.
	// +optional
	Digest string `json:"digest,omitempty"`

	// Size is the artifact size in bytes.
	// +optional
	Size int64 `json:"size,omitempty"`

	// ConfigDigest is the rendered config (the TalosNode's
	// status.configDigest) the stored artifact carries. embeddedInstaller
	// only.
	// +optional
	ConfigDigest string `json:"configDigest,omitempty"`

	// BuildKey names the stored artifact in the media store. For an
	// embeddedInstaller it names the inputs: config, Talos version,
	// schematic and imager image; a change of any input is a new key and a
	// new build. For a factory image it is a hash of URL. embeddedInstaller
	// and factory.
	// +optional
	BuildKey string `json:"buildKey,omitempty"`

	// BuildJob is the imager Job of the latest build, in the operator's
	// namespace. A failed Job is kept so its log can be read.
	// embeddedInstaller only.
	// +optional
	BuildJob string `json:"buildJob,omitempty"`

	// ArtifactSecretRef is where the built artifact rests (key asset.iso), owned
	// by this BootAsset and GC'd with it. configISO only.
	// +optional
	ArtifactSecretRef *SecretReference `json:"artifactSecretRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:validation:XValidation:rule="self.spec.strategy != 'configISO' || (has(self.spec.nodeRef) && !has(self.spec.talosVersion) && !has(self.spec.schematic))",message="configISO takes nodeRef and nothing else"
// +kubebuilder:validation:XValidation:rule="self.spec.strategy != 'factory' || (!has(self.spec.nodeRef) && has(self.spec.talosVersion))",message="factory takes talosVersion (and optionally schematic), never nodeRef"
// +kubebuilder:validation:XValidation:rule="self.spec.strategy != 'embeddedInstaller' || (has(self.spec.nodeRef) && has(self.spec.talosVersion))",message="embeddedInstaller takes nodeRef and talosVersion (and optionally schematic)"
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.strategy`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeRef.name`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.size`

// BootAsset is a built boot artifact and how to get it. The controller builds
// or downloads the artifact and reports readiness and digest. For the factory
// strategy, it also reports the URL it downloads from.
//
// One-slot BMCs: a BMC with one virtual media slot cannot mount the installer
// and a config ISO side by side. For a blank machine behind such a BMC, the
// operator creates an embeddedInstaller BootAsset named `<node>-embedded`.
// This is an installer for that node with its machine config inside, mounted
// in the one slot. The operator does not send the config to a machine in
// maintenance mode, because the machine takes it from anybody who reaches it.
// The installer reaches the BMC only through the media service and its
// grants, as a config ISO does.
//
// Secrets at rest: the embedded installer holds the node's machine config in
// plain text, keys and tokens included. It is on the media store volume (the
// manager's --media-store), and only the user of the operator can read it.
// It is deleted with its BootAsset, or when a newer build replaces it. It is not
// encrypted: use an encrypted StorageClass for the media store where Secrets
// are encrypted at rest.
type BootAsset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BootAssetSpec `json:"spec"`
	// +optional
	Status BootAssetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BootAssetList contains a list of BootAsset.
type BootAssetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BootAsset `json:"items"`
}
