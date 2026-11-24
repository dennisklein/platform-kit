package render_test

import (
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/role"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

func TestSupportedPair(t *testing.T) {
	require.NoError(t, render.SupportedPair("v1.14.6", "v1.36.2"))
	err := render.SupportedPair("v1.14.6", "v1.30.0")
	require.Error(t, err, "Kubernetes 1.30 is too old for Talos 1.14")
	assert.Contains(t, err.Error(), "too old")
	assert.Error(t, render.SupportedPair("v1.14.6", "v1.40.0"))
	assert.Error(t, render.SupportedPair("v9.0.0", "v1.36.2"), "a Talos release machinery does not know is refused")
}

func TestUpgradeableFrom(t *testing.T) {
	require.NoError(t, render.UpgradeableFrom("v1.14.5", "v1.14.6"))
	require.NoError(t, render.UpgradeableFrom("v1.13.4", "v1.14.6"))
	assert.Error(t, render.UpgradeableFrom("v1.10.0", "v1.14.6"), "too far behind for one step")
}

func TestKubernetesUpgradeable(t *testing.T) {
	require.NoError(t, render.KubernetesUpgradeable("v1.35.4", "v1.35.5"), "a patch release")
	require.NoError(t, render.KubernetesUpgradeable("v1.35.4", "v1.36.0"), "the next minor")
	err := render.KubernetesUpgradeable("v1.34.2", "v1.36.0")
	require.Error(t, err, "two minors in one step skip the version-skew order")
	assert.Contains(t, err.Error(), "v1.35 first")
	assert.Error(t, render.KubernetesUpgradeable("v1.36.0", "v1.35.9"), "a downgrade")
	assert.Error(t, render.KubernetesUpgradeable("v1.36.0", "v2.0.0"), "another major")
}

func TestRenderVersion(t *testing.T) {
	for name, tc := range map[string]struct{ cluster, running, last, want string }{
		"a new node renders for the cluster":           {"v1.14.6", "", "", "v1.14.6"},
		"an older machine keeps its contract":          {"v1.14.6", "v1.14.5", "", "v1.14.5"},
		"a silent machine keeps the last render":       {"v1.14.6", "", "v1.14.5", "v1.14.5"},
		"an upgraded machine follows the cluster":      {"v1.14.6", "v1.14.6", "v1.14.5", "v1.14.6"},
		"a newer machine renders for the cluster":      {"v1.14.6", "v1.15.0", "", "v1.14.6"},
		"too old to render for falls to the cluster's": {"v1.14.6", "v1.12.0", "", "v1.14.6"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, render.RenderVersion(tc.cluster, tc.running, tc.last))
		})
	}
}

func TestCompareVersions(t *testing.T) {
	assert.Equal(t, -1, render.CompareVersions("v1.14.5", "1.14.6"))
	assert.Equal(t, 0, render.CompareVersions("v1.14.6", "1.14.6"))
	assert.Equal(t, 1, render.CompareVersions("v1.15.0", "v1.14.9"))
	assert.Equal(t, -1, render.CompareVersions("garbage", "v1.0.0"), "garbage is never the newer one")
}

func TestKubeProxyImage(t *testing.T) {
	m := samples(t)
	image, enabled, err := render.KubeProxyImage(renderNode(t, m, "node-1"))
	require.NoError(t, err)
	assert.True(t, enabled)
	assert.True(t, strings.HasSuffix(image, ":"+m.Cluster.Spec.KubernetesVersion), image)
}

func TestReaderTalosconfigAndExpiry(t *testing.T) {
	b := testBundle(t)
	tc, err := render.TalosconfigWithRole(b, "lab", []string{"10.77.0.11"}, 2*time.Hour, role.Reader)
	require.NoError(t, err)
	roles, err := render.TalosconfigRoles(tc)
	require.NoError(t, err)
	assert.Equal(t, []string{"os:reader"}, roles)
	notAfter, err := render.TalosconfigNotAfter(tc)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), notAfter, 5*time.Minute)

	kc, err := render.Kubeconfig(b, "lab", "https://10.77.0.10:6443", 3*time.Hour)
	require.NoError(t, err)
	notAfter, err = render.KubeconfigNotAfter(kc)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(3*time.Hour), notAfter, 5*time.Minute)
}
