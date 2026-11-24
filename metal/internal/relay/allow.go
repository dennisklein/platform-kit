package relay

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// DefaultAllow is the onward-dial allowlist of pk-metal relay: any address,
// port 443 only. Redfish and the BMCs' web interfaces answer there; a DIAL to
// anything else (ssh, IPMI's web ports, the rest of the network behind the
// relay) is refused. A site whose BMCs answer elsewhere names its own list.
const DefaultAllow = "*:443"

// Allow is the set of targets the relay dials onward to. A DIAL whose target
// matches no rule is answered with ERR and nothing is dialled, so a holder of
// the secret cannot use the relay as a general TCP pivot into the network it
// sits on. The nil Allow permits every target; Serve without WithAllow keeps
// that for the library's own callers (tests, the simulator), while the CLI
// always passes a list.
type Allow []allowRule

type allowRule struct {
	prefix netip.Prefix // invalid: any address
	port   uint16       // 0: any port
}

// ParseAllow reads a comma-separated list of <address>:<port> rules. The
// address is a CIDR (10.0.0.0/24, [2001:db8::/64]), a single IP, or * for any
// address; the port is a number or * for any port. Example:
// "10.0.0.0/24:443,[2001:db8:0:100::/64]:443".
func ParseAllow(s string) (Allow, error) {
	var a Allow
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		rule, err := parseRule(entry)
		if err != nil {
			return nil, err
		}
		a = append(a, rule)
	}
	if len(a) == 0 {
		return nil, fmt.Errorf("relay: the allow list %q names no target; give e.g. %s or 10.0.0.0/24:443", s, DefaultAllow)
	}
	return a, nil
}

func parseRule(entry string) (allowRule, error) {
	host, port, err := net.SplitHostPort(entry)
	if err != nil {
		return allowRule{}, fmt.Errorf("relay: allow rule %q is not <address>:<port>, e.g. 10.0.0.0/24:443 or [2001:db8::/64]:443", entry)
	}
	var r allowRule
	switch {
	case host == "*":
	case strings.Contains(host, "/"):
		p, err := netip.ParsePrefix(host)
		if err != nil {
			return allowRule{}, fmt.Errorf("relay: allow rule %q: %q is not a CIDR", entry, host)
		}
		r.prefix = p.Masked()
	default:
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return allowRule{}, fmt.Errorf("relay: allow rule %q: %q is not an IP address, a CIDR or *", entry, host)
		}
		r.prefix = netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen())
	}
	if port != "*" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return allowRule{}, fmt.Errorf("relay: allow rule %q: port %q is not a number from 1 to 65535 or *", entry, port)
		}
		r.port = uint16(n)
	}
	return r, nil
}

func (r allowRule) anyAddr() bool { return !r.prefix.IsValid() }

func (r allowRule) matches(ip netip.Addr, port uint16) bool {
	if r.port != 0 && r.port != port {
		return false
	}
	return r.anyAddr() || r.prefix.Contains(ip.Unmap())
}

// resolver looks a host name up; a field of the check so tests need no DNS.
type resolver func(ctx context.Context, host string) ([]netip.Addr, error)

func lookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// target decides a DIAL. It returns the address to dial, which for a host
// name is one of its addresses that a rule allows, so the name is not looked
// up a second time by the dial (and cannot answer differently then). A nil
// Allow returns hostport unchanged.
func (a Allow) target(ctx context.Context, lookup resolver, hostport string) (string, error) {
	if a == nil {
		return hostport, nil
	}
	host, portText, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("target %q is not host:port", hostport)
	}
	n, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return "", fmt.Errorf("target %q: port %q is not a number", hostport, portText)
	}
	port := uint16(n)

	if ip, err := netip.ParseAddr(host); err == nil {
		if a.allows(ip, port) {
			return hostport, nil
		}
		return "", fmt.Errorf("target %s is not in the relay's allow list", hostport)
	}
	// A name: when a rule allows any address on this port, the name needs no
	// lookup here.
	for _, r := range a {
		if r.anyAddr() && (r.port == 0 || r.port == port) {
			return hostport, nil
		}
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return "", fmt.Errorf("target %s: %w", hostport, err)
	}
	for _, ip := range addrs {
		if a.allows(ip, port) {
			return net.JoinHostPort(ip.Unmap().String(), portText), nil
		}
	}
	return "", fmt.Errorf("target %s is not in the relay's allow list", hostport)
}

func (a Allow) allows(ip netip.Addr, port uint16) bool {
	for _, r := range a {
		if r.matches(ip, port) {
			return true
		}
	}
	return false
}

// Option changes how Serve runs.
type Option func(*relay)

// WithAllow limits the onward dials to the targets a allows.
func WithAllow(a Allow) Option {
	return func(r *relay) { r.allow = a }
}
