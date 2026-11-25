package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// params is the approval policies' ConfigMap; a nil value leaves the key out.
func params(value *string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: approvalParams},
		Data:       map[string]string{"operator": operator, "adminGroup": "pk-metal:admins"},
	}
	if value != nil {
		cm.Data["selfApproval"] = *value
	}
	return cm
}

func str(s string) *string { return &s }

// denied is the error the API server returns when an admission policy
// refuses a request.
func denied(kind, name, msg string) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "metal.pk.io", Resource: kind}, name,
		errors.New("ValidatingAdmissionPolicy 'hostoperation-approval' with binding 'hostoperation-approval' denied request: "+msg))
}

// refusingPatches is a client whose every patch is refused with err.
func refusingPatches(err error, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return err
			},
		}).Build()
}

func TestSelfApprovalSetting(t *testing.T) {
	for _, c := range []struct {
		s       selfApproval
		allowed bool
	}{
		{selfApproval{}, true}, // unreadable: admission decides
		{selfApproval{Known: true}, true},
		{selfApproval{Known: true, Value: "Allowed"}, true},
		{selfApproval{Known: true, Value: "Forbidden"}, false},
		{selfApproval{Known: true, Value: "allowed"}, false}, // a typo closes the gate
	} {
		assert.Equal(t, c.allowed, c.s.allowed(), "%+v", c.s)
		assert.Equal(t, c.allowed, c.s.ownProposal("operation x", "alice") == nil, "%+v", c.s)
	}
}

// TestApproveOwnProposal: the proposer approves their own operation where
// the site allows it, and is refused before the plan where it does not.
func TestApproveOwnProposal(t *testing.T) {
	ctx := context.Background()
	own := func(extra ...client.Object) (client.Client, approval, *bytes.Buffer) {
		op, objs := bootPlan()
		op.Spec.ProposedBy = "alice"
		var out bytes.Buffer
		return fakeClient(append(objs, extra...)...),
			approval{Namespace: op.Namespace, Name: op.Name, User: "alice", Yes: true, Out: &out}, &out
	}
	approvedOf := func(t *testing.T, cl client.Client, a approval) string {
		op, _ := bootPlan()
		op.Namespace, op.Name = a.Namespace, a.Name
		return approvedBy(t, cl, op)
	}

	for name, cm := range map[string]*corev1.ConfigMap{
		"the setting is Allowed":    params(str("Allowed")),
		"the setting is missing":    params(nil),
		"the setting is unreadable": nil,
	} {
		t.Run("allowed: "+name, func(t *testing.T) {
			var extra []client.Object
			if cm != nil {
				extra = append(extra, cm)
			}
			cl, a, out := own(extra...)
			require.NoError(t, approve(ctx, cl, a))
			assert.Equal(t, "alice", approvedOf(t, cl, a))
			assert.Contains(t, out.String(), "You proposed this operation yourself.")
			if cm != nil {
				assert.Contains(t, out.String(), "This site allows you to approve it")
			} else {
				assert.Contains(t, out.String(), "If this site forbids that (selfApproval), the cluster refuses the approval")
			}
		})
	}

	t.Run("forbidden: refused before the plan", func(t *testing.T) {
		cl, a, out := own(params(str("Forbidden")))
		err := approve(ctx, cl, a)
		require.EqualError(t, err, "operation pk-metal/provision-node1-1a2b3c4d was proposed by you (alice), "+
			"and this site forbids approving your own proposal (selfApproval: Forbidden in ConfigMap "+
			"pk-metal/hostoperation-approval); another person must approve it")
		assert.Empty(t, approvedOf(t, cl, a))
		assert.NotContains(t, out.String(), "steps, in this order:")
	})

	t.Run("an unknown value counts as Forbidden", func(t *testing.T) {
		cl, a, _ := own(params(str("yes")))
		err := approve(ctx, cl, a)
		require.ErrorContains(t, err, `selfApproval in ConfigMap pk-metal/hostoperation-approval is "yes"`)
		assert.ErrorContains(t, err, "counts as Forbidden; another person must approve it, or an admin sets it")
		assert.Empty(t, approvedOf(t, cl, a))
	})

	t.Run("another person's proposal does not read the setting", func(t *testing.T) {
		op, objs := bootPlan()
		var out bytes.Buffer
		cl := fakeClient(append(objs, params(str("Forbidden")))...)
		require.NoError(t, approve(ctx, cl, approval{Namespace: op.Namespace, Name: op.Name, User: "alice", Yes: true, Out: &out}))
		assert.NotContains(t, out.String(), "You proposed")
	})
}

