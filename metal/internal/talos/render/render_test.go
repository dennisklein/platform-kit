package render_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/network"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	tconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// samples loads config/samples/talos.yaml, the virtual lab's cluster.
func samples(t *testing.T) *render.Manifests {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "samples", "talos.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "talos.yaml"), data, 0o600))
	m, err := render.LoadDir(dir)
	require.NoError(t, err)
	return m
}

// testBundle is the committed throwaway bundle, a stand-in for the one a
// person generates with `talosctl gen secrets`.
func testBundle(t *testing.T) *secrets.Bundle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "secrets.yaml"))
	require.NoError(t, err)
	b, err := render.LoadBundle(data, "v1.14.6")
	require.NoError(t, err)
	return b
}

// renderNode renders one sample node the way the controller does.
func renderNode(t *testing.T, m *render.Manifests, name string) []byte {
	t.Helper()
	node := m.Node(name)
	require.NotNil(t, node, name)
	installer, err := render.NewFactory("").InstallerImage(render.EffectiveSchematic(m.Cluster, node), m.Cluster.Spec.TalosVersion)
	require.NoError(t, err)
	cp, err := render.ResolvePatches(m.Cluster.Spec.Patches, m.ConfigMapReader())
	require.NoError(t, err)
	np, err := render.ResolvePatches(node.Spec.Patches, m.ConfigMapReader())
	require.NoError(t, err)
	out, err := render.Node(render.Input{
		Cluster: m.Cluster, Node: node, Bundle: testBundle(t),
		InstallerImage: installer, ClusterPatches: cp, NodePatches: np,
	})
	require.NoError(t, err)
	return out
}

// documents splits a rendered config into kind (plus name, when the document
// has one) and the decoded document.
func documents(t *testing.T, cfg []byte) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, raw := range strings.Split(string(cfg), "\n---\n") {
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(raw), &doc))
		key, _ := doc["kind"].(string)
		if key == "" {
			key = "v1alpha1"
		}
		if name, ok := doc["name"].(string); ok {
			key += "/" + name
		}
		require.NotContains(t, out, key, "document %s rendered twice", key)
		out[key] = doc
	}
	return out
}

func get(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		require.True(t, ok, "%v: %s is not a map", path, p)
		cur = m[p]
	}
	return cur
}

func TestRenderLabControlPlane(t *testing.T) {
	m := samples(t)
	cfg := renderNode(t, m, "node-1")
	if os.Getenv("RENDER_DUMP") != "" {
		require.NoError(t, os.WriteFile(os.Getenv("RENDER_DUMP"), cfg, 0o600))
	}
	docs := documents(t, cfg)

	// The documents that make a 1.14 control plane, and the ones this
	// renderer adds: install target, link alias and config, VIP.
	for _, kind := range []string{
		"v1alpha1",
		"HostnameConfig", "ResolverConfig",
		"UnattendedInstallConfig",
		"LinkAliasConfig/lan", "LinkConfig/lan", "Layer2VIPConfig/10.77.0.10",
		"KubeClusterConfig", "KubeNodeConfig", "KubeletConfig", "KubeNetworkConfig",
		"KubeAPIServerConfig", "KubeAPIServerCAConfig", "KubeFlannelCNIConfig",
		"DiscoveryServiceConfig/default", "DiscoveryIdentityConfig",
		"TimeSyncConfig",
	} {
		assert.Contains(t, docs, kind)
	}

	v1 := docs["v1alpha1"]
	assert.Equal(t, "controlplane", get(t, v1, "machine", "type"))
	assert.Nil(t, get(t, v1, "machine", "install"), "1.14 installs through UnattendedInstallConfig")
	assert.Nil(t, get(t, v1, "machine", "network"), "1.14 networking is document based")

	assert.Equal(t, "node-1", get(t, docs["HostnameConfig"], "hostname"))
	assert.Nil(t, get(t, docs["HostnameConfig"], "auto"))

	install := docs["UnattendedInstallConfig"]
	assert.Equal(t, `disk.serial == "pklab-disk-1"`, get(t, install, "provisioning", "diskSelector", "match"))
	assert.Equal(t, false, get(t, install, "provisioning", "wipe"))
	assert.Regexp(t, `^factory\.talos\.dev/metal-installer/[0-9a-f]{64}:v1\.14\.6$`, get(t, install, "installer", "image"))

	assert.Contains(t, get(t, docs["LinkAliasConfig/lan"], "selector", "match"), `mac(link.permanent_addr) == "52:54:00:77:00:11"`)
	link := docs["LinkConfig/lan"]
	assert.Equal(t, []any{map[string]any{"address": "10.77.0.11/24"}}, get(t, link, "addresses"))
	assert.Equal(t, []any{map[string]any{"gateway": "10.77.0.1"}}, get(t, link, "routes"))
	assert.Equal(t, "lan", get(t, docs["Layer2VIPConfig/10.77.0.10"], "link"))

	assert.Equal(t, "lab", get(t, docs["KubeClusterConfig"], "clusterName"))
	assert.Equal(t, "https://10.77.0.10:6443", get(t, docs["KubeClusterConfig"], "endpoint"))
	assert.Contains(t, get(t, docs["KubeAPIServerConfig"], "certExtraSANs"), "10.77.0.10",
		"the VIP must be a SAN even when the endpoint is a DNS name")
	assert.Equal(t, "NoSchedule", get(t, docs["KubeNodeConfig"], "taints", "node-role.kubernetes.io/control-plane"))
	assert.Equal(t, []any{"time.cloudflare.com"}, get(t, docs["TimeSyncConfig"], "ntp", "servers"),
		"the cluster patch must land")
}

