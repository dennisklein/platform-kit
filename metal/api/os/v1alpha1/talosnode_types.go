package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeRole is the part a machine plays in its cluster.
// +kubebuilder:validation:Enum=controlplane;worker
type NodeRole string

const (
	RoleControlPlane NodeRole = "controlplane"
	RoleWorker       NodeRole = "worker"
)

// TalosNodeSpec is what belongs to one machine. Its hostname is the object's
// metadata.name.
type TalosNodeSpec struct {
	// ClusterRef names the TalosCluster this node is a member of.
	ClusterRef Reference `json:"clusterRef"`

	// Role is controlplane or worker.
	Role NodeRole `json:"role"`

	// Address is where the operator and talosctl reach the node's Talos API.
	// Empty means the first static address of the first link. A node that only
	// takes DHCP addresses needs it set, or its check-in has nothing to dial.
	// +optional
	Address string `json:"address,omitempty"`

	// InstallDiskSelector picks the disk Talos installs onto. That disk is
	// repartitioned and everything on it is erased. A disk matches when every
	// matcher you set holds; if several disks match, Talos takes the first one
	// it finds, which can change between boots. Prefer serial or wwid, which
	// name exactly one disk.
	InstallDiskSelector DiskSelector `json:"installDiskSelector"`

	// Network configures the node's links.
	// +optional
	Network NodeNetwork `json:"network,omitempty"`

	// Labels are Kubernetes node labels the kubelet registers with. Worker
	// nodes may only set labels the NodeRestriction admission plugin allows.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Taints are Kubernetes node taints the kubelet registers with.
	// +optional
	Taints []Taint `json:"taints,omitempty"`

	// Schematic customizes this node's installer image and replaces the
	// cluster's schematic when set. Extensions and kernel arguments both live
	// here: Talos takes the installed kernel command line from the image, so an
	// argument that is not part of the schematic does not survive the install.
	// +optional
	Schematic *Schematic `json:"schematic,omitempty"`

	// Patches apply to this node only, in order, after the cluster's patches.
	// +optional
	Patches []Patch `json:"patches,omitempty"`
}

// DiskSelector says which disk to install onto, and so which disk to erase.
// String matchers take glob patterns (*, ?); Match is a Talos disk expression
// for anything else. Set at least one; serial or wwid is safest.
// +kubebuilder:validation:XValidation:rule="has(self.serial) || has(self.model) || has(self.wwid) || has(self.busPath) || has(self.match)",message="at least one matcher is required: a node that does not say where to install does not install"
type DiskSelector struct {
	// Serial matches the disk serial number. It names one disk and does not
	// change when disks are re-cabled or found in another order.
	// +optional
	Serial string `json:"serial,omitempty"`
	// Model matches the disk model. Machines often have several disks of
	// the same model; combine it with another matcher.
	// +optional
	Model string `json:"model,omitempty"`
	// WWID matches the disk's world wide identifier.
	// +optional
	WWID string `json:"wwid,omitempty"`
	// BusPath matches the disk's bus path. It changes when a disk moves to
	// another slot.
	// +optional
	BusPath string `json:"busPath,omitempty"`
	// Match is a CEL expression over `disk` (Talos' disk locator
	// environment), for example `disk.size > 100u * GB && !disk.rotational`.
	// +optional
	Match string `json:"match,omitempty"`
}

// NodeNetwork is the node's link configuration.
type NodeNetwork struct {
	// Links configure physical links, each selected by its MAC address.
	// +optional
	// +listType=map
	// +listMapKey=name
	Links []Link `json:"links,omitempty"`
}

