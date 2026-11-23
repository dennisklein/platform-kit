// Package v1alpha1 is the os.pk.io API group: the operating system
// payload rendered onto the machines the metal group manages.
//
// TalosCluster carries what every machine in a cluster shares — the Talos and
// Kubernetes versions, the control-plane endpoint, the certificate names that must
// be valid cluster-wide. TalosNode carries what belongs to one machine: the
// address it is reached at, whether it is a control-plane member, which disk to
// install onto. A BootAsset is the artifact a machine actually boots: either the
// generic install image, shared by every node and free of secrets, or the small
// per-node medium carrying that one node's rendered configuration.
//
// The spec shapes mirror Talos' own configuration field for field, and carry only
// the fields this operator uses. A configuration that names something this schema cannot
// represent is refused with an error rather than silently dropped — a config that
// renders without the field you set is worse than one that will not render.
//
// +kubebuilder:object:generate=true
// +groupName=os.pk.io
package v1alpha1

// Maintainers: this package imports apimachinery only. Rendering fills talhelper's
// own structs from these types in internal/talos/render; those types never leak
// into the API.

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
