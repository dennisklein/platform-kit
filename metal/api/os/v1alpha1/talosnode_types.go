package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TalosNodeSpec is one `nodes:` entry of talconfig.yaml. The hostname is the CR's
// metadata.name. Disk and interface selectors are copied facts today (serials, MACs
// from the inventory ledger); resolving them as hints against discovered hardware is
// the M-later step that retires the copy.
type TalosNodeSpec struct {
	// ClusterRef names the TalosCluster this node renders against.
	ClusterRef Reference `json:"clusterRef"`

	// IPAddress is where talosctl reaches the node (feeds the rendered talosconfig
	// and cert SANs; comma-separated addresses allowed, talhelper semantics).
	// +kubebuilder:validation:MinLength=1
	IPAddress string `json:"ipAddress"`

	// ControlPlane marks the node a control-plane member.
	// +optional
	ControlPlane bool `json:"controlPlane,omitempty"`

	// NodeLabels are Kubernetes labels the node registers with.
	// +optional
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`

	// Schematic customizes the Image Factory install image (system extensions such
	// as siderolabs/drbd). Shape mirrors talconfig/the factory schematic.
	// +optional
	Schematic *Schematic `json:"schematic,omitempty"`

	// InstallDiskSelector looks up the install disk on the machine (serial is the
	// raven convention — the one fact that survives cabling and enumeration).
	// +optional
	InstallDiskSelector *InstallDiskSelector `json:"installDiskSelector,omitempty"`

	// Patches apply to this node only, after the cluster-wide ones: inline
	// strategic-merge YAML or "@./path" references (same resolution as cluster
	// patches).
	// +optional
	Patches []string `json:"patches,omitempty"`

	// NetworkInterfaces configure the node's links. CP nodes use named interfaces
	// (talhelper derives a VIP's link from the name); workers may select by MAC.
	// +optional
	NetworkInterfaces []NetworkInterface `json:"networkInterfaces,omitempty"`
}

// Schematic mirrors the Image Factory schematic subset raven uses.
type Schematic struct {
	// +optional
	Customization SchematicCustomization `json:"customization,omitempty"`
}

// SchematicCustomization holds the image customization knobs.
type SchematicCustomization struct {
	// +optional
	SystemExtensions SystemExtensions `json:"systemExtensions,omitempty"`

	// ExtraKernelArgs append to the image's kernel command line. This is how a
	// boot medium is told to read its config off the second medium
	// (talos.config=metal-iso) and where to put the console
	// (console=ttyS0 — what the BMC's serial-over-LAN shows).
	// +optional
	ExtraKernelArgs []string `json:"extraKernelArgs,omitempty"`
}

// SystemExtensions bakes extensions into the install image.
type SystemExtensions struct {
	// OfficialExtensions are siderolabs/* extension names (drbd, intel-ucode).
	// +optional
	OfficialExtensions []string `json:"officialExtensions,omitempty"`
}

// InstallDiskSelector is the machinery disk matcher subset raven uses — all
// matchers are ANDed by Talos; wildcards supported.
type InstallDiskSelector struct {
	// Serial matches the disk serial number.
	// +optional
	Serial string `json:"serial,omitempty"`
	// Model matches the disk model string.
	// +optional
	Model string `json:"model,omitempty"`
	// Name matches the kernel device name (least stable — prefer serial).
	// +optional
	Name string `json:"name,omitempty"`
	// BusPath matches the device bus path.
	// +optional
	BusPath string `json:"busPath,omitempty"`
}

// NetworkInterface is one machine.network.interfaces entry. Exactly one of
// Interface or DeviceSelector picks the link.
// +kubebuilder:validation:XValidation:rule="has(self.interface) != has(self.deviceSelector)",message="exactly one of interface or deviceSelector must pick the link"
type NetworkInterface struct {
	// Interface names the link (eno1np0). CP nodes must use names where a VIP
	// rides the link — talhelper derives the VIP's link from the interface name.
	// +optional
	Interface string `json:"interface,omitempty"`

	// DeviceSelector picks the link by hardware fact instead of name.
	// +optional
	DeviceSelector *DeviceSelector `json:"deviceSelector,omitempty"`

	// DHCP; nil renders nothing, an explicit false renders `dhcp: false` — the
	// talconfig convention for statically-addressed links.
	// +optional
	DHCP *bool `json:"dhcp,omitempty"`

	// MTU overrides the link MTU.
	// +optional
	MTU int `json:"mtu,omitempty"`

	// Addresses are the static addresses, primary family first.
	// +optional
	Addresses []string `json:"addresses,omitempty"`

	// Routes are static routes over this link.
	// +optional
	Routes []Route `json:"routes,omitempty"`

	// VIP is the Talos-managed shared virtual IP on this link (CP only).
	// +optional
	VIP *VIP `json:"vip,omitempty"`
}

// DeviceSelector matches a link by hardware fact (wildcards supported).
type DeviceSelector struct {
	// HardwareAddr matches the MAC address.
	// +optional
	HardwareAddr string `json:"hardwareAddr,omitempty"`
	// BusPath matches the PCI/USB bus prefix.
	// +optional
	BusPath string `json:"busPath,omitempty"`
}

// Route is a static route.
type Route struct {
	// Network is the destination CIDR.
	Network string `json:"network"`
	// Gateway is the next hop.
	// +optional
	Gateway string `json:"gateway,omitempty"`
	// Metric orders competing routes.
	// +optional
	Metric uint32 `json:"metric,omitempty"`
}

// VIP is a Talos-managed shared virtual IP.
type VIP struct {
	IP string `json:"ip"`
}

// TalosNodeStatus reports the render. Rendered=False is alert-and-hold: the last
// rendered Secret stays in place — a broken input never un-renders a machine's
// config.
type TalosNodeStatus struct {
	// Conditions: Rendered, plus the check-in observations MachineReachable and
	// Ready while a Host hands this node a machine.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ConfigSecretRef points at the Secret holding this node's rendered machine
	// config (key config.yaml). Owned by the TalosNode and GC'd with it — the
	// single place the rendered secrets exist at rest.
	// +optional
	ConfigSecretRef *SecretReference `json:"configSecretRef,omitempty"`

	// ConfigDigest is sha256:<hex> of the rendered config — what BootAsset builds
	// key their rebuilds on.
	// +optional
	ConfigDigest string `json:"configDigest,omitempty"`

	// Stage is the machine's observed boot stage (maintenance | booting |
	// running | …) — empty while no machine has answered the check-in.
	// +optional
	Stage string `json:"stage,omitempty"`

	// MachineVersion is the Talos version the machine reports.
	// +optional
	MachineVersion string `json:"machineVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="CP",type=boolean,JSONPath=`.spec.controlPlane`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.ipAddress`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`

// TalosNode is the per-machine half of the rendered OS config.
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
