package identity_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
)

func TestNormalizeVendorStrategies(t *testing.T) {
	tests := []struct {
		name           string
		endpointVendor string
		boardSerial    string
		sys            metalv1alpha1.DiscoveredSystem
		want           metalv1alpha1.HostIdentity
	}{
		{name: "Huawei anchors on the system serial",
			endpointVendor: "Huawei", boardSerial: "SIMHW2288V5000391",
			sys: metalv1alpha1.DiscoveredSystem{
				UUID:         "5A1B0001-0000-4000-8000-000000000391",
				SerialNumber: "SIMHW2288V5000391", Manufacturer: "Huawei"},
			want: metalv1alpha1.HostIdentity{
				UUID:   "5A1B0001-0000-4000-8000-000000000391",
				Serial: "SIMHW2288V5000391", Vendor: "Huawei"}},
		{name: "Supermicro anchors on the board serial (system serves none)",
			endpointVendor: "Supermicro", boardSerial: "SIMX12B000001",
			sys: metalv1alpha1.DiscoveredSystem{
				UUID: "5A1B0002-0000-4000-8000-000000000001", Manufacturer: "Supermicro"},
			want: metalv1alpha1.HostIdentity{
				UUID:   "5A1B0002-0000-4000-8000-000000000001",
				Serial: "SIMX12B000001", Vendor: "Supermicro"}},
		{name: "system manufacturer outranks the endpoint vendor (aggregator path)",
			endpointVendor: "SomeAggregator", boardSerial: "",
			sys:  metalv1alpha1.DiscoveredSystem{SerialNumber: "S1", Manufacturer: "Huawei"},
			want: metalv1alpha1.HostIdentity{Serial: "S1", Vendor: "Huawei"}},
		{name: "endpoint vendor fills in when the system serves no manufacturer",
			endpointVendor: "Supermicro", boardSerial: "BOARD",
			sys:  metalv1alpha1.DiscoveredSystem{},
			want: metalv1alpha1.HostIdentity{Serial: "BOARD", Vendor: "Supermicro"}},
		{name: "unknown vendor prefers the system serial",
			endpointVendor: "Lenovo", boardSerial: "BOARD",
			sys:  metalv1alpha1.DiscoveredSystem{SerialNumber: "SYS", Manufacturer: "Lenovo"},
			want: metalv1alpha1.HostIdentity{Serial: "SYS", Vendor: "Lenovo"}},
		{name: "unknown vendor falls back to the board serial",
			endpointVendor: "Lenovo", boardSerial: "BOARD",
			sys:  metalv1alpha1.DiscoveredSystem{Manufacturer: "Lenovo"},
			want: metalv1alpha1.HostIdentity{Serial: "BOARD", Vendor: "Lenovo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, identity.Normalize(tt.endpointVendor, tt.boardSerial, tt.sys))
		})
	}
}

func TestName(t *testing.T) {
	name, ok := identity.Name(metalv1alpha1.HostIdentity{Serial: "SIMHW2288V5000391"})
	assert.True(t, ok)
	assert.Equal(t, "simhw2288v5000391", name)

	name, ok = identity.Name(metalv1alpha1.HostIdentity{Serial: "AB_12.C/"})
	assert.True(t, ok, "non-DNS characters are mapped, not fatal")
	assert.Equal(t, "ab-12-c", name, "mapped inside, trimmed at the edges")

	_, ok = identity.Name(metalv1alpha1.HostIdentity{UUID: "has-uuid-but-no-serial"})
	assert.False(t, ok, "no serial anchor — cannot enroll")
}

func TestMAC(t *testing.T) {
	for in, want := range map[string]string{
		"0c:c4:7a:12:34:56":   "0c:c4:7a:12:34:56",
		"0C:C4:7A:12:34:56":   "0c:c4:7a:12:34:56",
		"0c-c4-7a-12-34-56":   "0c:c4:7a:12:34:56",
		"0C-C4-7A-12-34-5F":   "0c:c4:7a:12:34:5f",
		"0cc4.7a12.3456":      "0c:c4:7a:12:34:56",
		" 0c:c4:7a:12:34:56 ": "0c:c4:7a:12:34:56",
	} {
		got, ok := identity.MAC(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "0c:c4:7a:12:34", "zz:c4:7a:12:34:56", "0c:c4:7a:12:34:56:78:9a", "not a mac"} {
		_, ok := identity.MAC(bad)
		assert.False(t, ok, bad)
	}
	assert.True(t, identity.SameMAC("0C-C4-7A-12-34-56", "0c:c4:7a:12:34:56"))
	assert.False(t, identity.SameMAC("0c:c4:7a:12:34:57", "0c:c4:7a:12:34:56"))
}
