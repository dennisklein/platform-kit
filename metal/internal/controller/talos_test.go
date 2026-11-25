package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The manager may read Secrets and ConfigMaps in its own namespace only; its
// Role grants nothing else. A reference that points elsewhere has to surface as
// the semantic hold a person can act on, and must not reach the API at all.
func TestReferencesOutsideTheOperatorNamespaceDoNotResolve(t *testing.T) {
	scheme, err := pkscheme.New()
	require.NoError(t, err)

	// Both objects exist, in the namespace the references name. A reader that
	// followed the reference would find them, so finding nothing proves the guard.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "bundle"},
			Data:       map[string][]byte{render.BundleKey: []byte("cluster: {}")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "patches",
				Labels: map[string]string{osv1alpha1.LabelPatch: "true"}},
			Data: map[string]string{"p.yaml": "machine: {}"},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "unlabeled"},
			Data:       map[string]string{"p.yaml": "machine: {}"},
		},
	).Build()

	t.Run("secrets bundle", func(t *testing.T) {
		cluster := &osv1alpha1.TalosCluster{Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: "v1.14.6",
			SecretsRef:   osv1alpha1.SecretReference{Namespace: "tenant", Name: "bundle"},
		}}
		_, err := loadBundle(t.Context(), cl, "pk-metal", cluster)
		var unavailable *bundleUnavailable
		require.True(t, errors.As(err, &unavailable),
			"a bundle outside the operator namespace must be a SecretsUnavailable hold, not a reconcile error: %v", err)
		assert.Contains(t, unavailable.msg, "tenant/bundle", "the hold must name the Secret it refused")
		assert.Contains(t, unavailable.msg, "pk-metal", "the hold must name the namespace that would work")
	})

	t.Run("patch ConfigMap", func(t *testing.T) {
		read := configMapReader(t.Context(), cl, "pk-metal")
		_, err := read(osv1alpha1.ConfigMapKeyReference{Namespace: "tenant", Name: "patches", Key: "p.yaml"})
		require.ErrorIs(t, err, render.ErrPatchUnresolvable,
			"a patch outside the operator namespace must hold the render like a missing one")
		assert.Contains(t, err.Error(), "pk-metal", "the hold must name the namespace that would work")

		got, err := configMapReader(t.Context(), cl, "tenant")(
			osv1alpha1.ConfigMapKeyReference{Namespace: "tenant", Name: "patches", Key: "p.yaml"})
		require.NoError(t, err, "the same reference resolves when it is the operator's namespace")
		assert.Equal(t, "machine: {}", got)

		_, err = configMapReader(t.Context(), cl, "tenant")(
			osv1alpha1.ConfigMapKeyReference{Namespace: "tenant", Name: "unlabeled", Key: "p.yaml"})
		require.ErrorIs(t, err, render.ErrPatchUnresolvable, "a ConfigMap without the patch label holds the render")
		assert.ErrorContains(t, err, "kubectl -n tenant label configmap unlabeled os.pk.io/patch=true")
	})
}

// A cluster an older operator saw bootstrapped carries only Bootstrapped=True;
// the upgrade must not forget it, or the first loss after the upgrade would
// be bootstrapped over.
func TestRememberedBootstrapBackfillsFromTheCondition(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	c := &osv1alpha1.TalosCluster{Status: osv1alpha1.TalosClusterStatus{Conditions: []metav1.Condition{{
		Type: osv1alpha1.ConditionBootstrapped, Status: metav1.ConditionTrue, LastTransitionTime: at,
	}}}}
	assert.Equal(t, at.Time, rememberedBootstrap(c))
	require.NotNil(t, c.Status.BootstrappedAt)
	assert.Equal(t, at.Time, c.Status.BootstrappedAt.Time)

	never := &osv1alpha1.TalosCluster{Status: osv1alpha1.TalosClusterStatus{Conditions: []metav1.Condition{{
		Type: osv1alpha1.ConditionBootstrapped, Status: metav1.ConditionFalse, LastTransitionTime: at,
	}}}}
	assert.True(t, rememberedBootstrap(never).IsZero())
	assert.Nil(t, never.Status.BootstrappedAt)
}
