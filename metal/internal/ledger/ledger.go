// Package ledger maps a management controller's NIC MAC to its Redfish base URL — a
// projection of control/platform/base/services/bmc-access/dnsmasq.conf, the address ledger
// of record (DHCPv6 reservations on the 1034 island). The operator never births an
// address fact: materializing an Asset into a ManagementEndpoint JOINS the org-asserted
// bmcMac against this file (live: the reservations; dev: sim URLs).
//
// Format is a flat YAML map, MAC → https base URL:
//
//	"02:00:48:0b:74:83": https://[fd6b:5a2e:1c4d:1034::14]
package ledger

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var macRE = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

type Ledger struct {
	byMAC map[string]string
}

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

func Parse(raw []byte) (*Ledger, error) {
	var entries map[string]string
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	l := &Ledger{byMAC: make(map[string]string, len(entries))}
	for mac, addr := range entries {
		if !macRE.MatchString(mac) {
			return nil, fmt.Errorf("entry %q: not a canonical MAC (lowercase, colon-separated)", mac)
		}
		u, err := url.Parse(addr)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("entry %q: %q is not an https base URL", mac, addr)
		}
		l.byMAC[mac] = addr
	}
	return l, nil
}

// Lookup resolves a MAC (any case) to its base URL.
func (l *Ledger) Lookup(mac string) (string, bool) {
	addr, ok := l.byMAC[strings.ToLower(mac)]
	return addr, ok
}

func (l *Ledger) Len() int { return len(l.byMAC) }