// TestApproveAdmissionRefusal: when admission refuses an approval that the
// CLI could not foresee (the setting was unreadable, or another rule), the
// person reads the policy's own message, without the policy's name.
func TestApproveAdmissionRefusal(t *testing.T) {
	ctx := context.Background()
	const msg = "this site forbids approving your own proposal (selfApproval: Forbidden in ConfigMap " +
		"pk-metal/hostoperation-approval): another person must approve"

	t.Run("operation", func(t *testing.T) {
		op, objs := bootPlan()
		op.Spec.ProposedBy = "alice"
		cl := refusingPatches(denied("hostoperations", op.Name, msg), objs...)
		err := approve(ctx, cl, approval{Namespace: op.Namespace, Name: op.Name, User: "alice", Yes: true, Out: &bytes.Buffer{}})
		require.EqualError(t, err, "the cluster refused the approval of operation pk-metal/provision-node1-1a2b3c4d; "+
			"nothing changed: "+msg)
	})

	t.Run("rollout", func(t *testing.T) {
		ro := planned()
		ro.Spec.ProposedBy = "alice"
		cl := refusingPatches(denied("talosrollouts", ro.Name, msg), ro)
		var out bytes.Buffer
		err := approveRollout(ctx, cl, rolloutApproval{Name: ro.Name, User: "alice", Yes: true, Out: &out})
		require.EqualError(t, err, "the cluster refused the approval of rollout lab-talosupgrade-1a2b3c4d; "+
			"nothing changed: "+msg)
		assert.Contains(t, out.String(), "You proposed this rollout yourself.")
	})

	t.Run("other errors pass as they are", func(t *testing.T) {
		op, objs := bootPlan()
		cl := refusingPatches(errors.New("connection refused"), objs...)
		err := approve(ctx, cl, approval{Namespace: op.Namespace, Name: op.Name, User: "alice", Yes: true, Out: &bytes.Buffer{}})
		require.EqualError(t, err, "approve operation pk-metal/provision-node1-1a2b3c4d: connection refused")
	})
}

// TestApproveOwnRollout: the same setting governs a rollout.
func TestApproveOwnRollout(t *testing.T) {
	ctx := context.Background()
	mine := func() *osv1alpha1.TalosRollout {
		ro := planned()
		ro.Spec.ProposedBy = "alice"
		return ro
	}
	approvedOf := func(t *testing.T, cl client.Client) string {
		ro := &osv1alpha1.TalosRollout{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "lab-talosupgrade-1a2b3c4d"}, ro))
		return ro.Spec.Approval.ApprovedBy
	}

	t.Run("allowed", func(t *testing.T) {
		cl := fakeClient(mine(), params(str("Allowed")))
		var out bytes.Buffer
		require.NoError(t, approveRollout(ctx, cl, rolloutApproval{Name: "lab-talosupgrade-1a2b3c4d", User: "alice", Yes: true, Out: &out}))
		assert.Equal(t, "alice", approvedOf(t, cl))
		assert.Contains(t, out.String(), "This site allows you to approve it")
	})

	t.Run("forbidden", func(t *testing.T) {
		cl := fakeClient(mine(), params(str("Forbidden")))
		var out bytes.Buffer
		err := approveRollout(ctx, cl, rolloutApproval{Name: "lab-talosupgrade-1a2b3c4d", User: "alice", Yes: true, Out: &out})
		require.ErrorContains(t, err, "rollout lab-talosupgrade-1a2b3c4d was proposed by you (alice), and this site forbids")
		assert.Empty(t, approvedOf(t, cl))
		assert.Empty(t, out.String(), "no plan is shown that cannot be approved")
	})
}
