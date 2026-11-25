// The requested-by suite: an edit that makes the operator propose work names
// its requester (config/policy/requested-by.yaml), the operator copies the
// names into the proposal's spec.requestedBy, and where the site forbids
// self-approval the approval policies refuse an approval by one of them.
package envtest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestRequestedBy(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()

	const operator = "op-sa"
	installApprovalPolicy(t, ctx, cl, operator)
	installRolloutPolicy(t, ctx, cl)
	installPolicy(t, ctx, cl, "requested-by.yaml")
	operatorCl := st.userClient(t, operator)
	alice := st.userClient(t, "alice")
	bob := st.userClient(t, "bob")
	key := metalv1alpha1.AnnotationRequestedBy
	stamp := func(name string) string {
		return `{"metadata":{"annotations":{"` + key + `":"` + name + `"}}}`
	}
	patch := func(c client.Client, obj client.Object, body string) error {
		return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(body)))
	}

	t.Run("a change that makes the operator propose work names its requester", func(t *testing.T) {
		cluster := &osv1alpha1.TalosCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "rb-fleet"},
			Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2",
				Endpoint:   "https://192.0.2.10:6443",
				SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "rb-fleet-secrets"}},
		}
		err := refusedOnceEnforcing(t, ctx, cl, alice, cluster, "must name who asked for it")
		assert.Contains(t, err.Error(), `set the annotation metal.pk.io/requested-by to "alice"`)

		other := cluster.DeepCopy()
		other.Annotations = map[string]string{key: "bob"}
		require.Error(t, alice.Create(ctx, other), "nobody names another person")

		mine := cluster.DeepCopy()
		mine.Annotations = map[string]string{key: "alice"}
		require.NoError(t, alice.Create(ctx, mine))

		require.NoError(t, patch(alice, mine, `{"spec":{"talosVersion":"v1.14.7"}}`),
			"the requester changes the spec again under the same name")
		err = patch(bob, mine, `{"spec":{"talosVersion":"v1.14.8"}}`)
		require.Error(t, err, "another person's change of the spec needs their own name")
		assert.Contains(t, err.Error(), `"bob"`)
		require.NoError(t, patch(bob, mine, `{"metadata":{"annotations":{"`+key+`":"bob"}},"spec":{"talosVersion":"v1.14.8"}}`),
			"a person takes the change on as theirs")
		require.NoError(t, patch(alice, mine, `{"metadata":{"labels":{"team":"a"}}}`),
			"a label is no intent: it needs no name")
		require.Error(t, patch(bob, mine, `{"metadata":{"annotations":{"`+key+`":null}}}`), "nobody removes the name")

		err = alice.Delete(ctx, mine)
		require.Error(t, err, "a delete names its requester first")
		assert.Contains(t, err.Error(), "first set the annotation")
		require.NoError(t, patch(alice, mine, stamp("alice")))
		require.NoError(t, alice.Delete(ctx, mine))
	})

	t.Run("a Host needs a name for a wipe only", func(t *testing.T) {
		host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "rb-host"}}
		require.NoError(t, cl.Create(ctx, host))
		t.Cleanup(func() { _ = cl.Delete(ctx, host) })
		require.NoError(t, patch(alice, host, `{"metadata":{"labels":{"rack":"r1"}}}`))
		err := patch(alice, host, `{"metadata":{"annotations":{"`+metalv1alpha1.AnnotationWipe+`":"v1.14.6"}}}`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must name who asked for it")
		require.NoError(t, patch(alice, host,
			`{"metadata":{"annotations":{"`+metalv1alpha1.AnnotationWipe+`":"v1.14.6","`+key+`":"alice"}}}`))
	})

	t.Run("a ConfigMap needs a name only as a Talos config patch", func(t *testing.T) {
		plain := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rb-plain", Namespace: "pk-metal"},
			Data: map[string]string{"a": "b"}}
		require.NoError(t, alice.Create(ctx, plain))
		labeled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rb-patch", Namespace: "pk-metal",
			Labels: map[string]string{osv1alpha1.LabelPatch: "true"}}, Data: map[string]string{"patch.yaml": "{}"}}
		require.Error(t, alice.Create(ctx, labeled))
		labeled.Annotations = map[string]string{key: "alice"}
		require.NoError(t, alice.Create(ctx, labeled))
		require.Error(t, patch(bob, labeled, `{"data":{"patch.yaml":"machine: {}"}}`))
		require.Error(t, patch(bob, plain, `{"metadata":{"labels":{"`+osv1alpha1.LabelPatch+`":"true"}},"data":{"a":"c"}}`),
			"a ConfigMap that becomes a patch with new data names its requester")
		err := patch(bob, plain, `{"metadata":{"labels":{"`+osv1alpha1.LabelPatch+`":"true"}}}`)
		require.Error(t, err, "a ConfigMap that becomes a patch with the data it has names its requester too")
		assert.Contains(t, err.Error(), "must name who asked for it")
		require.NoError(t, patch(bob, plain,
			`{"metadata":{"labels":{"`+osv1alpha1.LabelPatch+`":"true"},"annotations":{"`+key+`":"bob"}}}`))
	})

	t.Run("pausing or aborting a rollout names who did it", func(t *testing.T) {
		ro := &osv1alpha1.TalosRollout{
			ObjectMeta: metav1.ObjectMeta{Name: "rb-rollout"},
			Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "fleet"},
				Kind: osv1alpha1.RolloutReboot, Nodes: []string{"node-1"}, ProposedBy: operator},
		}
		require.NoError(t, operatorCl.Create(ctx, ro))
		t.Cleanup(func() { _ = cl.Delete(ctx, ro) })
		require.Error(t, patch(alice, ro, `{"spec":{"paused":true}}`))
		require.NoError(t, patch(alice, ro, `{"metadata":{"annotations":{"`+key+`":"alice"}},"spec":{"paused":true}}`))
		require.Error(t, patch(bob, ro, `{"spec":{"abort":true}}`))
	})

	// The approvals. The operator proposed each object; alice's edit caused it.
	const planHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	seq := 0
	propose := func(t *testing.T, kind string) client.Object {
		t.Helper()
		seq++
		name := fmt.Sprintf("rb-%s-%d", strings.ToLower(kind), seq)
		var obj client.Object
		if kind == "HostOperation" {
			obj = &metalv1alpha1.HostOperation{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: metalv1alpha1.HostOperationSpec{
					HostRef: metalv1alpha1.Reference{Name: "no-such-host"}, ProposedBy: operator,
					RequestedBy: []string{"alice"},
					Actions:     []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
				},
			}
		} else {
			obj = &osv1alpha1.TalosRollout{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "fleet"},
					Kind: osv1alpha1.RolloutReboot, Nodes: []string{"node-1"}, ProposedBy: operator,
					RequestedBy: []string{"alice"}},
			}
		}
		require.NoError(t, operatorCl.Create(ctx, obj))
		t.Cleanup(func() { _ = cl.Delete(ctx, obj) })
		return obj
	}
	approve := func(c client.Client, obj client.Object, who string) error {
		return patch(c, obj, `{"spec":{"approval":{"approvedBy":"`+who+`","planHash":"`+planHash+`"}}}`)
	}
	for _, kind := range []string{"HostOperation", "TalosRollout"} {
		t.Run(kind+": the requester may not approve where the site forbids it", func(t *testing.T) {
			setSelfApproval(t, ctx, cl, ptr.To("Forbidden"))
			obj := propose(t, kind)
			var last error
			require.Eventually(t, func() bool {
				last = approve(alice, obj, "alice")
				return last != nil && strings.Contains(last.Error(), "you asked for this change")
			}, 30*time.Second, 100*time.Millisecond, "the requester's approval is never refused (last: %v)", last)
			require.NoError(t, approve(bob, obj, "bob"), "another person approves")
		})
		t.Run(kind+": the requester may approve where the site allows it", func(t *testing.T) {
			setSelfApproval(t, ctx, cl, ptr.To("Allowed"))
			obj := propose(t, kind)
			require.Eventually(t, func() bool { return approve(alice, obj, "alice") == nil },
				30*time.Second, 100*time.Millisecond, "the requester's approval is refused where it is allowed")
		})
		t.Run(kind+": the requesters are frozen", func(t *testing.T) {
			obj := propose(t, kind)
			err := patch(cl, obj, `{"spec":{"requestedBy":["carol"]}}`)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "requestedBy")
		})
	}
}
