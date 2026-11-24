// Package v1alpha1 is the os.pk.io API group: the operating system payload
// rendered onto the machines the metal group manages.
//
// TalosCluster carries what every member of a cluster shares (the Talos and
// Kubernetes versions, the API endpoint and VIP, pod and service networking,
// registry mirrors, the reference to the secrets bundle) and reports what the
// members form together: whether etcd was bootstrapped, whether the Kubernetes
// API answers, and where the admin credentials were published. TalosNode
// carries what belongs to one machine (its role, install disk, links, labels
// and taints, image schematic) and reports its rendered config and check-in. A
// BootAsset is the artifact a machine actually boots: either the generic
// install image, shared by every node and free of secrets, or the small
// per-node medium carrying that one node's rendered configuration.
//
// The kinds describe intent in this operator's terms, not Talos' document
// layout: internal/talos/render turns them into a Talos 1.14 multi-document
// machine configuration, and patches reach anything the kinds do not model.
//
// +kubebuilder:object:generate=true
// +groupName=os.pk.io
package v1alpha1

// Maintainers: this package imports apimachinery only. Rendering builds Talos'
// own config documents from these types in internal/talos/render; those types
// never leak into the API.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion identifies the API group and version of every kind in this package.
	GroupVersion = schema.GroupVersion{Group: "os.pk.io", Version: "v1alpha1"}

	// SchemeBuilder registers the group's kinds; AddToScheme is what consumers call.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&TalosCluster{}, &TalosClusterList{},
		&TalosNode{}, &TalosNodeList{},
		&BootAsset{}, &BootAssetList{},
		&TalosRollout{}, &TalosRolloutList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Reference names another cluster-scoped object of this group. Declared here
// rather than shared with the metal group, so the two groups can version apart.
type Reference struct {
	Name string `json:"name"`
}

// SecretReference points at a namespaced Secret from a cluster-scoped object.
type SecretReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
