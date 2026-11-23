package render_test

import (
	"strings"
	"testing"

	thtalos "github.com/budimanjojo/talhelper/v3/pkg/talos"
	tconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// A node's schematic has to reach machine.install.image, because that is the
// only way kernel args survive the install: Talos renders grubUseUKICmdline and
// takes the installed cmdline from the image's baked UKI, and refuses
// install.extraKernelArgs outright while it does. An installer that silently
// fell back to the vanilla image would lose every arg the schematic asked for —
// serial console included, which is how a node goes dark after installing.
func TestNodeSchematicReachesInstallImage(t *testing.T) {
	const version = "v1.13.4"

	vanilla := renderWithSchematic(t, version, nil)
	assert.Contains(t, vanilla, "grubUseUKICmdline: true",
		"the installed cmdline comes from the image, which is why args must ride the schematic")

	args := renderWithSchematic(t, version, &osv1alpha1.Schematic{
		Customization: osv1alpha1.SchematicCustomization{
			ExtraKernelArgs: []string{"console=ttyS0"},
		},
	})
	assert.Regexp(t, `image: factory\.talos\.dev/metal-installer/[0-9a-f]{64}:`+version, args)
	assert.NotEqual(t, installImage(t, vanilla), installImage(t, args),
		"kernel args must change the installer image, or they never reach the disk")
}

// renderWithSchematic renders a one-node control plane carrying the schematic.
func renderWithSchematic(t *testing.T, version string, sch *osv1alpha1.Schematic) string {
	t.Helper()
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion:      version,
			KubernetesVersion: "v1.36.0",
			Endpoint:          "https://10.0.0.1:6443",
		},
	}
	node := osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1"},
		Spec: osv1alpha1.TalosNodeSpec{
			ClusterRef:          osv1alpha1.Reference{Name: "c"},
			IPAddress:           "10.0.0.1",
			ControlPlane:        true,
			InstallDiskSelector: &osv1alpha1.InstallDiskSelector{Serial: "disk0"},
			Schematic:           sch,
		},
	}
	thc, err := render.Map(cluster, []osv1alpha1.TalosNode{node}, "")
	require.NoError(t, err)
	vc, err := tconfig.ParseContractFromVersion(version)
	require.NoError(t, err)
	bundle, err := thtalos.NewSecretBundle(secrets.NewClock(), *vc)
	require.NoError(t, err)
	bundleBytes, err := yamlv3.Marshal(bundle)
	require.NoError(t, err)
	input, err := render.Input(thc, bundleBytes)
	require.NoError(t, err)
	cfg, err := render.RenderNode(thc, "cp1", input, true)
	require.NoError(t, err)
	return string(cfg)
}

// installImage picks the installer out of a rendered config — the vanilla and
// schematic forms live at different registries, so match on the role.
func installImage(t *testing.T, cfg string) string {
	t.Helper()
	for _, line := range strings.Split(cfg, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "image: ") && strings.Contains(s, "installer") {
			return s
		}
	}
	t.Fatalf("no installer image in rendered config")
	return ""
}
