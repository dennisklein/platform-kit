// Package samples_test keeps the samples that a person copies first in step
// with the API types: a field that the API does not have fails the test.
package samples_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// decode reads the one object of a sample file into out and refuses a field
// that the type does not know.
func decode(t *testing.T, file string, out any) {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, []byte("\n---\n")), "%s holds one object", file)
	require.NoError(t, yaml.UnmarshalStrict(data, out), file)
}

func TestClaimSample(t *testing.T) {
	claim := &metalv1alpha1.HostClaim{}
	decode(t, "claim.yaml", claim)
	require.Equal(t, "HostClaim", claim.Kind)
	asset := &metalv1alpha1.Asset{}
	decode(t, "asset.yaml", asset)
	require.Equal(t, asset.Name, claim.Spec.AssetRef.Name, "the claim names the Asset in asset.yaml")
	require.NotNil(t, claim.Spec.OS, "the sample installs a node")
	require.Equal(t, "node-1", claim.Spec.OS.NodeRef.Name, "the node is in talos.yaml")
}

func TestWindowSample(t *testing.T) {
	window := &metalv1alpha1.MaintenanceWindow{}
	decode(t, "window.yaml", window)
	require.Equal(t, "MaintenanceWindow", window.Kind)
	require.NotNil(t, window.Spec.HostSelector)
	require.NotEmpty(t, window.Spec.Weekly)
}

func TestRebootRolloutSample(t *testing.T) {
	rollout := &osv1alpha1.TalosRollout{}
	decode(t, "rollout-reboot.yaml", rollout)
	require.Equal(t, osv1alpha1.RolloutReboot, rollout.Spec.Kind)
	require.Nil(t, rollout.Spec.Target, "a Reboot rollout takes no target")
	require.Empty(t, rollout.Spec.Approval.ApprovedBy, "a rollout cannot be created pre-approved")
	require.NotEmpty(t, rollout.Spec.ProposedBy)
}
