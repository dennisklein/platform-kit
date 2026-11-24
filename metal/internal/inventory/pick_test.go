package inventory_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/inventory"
)

func TestPickNarrowsToOneSystem(t *testing.T) {
	two := inventory.Facts{Systems: []bmc.System{{ID: "1", SerialNumber: "A"}, {ID: "2", SerialNumber: "B"}}}

	got, err := two.Pick("2")
	require.NoError(t, err)
	require.Len(t, got.Systems, 1)
	assert.Equal(t, "B", got.Systems[0].SerialNumber)
	assert.Len(t, two.Systems, 2, "the gathered facts are left as they are")

	_, err = two.Pick("")
	require.ErrorContains(t, err, "fronts 2 systems (1, 2); give one with --system")
	_, err = two.Pick("3")
	require.ErrorContains(t, err, `no system "3"`)

	one := inventory.Facts{Systems: []bmc.System{{ID: "Self"}}}
	got, err = one.Pick("")
	require.NoError(t, err)
	assert.Equal(t, "Self", got.Systems[0].ID)
}
