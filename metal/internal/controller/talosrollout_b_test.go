package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// A spec written past the CLI (kubectl, GitOps) that skips a Kubernetes minor
// is refused by the plan, from the version the API server reports.
func TestPlanRefusesASkippedKubernetesMinor(t *testing.T) {
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	cluster.Status.KubernetesVersion = "v1.34.1"
	r := &TalosRolloutReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster).Build()}
	ro := &osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{
		ClusterRef: osv1alpha1.Reference{Name: "c"}, Kind: osv1alpha1.RolloutKubernetesUpgrade,
		Target: &osv1alpha1.RolloutTarget{KubernetesVersion: "v1.36.2"}}}

	plan, why, err := r.plan(t.Context(), ro)
	require.NoError(t, err)
	assert.Empty(t, plan)
	assert.Contains(t, why, "one minor at a time")
}

// During a CA rotation by hand the dry run reports drift against the held,
// old bundle. A Config or KubernetesUpgrade rollout would apply the old CA
// again, so none is proposed while the bundle is held.
func TestProposerHoldsConfigRolloutsWhileTheBundleIsHeld(t *testing.T) {
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	cluster.Status.KubernetesVersion = "v1.36.2"
	drift := []metav1.Condition{{Type: osv1alpha1.ConditionConfigInSync, Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonConfigDrift}}
	nodes := []osv1alpha1.TalosNode{{ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Status: osv1alpha1.TalosNodeStatus{Version: "v1.14.6", Conditions: drift, ConfigDigest: "sha256:1"}}}
	hosts := map[string]string{"a": "box-a"}
	require.NotNil(t, needed(cluster, nodes, hosts), "drift without a hold is proposed")

	cluster.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionSecretsBundleHeld, Status: metav1.ConditionTrue}}
	assert.Nil(t, needed(cluster, nodes, hosts), "a held bundle proposes no Config rollout")
	cluster.Status.KubernetesVersion = "v1.36.1"
	assert.Nil(t, needed(cluster, nodes, hosts), "nor a KubernetesUpgrade, which applies configs too")

	nodes[0].Status.Version = "v1.14.5"
	ro := needed(cluster, nodes, hosts)
	require.NotNil(t, ro)
	assert.Equal(t, osv1alpha1.RolloutTalosUpgrade, ro.Spec.Kind, "a Talos upgrade applies no config")
}

// A Config rollout proposed before the hold is not planned while it lasts.
func TestPlanBlocksConfigWhileTheBundleIsHeld(t *testing.T) {
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	cluster.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionSecretsBundleHeld, Status: metav1.ConditionTrue}}
	r := &TalosRolloutReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster).Build()}
	ro := &osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{
		ClusterRef: osv1alpha1.Reference{Name: "c"}, Kind: osv1alpha1.RolloutConfig}}

	plan, why, err := r.plan(t.Context(), ro)
	require.NoError(t, err)
	assert.Empty(t, plan)
	assert.Contains(t, why, "SecretsBundleHeld=True")
}
