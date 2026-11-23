package controller

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The manager may read Secrets and ConfigMaps in its own namespace only; its
// Role grants nothing else. A reference that points elsewhere has to surface as
// the semantic hold a person can act on, and must not reach the API at all.
func TestReferencesOutsideTheOperatorNamespaceDoNotResolve(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	// Both objects exist, in the namespace the references name. A reader that
	// followed the reference would find them, so finding nothing proves the guard.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "bundle"},
			Data:       map[string][]byte{render.BundleKey: []byte("cluster: {}")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "patches"},
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
	})
}
