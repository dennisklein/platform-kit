// Package v1alpha1 is the metal.pk.io API group: the machines in the
// rack, and what the cluster has been asked to do with them.
//
// Two kinds of truth live here and never mix. An Asset is what the organization
// asserts about a machine — the serial it was bought under, the MAC of its
// management controller, where it was installed. A ManagementEndpoint and a Host
// are what the cluster found when it went and looked. Where the two disagree the
// disagreement is reported rather than quietly resolved: a Host whose discovered
// serial contradicts its Asset stops and says so, because one of the two facts is
// wrong and only a human knows which.
//
// A HostClaim asks for a machine the way a PersistentVolumeClaim asks for storage;
// a HostOperation is one thing done to one machine — a power cycle, a BIOS apply,
// a firmware flash — and the record that it happened. The profile kinds
// (BiosProfile, FirmwareProfile, EndpointProfile) are the desired settings a
// machine is held against.
//
// Read status through conditions. Every kind reports what is true about it right
// now as a set of conditions; the phase and blockedOn fields summarize those for
// people and for kubectl's columns. Conditions are the contract — branch on them,
// not on the phase string.
//
// +kubebuilder:object:generate=true
// +groupName=metal.pk.io
package v1alpha1

// Maintainers: this package imports apimachinery and core/v1 and nothing else.
// API packages must stay cheap for anyone to import.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion identifies the API group and version of every kind in this package.
	GroupVersion = schema.GroupVersion{Group: "metal.pk.io", Version: "v1alpha1"}

	// SchemeBuilder registers the group's kinds; AddToScheme is what consumers call.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Asset{}, &AssetList{},
		&ManagementEndpoint{}, &ManagementEndpointList{},
		&Host{}, &HostList{},
		&HostClaim{}, &HostClaimList{},
		&HostOperation{}, &HostOperationList{},
		&BiosProfile{}, &BiosProfileList{},
		&FirmwareProfile{}, &FirmwareProfileList{},
		&EndpointProfile{}, &EndpointProfileList{},
		&BiosSettings{}, &BiosSettingsList{},
		&BiosSchema{}, &BiosSchemaList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Reference names another cluster-scoped object of this group.
type Reference struct {
	Name string `json:"name"`
}
