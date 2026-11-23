package render_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// TestRenderNodeAgainstFiles locks the two entrances of the engine together: the
// in-memory path (Input from bundle bytes + RenderNode) must produce machine
// configs byte-identical to the file pipeline (Render), for the whole node set
// and for the single-node mapping the reconciler uses.
func TestRenderNodeAgainstFiles(t *testing.T) {
	src := "testdata/talos"
	cfg, err := render.LoadTalconfig(filepath.Join(src, "talconfig.yaml"), []string{filepath.Join(src, "talenv.yaml")})
	require.NoError(t, err)
	cluster, nodes, err := render.Convert(cfg, src)
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	secret := writeSecret(t, cfg.GetTalosVersion())
	bundle, err := os.ReadFile(secret)
	require.NoError(t, err)

	mapped, err := render.Map(cluster, nodes, src)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "ref")
	require.NoError(t, render.Render(mapped, dir, secret, true))

	t.Run("whole node set", func(t *testing.T) {
		// Fresh Map: Render rewrote selector devices in place (see TestLibraryParity).
		c, err := render.Map(cluster, nodes, src)
		require.NoError(t, err)
		input, err := render.Input(c, bundle)
		require.NoError(t, err)
		for _, n := range nodes {
			want, err := os.ReadFile(filepath.Join(dir, cluster.Name+"-"+n.Name+".yaml"))
			require.NoError(t, err)
			got, err := render.RenderNode(c, n.Name, input, true)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got), "%s drifted between file and in-memory paths", n.Name)
		}
	})

	t.Run("single-node mapping", func(t *testing.T) {
		// The reconciler maps one TalosNode at a time — same bytes must fall out.
		for i := range nodes {
			c, err := render.Map(cluster, []osv1alpha1.TalosNode{nodes[i]}, src)
			require.NoError(t, err)
			input, err := render.Input(c, bundle)
			require.NoError(t, err)
			want, err := os.ReadFile(filepath.Join(dir, cluster.Name+"-"+nodes[i].Name+".yaml"))
			require.NoError(t, err)
			got, err := render.RenderNode(c, nodes[i].Name, input, true)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got), "%s single-node mapping drifted", nodes[i].Name)
		}
	})

	t.Run("unknown host refused", func(t *testing.T) {
		c, err := render.Map(cluster, nodes, src)
		require.NoError(t, err)
		input, err := render.Input(c, bundle)
		require.NoError(t, err)
		_, err = render.RenderNode(c, "no-such-node", input, true)
		assert.Error(t, err)
	})

	t.Run("empty bundle refused", func(t *testing.T) {
		c, err := render.Map(cluster, nodes, src)
		require.NoError(t, err)
		_, err = render.Input(c, nil)
		assert.Error(t, err)
	})
}

func TestFactoryISOURL(t *testing.T) {
	factory := render.NewFactory("")
	vanilla, err := factory.ISOURL(nil, "v1.13.4")
	require.NoError(t, err)
	// The empty schematic's well-known content hash — the same ID every Talos
	// installation without extensions pulls.
	assert.Equal(t,
		"https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.13.4/metal-amd64.iso",
		vanilla)

	drbd, err := factory.ISOURL(&osv1alpha1.Schematic{
		Customization: osv1alpha1.SchematicCustomization{
			SystemExtensions: osv1alpha1.SystemExtensions{
				OfficialExtensions: []string{"siderolabs/drbd"},
			},
		},
	}, "v1.13.4")
	require.NoError(t, err)
	assert.NotEqual(t, vanilla, drbd, "schematic must change the content address")
	assert.Regexp(t, `^https://factory\.talos\.dev/image/[0-9a-f]{64}/v1\.13\.4/metal-amd64\.iso$`, drbd)

	// Kernel args are what a boot medium is told about itself — where its
	// config lives and where the console goes. They ride the same content
	// address, so an image that carries them is a different image.
	args, err := factory.ISOURL(&osv1alpha1.Schematic{
		Customization: osv1alpha1.SchematicCustomization{
			ExtraKernelArgs: []string{"talos.config=metal-iso", "console=ttyS0"},
		},
	}, "v1.13.4")
	require.NoError(t, err)
	assert.NotEqual(t, vanilla, args, "kernel args must change the content address")
	assert.NotEqual(t, drbd, args)
}
