package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestClaimHostNames(t *testing.T) {
	sysA := metalv1alpha1.DiscoveredSystem{ID: "1", SerialNumber: "SER-A", Manufacturer: "Huawei"}
	sysB := metalv1alpha1.DiscoveredSystem{ID: "2", SerialNumber: "SER-B", Manufacturer: "Huawei"}
	ref := func(name string) *metalv1alpha1.Reference { return &metalv1alpha1.Reference{Name: name} }
	ep := func(systems ...metalv1alpha1.DiscoveredSystem) *metalv1alpha1.ManagementEndpoint {
		return &metalv1alpha1.ManagementEndpoint{
			Status: metalv1alpha1.ManagementEndpointStatus{Vendor: "Huawei", Systems: systems},
		}
	}

	assert.Equal(t, []string{"ser-a"}, claimHostNames(ep(sysA), nil),
		"one system needs no systemRef")
	assert.Equal(t, []string{"ser-a", "ser-b"}, claimHostNames(ep(sysA, sysB), nil),
		"two systems without a systemRef are ambiguous (2 matches)")
	assert.Equal(t, []string{"ser-b"}, claimHostNames(ep(sysA, sysB), ref("2")),
		"systemRef narrows by the Redfish member id")
	assert.Empty(t, claimHostNames(ep(sysA), ref("9")),
		"a systemRef naming no fronted system matches nothing")
	assert.Empty(t, claimHostNames(ep(), nil),
		"no discovery yet, nothing to select")

	// Two member ids reporting the same machine (a vendor quirk) still select ONE
	// host — names are deduped, not double-counted into a false ambiguity.
	assert.Equal(t, []string{"ser-a"},
		claimHostNames(ep(sysA, metalv1alpha1.DiscoveredSystem{ID: "2", SerialNumber: "SER-A", Manufacturer: "Huawei"}), nil))
}
