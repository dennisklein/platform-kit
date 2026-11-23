// Package identity normalizes vendor-specific serial anchors into the
// path-independent Host identity (ADR 0012, the multipath discipline). The
// signature is the invariant's first line of defense: nothing about the access
// path — endpoint name, address, manager MAC, manager firmware — is an input, so
// two endpoints fronting the same system cannot disagree unless they serve
// different facts about the machine itself.
package identity

import (
	"strings"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Normalize computes the identity anchor for one discovered system. The vendor
// driving the strategy is the system's own manufacturer (an aggregator path
// reports its manager vendor, not the machine's), falling back to the endpoint's
// vendor when the system serves none. Per-vendor anchors: Supermicro's
// ComputerSystem carries no serial — the chassis board serial anchors; Huawei
// serves the chassis serial on the system. Unknown vendors prefer the system
// serial and fall back to the board serial.
func Normalize(endpointVendor, boardSerial string, sys metalv1alpha1.DiscoveredSystem) metalv1alpha1.HostIdentity {
	vendor := sys.Manufacturer
	if vendor == "" {
		vendor = endpointVendor
	}
	id := metalv1alpha1.HostIdentity{UUID: sys.UUID, Vendor: vendor}
	switch {
	case strings.EqualFold(vendor, "Supermicro"):
		id.Serial = boardSerial
	case strings.EqualFold(vendor, "Huawei"):
		id.Serial = sys.SerialNumber
	default:
		id.Serial = sys.SerialNumber
		if id.Serial == "" {
			id.Serial = boardSerial
		}
	}
	return id
}

// Name derives the Host object name from the identity: the serial anchor
// lowercased with anything outside DNS-1123 mapped to '-'. Deterministic, so
// every path to the same system converges on the same Host object. ok=false
// when there is no serial anchor to key on — such a system cannot enroll.
func Name(id metalv1alpha1.HostIdentity) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(id.Serial) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	return name, name != ""
}
