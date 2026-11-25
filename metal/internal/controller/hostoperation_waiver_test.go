package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The executor runs an operation the operator proposed without approval
// only for the kinds the operator code makes so; anything else in the
// operator's name was not made by that code.
func TestWaiverRefusal(t *testing.T) {
	claim := func(automatic bool) *metalv1alpha1.HostClaim {
		c := &metalv1alpha1.HostClaim{}
		c.Namespace, c.Name, c.UID = "default", "node-1", "claim-uid"
		if automatic {
			c.Spec.Provisioning = &metalv1alpha1.ClaimProvisioning{Approval: metalv1alpha1.ProvisioningApprovalAutomatic}
		}
		return c
	}
	install := func() *metalv1alpha1.HostOperation {
		op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
		op.Spec.Reason = reasonProvisioning
		op.Labels = claimLabels(claim(true), purposeProvision)
		return op
	}
	for name, tc := range map[string]struct {
		op      func() *metalv1alpha1.HostOperation
		objs    []client.Object
		refused string
	}{
		"an install of an Automatic claim runs": {op: install, objs: []client.Object{claim(true)}},
		"an install of a Manual claim is refused": {op: install, objs: []client.Object{claim(false)},
			refused: "does not set provisioning.approval: Automatic"},
		"an install whose claim is gone is refused": {op: install, refused: "does not exist"},
		"a release runs": {op: func() *metalv1alpha1.HostOperation {
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}})
			op.Spec.Reason = reasonRelease
			op.Labels = map[string]string{labelPurpose: purposeRelease}
			return op
		}},
		"a release without its label is refused": {op: func() *metalv1alpha1.HostOperation {
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}})
			op.Spec.Reason = reasonRelease
			return op
		}, refused: "purpose label"},
		"a rollout step is left to the rollout gate": {op: func() *metalv1alpha1.HostOperation {
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
			op.Spec.RolloutRef = &metalv1alpha1.RolloutReference{Name: "r", PlanHash: "h"}
			return op
		}},
		"any other plan is refused": {op: func() *metalv1alpha1.HostOperation {
			return preClearedOp(metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{}})
		}, refused: "none of them"},
		"a proposal by a person is left to admission": {op: func() *metalv1alpha1.HostOperation {
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
			op.Spec.ProposedBy = "admin-alice"
			return op
		}},
		"an operation that needs approval is not checked": {op: func() *metalv1alpha1.HostOperation {
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
			op.Spec.Approval.Required = ptr.To(true)
			return op
		}},
	} {
		t.Run(name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tc.objs...).Build()
			r := &HostOperationReconciler{Client: cl, Operator: testOperator}
			why, err := r.waiverRefusal(t.Context(), tc.op())
			require.NoError(t, err)
			if tc.refused == "" {
				assert.Empty(t, why)
				return
			}
			assert.Contains(t, why, tc.refused)
		})
	}
}
