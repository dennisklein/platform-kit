package sim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadFixtureRefusesUnknownKeys: a fixture given by path is a user's
// file. A misspelled quirk key must be an error, or the sim silently stops
// reproducing that quirk.
func TestLoadFixtureRefusesUnknownKeys(t *testing.T) {
	good, err := embedded.ReadFile("fixtures/generic.yaml")
	require.NoError(t, err)
	require.Contains(t, string(good), "require_if_match:")

	dir := t.TempDir()
	goodPath := filepath.Join(dir, "good.yaml")
	require.NoError(t, os.WriteFile(goodPath, good, 0o600))
	_, err = LoadFixture(goodPath)
	require.NoError(t, err, "the unchanged fixture loads from a path")

	typo := strings.Replace(string(good), "require_if_match:", "require_if_mach:", 1)
	typoPath := filepath.Join(dir, "typo.yaml")
	require.NoError(t, os.WriteFile(typoPath, []byte(typo), 0o600))
	_, err = LoadFixture(typoPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require_if_mach")
}

// TestEmbeddedFixturesLoad keeps every shipped fixture valid under strict
// decoding.
func TestEmbeddedFixturesLoad(t *testing.T) {
	for _, name := range Fixtures() {
		_, err := LoadFixture(name)
		assert.NoError(t, err, name)
	}
}