func TestRenderLabWorker(t *testing.T) {
	m := samples(t)
	docs := documents(t, renderNode(t, m, "node-2"))

	assert.Equal(t, "worker", get(t, docs["v1alpha1"], "machine", "type"))
	assert.Equal(t, "node-2", get(t, docs["HostnameConfig"], "hostname"))
	assert.Equal(t, "general", get(t, docs["KubeNodeConfig"], "labels", "pk.io/pool"))
	assert.NotContains(t, docs, "Layer2VIPConfig/10.77.0.10", "workers never announce the VIP")
	assert.NotContains(t, docs, "KubeAPIServerConfig")
	assert.Contains(t, docs, "LinkConfig/lan")
}

// Every sample renders and passes Talos' own validation in both the mode the
// operator renders for and container mode, so a config that only passes
// because of the install requirement does not hide.
func TestRenderValidatesInBothModes(t *testing.T) {
	m := samples(t)
	for _, n := range m.Nodes {
		cfg := renderNode(t, m, n.Name)
		loaded, err := configloader.NewFromBytes(cfg)
		require.NoError(t, err, n.Name)
		require.NoError(t, render.Validate(loaded, render.Metal), n.Name)
		require.NoError(t, render.Validate(loaded, render.Container), n.Name)
	}
}

// The digest the BootAsset rebuild keys on must not move when nothing moved.
func TestRenderIsDeterministic(t *testing.T) {
	m := samples(t)
	assert.Equal(t, renderNode(t, m, "node-1"), renderNode(t, m, "node-1"))
}

