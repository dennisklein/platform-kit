package asset

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrants(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	table := NewGrants()
	table.now = func() time.Time { return now }

	content := []byte("iso bytes")

	t.Run("issue and lookup", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Minute, nil)
		require.NoError(t, err)
		assert.Len(t, g.Token, 32, "128-bit token, hex")
		assert.Equal(t, "/grants/"+g.Token+"/node-x12.iso", g.Path())
		require.NotNil(t, table.lookup(g.Token))
	})

	t.Run("input validation", func(t *testing.T) {
		_, err := table.Issue("a", "no-extension", content, time.Minute, nil)
		assert.Error(t, err, "must end in .iso — vendor URL rule")
		_, err = table.Issue("a", "sub/dir.iso", content, time.Minute, nil)
		assert.Error(t, err, "one path element")
		_, err = table.Issue("a", "a.iso", nil, time.Minute, nil)
		assert.Error(t, err, "no empty content")
		_, err = table.Issue("a", "a.iso", content, 0, nil)
		assert.Error(t, err, "no unbounded grants")
	})

	t.Run("expiry", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Minute, nil)
		require.NoError(t, err)
		now = now.Add(time.Minute) // exactly at ExpiresAt: expired
		assert.Nil(t, table.lookup(g.Token))
		assert.False(t, table.Revoke(g.Token), "lookup already dropped it")
	})

	t.Run("revoke", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Minute, nil)
		require.NoError(t, err)
		assert.True(t, table.Revoke(g.Token))
		assert.Nil(t, table.lookup(g.Token))
	})

	t.Run("revoke by asset", func(t *testing.T) {
		g1, err := table.Issue("node-2288h-config", "node-2288h.iso", content, time.Minute, nil)
		require.NoError(t, err)
		g2, err := table.Issue("node-2288h-config", "node-2288h.iso", content, time.Minute, nil)
		require.NoError(t, err)
		keep, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Minute, nil)
		require.NoError(t, err)

		assert.Equal(t, 2, table.RevokeAsset("node-2288h-config"))
		assert.Nil(t, table.lookup(g1.Token))
		assert.Nil(t, table.lookup(g2.Token))
		assert.NotNil(t, table.lookup(keep.Token))
	})
}
