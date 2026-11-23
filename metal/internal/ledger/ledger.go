// Package ledger maps the MAC address of a machine's management controller
// (BMC) to the https URL the operator reaches it at. It is the operator's copy
// of the site's BMC address reservations — the same facts as the dhcpd or
// dnsmasq host entries for the management network — and the operator never
// invents an address: an Asset gets a ManagementEndpoint only when its bmcMac
// is listed here.
//
// The format is a flat YAML map, MAC → https base URL:
//
//	"0c:c4:7a:12:34:56": https://10.0.0.21
//	"0C-C4-7A-12-34-57": https://[2001:db8::22]
//
// Keys may be written in either case, with ':' or '-' between the bytes; they
// are normalised to lower case with colons on load. An entry that cannot be
// used is skipped and reported (Problems), never fatal: one typo must not stop
// the operator from managing every other machine.
package ledger

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/dennisklein/platform-kit/metal/internal/identity"
)

// Ledger is a loaded, normalised ledger.
type Ledger struct {
	byMAC map[string]string
	// invalid holds, per normalised MAC, why its entry was skipped — so an
	// Asset naming that MAC can be told what is wrong with its line.
	invalid  map[string]string
	problems []Problem
}

// Problem is one entry that was skipped on load.
type Problem struct {
	// Key is the entry's key as written.
	Key string
	// Reason says what is wrong with it and how to write it instead.
	Reason string
}

func (p Problem) String() string { return fmt.Sprintf("entry %q: %s", p.Key, p.Reason) }

// Load reads and parses a ledger file. It fails only when the file cannot be
// read or is not a YAML map; bad entries are skipped (see Problems).
func Load(path string) (*Ledger, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	l, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ledger: %s: %w", path, err)
	}
	return l, nil
}

// Parse parses ledger YAML. It fails only when the document is not a map of
// strings; every entry that cannot be used is skipped and listed in Problems.
func Parse(raw []byte) (*Ledger, error) {
	var entries map[string]string
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("not a YAML map of MAC to URL: %w", err)
	}
	l := &Ledger{byMAC: map[string]string{}, invalid: map[string]string{}}
	// Sorted, so that problems are reported in a stable order.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	seen := map[string]string{} // normalised MAC → key as written
	for _, key := range keys {
		addr := entries[key]
		mac, ok := identity.MAC(key)
		if !ok {
			l.problems = append(l.problems, Problem{Key: key,
				Reason: "not a MAC address; write it like 0c:c4:7a:12:34:56"})
			continue
		}
		if first, dup := seen[mac]; dup {
			if l.byMAC[mac] != addr {
				reason := fmt.Sprintf("the same MAC as %q, with another URL; keep one line", first)
				l.problems = append(l.problems, Problem{Key: key, Reason: reason})
				delete(l.byMAC, mac)
				l.invalid[mac] = reason
			}
			continue
		}
		seen[mac] = key
		u, err := url.Parse(strings.TrimSpace(addr))
		if err != nil || u.Scheme != "https" || u.Host == "" {
			reason := fmt.Sprintf("%q is not an https URL; write it like https://10.0.0.21", addr)
			l.problems = append(l.problems, Problem{Key: key, Reason: reason})
			l.invalid[mac] = reason
			continue
		}
		l.byMAC[mac] = strings.TrimSpace(addr)
	}
	return l, nil
}

// Lookup resolves a MAC, in any of the accepted forms, to its base URL.
func (l *Ledger) Lookup(mac string) (string, bool) {
	n, ok := identity.MAC(mac)
	if !ok {
		return "", false
	}
	addr, ok := l.byMAC[n]
	return addr, ok
}

// Invalid reports whether the ledger lists mac on a line that was skipped,
// and why.
func (l *Ledger) Invalid(mac string) (string, bool) {
	n, ok := identity.MAC(mac)
	if !ok {
		return "", false
	}
	reason, bad := l.invalid[n]
	return reason, bad
}

// Problems lists the entries skipped on load, in key order.
func (l *Ledger) Problems() []Problem { return slices.Clone(l.problems) }

// Len is the number of usable entries.
func (l *Ledger) Len() int { return len(l.byMAC) }
