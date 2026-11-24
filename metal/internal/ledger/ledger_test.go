package ledger_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/ledger"
)

func TestParse(t *testing.T) {
	l, err := ledger.Parse([]byte(`
"02:00:48:0b:74:83": https://[2001:db8::14]
"02:00:5a:3a:cc:18": https://127.0.0.1:8443
`))
	require.NoError(t, err)
	assert.Equal(t, 2, l.Len())
	assert.Empty(t, l.Problems())

	addr, ok := l.Lookup("02:00:48:0B:74:83")
	assert.True(t, ok, "lookup is case-insensitive")
	assert.Equal(t, "https://[2001:db8::14]", addr)

	_, ok = l.Lookup("00:00:00:00:00:00")
	assert.False(t, ok)
}

func TestParseAcceptsTheFormsPeoplePaste(t *testing.T) {
	// Upper case from a BMC label, dashes from a Windows inventory, mixed in
	// one file: every one of them loads and answers in every form.
	l, err := ledger.Parse([]byte(`
"0C:C4:7A:12:34:56": https://10.0.0.21
"0c-c4-7a-12-34-57": https://10.0.0.22
"0C-C4-7A-12-34-58": https://10.0.0.23
`))
	require.NoError(t, err)
	assert.Equal(t, 3, l.Len())
	assert.Empty(t, l.Problems())
	for mac, want := range map[string]string{
		"0c:c4:7a:12:34:56": "https://10.0.0.21",
		"0C-C4-7A-12-34-57": "https://10.0.0.22",
		"0c:c4:7a:12:34:58": "https://10.0.0.23",
	} {
		got, ok := l.Lookup(mac)
		assert.True(t, ok, mac)
		assert.Equal(t, want, got, mac)
	}
}

func TestBadEntriesAreSkippedNotFatal(t *testing.T) {
	l, err := ledger.Parse([]byte(`
"0c:c4:7a:12:34:56": https://10.0.0.21
"0c:c4:7a:12:34": https://10.0.0.22
"0c:c4:7a:12:34:59": http://10.0.0.24
"0C:C4:7A:12:34:5A": https://10.0.0.25
"0c-c4-7a-12-34-5a": https://10.0.0.26
`))
	require.NoError(t, err, "one bad line never stops the operator")
	assert.Equal(t, 1, l.Len(), "only the good line is usable")
	_, ok := l.Lookup("0c:c4:7a:12:34:56")
	assert.True(t, ok)

	problems := l.Problems()
	require.Len(t, problems, 3)
	assert.Equal(t, "0c-c4-7a-12-34-5a", problems[0].Key, "sorted by key")
	assert.Contains(t, problems[0].Reason, "same MAC")
	assert.Equal(t, "0c:c4:7a:12:34", problems[1].Key)
	assert.Contains(t, problems[1].Reason, "not a MAC address")
	assert.Equal(t, "0c:c4:7a:12:34:59", problems[2].Key)
	assert.Contains(t, problems[2].Reason, "not an https URL")

	reason, bad := l.Invalid("0C:C4:7A:12:34:59")
	assert.True(t, bad, "an Asset naming this MAC can be told what is wrong")
	assert.Contains(t, reason, "https")
	_, bad = l.Invalid("0c:c4:7a:12:34:5a")
	assert.True(t, bad, "a MAC listed twice with two URLs is used for neither")
	_, bad = l.Invalid("0c:c4:7a:12:34:56")
	assert.False(t, bad)
}

func TestParseRejectsANonMap(t *testing.T) {
	_, err := ledger.Parse([]byte(`{not yaml`))
	assert.Error(t, err)
	_, err = ledger.Parse([]byte(`- a list`))
	assert.Error(t, err)
}

func TestParseEmpty(t *testing.T) {
	l, err := ledger.Parse(nil)
	require.NoError(t, err)
	assert.Equal(t, 0, l.Len())
}
