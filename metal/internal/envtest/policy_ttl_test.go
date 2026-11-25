package envtest_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// TestCredentialTTLGuard: a published admin credential cannot be revoked,
// so its lifetime is an admin's decision above the 24h default. Anybody who
// may patch the cluster may shorten it; nobody else may stretch it.
func TestCredentialTTLGuard(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl
	installApprovalPolicy(t, ctx, cl, "op-sa")
	installPolicy(t, ctx, cl, "taloscluster-credentials-guard.yaml")
	alice := st.userClient(t, "alice")
	ada := st.userClient(t, "ada", adminGroup)

	cluster := func(name string, ttl time.Duration) *osv1alpha1.TalosCluster {
		c := &osv1alpha1.TalosCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosClusterSpec{
				TalosVersion: "v1.14.6", Endpoint: "https://10.0.0.10:6443", KubernetesVersion: "v1.36.0",
				SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: name + "-secrets"},
			},
		}
		if ttl > 0 {
			c.Spec.Credentials = &osv1alpha1.CredentialsSpec{TTL: &metav1.Duration{Duration: ttl}}
		}
		return c
	}

	err := refusedOnceEnforcing(t, ctx, cl, alice, cluster("long", 720*time.Hour), "only an admin")
	assert.Contains(t, err.Error(), "spec.credentials.ttl above 24h")
	require.NoError(t, ada.Create(ctx, cluster("long", 720*time.Hour)), "an admin sets a long ttl")
	require.NoError(t, alice.Create(ctx, cluster("short", 12*time.Hour)), "a short ttl needs no group")
	require.NoError(t, alice.Create(ctx, cluster("default", 0)))

	patch := func(c client.Client, name, body string) error {
		return c.Patch(ctx, &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: name}},
			client.RawPatch(types.MergePatchType, []byte(body)))
	}
	err = patch(alice, "default", `{"spec":{"credentials":{"ttl":"720h"}}}`)
	require.Error(t, err, "a non-admin must not stretch the ttl")
	assert.Contains(t, err.Error(), "only an admin")
	require.NoError(t, patch(alice, "long", `{"spec":{"kubernetesVersion":"v1.36.1"}}`),
		"another edit of a cluster with a long ttl passes")
	require.NoError(t, patch(alice, "long", `{"spec":{"credentials":{"ttl":"8h"}}}`), "anybody may shorten it")
	require.NoError(t, patch(ada, "short", `{"spec":{"credentials":{"ttl":"168h"}}}`))
}