func TestRenderOptions(t *testing.T) {
	m := samples(t)

	t.Run("no CNI drops flannel", func(t *testing.T) {
		m.Cluster.Spec.Network.CNI = &osv1alpha1.CNI{Name: osv1alpha1.CNINone}
		defer func() { m.Cluster.Spec.Network.CNI = nil }()
		assert.NotContains(t, documents(t, renderNode(t, m, "node-1")), "KubeFlannelCNIConfig")
	})

	t.Run("custom CNI replaces flannel with manifests", func(t *testing.T) {
		m.Cluster.Spec.Network.CNI = &osv1alpha1.CNI{Name: osv1alpha1.CNICustom, URLs: []string{"https://example.com/cni.yaml"}}
		defer func() { m.Cluster.Spec.Network.CNI = nil }()
		docs := documents(t, renderNode(t, m, "node-1"))
		assert.NotContains(t, docs, "KubeFlannelCNIConfig")
		assert.Equal(t, "https://example.com/cni.yaml", get(t, docs["KubeExternalManifestConfig/custom-cni-0"], "url"))
	})

	t.Run("registry mirrors become documents", func(t *testing.T) {
		m.Cluster.Spec.RegistryMirrors = []osv1alpha1.RegistryMirror{{Registry: "docker.io", Endpoints: []string{"https://mirror.example.com"}}}
		defer func() { m.Cluster.Spec.RegistryMirrors = nil }()
		docs := documents(t, renderNode(t, m, "node-2"))
		assert.Equal(t, "https://mirror.example.com", get(t, docs["RegistryMirrorConfig/docker.io"], "endpoints").([]any)[0].(map[string]any)["url"])
	})

	t.Run("disk selector globs and CEL", func(t *testing.T) {
		expr, err := render.DiskMatch(osv1alpha1.DiskSelector{Model: "QEMU*", Match: "!disk.rotational"})
		require.NoError(t, err)
		assert.Equal(t, `glob("QEMU*", disk.model) && !disk.rotational`, expr.String())

		_, err = render.DiskMatch(osv1alpha1.DiskSelector{Match: "disk.nope == 1"})
		assert.Error(t, err, "a CEL error must fail the render, not the install")
	})

	t.Run("node patches apply after cluster patches", func(t *testing.T) {
		node := m.Node("node-3")
		node.Spec.Patches = []osv1alpha1.Patch{{Inline: "apiVersion: v1alpha1\nkind: TimeSyncConfig\nntp:\n  servers: [10.77.0.1]\n"}}
		defer func() { node.Spec.Patches = nil }()
		docs := documents(t, renderNode(t, m, "node-3"))
		// Strategic merge appends lists: the order shows which patch ran first.
		assert.Equal(t, []any{"time.cloudflare.com", "10.77.0.1"}, get(t, docs["TimeSyncConfig"], "ntp", "servers"))
	})

	t.Run("a ConfigMap patch resolves from the manifests", func(t *testing.T) {
		m.Cluster.Spec.Patches = append(m.Cluster.Spec.Patches, osv1alpha1.Patch{
			ConfigMapRef: &osv1alpha1.ConfigMapKeyReference{Namespace: "pk-metal", Name: "absent", Key: "p.yaml"},
		})
		_, err := render.ResolvePatches(m.Cluster.Spec.Patches, m.ConfigMapReader())
		assert.ErrorIs(t, err, render.ErrPatchUnresolvable)
		m.Cluster.Spec.Patches = m.Cluster.Spec.Patches[:1]
	})

	t.Run("JSON patches are refused", func(t *testing.T) {
		node := m.Node("node-3")
		node.Spec.Patches = []osv1alpha1.Patch{{Inline: `[{"op": "add", "path": "/machine/env", "value": {}}]`}}
		defer func() { node.Spec.Patches = nil }()
		installer, _ := render.NewFactory("").InstallerImage(nil, "v1.14.6")
		np, err := render.ResolvePatches(node.Spec.Patches, nil)
		require.NoError(t, err)
		_, err = render.Node(render.Input{Cluster: m.Cluster, Node: node, Bundle: testBundle(t), InstallerImage: installer, NodePatches: np})
		assert.ErrorContains(t, err, "strategic merge")
	})

	t.Run("a VIP no link can carry is an error", func(t *testing.T) {
		m.Cluster.Spec.ControlPlane.VIP = "192.0.2.10"
		defer func() { m.Cluster.Spec.ControlPlane.VIP = "10.77.0.10" }()
		installer, _ := render.NewFactory("").InstallerImage(nil, "v1.14.6")
		_, err := render.Node(render.Input{Cluster: m.Cluster, Node: m.Node("node-1"), Bundle: testBundle(t), InstallerImage: installer})
		assert.ErrorContains(t, err, "no link")
	})

	t.Run("pre-1.14 contracts are refused", func(t *testing.T) {
		_, err := render.Contract("v1.13.4")
		assert.Error(t, err)
	})
}

