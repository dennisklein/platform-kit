package ledger_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/dennisklein/platform-kit/metal/internal/identity"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
)

// FuzzLedgerParse feeds arbitrary bytes to Parse. The ledger is the only
// source of BMC addresses, so whatever it accepts must be an https URL, and a
// MAC listed twice with two URLs must resolve to neither.
func FuzzLedgerParse(f *testing.F) {
	for _, seed := range []string{
		`"0c:c4:7a:12:34:56": https://10.0.0.21`,
		`"0C-C4-7A-12-34-57": https://[2001:db8::22]`,
		`"0cc4.7a12.3458": https://10.0.0.23:8443`,
		"\"0c:c4:7a:12:34:56\": https://10.0.0.21\n\"0C:C4:7A:12:34:56\": https://10.0.0.22\n",
		"\"0c:c4:7a:12:34:56\": https://10.0.0.21\n\"0C-C4-7A-12-34-56\": https://10.0.0.21\n",
		`"0c:c4:7a:12:34:56": http://10.0.0.21`,
		`"not-a-mac": https://10.0.0.21`,
		`"0c:c4:7a:12:34:56": "  https://10.0.0.21  "`,
		`[]`,
		``,
		`{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		l, err := ledger.Parse(raw)
		if err != nil {
			return
		}
		var entries map[string]string
		require.NoError(t, yaml.Unmarshal(raw, &entries), "Parse accepted what yaml refuses")

		// Group the entries by the MAC their key normalises to.
		urls := map[string]map[string]bool{}
		for k, v := range entries {
			mac, ok := identity.MAC(k)
			if !ok {
				continue
			}
			if urls[mac] == nil {
				urls[mac] = map[string]bool{}
			}
			urls[mac][v] = true
		}

		resolved := 0
		for mac, vals := range urls {
			addr, ok := l.Lookup(mac)
			_, bad := l.Invalid(mac)
			assert.False(t, ok && bad, "MAC %s is both resolvable and invalid", mac)
			if ok {
				resolved++
				u, err := url.Parse(addr)
				require.NoError(t, err)
				assert.Equal(t, "https", u.Scheme, "Lookup(%s) = %q", mac, addr)
				assert.NotEmpty(t, u.Host, "Lookup(%s) = %q", mac, addr)
			}
			if len(vals) > 1 {
				assert.False(t, ok, "MAC %s has %d URLs but resolves to %q", mac, len(vals), addr)
			}
		}
		assert.Equal(t, resolved, l.Len())
		assert.LessOrEqual(t, l.Len()+len(l.Problems()), len(entries))

		again, err := ledger.Parse(raw)
		require.NoError(t, err)
		assert.Equal(t, l.Problems(), again.Problems(), "re-parse gives other problems")
	})
}
