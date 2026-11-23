package ledger_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/ledger"
)

func TestParse(t *testing.T) {
	l, err := ledger.Parse([]byte(`
"02:00:48:0b:74:83": https://[fd6b:5a2e:1c4d:1034::14]
"02:00:5a:3a:cc:18": https://127.0.0.1:8443
`))
	require.NoError(t, err)
	assert.Equal(t, 2, l.Len())

	addr, ok := l.Lookup("02:00:48:0B:74:83")
	assert.True(t, ok, "lookup is case-insensitive")
	assert.Equal(t, "https://[fd6b:5a2e:1c4d:1034::14]", addr)

	_, ok = l.Lookup("00:00:00:00:00:00")
	assert.False(t, ok)
}

func TestParseRejects(t *testing.T) {
	_, err := ledger.Parse([]byte(`"48-DC-2D-0B-74-83": https://x`))
	assert.Error(t, err, "non-canonical MAC")

	_, err = ledger.Parse([]byte(`"02:00:48:0b:74:83": http://[fd6b::14]`))
	assert.Error(t, err, "plain http")

	_, err = ledger.Parse([]byte(`{not yaml`))
	assert.Error(t, err)
}

func TestParseEmpty(t *testing.T) {
	l, err := ledger.Parse(nil)
	require.NoError(t, err)
	assert.Equal(t, 0, l.Len())
}
