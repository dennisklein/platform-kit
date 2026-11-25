// The named guard-lift suite: setting or changing an annotation that lifts a
// safety rule needs the value "<username>: <reason>", with the username of
// the request (config/policy/guard-lift.yaml). Removing one, and updates
// that leave the value alone, stay open.
package envtest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestNamedGuardLifts(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()
	installPolicy(t, ctx, cl, "guard-lift.yaml")
	alice := st.userClient(t, "alice")
	bob := st.userClient(t, "bob")

	annotate := func(c client.Client, obj client.Object, key string, value *string) error {
		v := any(nil)
		if value != nil {
			v = *value
		}
		raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: v}}})
		require.NoError(t, err)
		return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
	}
	str := func(s string) *string { return &s }

	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "lift-op", Namespace: "default"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: "no-such-host"},
			ProposedBy: "alice",
			Actions:    []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
		},
	}
	require.NoError(t, cl.Create(ctx, op))
	claim := &metalv1alpha1.HostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lift-claim", Namespace: "default"},
		Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "no-such-asset"}},
	}
	require.NoError(t, cl.Create(ctx, claim))
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "lift-node"},
		Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lift"}, Role: osv1alpha1.RoleControlPlane,
			Address: "192.0.2.41", InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "disk-1"}},
	}
	require.NoError(t, cl.Create(ctx, node))

	// The policy takes a moment to become enforcing after its creation.
	var last error
	require.Eventually(t, func() bool {
		last = annotate(bob, op, metalv1alpha1.AnnotationIgnoreWindow, str("alice: CVE fix"))
		if last == nil {
			require.NoError(t, annotate(cl, op, metalv1alpha1.AnnotationIgnoreWindow, nil))
		}
		return last != nil
	}, 30*time.Second, 100*time.Millisecond, "the policy never refused a lift naming another person (last: %v)", last)
	assert.Contains(t, last.Error(), `a guard lift must name you and say why: set metal.pk.io/ignore-window to "bob: <reason>"`)

	for _, tc := range []struct {
		kind string
		obj  client.Object
		key  string
	}{
		{"HostOperation", op, metalv1alpha1.AnnotationIgnoreWindow},
		{"HostOperation", op, metalv1alpha1.AnnotationOverrideClusterGuard},
		{"HostClaim", claim, metalv1alpha1.AnnotationAckEtcdRisk},
		{"TalosNode", node, metalv1alpha1.AnnotationAckEtcdRisk},
	} {
		t.Run(tc.kind+" "+tc.key, func(t *testing.T) {
			err := annotate(alice, tc.obj, tc.key, str("bob: he said so"))
			require.Error(t, err, "a lift naming another user is refused")
			assert.Contains(t, err.Error(), "must name you")

			for _, bad := range []string{"etcd is down already", "alice:", "alice:   ", "alice etcd is down", "alice:no space"} {
				err := annotate(alice, tc.obj, tc.key, &bad)
				require.Error(t, err, "%q names nobody or gives no reason", bad)
				assert.Contains(t, err.Error(), "must name you", bad)
			}

			require.NoError(t, annotate(alice, tc.obj, tc.key, str("alice: etcd is down already")),
				"the right user with a reason is accepted")
			require.NoError(t, annotate(alice, tc.obj, tc.key, str("alice: etcd is down already")),
				"setting the same value again changes nothing")

			// Bob edits the object without touching the lift: allowed.
			require.NoError(t, annotate(bob, tc.obj, "example.com/note", str("unrelated")))
			// Bob changes the lift: the new value must name him.
			err = annotate(bob, tc.obj, tc.key, str("alice: still down"))
			require.Error(t, err, "a changed value names whoever changes it")
			require.NoError(t, annotate(bob, tc.obj, tc.key, str("bob: still down")))

			require.NoError(t, annotate(alice, tc.obj, tc.key, nil), "anyone who may update the object removes a lift")
		})
	}

	t.Run("a create with a lift names its creator", func(t *testing.T) {
		mk := func(name, value string) *metalv1alpha1.HostOperation {
			o := op.DeepCopy()
			o.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: "default",
				Annotations: map[string]string{metalv1alpha1.AnnotationOverrideClusterGuard: value}}
			return o
		}
		err := alice.Create(ctx, mk("lift-create-bad", "repair"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must name you")
		good := mk("lift-create-good", "alice: repair")
		require.NoError(t, alice.Create(ctx, good))
		require.NoError(t, cl.Delete(ctx, good))
	})

	t.Run("other annotations are not lifts", func(t *testing.T) {
		require.NoError(t, annotate(alice, op, metalv1alpha1.AnnotationAbort, str("stop")))
	})
}
