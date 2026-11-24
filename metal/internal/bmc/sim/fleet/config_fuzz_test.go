package fleet_test

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

// listenKey is the address a listener binds, whatever its spelling: the
// port as a number and an IP host in its canonical form. Empty for port 0,
// which never clashes.
func listenKey(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if n, err := strconv.Atoi(port); err == nil {
		if n == 0 {
			return ""
		}
		port = strconv.Itoa(n)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, port)
}

// FuzzFleetParse: a fleet file that Parse accepts never has two machines
// with one name, one listen address or one MAC, however they are spelt.
func FuzzFleetParse(f *testing.F) {
	for _, seed := range []string{
		labFile,
		"machines:\n  - { name: a, listen: \"127.0.0.1:1\" }\n  - { name: b, listen: \"127.0.0.1:2\" }\n",
		"machines:\n  - { name: a, listen: \"127.0.0.1:0\" }\n  - { name: b, listen: \"127.0.0.1:0\" }\n",
		"machines:\n  - { name: a, listen: \"127.0.0.1:1\", identity: { bmcMAC: \"02:00:00:00:00:01\" } }\n" +
			"  - { name: b, listen: \"127.0.0.1:2\", identity: { nicMACs: [\"02:00:00:00:00:02\"] } }\n",
		"machines: []",
		"{",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		cfg, err := fleet.Parse(raw)
		if err != nil {
			return
		}
		assert.NotEmpty(t, cfg.Machines)
		names, listens, macs := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, m := range cfg.Machines {
			assert.False(t, names[m.Name], "name %q twice", m.Name)
			names[m.Name] = true
			if key := listenKey(m.Listen); key != "" {
				assert.False(t, listens[key], "listen %q twice", m.Listen)
				listens[key] = true
			}
			for _, mac := range append([]string{m.Identity.BMCMAC}, m.Identity.NICMACs...) {
				if mac == "" {
					continue
				}
				key := strings.ToLower(mac)
				assert.False(t, macs[key], "MAC %q twice", mac)
				macs[key] = true
			}
		}
	})
}
