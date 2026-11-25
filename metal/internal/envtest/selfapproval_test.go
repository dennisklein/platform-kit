// The self-approval suite: whether a person may approve what they proposed is
// a site setting (selfApproval in the approval policies' ConfigMap), and the
// rules around it hold in every mode — the operator approves nothing, an
// approval is stamped as the caller, and nothing is created pre-approved.
// Each mode is checked on a HostOperation and on a TalosRollout, the two
// kinds whose approval the setting governs.
package envtest_test

import (
	"context"
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

// approvalKind is one kind under the approval gate, as the suite drives it:
// create a fresh object proposed by someone, and stamp an approval on it.
type approvalKind struct {
	name    string
	propose func(c client.Client, name, proposer string, approvedBy string) (client.Object, error)
	approve func(c client.Client, obj client.Object, who string) error
}

func TestSelfApproval(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()

	const operator = "op-sa"
	installApprovalPolicy(t, ctx, cl, operator)
	installRolloutPolicy(t, ctx, cl)
	operatorCl := st.userClient(t, operator)
	alice := st.userClient(t, "alice")
	bob := st.userClient(t, "bob")
	clients := map[string]client.Client{operator: operatorCl, "alice": alice, "bob": bob}

	const planHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	kinds := []approvalKind{
		{
			name: "HostOperation",
			propose: func(c client.Client, name, proposer, approvedBy string) (client.Object, error) {
				op := &metalv1alpha1.HostOperation{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
					Spec: metalv1alpha1.HostOperationSpec{
						HostRef:    metalv1alpha1.Reference{Name: "no-such-host"},
						ProposedBy: proposer,
						Actions:    []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
						Approval:   metalv1alpha1.OperationApproval{ApprovedBy: approvedBy},
					},
				}
				return op, c.Create(ctx, op)
			},
			approve: func(c client.Client, obj client.Object, who string) error {
				return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType,
					[]byte(`{"spec":{"approval":{"approvedBy":"`+who+`"}}}`)))
			},
		},
		{
			name: "TalosRollout",
			propose: func(c client.Client, name, proposer, approvedBy string) (client.Object, error) {
				ro := &osv1alpha1.TalosRollout{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Spec: osv1alpha1.TalosRolloutSpec{
						ClusterRef: osv1alpha1.Reference{Name: "fleet"},
						Kind:       osv1alpha1.RolloutReboot,
						Nodes:      []string{"node-1"},
						ProposedBy: proposer,
					},
				}
				if approvedBy != "" {
					ro.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: approvedBy, PlanHash: planHash}
				}
				return ro, c.Create(ctx, ro)
			},
			approve: func(c client.Client, obj client.Object, who string) error {
				return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType,
					[]byte(`{"spec":{"approval":{"approvedBy":"`+who+`","planHash":"`+planHash+`"}}}`)))
			},
		},
	}

	// try proposes a fresh object as proposer, stamps an approval by
	// approver as who, and deletes the object again. It returns the
	// approval's error.
	seq := 0
	try := func(t *testing.T, k approvalKind, proposer, approver, who string) error {
		t.Helper()
		seq++
		obj, err := k.propose(clients[proposer], fmt.Sprintf("self-%s-%d", strings.ToLower(k.name), seq), proposer, "")
		require.NoError(t, err)
		defer func() { require.NoError(t, cl.Delete(ctx, obj)) }()
		return k.approve(clients[approver], obj, who)
	}

	modes := []struct {
		name    string
		value   *string // nil: the key is missing
		allowed bool
		refusal string
	}{
		{name: "missing key means Allowed", value: nil, allowed: true},
		{name: "Allowed", value: ptr.To("Allowed"), allowed: true},
		{name: "Forbidden", value: ptr.To("Forbidden"), allowed: false,
			refusal: "this site forbids approving your own proposal (selfApproval: Forbidden in ConfigMap pk-metal/hostoperation-approval): another person must approve"},
		{name: "an unknown value counts as Forbidden", value: ptr.To("yes"), allowed: false,
			refusal: `selfApproval in ConfigMap pk-metal/hostoperation-approval is "yes", which is not Allowed or Forbidden`},
	}
	for _, mode := range modes {
		setSelfApproval(t, ctx, cl, mode.value)
		t.Run(mode.name, func(t *testing.T) {
			for _, k := range kinds {
				t.Run(k.name, func(t *testing.T) {
					// The policies read their params through an informer, so a
					// changed setting takes a moment to apply; the first
					// self-approval waits for it.
					var last error
					require.Eventually(t, func() bool {
						last = try(t, k, "alice", "alice", "alice")
						if mode.allowed {
							return last == nil
						}
						return last != nil && strings.Contains(last.Error(), mode.refusal)
					}, 30*time.Second, 100*time.Millisecond,
						"self-approval: want allowed=%v (last: %v)", mode.allowed, last)

					err := try(t, k, operator, operator, operator)
					require.Error(t, err, "the operator approves nothing, not even its own proposal")
					assert.Contains(t, err.Error(), "the operator approves nothing: a person with the pk-metal-approver role must approve")

					err = try(t, k, "alice", operator, operator)
					require.Error(t, err, "the operator approves nothing, not even a person's proposal")
					assert.Contains(t, err.Error(), "the operator approves nothing")

					err = try(t, k, "alice", "bob", "alice")
					require.Error(t, err, "nobody stamps another person's name, not even the proposer's")
					assert.Contains(t, err.Error(), "only approve as yourself")

					require.NoError(t, try(t, k, "alice", "bob", "bob"), "another person approves in every mode")

					obj, err := k.propose(alice, "self-preapproved", "alice", "alice")
					if err == nil {
						require.NoError(t, cl.Delete(ctx, obj))
					}
					require.Error(t, err, "nothing is created pre-approved, not even by the person who may approve it")
					assert.Contains(t, err.Error(), "pre-approved")
				})
			}
		})
	}

	t.Run("an approval stays write-once when self-approval is allowed", func(t *testing.T) {
		setSelfApproval(t, ctx, cl, ptr.To("Allowed"))
		for _, k := range kinds {
			obj, err := k.propose(alice, "self-once-"+strings.ToLower(k.name), "alice", "")
			require.NoError(t, err)
			require.Eventually(t, func() bool { return k.approve(alice, obj, "alice") == nil },
				30*time.Second, 100*time.Millisecond, "%s: the proposer approves", k.name)
			err = k.approve(bob, obj, "bob")
			require.Error(t, err, "%s: a second approval replaces nothing", k.name)
			assert.Contains(t, err.Error(), "write-once")
			require.NoError(t, cl.Delete(ctx, obj))
		}
	})
}

// setSelfApproval sets the selfApproval key of the approval policies'
// params, or removes it when value is nil.
func setSelfApproval(t *testing.T, ctx context.Context, cl client.Client, value *string) {
	t.Helper()
	params := &corev1.ConfigMap{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: "pk-metal", Name: "hostoperation-approval"}, params))
	if value == nil {
		delete(params.Data, "selfApproval")
	} else {
		params.Data["selfApproval"] = *value
	}
	require.NoError(t, cl.Update(ctx, params))
}
