package render_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// documentedAuditPatch is the TalosCluster patch that the audit trail page
// gives for the kube-apiserver audit policy, read from the page itself.
func documentedAuditPatch(t *testing.T) []byte {
	t.Helper()
	page, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "src", "content", "docs",
		"reference", "security", "audit-trail.mdx"))
	require.NoError(t, err)
	const open = "```yaml title=\"TalosCluster patch: audit policy\"\n"
	_, rest, ok := strings.Cut(string(page), open)
	require.True(t, ok, "the page has no block %q", strings.TrimSpace(open))
	block, _, ok := strings.Cut(rest, "```")
	require.True(t, ok)
	var excerpt struct {
		Spec struct {
			Patches []osv1alpha1.Patch `json:"patches"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(block), &excerpt))
	require.Len(t, excerpt.Spec.Patches, 1)
	require.NotEmpty(t, excerpt.Spec.Patches[0].Inline)
	return []byte(excerpt.Spec.Patches[0].Inline)
}

// The documented audit policy, added to the cluster's patches, renders and
// validates with the pinned Talos machinery: the control plane gets it as
// its KubeAuditPolicyConfig, and a worker, where Talos refuses the
// document, renders without it.
func TestDocumentedAuditPolicyRenders(t *testing.T) {
	m := samples(t)
	patch := documentedAuditPatch(t)
	for _, n := range m.Nodes {
		node := m.Node(n.Name)
		installer, err := render.NewFactory("").InstallerImage(render.EffectiveSchematic(m.Cluster, node), m.Cluster.Spec.TalosVersion)
		require.NoError(t, err)
		cp, err := render.ResolvePatches(m.Cluster.Spec.Patches, m.ConfigMapReader())
		require.NoError(t, err)
		out, err := render.Node(render.Input{
			Cluster: m.Cluster, Node: node, Bundle: testBundle(t), InstallerImage: installer,
			ClusterPatches: append(cp, patch),
		})
		require.NoError(t, err, n.Name)
		loaded, err := configloader.NewFromBytes(out)
		require.NoError(t, err, n.Name)
		require.NoError(t, render.Validate(loaded, render.Metal), n.Name)

		docs := documents(t, out)
		if node.Spec.Role != osv1alpha1.RoleControlPlane {
			assert.NotContains(t, docs, "KubeAuditPolicyConfig", "%s: a worker gets no control-plane document", n.Name)
			continue
		}
		require.Contains(t, docs, "KubeAuditPolicyConfig", n.Name)
		policy := loaded.K8sAuditPolicyConfig()
		require.NotNil(t, policy, n.Name)
		rules, ok := policy.Configuration()["rules"].([]any)
		require.True(t, ok)
		require.Len(t, rules, 6, "the documented rules replace the default, not merge with it")
		first := rules[0].(map[string]any)
		assert.Equal(t, "Metadata", first["level"], "secrets first, never with a body")
		assert.Equal(t, "RequestResponse", rules[1].(map[string]any)["level"])
	}
}

// A cluster patch with a control-plane document reaches the control planes
// only; the same document in a worker's own patches still fails, so a
// mistake on one node is not hidden.
func TestControlPlaneDocumentInAWorkerPatchFails(t *testing.T) {
	m := samples(t)
	patch := documentedAuditPatch(t)
	node := m.Node("node-2")
	require.NotEqual(t, osv1alpha1.RoleControlPlane, node.Spec.Role)
	installer, err := render.NewFactory("").InstallerImage(render.EffectiveSchematic(m.Cluster, node), m.Cluster.Spec.TalosVersion)
	require.NoError(t, err)
	_, err = render.Node(render.Input{
		Cluster: m.Cluster, Node: node, Bundle: testBundle(t), InstallerImage: installer,
		NodePatches: [][]byte{patch},
	})
	require.ErrorContains(t, err, "only allowed on control plane machines")
}
