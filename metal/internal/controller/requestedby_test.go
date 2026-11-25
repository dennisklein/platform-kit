package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func requested(name string) map[string]string {
	return map[string]string{metalv1alpha1.AnnotationRequestedBy: name}
}

// A rollout names everyone who last changed what it applies: the cluster,
// its nodes and the patch ConfigMaps in the operator's namespace.
func TestClusterRequesters(t *testing.T) {
	patch := func(ns, name string) osv1alpha1.Patch {
		return osv1alpha1.Patch{ConfigMapRef: &osv1alpha1.ConfigMapKeyReference{Namespace: ns, Name: name, Key: "p"}}
	}
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: requested("alice")},
		Spec: osv1alpha1.TalosClusterSpec{Patches: []osv1alpha1.Patch{patch("pk-metal", "net"), patch("tenant", "far")}}}
	members := []osv1alpha1.TalosNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Annotations: requested("bob")},
			Spec: osv1alpha1.TalosNodeSpec{Patches: []osv1alpha1.Patch{patch("pk-metal", "net"), patch("pk-metal", "gone")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Annotations: requested("alice")}},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "net", Annotations: requested("carol")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "far", Annotations: requested("mallory")}},
	).Build()
	got, err := clusterRequesters(t.Context(), cl, "pk-metal", cluster, members)
	require.NoError(t, err)
	assert.Equal(t, []string{"alice", "bob", "carol"}, got,
		"sorted, once each; a ConfigMap the operator does not read names nobody")
}

// An unapproved rollout the operator proposed ends when a person outside
// its requestedBy changed what it applies; the proposer proposes it again.
func TestRolloutEndsWhenItsRequestersMove(t *testing.T) {
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: requested("alice")}}
	ro := &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: "c-config-1"},
		Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}, Kind: osv1alpha1.RolloutConfig,
			ProposedBy: "operator", RequestedBy: []string{"alice"}}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster).Build()
	r := &TalosRolloutReconciler{Client: cl, Namespace: "pk-metal", Proposer: "operator"}

	stop, err := r.requestersMoved(t.Context(), ro)
	require.NoError(t, err)
	assert.False(t, stop, "the same requesters: the rollout goes on")

	cluster.Annotations = requested("bob")
	require.NoError(t, cl.Update(t.Context(), cluster))
	stop, err = r.requestersMoved(t.Context(), ro)
	require.NoError(t, err)
	require.True(t, stop)
	c := meta.FindStatusCondition(ro.Status.Conditions, osv1alpha1.ConditionComplete)
	require.NotNil(t, c)
	assert.Equal(t, osv1alpha1.ReasonPlanChanged, c.Reason)
	assert.Contains(t, c.Message, "bob changed what this rollout applies")

	person := ro.DeepCopy()
	person.Status = osv1alpha1.TalosRolloutStatus{}
	person.Spec.ProposedBy = "dave"
	stop, err = r.requestersMoved(t.Context(), person)
	require.NoError(t, err)
	assert.False(t, stop, "a rollout a person created names that person; the check is for the operator's")
}
