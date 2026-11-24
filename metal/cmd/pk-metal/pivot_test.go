package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/pivot"
)

func TestConfirmPivot(t *testing.T) {
	plan := &pivot.Plan{Reachable: []string{"the BMCs, over HTTPS, from every node of mgmt: 192.0.2.101:443"}}

	var out bytes.Buffer
	require.NoError(t, confirmPivot(&out, strings.NewReader("y\n"), plan, "kind", "mgmt", false, true))
	assert.Contains(t, out.String(), "192.0.2.101:443")
	assert.Contains(t, out.String(), "Move? [y/N]")

	out.Reset()
	withStore := *plan
	withStore.Claims = []pivot.ClaimMove{{Volume: "media-store", Name: "pk-metal-media-store", Action: pivot.ClaimEphemeral}}
	require.NoError(t, confirmPivot(&out, strings.NewReader(""), &withStore, "kind", "mgmt", true, false))
	assert.Contains(t, out.String(), "The volumes the operator mounts, on mgmt:")
	assert.Contains(t, out.String(), "PersistentVolumeClaim pk-metal-media-store is replaced by an emptyDir")

	out.Reset()
	require.ErrorIs(t, confirmPivot(&out, strings.NewReader("\n"), plan, "kind", "mgmt", false, true), errDeclined)

	err := confirmPivot(&out, strings.NewReader(""), plan, "kind", "mgmt", false, false)
	require.ErrorContains(t, err, "--yes", "without a terminal it refuses to ask")

	out.Reset()
	require.NoError(t, confirmPivot(&out, strings.NewReader(""), plan, "kind", "mgmt", true, false))
	assert.NotContains(t, out.String(), "Move?")

	out.Reset()
	require.NoError(t, confirmPivot(&out, strings.NewReader(""), &pivot.Plan{Resume: true}, "kind", "mgmt", false, false))
	assert.Contains(t, out.String(), "checking it again", "a resumed move only verifies")
}

func TestPivotDoneSaysHowToGoBack(t *testing.T) {
	text := pivotDone(&pivot.Report{Rewritten: 3}, "kind-pk-metal", "admin@mgmt", "mgmt.kubeconfig")
	assert.Contains(t, text, "3 references")
	assert.Contains(t, text, "pk-metal --kubeconfig mgmt.kubeconfig pivot --to <kubeconfig of kind-pk-metal>")
	assert.Contains(t, text, "Do not start it again")
}

func TestRestConfigNamesTheContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: mgmt
  cluster: {server: "https://192.0.2.10:6443"}
users:
- name: admin
  user: {token: synthetic}
contexts:
- name: admin@mgmt
  context: {cluster: mgmt, user: admin}
- name: other
  context: {cluster: mgmt, user: admin}
current-context: admin@mgmt
`), 0o600))
	cfg, name, err := namedRestConfig(path, "")
	require.NoError(t, err)
	assert.Equal(t, "admin@mgmt", name)
	assert.Equal(t, "https://192.0.2.10:6443", cfg.Host)
	_, name, err = namedRestConfig(path, "other")
	require.NoError(t, err)
	assert.Equal(t, "other", name)
	_, _, err = namedRestConfig(filepath.Join(t.TempDir(), "missing"), "")
	require.Error(t, err)
}
