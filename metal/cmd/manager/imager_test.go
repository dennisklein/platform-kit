package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSubcommand(t *testing.T) {
	var out bytes.Buffer
	_, ok := runSubcommand([]string{"--ledger=/etc/pk-metal/ledger.yaml"}, &out)
	assert.False(t, ok, "flags start the manager")
	_, ok = runSubcommand(nil, &out)
	assert.False(t, ok)

	code, ok := runSubcommand([]string{cmdImagerStage}, &out)
	assert.True(t, ok)
	assert.Equal(t, 2, code, "a destination is required")

	code, ok = runSubcommand([]string{cmdImagerRun, "/config/config.yaml"}, &out)
	assert.True(t, ok)
	assert.Equal(t, 2, code, "an imager to run is required")

	dest := filepath.Join(t.TempDir(), "manager")
	code, ok = runSubcommand([]string{cmdImagerStage, dest}, &out)
	assert.True(t, ok)
	assert.Equal(t, 0, code)
	_, err := os.Stat(dest)
	require.NoError(t, err)
}
