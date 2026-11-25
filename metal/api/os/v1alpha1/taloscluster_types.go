package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TalosClusterSpec is what every member of one Talos cluster shares. The cluster
// name is the object's metadata.name.
type TalosClusterSpec struct {
	// TalosVersion is the Talos release the cluster runs. It selects the
	// configuration contract the render targets and the installer image tag, so
	// it must match what the machines boot. Talos v1.14 or later.
	// +kubebuilder:validation:Pattern=`^v1\.(1[4-9]|[2-9][0-9])\.[0-9]+$`
	TalosVersion string `json:"talosVersion"`

	// KubernetesVersion is the Kubernetes release the control plane runs.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	KubernetesVersion string `json:"kubernetesVersion"`

	// Endpoint is the Kubernetes API URL every member enrolls against and the
	// admin kubeconfig names: the control-plane VIP or a DNS name for it, with
	// the port (https://10.77.0.10:6443).
	// +kubebuilder:validation:Pattern=`^https://[^/]+$`
	Endpoint string `json:"endpoint"`

	// ControlPlane carries the settings only control-plane members render.
	// +optional
	ControlPlane *ControlPlaneSpec `json:"controlPlane,omitempty"`

	// Network is the cluster's pod and service networking.
	// +optional
	Network ClusterNetwork `json:"network,omitempty"`

	// CertSANs are extra names the Kubernetes API server and the Talos API
	// certificates must be valid for, beyond the endpoint, the VIP and the
	// node addresses Talos adds on its own.
	// +optional
	// +listType=set
	CertSANs []string `json:"certSANs,omitempty"`

	// RegistryMirrors redirect image pulls, one entry per upstream registry. A
	// site without internet access needs them before the first node installs,
	// which is why they live here rather than in a patch someone has to
	// remember.
	// +optional
	// +listType=map
	// +listMapKey=registry
	RegistryMirrors []RegistryMirror `json:"registryMirrors,omitempty"`

	// SecretsRef names the Secret holding the cluster's Talos secrets bundle
	// under key secrets.yaml, as `talosctl gen secrets` writes it. A person
	// generates the bundle once and commits it sealed; the operator only reads
	// it. Every certificate and key in a rendered config, and the
	// kubeconfig and talosconfig the operator publishes, derive from it.
	SecretsRef SecretReference `json:"secretsRef"`

	// Schematic is the installer image customization every node uses unless it
	// sets its own (a node's schematic replaces this one, it does not merge
	// with it).
	// +optional
	Schematic *Schematic `json:"schematic,omitempty"`

	// Patches apply to every node, in order, after the operator's own
	// rendering and before the node's patches. A document that Talos allows
	// on control planes only (KubeAuditPolicyConfig, KubeAPIServerConfig)
	// reaches the control planes only: the operator leaves it out of a
	// worker's configuration.
	// +optional
	Patches []Patch `json:"patches,omitempty"`

	// Credentials sets the lifetime of the admin credentials the operator
	// publishes, and whether it also publishes a read-only talosconfig.
	// +optional
	Credentials *CredentialsSpec `json:"credentials,omitempty"`
}

// CredentialsSpec configures the published credentials. Certificates signed
// by the cluster's CAs cannot be revoked short of a CA rotation, so a short
// lifetime is what bounds a leaked copy.
type CredentialsSpec struct {
	// TTL is how long a published admin kubeconfig and talosconfig are
	// valid. A credential is minted again once less than a third of it is
	// left. Default 24h, at most 720h (30 days): a longer one stays valid
	// long after the person who copied it has gone.
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('720h')",message="ttl must be at most 720h: a published credential cannot be revoked short of a CA rotation"
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// ReaderTalosconfig also publishes <cluster>-talosconfig-reader: a
	// talosconfig with the os:reader role, for monitoring, which can read
	// but not change a machine.
	// +optional
	ReaderTalosconfig bool `json:"readerTalosconfig,omitempty"`
}

// ControlPlaneSpec holds control-plane-only rendering.
type ControlPlaneSpec struct {
	// VIP is a shared address the control-plane members announce over layer 2
	// (Talos' Layer2VIPConfig). Each control-plane node carries it on the link
	// whose static address shares a subnet with it.
	// +optional
	VIP string `json:"vip,omitempty"`

	// AllowScheduling lets ordinary workloads run on control-plane nodes (the
	// control-plane NoSchedule taint is not rendered).
	// +optional
	AllowScheduling bool `json:"allowScheduling,omitempty"`
}

// CNIName selects the pod network plugin Talos deploys.
// +kubebuilder:validation:Enum=flannel;none;custom
type CNIName string

const (
	// CNIFlannel deploys Talos' built-in Flannel.
	CNIFlannel CNIName = "flannel"
	// CNINone deploys nothing: the cluster's own tooling installs a CNI after
	// bootstrap, and nodes stay NotReady until it does.
	CNINone CNIName = "none"
	// CNICustom applies the manifests at CNI.URLs instead of Flannel.
	CNICustom CNIName = "custom"
)

