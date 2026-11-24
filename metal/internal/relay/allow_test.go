package relay

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllow(t *testing.T) {
	a, err := ParseAllow(DefaultAllow)
	require.NoError(t, err)
	require.Len(t, a, 1)
	assert.True(t, a[0].anyAddr())
	assert.Equal(t, uint16(443), a[0].port)

	a, err = ParseAllow(" 10.0.0.0/24:443, [2001:db8:0:100::/64]:* ,192.0.2.7:8443")
	require.NoError(t, err)
	require.Len(t, a, 3)
	assert.True(t, a.allows(netip.MustParseAddr("10.0.0.9"), 443))
	assert.False(t, a.allows(netip.MustParseAddr("10.0.0.9"), 22))
	assert.False(t, a.allows(netip.MustParseAddr("10.0.1.9"), 443))
	assert.True(t, a.allows(netip.MustParseAddr("2001:db8:0:100::14"), 623))
	assert.True(t, a.allows(netip.MustParseAddr("192.0.2.7"), 8443))
	assert.False(t, a.allows(netip.MustParseAddr("192.0.2.8"), 8443))
	assert.True(t, a.allows(netip.MustParseAddr("::ffff:10.0.0.9"), 443), "a v4-mapped address matches its v4 rule")

	for _, bad := range []string{"", " , ", "443", "10.0.0.0/24", "10.0.0.0/33:443", "host.example:443", "*:0", "*:http", "*:70000"} {
		_, err := ParseAllow(bad)
		assert.Error(t, err, "%q", bad)
	}
}

func TestAllowTarget(t *testing.T) {
	ctx := context.Background()
	names := map[string][]netip.Addr{
		"bmc.example":   {netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("10.0.0.21")},
		"other.example": {netip.MustParseAddr("198.51.100.2")},
	}
	lookups := 0
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		lookups++
		if addrs, ok := names[host]; ok {
			return addrs, nil
		}
		return nil, errors.New("no such host")
	}

	var none Allow
	got, err := none.target(ctx, lookup, "anything:22")
	require.NoError(t, err)
	assert.Equal(t, "anything:22", got, "no allow list: every target, unchanged")

	cidr, err := ParseAllow("10.0.0.0/24:443")
	require.NoError(t, err)
	got, err = cidr.target(ctx, lookup, "10.0.0.21:443")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.21:443", got)
	_, err = cidr.target(ctx, lookup, "10.0.0.21:22")
	assert.ErrorContains(t, err, "not in the relay's allow list")
	got, err = cidr.target(ctx, lookup, "bmc.example:443")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.21:443", got, "a name is dialled at the address the rule allowed")
	_, err = cidr.target(ctx, lookup, "other.example:443")
	assert.ErrorContains(t, err, "not in the relay's allow list")
	_, err = cidr.target(ctx, lookup, "missing.example:443")
	assert.ErrorContains(t, err, "no such host")
	_, err = cidr.target(ctx, lookup, "no-port")
	assert.Error(t, err)

	lookups = 0
	def, err := ParseAllow(DefaultAllow)
	require.NoError(t, err)
	got, err = def.target(ctx, lookup, "bmc.example:443")
	require.NoError(t, err)
	assert.Equal(t, "bmc.example:443", got)
	assert.Zero(t, lookups, "a rule for any address needs no lookup")
	_, err = def.target(ctx, lookup, "bmc.example:22")
	assert.ErrorContains(t, err, "not in the relay's allow list")
}

// TestDialOutsideAllowListIsRefused: a signed DIAL to a target outside the
// allow list is answered with ERR, and nothing is dialled.
func TestDialOutsideAllowListIsRefused(t *testing.T) {
	allow, err := ParseAllow("10.0.0.0/24:443")
	require.NoError(t, err)
	tr := startTestRelay(t, time.Second, WithAllow(allow))

	for _, target := range []string{"10.0.0.21:22", "192.0.2.1:443"} {
		c, br := signedOpen(t, tr.tunnel, testSecret, "DIAL", target)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := io.ReadAll(br)
		require.NoError(t, err)
		assert.Equal(t, "ERR target "+target+" is not in the relay's allow list\n", string(got))
		_ = c.Close()
	}
	assert.Zero(t, tr.dials.count(), "a refused target is never dialled")

	// A target in the list is dialled (the recorder refuses the connection).
	c, br := signedOpen(t, tr.tunnel, testSecret, "DIAL", "10.0.0.21:443")
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.ReadAll(br)
	_ = c.Close()
	assert.Equal(t, 1, tr.dials.count())
}
