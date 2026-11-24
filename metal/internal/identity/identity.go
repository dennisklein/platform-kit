// Package identity normalizes vendor-specific serial anchors into the
// path-independent Host identity (the multipath discipline). The
// signature is the invariant's first line of defense: nothing about the access
// path — endpoint name, address, manager MAC, manager firmware — is an input, so
// two endpoints fronting the same system cannot disagree unless they serve
// different facts about the machine itself.
package identity

import (
	"net"
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
//
// The name is cut to the 253 characters an object name may have. Two serials
// that share the cut prefix map to one Host, but the identity keeps the full
// serial, so the Host reports IdentityDiverged rather than merging them.
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
	if len(name) > maxNameLen {
		name = strings.TrimRight(name[:maxNameLen], "-")
	}
	return name, name != ""
}

// maxNameLen is the longest Kubernetes object name (a DNS-1123 subdomain).
const maxNameLen = 253

// MAC returns a hardware address in the one form this operator compares and
// stores: lower case, colon-separated (0c:c4:7a:12:34:56). It accepts what
// people paste from labels, dhcpd.conf and `ipmitool lan print`: either case,
// with ':' or '-' between the bytes, or the dotted 0cc4.7a12.3456 form.
// ok=false for anything that is not a 6-byte (EUI-48) address.
func MAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", false
	}
	return hw.String(), true
}

// SameMAC reports whether two hardware addresses name the same NIC, whatever
// form each is written in. Two strings that are not addresses compare as
// case-insensitive text, so a malformed value still matches itself.
func SameMAC(a, b string) bool {
	na, okA := MAC(a)
	nb, okB := MAC(b)
	if okA && okB {
		return na == nb
	}
	return strings.EqualFold(a, b)
}
