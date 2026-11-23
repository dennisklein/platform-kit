package render_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	thtalos "github.com/budimanjojo/talhelper/v3/pkg/talos"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// writeSecret generates a throwaway secrets bundle the way `talhelper gensecret`
// does and writes it as plain YAML — both render paths consume the same file, so
// every certificate that comes FROM the bundle is identical on both sides.
func writeSecret(t *testing.T, talosVersion string) string {
	t.Helper()
	vc, err := config.ParseContractFromVersion(talosVersion)
	require.NoError(t, err)
	bundle, err := thtalos.NewSecretBundle(secrets.NewClock(), *vc)
	require.NoError(t, err)
	data, err := yaml.Marshal(bundle)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "talsecret.yaml")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// TestLibraryParity proves the CR path end to end: talconfig -> Convert -> CR files
// -> LoadDir -> Map -> Render must produce byte-identical machine configs to
// rendering the loaded talconfig directly (the same comparison `make parity` runs
// against the real control/talos with the talhelper CLI as reference).
func TestLibraryParity(t *testing.T) {
	src := "testdata/talos"
	cfg, err := render.LoadTalconfig(filepath.Join(src, "talconfig.yaml"), []string{filepath.Join(src, "talenv.yaml")})
	require.NoError(t, err)

	// Convert BEFORE rendering: talhelper's generate rewrites selector-devices'
	// DeviceInterface to the link-alias name IN PLACE (pkg/talos/networkconfig.go),
	// so a rendered config no longer survives the round-trip guard. Every real
	// flow loads fresh per verb; only a shared in-process struct can trip this.
	cluster, nodes, err := render.Convert(cfg, src)
	require.NoError(t, err)
	require.Len(t, nodes, 2)

	secret := writeSecret(t, cfg.GetTalosVersion())
	ref := filepath.Join(t.TempDir(), "ref")
	require.NoError(t, render.Render(cfg, ref, secret, true))

	crs := filepath.Join(t.TempDir(), "crs")
	require.NoError(t, render.WriteFiles(crs, cluster, nodes))
	cluster, nodes, err = render.LoadDir(crs)
	require.NoError(t, err)
	assert.Equal(t, "parity", cluster.Name)

	mapped, err := render.Map(cluster, nodes, src)
	require.NoError(t, err)
	got := filepath.Join(t.TempDir(), "got")
	require.NoError(t, render.Render(mapped, got, secret, true))

	refFiles, err := os.ReadDir(ref)
	require.NoError(t, err)
	var machineConfigs int
	for _, e := range refFiles {
		want, err := os.ReadFile(filepath.Join(ref, e.Name()))
		require.NoError(t, err)
		have, err := os.ReadFile(filepath.Join(got, e.Name()))
		require.NoError(t, err, "file %s missing from the CR-path render", e.Name())
		if e.Name() == "talosconfig" {
			continue // client cert is minted per run — presence is the contract, bytes are not
		}
		machineConfigs++
		assert.Equal(t, string(want), string(have), "%s drifted between the talconfig and CR paths", e.Name())

		// The patch pipeline ran: the global patch lands in every machine config.
		assert.Contains(t, string(have), "10.77.253.1", "%s misses the global patch", e.Name())
	}
	assert.Equal(t, 2, machineConfigs, "expected one machine config per node")

	cp1, err := os.ReadFile(filepath.Join(got, "parity-cp1.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(cp1), "vm.max_map_count", "node patch missing")
	assert.Contains(t, string(cp1), "10.77.38.200", "VIP missing")
	w1, err := os.ReadFile(filepath.Join(got, "parity-w1.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(w1), "02:00:48:0b:74:47", "deviceSelector missing")
}

// TestConvertRefusesUnmappedFields: a talconfig field the CR schema does not carry
// must fail conversion, never vanish.
func TestConvertRefusesUnmappedFields(t *testing.T) {
	src, err := os.ReadFile("testdata/talos/talconfig.yaml")
	require.NoError(t, err)
	mutated := strings.Replace(string(src),
		"    controlPlane: false\n",
		"    controlPlane: false\n    nodeAnnotations:\n      rack: r1\n", 1)
	require.NotEqual(t, string(src), mutated, "mutation did not apply")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "talconfig.yaml"), []byte(mutated), 0o644))
	cfg, err := render.LoadTalconfig(filepath.Join(dir, "talconfig.yaml"),
		[]string{filepath.Join("testdata/talos", "talenv.yaml")})
	require.NoError(t, err)

	_, _, err = render.Convert(cfg, dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "round-trip guard")
	assert.Contains(t, err.Error(), "nodeAnnotations")
}
