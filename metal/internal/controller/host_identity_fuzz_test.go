package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
)

// FuzzHostNameCollision: two serials that map to one Host name must never
// merge silently. projectHost must report IdentityDiverged instead.
func FuzzHostNameCollision(f *testing.F) {
	f.Add("AB_12", "AB.12")
	f.Add("ab12", "AB12")
	f.Add("SIM-1", "SIM-1")
	f.Fuzz(func(t *testing.T, serialA, serialB string) {
		sysA := metalv1alpha1.DiscoveredSystem{ID: "1", SerialNumber: serialA, Manufacturer: "Huawei"}
		sysB := metalv1alpha1.DiscoveredSystem{ID: "1", SerialNumber: serialB, Manufacturer: "Huawei"}
		epA := endpointFor("ep-a", "https://10.0.0.1", "1", sysA)
		epB := endpointFor("ep-b", "https://10.0.0.2", "1", sysB)
		nameA, okA := identity.Name(identity.Normalize(epA.Status.Vendor, epA.Status.BoardSerial, sysA))
		nameB, okB := identity.Name(identity.Normalize(epB.Status.Vendor, epB.Status.BoardSerial, sysB))
		if !okA || !okB || nameA != nameB {
			return
		}
		host := &metalv1alpha1.Host{}
		host.Name = nameA
		projectHost(host, []hostPath{{ep: *epA, sys: sysA}, {ep: *epB, sys: sysB}}, nil, nil)
		c := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionInspected)
		require.NotNil(t, c)
		if serialA == serialB {
			assert.NotEqual(t, metalv1alpha1.ReasonIdentityDiverged, c.Reason)
			return
		}
		assert.Equal(t, metalv1alpha1.ReasonIdentityDiverged, c.Reason,
			"serials %q and %q share Host %q without a divergence", serialA, serialB, nameA)
	})
}