// The link selector is evaluated against link statuses the way Talos
// evaluates it, including the cases the hardware_addr fallback must not
// reach: an alias has to match exactly one link.
func TestLinkMatch(t *testing.T) {
	env := celenv.LinkLocator()
	expr, err := cel.ParseBooleanExpression(render.LinkMatch("52:54:00:77:00:11"), env)
	require.NoError(t, err)
	mac := func(s string) []byte {
		hw, err := net.ParseMAC(s)
		require.NoError(t, err)
		return hw
	}
	for _, tc := range []struct {
		name string
		link *network.LinkStatusSpec
		want bool
	}{
		{"permanent address", &network.LinkStatusSpec{PermanentAddr: mac("52:54:00:77:00:11"), HardwareAddr: mac("52:54:00:77:00:11")}, true},
		{"permanent address, MAC changed", &network.LinkStatusSpec{PermanentAddr: mac("52:54:00:77:00:11"), HardwareAddr: mac("02:00:00:00:00:01")}, true},
		{"no permanent address reported", &network.LinkStatusSpec{HardwareAddr: mac("52:54:00:77:00:11")}, true},
		{"another NIC", &network.LinkStatusSpec{PermanentAddr: mac("52:54:00:77:00:12"), HardwareAddr: mac("52:54:00:77:00:12")}, false},
		{"another NIC wearing this MAC", &network.LinkStatusSpec{PermanentAddr: mac("52:54:00:77:00:12"), HardwareAddr: mac("52:54:00:77:00:11")}, false},
		{"bond over this NIC", &network.LinkStatusSpec{Kind: "bond", HardwareAddr: mac("52:54:00:77:00:11")}, false},
		{"vlan over this NIC", &network.LinkStatusSpec{Kind: "vlan", HardwareAddr: mac("52:54:00:77:00:11")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expr.EvalBool(env, map[string]any{"link": tc.link})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNodeAddress(t *testing.T) {
	m := samples(t)
	assert.Equal(t, "10.77.0.11", render.NodeAddress(m.Node("node-1")))
	n := m.Node("node-2").DeepCopy()
	n.Spec.Address = "10.77.0.99"
	assert.Equal(t, "10.77.0.99", render.NodeAddress(n))
}

func TestCredentials(t *testing.T) {
	b := testBundle(t)

	kc, err := render.Kubeconfig(b, "lab", "https://10.77.0.10:6443", 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, render.KubeconfigCurrent(kc, b, "https://10.77.0.10:6443", time.Hour))
	assert.False(t, render.KubeconfigCurrent(kc, b, "https://10.77.0.10:6443", 48*time.Hour), "a credential about to expire is renewed")
	assert.False(t, render.KubeconfigCurrent(kc, b, "https://elsewhere:6443", time.Hour), "a moved endpoint is renewed")

	tc, err := render.Talosconfig(b, "lab", []string{"10.77.0.11"}, 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, render.TalosconfigCurrent(tc, b, []string{"10.77.0.11"}, time.Hour))
	assert.False(t, render.TalosconfigCurrent(tc, b, []string{"10.77.0.11", "10.77.0.12"}, time.Hour))

	// A credential from another bundle is never current.
	other, err := secrets.NewBundle(secrets.NewClock(), mustContract(t))
	require.NoError(t, err)
	assert.False(t, render.KubeconfigCurrent(kc, other, "https://10.77.0.10:6443", time.Hour))
	assert.False(t, render.TalosconfigCurrent(tc, other, []string{"10.77.0.11"}, time.Hour))
}

func mustContract(t *testing.T) *tconfig.VersionContract {
	t.Helper()
	c, err := render.Contract("v1.14.6")
	require.NoError(t, err)
	return c
}

func TestLoadBundleRejects(t *testing.T) {
	_, err := render.LoadBundle(nil, "v1.14.6")
	assert.Error(t, err)
	_, err = render.LoadBundle([]byte("cluster:\n  idd: x\n"), "v1.14.6")
	assert.Error(t, err, "a misspelled key must not decode into an empty certificate")
	_, err = render.LoadBundle([]byte("cluster:\n  id: x\n"), "v1.14.6")
	assert.Error(t, err, "an incomplete bundle must not validate")
}
