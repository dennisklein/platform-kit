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
