package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGlobal(t *testing.T) {
	global.kubeconfig, global.context = "", ""
	t.Cleanup(func() { global.kubeconfig, global.context = "", "" })

	rest, err := parseGlobal([]string{"--kubeconfig", "/tmp/kc", "--context", "lab", "status", "-o", "json"})
	require.NoError(t, err)
	assert.Equal(t, []string{"status", "-o", "json"}, rest)
	assert.Equal(t, "/tmp/kc", global.kubeconfig)
	assert.Equal(t, "lab", global.context)

	rest, err = parseGlobal([]string{"--help"})
	require.NoError(t, err)
	assert.Equal(t, []string{"help"}, rest)

	_, err = parseGlobal([]string{"--bogus"})
	assert.ErrorContains(t, err, "pk-metal help")
}

// TestUsageHasNoInternalJargon keeps the top-level help readable for someone
// who has not read the design documents.
func TestUsageHasNoInternalJargon(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	for _, word := range []string{"VAP", "ladder", "grant-serve", "L4", "reverse tunnel", "ADR"} {
		assert.NotContains(t, out.String(), word)
	}
	for _, c := range commands {
		assert.Contains(t, out.String(), "  "+c.name+" ", "every command is listed")
	}
	assert.Contains(t, out.String(), "REBOOT", "the boot line says it reboots the machine")
}
