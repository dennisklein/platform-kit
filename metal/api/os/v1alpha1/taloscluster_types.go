package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TalosClusterSpec is everything talconfig.yaml says above `nodes:` — the render
// inputs every member shares. The cluster name is the CR's metadata.name.
type TalosClusterSpec struct {
	// TalosVersion pins the Talos release whose config contract the render targets
	// (talenv.yaml's TALOS_VERSION, resolved — CRs carry literals, never ${VAR}).
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	TalosVersion string `json:"talosVersion"`

	// KubernetesVersion pins the Kubernetes release to install.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	KubernetesVersion string `json:"kubernetesVersion"`

	// Endpoint is the cluster-internal control-plane endpoint machines enroll
	// against (the 1035 VIP — org/admin access rides the 138 VIP via cert SANs).
	// +kubebuilder:validation:Pattern=`^https://`
	Endpoint string `json:"endpoint"`

	// AllowSchedulingOnControlPlanes lets workloads run on control-plane nodes
	// (ADR 0001: few nodes — use all).
	// +optional
	AllowSchedulingOnControlPlanes bool `json:"allowSchedulingOnControlPlanes,omitempty"`

	// ClusterPodNets are the pod subnets, primary family first (IPv6-primary).
	// +optional
	ClusterPodNets []string `json:"clusterPodNets,omitempty"`

	// ClusterSvcNets are the service subnets, primary family first.
	// +optional
	ClusterSvcNets []string `json:"clusterSvcNets,omitempty"`

	// AdditionalApiServerCertSans lets the API server answer on every VIP and name
	// admins reach it by.
	// +optional
	AdditionalApiServerCertSans []string `json:"additionalApiServerCertSans,omitempty"`

	// AdditionalMachineCertSans is the same for the machine (apid) certificate.
	// +optional
	AdditionalMachineCertSans []string `json:"additionalMachineCertSans,omitempty"`

	// Patches apply to every node: inline strategic-merge YAML, or "@./path"
	// references resolved against the render's patch root (the ConfigMap-mounted
	// mirror of control/talos/ in-cluster, the CR directory on the CLI).
	// +optional
	Patches []string `json:"patches,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Talos",type=string,JSONPath=`.spec.talosVersion`
// +kubebuilder:printcolumn:name="Kubernetes",type=string,JSONPath=`.spec.kubernetesVersion`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`

// TalosCluster is the cluster-level half of the rendered OS config. Spec-only for
// now: status arrives with the reconciler that projects render/convergence state
// not before something writes it.
type TalosCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TalosClusterSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// TalosClusterList contains a list of TalosCluster.
type TalosClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TalosCluster `json:"items"`
}