// Link is one physical link: Talos aliases the link with the MAC address to
// Name (LinkAliasConfig), then configures it under that name (LinkConfig, and
// DHCPv4Config when DHCP is on). Naming links by alias keeps the rest of the
// configuration, and every patch, independent of what the kernel calls the
// interface on a given board.
// +kubebuilder:validation:XValidation:rule="(has(self.dhcp) && self.dhcp) || (has(self.addresses) && size(self.addresses) > 0)",message="a link without DHCP needs at least one static address"
type Link struct {
	// Name is the alias the link gets. Avoid kernel names (eth0, enp0s2):
	// an alias must not collide with a real interface.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,14}$`
	Name string `json:"name"`

	// HardwareAddr is the link's permanent MAC address. A physical link
	// whose driver reports no permanent address is matched on its current
	// one instead. Lower case and colon-separated (0c:c4:7a:12:34:56): Talos
	// compares it as text.
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`
	HardwareAddr string `json:"hardwareAddr"`

	// Addresses are static addresses in CIDR form, primary family first.
	// +optional
	Addresses []string `json:"addresses,omitempty"`

	// Routes are static routes over this link.
	// +optional
	Routes []Route `json:"routes,omitempty"`

	// DHCP runs a DHCPv4 client on the link, alongside any static addresses.
	// +optional
	DHCP bool `json:"dhcp,omitempty"`

	// MTU overrides the link MTU.
	// +kubebuilder:validation:Minimum=576
	// +optional
	MTU int32 `json:"mtu,omitempty"`
}

// Route is a static route.
type Route struct {
	// Network is the destination CIDR; empty is the default route.
	// +optional
	Network string `json:"network,omitempty"`
	// Gateway is the next hop.
	// +optional
	Gateway string `json:"gateway,omitempty"`
	// Metric orders competing routes.
	// +optional
	Metric int32 `json:"metric,omitempty"`
}

// TaintEffect is a Kubernetes taint effect.
// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
type TaintEffect string

// Taint is a Kubernetes node taint.
type Taint struct {
	Key string `json:"key"`
	// +optional
	Value  string      `json:"value,omitempty"`
	Effect TaintEffect `json:"effect"`
}

// Schematic is the Image Factory schematic subset this operator renders: the
// content its id is a hash of, so every field here changes the image.
type Schematic struct {
	// +optional
	Customization SchematicCustomization `json:"customization,omitempty"`
}

// SchematicCustomization holds the image customization knobs.
type SchematicCustomization struct {
	// +optional
	SystemExtensions SystemExtensions `json:"systemExtensions,omitempty"`

	// ExtraKernelArgs append to the image's kernel command line, for example
	// where to put the console (console=ttyS0, what the BMC's serial-over-LAN
	// shows). Do not add talos.config=metal-iso: the operator adds it to the
	// boot medium itself, and the installed system must not carry it.
	// +optional
	ExtraKernelArgs []string `json:"extraKernelArgs,omitempty"`
}

// SystemExtensions bakes extensions into the image.
type SystemExtensions struct {
	// OfficialExtensions are siderolabs/* extension names (siderolabs/drbd).
	// +optional
	OfficialExtensions []string `json:"officialExtensions,omitempty"`
}

// Patch is one Talos configuration patch: a multi-document YAML strategic
// merge patch (documents merge by apiVersion, kind and name; `$patch: delete`
// removes one), given inline or read from a ConfigMap key.
// +kubebuilder:validation:XValidation:rule="has(self.inline) != has(self.configMapRef)",message="exactly one of inline or configMapRef"
type Patch struct {
	// Inline is the patch text.
	// +optional
	Inline string `json:"inline,omitempty"`
	// ConfigMapRef reads the patch from a ConfigMap key.
	// +optional
	ConfigMapRef *ConfigMapKeyReference `json:"configMapRef,omitempty"`
}

// ConfigMapKeyReference points at one key of a namespaced ConfigMap.
type ConfigMapKeyReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// TalosNodeStatus reports the render and the machine's check-in. Rendered=False
// is alert-and-hold: the last rendered Secret stays in place, so a broken input
// never un-renders a machine's config.
type TalosNodeStatus struct {
	// Conditions: Rendered, plus the check-in observations MachineReachable
	// and Ready while a Host hands this node a machine, ConfigInSync (the
	// dry-run drift check) and VersionCurrent (Talos version skew).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ConfigSecretRef points at the Secret holding this node's rendered
	// machine config (key config.yaml). Owned by the TalosNode and collected
	// with it; it is the one place the rendered secrets rest.
	// +optional
	ConfigSecretRef *SecretReference `json:"configSecretRef,omitempty"`

	// ConfigDigest is sha256:<hex> of the rendered config, what BootAsset
	// builds key their rebuilds on.
	// +optional
	ConfigDigest string `json:"configDigest,omitempty"`

	// InstallerImage is the installer the rendered config installs from.
	// +optional
	InstallerImage string `json:"installerImage,omitempty"`

	// Address is where the check-in dials (spec.address, or the first static
	// link address).
	// +optional
	Address string `json:"address,omitempty"`

	// Stage is the machine's observed boot stage (maintenance, booting,
	// installing, running, ...), empty while no machine answers.
	// +optional
	Stage string `json:"stage,omitempty"`

	// Version is the Talos version the machine reports.
	// +optional
	Version string `json:"version,omitempty"`

	// RenderedFor is the Talos version the config was rendered for: the
	// cluster's talosVersion, or the older version the machine still runs
	// until its upgrade, so that a config is never written for a contract
	// the running machine does not accept yet.
	// +optional
	RenderedFor string `json:"renderedFor,omitempty"`

	// AppliedConfigDigest is the rendered config the machine was last seen
	// to run: the digest of a render a dry run found no change against.
	// Config Secrets of other digests are collected once nothing refers to
	// them.
	// +optional
	AppliedConfigDigest string `json:"appliedConfigDigest,omitempty"`

	// Drift is the last dry run of the rendered config against the
	// machine (ConfigInSync tells the verdict).
	// +optional
	Drift *ConfigDrift `json:"drift,omitempty"`
}

// ConfigDrift is what a dry-run apply of the rendered config reported. A dry
// run is a read: the machine validates the config, diffs it against the one
// it runs and says whether it could take it without a reboot.
type ConfigDrift struct {
	// ConfigDigest is the rendered config the dry run was made with.
	ConfigDigest string `json:"configDigest"`

	// Changed: the machine runs a config that differs from this render.
	Changed bool `json:"changed"`

	// RebootRequired: the machine said it needs a reboot to take it.
	// +optional
	RebootRequired bool `json:"rebootRequired,omitempty"`

	// Summary counts the changed lines and names the configuration keys
	// they touch. Values are left out: a diff of a machine config carries
	// secrets.
	// +optional
	Summary string `json:"summary,omitempty"`

	// ObservedAt is when the dry run was made.
	ObservedAt metav1.Time `json:"observedAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`,priority=1
// +kubebuilder:printcolumn:name="InSync",type=string,JSONPath=`.status.conditions[?(@.type=="ConfigInSync")].status`,priority=1

// TalosNode is one member of a TalosCluster: the machine-specific half of its
// configuration, and what the machine reports once it runs.
type TalosNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TalosNodeSpec `json:"spec"`
	// +optional
	Status TalosNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TalosNodeList contains a list of TalosNode.
type TalosNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TalosNode `json:"items"`
}
