package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The manager runs AllLanes. A lane added to the list of constants but not to
// AllLanes would build in the tests and never run in production.
func TestAllLanesNamesEveryLane(t *testing.T) {
	require.Len(t, AllLanes, int(laneCount))
	seen := map[Lane]bool{}
	for _, l := range AllLanes {
		assert.False(t, seen[l], "lane %d listed twice", l)
		seen[l] = true
	}
}

func TestSetupAllRefusesNoLanes(t *testing.T) {
	assert.Error(t, SetupAll(nil, Options{}))
	assert.Error(t, SetupAll(nil, Options{}, laneCount))
}
