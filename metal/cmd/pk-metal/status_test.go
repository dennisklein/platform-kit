package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestRenderStatus(t *testing.T) {
	hosts := []metalv1alpha1.Host{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "simx12b000001"},
			Status: metalv1alpha1.HostStatus{
				Identity: &metalv1alpha1.HostIdentity{Serial: "SIMX12B000001", Vendor: "Supermicro"},
				ManagedBy: []metalv1alpha1.ManagementPath{
					{EndpointRef: metalv1alpha1.Reference{Name: "ci-smc"}, SystemID: "1", Reachable: true},
				},
				Phase: metalv1alpha1.HostAvailable,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "simhw2288v5000391"},
			Spec: metalv1alpha1.HostSpec{
				ClaimRef: &metalv1alpha1.ClaimReference{Namespace: "default", Name: "fleet-a"},
			},
			Status: metalv1alpha1.HostStatus{
				Identity: &metalv1alpha1.HostIdentity{Serial: "SIMHW2288V5000391", Vendor: "Huawei"},
				ManagedBy: []metalv1alpha1.ManagementPath{
					{EndpointRef: metalv1alpha1.Reference{Name: "ci-huawei"}, SystemID: "1", Reachable: true},
					{EndpointRef: metalv1alpha1.Reference{Name: "ci-huawei2"}, SystemID: "1", Reachable: false},
				},
				Phase:     metalv1alpha1.HostAssigned,
				BlockedOn: "SerialMismatch",
			},
		},
	}
	eps := []metalv1alpha1.ManagementEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-dead"},
			Spec:       metalv1alpha1.ManagementEndpointSpec{Address: "https://127.0.0.1:1"},
			Status:     metalv1alpha1.ManagementEndpointStatus{Phase: metalv1alpha1.EndpointUnreachable},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-huawei"},
			Spec:       metalv1alpha1.ManagementEndpointSpec{Address: "https://127.0.0.1:8443"},
			Status:     metalv1alpha1.ManagementEndpointStatus{Vendor: "Huawei", Phase: metalv1alpha1.EndpointReady},
		},
	}
	claims := []metalv1alpha1.HostClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet-a"},
			Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "ci-huawei"}},
			Status:     metalv1alpha1.HostClaimStatus{Phase: metalv1alpha1.ClaimBound},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fleet-b"},
			Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "ci-huawei2"}},
			Status: metalv1alpha1.HostClaimStatus{
				Phase: metalv1alpha1.ClaimPending, BlockedOn: "HostAlreadyClaimed"},
		},
	}

	ops := []metalv1alpha1.HostOperation{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cycle"},
			Spec:       metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "simx12b000001"}},
			Status: metalv1alpha1.HostOperationStatus{
				Phase: metalv1alpha1.OperationAwaitingApproval, BlockedOn: "AwaitingApproval"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "done"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:  metalv1alpha1.Reference{Name: "simx12b000001"},
				Approval: metalv1alpha1.OperationApproval{ApprovedBy: "alice"},
			},
			Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "wedged"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:  metalv1alpha1.Reference{Name: "simhw2288v5000391"},
				Approval: metalv1alpha1.OperationApproval{ApprovedBy: "alice"},
			},
			Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationFailed},
		},
	}

	// Column content, not tabwriter's widths, is the contract — assert row shapes.
	out := renderStatus(hosts, eps, claims, ops)
	assert.Regexp(t, `(?m)^HOST\s+VENDOR\s+PHASE\s+CLAIM\s+PATHS\s+BLOCKED$`, out)
	assert.Regexp(t, `(?m)^simhw2288v5000391\s+Huawei\s+Assigned\s+default/fleet-a\s+1/2\s+SerialMismatch$`, out)
	assert.Regexp(t, `(?m)^simx12b000001\s+Supermicro\s+Available\s+-\s+1/1\s*$`, out)
	assert.Regexp(t, `(?m)^ci-dead\s+-\s+Unreachable\s+https://127\.0\.0\.1:1\s*$`, out)
	assert.Regexp(t, `(?m)^ci-huawei\s+Huawei\s+Ready\s+https://127\.0\.0\.1:8443\s*$`, out)
	assert.Regexp(t, `(?m)^default/fleet-b\s+ci-huawei2\s+Pending\s+HostAlreadyClaimed$`, out)
	assert.NotContains(t, out, "default/fleet-a  ci-huawei", "bound claims show on their host row only")
	assert.Regexp(t, `(?m)^OPERATION\s+HOST\s+PHASE\s+APPROVED\s+BLOCKED$`, out)
	assert.Regexp(t, `(?m)^default/cycle\s+simx12b000001\s+AwaitingApproval\s+-\s+AwaitingApproval$`, out)
	assert.Regexp(t, `(?m)^default/wedged\s+simhw2288v5000391\s+Failed\s+alice\s*$`, out,
		"failures stay visible until deleted")
	assert.NotContains(t, out, "default/done", "succeeded operations are history, not rack state")

	// No unbound claims → no claims section; no unfinished ops → no operations section.
	out = renderStatus(hosts, eps, claims[:1], ops[1:2])
	assert.NotContains(t, out, "\nCLAIM")
	assert.NotContains(t, out, "OPERATION")
}