// ClusterNetwork is the cluster-wide Kubernetes networking.
type ClusterNetwork struct {
	// PodSubnets are the pod CIDRs, primary family first. Empty selects
	// Talos' default for the endpoint's address family.
	// +optional
	PodSubnets []string `json:"podSubnets,omitempty"`

	// ServiceSubnets are the service CIDRs, primary family first. Empty
	// selects Talos' default for the endpoint's address family.
	// +optional
	ServiceSubnets []string `json:"serviceSubnets,omitempty"`

	// CNI selects the pod network plugin. Empty means flannel.
	// +optional
	CNI *CNI `json:"cni,omitempty"`
}

// CNI selects the pod network plugin.
// +kubebuilder:validation:XValidation:rule="(self.name == 'custom') == (has(self.urls) && size(self.urls) > 0)",message="urls are required with name custom and refused otherwise"
type CNI struct {
	Name CNIName `json:"name"`

	// URLs are the manifests a custom CNI is installed from.
	// +optional
	URLs []string `json:"urls,omitempty"`
}

// RegistryMirror redirects pulls from one registry to mirror endpoints.
type RegistryMirror struct {
	// Registry is the upstream registry host the mirror stands in for
	// (docker.io, ghcr.io, factory.talos.dev), or "*" for every registry.
	// +kubebuilder:validation:MinLength=1
	Registry string `json:"registry"`

	// Endpoints are the mirror URLs, tried in order.
	// +kubebuilder:validation:MinItems=1
	Endpoints []string `json:"endpoints"`
}

// TalosClusterStatus is observed: whether etcd was bootstrapped, whether the
// Kubernetes API answers, how many members are Ready, and where the admin
// credentials derived from the secrets bundle were published.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.bootstrappedAt) || (has(self.bootstrappedAt) && self.bootstrappedAt == oldSelf.bootstrappedAt)",message="bootstrappedAt is write-once: a cluster that was bootstrapped stays bootstrapped"
type TalosClusterStatus struct {
	// BootstrappedAt is when the operator first observed this cluster's etcd
	// running (or its Kubernetes API answering). It is the one remembered
	// fact in this status: once set the operator never bootstraps the
	// cluster again, so that a cluster whose control planes all lost their
	// etcd data is restored by a person instead of silently started empty
	// (Bootstrapped=False, reason EtcdDataLost). Write-once; starting over
	// means deleting and re-creating the TalosCluster.
	// +optional
	BootstrappedAt *metav1.Time `json:"bootstrappedAt,omitempty"`

	// Conditions: Bootstrapped and Available; VersionsSupported (the
	// Kubernetes version against the Talos version), VersionsCurrent (what
	// the machines and the API run against the spec) and SecretsBundleHeld
	// (an edited secrets bundle the machines do not run yet).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ControlPlanes and Workers count the TalosNodes that name this cluster
	// and that a machine is handed (a Host's spec.os names them); the Ready
	// counts are the ones whose Ready condition is True. A TalosNode with no
	// machine yet, or one being removed, is counted in UnassignedNodes
	// instead: it is not a member, so it does not hold Available at False.
	// +optional
	ControlPlanes int32 `json:"controlPlanes,omitempty"`
	// +optional
	ReadyControlPlanes int32 `json:"readyControlPlanes,omitempty"`
	// +optional
	Workers int32 `json:"workers,omitempty"`
	// +optional
	ReadyWorkers int32 `json:"readyWorkers,omitempty"`
	// UnassignedNodes counts the TalosNodes that name this cluster but run
	// on no machine: not handed to a Host yet, or being removed.
	// +optional
	UnassignedNodes int32 `json:"unassignedNodes,omitempty"`

	// KubernetesVersion is the version the Kubernetes API reports, empty
	// while it does not answer.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// KubeconfigSecretRef points at the Secret (key kubeconfig) holding an
	// admin kubeconfig minted from the bundle's Kubernetes CA.
	// +optional
	KubeconfigSecretRef *SecretReference `json:"kubeconfigSecretRef,omitempty"`

	// TalosconfigSecretRef points at the Secret (key talosconfig) holding an
	// admin talosconfig minted from the bundle's Talos CA, with every
	// control-plane address as an endpoint.
	// +optional
	TalosconfigSecretRef *SecretReference `json:"talosconfigSecretRef,omitempty"`

	// ReaderTalosconfigSecretRef points at the os:reader talosconfig, when
	// spec.credentials.readerTalosconfig asks for one.
	// +optional
	ReaderTalosconfigSecretRef *SecretReference `json:"readerTalosconfigSecretRef,omitempty"`

	// CredentialsExpireAt is when the first of the published credentials
	// expires. The operator mints new ones well before; a time in the past
	// means it could not.
	// +optional
	CredentialsExpireAt *metav1.Time `json:"credentialsExpireAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Talos",type=string,JSONPath=`.spec.talosVersion`
// +kubebuilder:printcolumn:name="Kubernetes",type=string,JSONPath=`.spec.kubernetesVersion`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="CP",type=string,JSONPath=`.status.readyControlPlanes`
// +kubebuilder:printcolumn:name="Workers",type=string,JSONPath=`.status.readyWorkers`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`

// TalosCluster is one Talos cluster: what its members share, and the
// observed state of the cluster they form.
type TalosCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TalosClusterSpec `json:"spec"`
	// +optional
	Status TalosClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TalosClusterList contains a list of TalosCluster.
type TalosClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TalosCluster `json:"items"`
}
