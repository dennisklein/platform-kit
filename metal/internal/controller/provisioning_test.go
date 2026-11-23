package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// claimReconcilerFor builds a claim reconciler over a fake API.
func claimReconcilerFor(t *testing.T, objs ...client.Object) *HostClaimReconciler {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}).
		WithObjects(objs...).Build()
	return &HostClaimReconciler{Client: cl, OperationsNamespace: "pk-metal", Proposer: "operator"}
}

func testClaim(node string) *metalv1alpha1.HostClaim {
	c := &metalv1alpha1.HostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "asset"}},
	}
	if node != "" {
		c.Spec.OS = &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: node}}
	}
	return c
}

// bootOp is a provisioning operation of the test claim that booted node onto
// host-a; executed says whether the executor ever took it up.
func bootOp(name, node string, succeeded, executed bool) *metalv1alpha1.HostOperation {
	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pk-metal",
			Labels: claimLabels(testClaim(""), purposeProvision)},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: "host-a"},
			ProposedBy: "operator",
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(true), ApprovedBy: "alice"},
			Actions: []metalv1alpha1.HostOperationAction{
				{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
				{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{NodeRef: metalv1alpha1.Reference{Name: node}}},
			},
		},
	}
	if executed {
		now := metav1.Now()
		op.Status.Baseline = &metalv1alpha1.OperationBaseline{PowerState: "On", CapturedAt: &now}
		op.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted,
			Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting, LastTransitionTime: now}}
	}
	if succeeded {
		op.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted,
			Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonStepsSucceeded, LastTransitionTime: metav1.Now()}}
	}
	return op
}

func TestHandOffBelongsToTheBootedNode(t *testing.T) {
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}

	for name, tc := range map[string]struct {
		booted string
		want   string
	}{
		"the claim's node was booted": {booted: "node-b", want: metalv1alpha1.ReasonHandedOff},
		"another node was booted":     {booted: "node-a", want: metalv1alpha1.ReasonAwaitingVerification},
	} {
		t.Run(name, func(t *testing.T) {
			r := claimReconcilerFor(t, host, node, bootOp("boot", tc.booted, true, true))
			claim := testClaim("node-b")
			require.NoError(t, r.provision(t.Context(), claim, "host-a"))
			c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
			require.NotNil(t, c)
			assert.Equal(t, tc.want, c.Reason, c.Message)
		})
	}
}

func TestReleasePowersOffOnlyWhatABootTouched(t *testing.T) {
	for name, tc := range map[string]struct {
		executed    bool
		wantRelease bool
	}{
		"a boot proposed, never taken up": {executed: false, wantRelease: false},
		"a boot that ran":                 {executed: true, wantRelease: true},
	} {
		t.Run(name, func(t *testing.T) {
			claim := testClaim("node-b")
			host := &metalv1alpha1.Host{
				ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
				Spec: metalv1alpha1.HostSpec{
					ClaimRef: &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
					OS:       &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "node-b"}},
				},
			}
			boot := bootOp("boot", "node-b", false, tc.executed)
			if !tc.executed {
				boot.Spec.Approval = metalv1alpha1.OperationApproval{Required: ptr.To(true)}
			}
			r := claimReconcilerFor(t, host, boot)

			done, err := r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
			require.NoError(t, err)
			release, err := r.claimOps(t.Context(), claim, purposeRelease)
			require.NoError(t, err)
			got := &metalv1alpha1.Host{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(host), got))
			if tc.wantRelease {
				assert.False(t, done, "released only once the machine is off")
				require.Len(t, release, 1)
				assert.NotNil(t, release[0].Spec.Actions[0].PowerOff)
				assert.NotNil(t, got.Spec.ClaimRef, "still bound until the power-off ran")
			} else {
				assert.True(t, done)
				assert.Empty(t, release, "nothing was booted, nothing is powered off")
				assert.Nil(t, got.Spec.ClaimRef, "unbound directly")
			}
			left, err := r.claimOps(t.Context(), claim, purposeProvision)
			require.NoError(t, err)
			assert.Empty(t, left, "the unfinished boot is withdrawn either way")
		})
	}
}

func TestBootSlotsFollowMediaTypes(t *testing.T) {
	ep := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep"},
		Status: metalv1alpha1.ManagementEndpointStatus{
			Capabilities: &metalv1alpha1.EndpointCapabilities{MediaDialects: map[string]string{
				"Floppy1": "insert-media", "USBStick1": "insert-media", "VirtualMedia2": "insert-media",
				"VirtualMedia10": "insert-media", "CD9": "none",
			}},
			// Collection order; lexical order would put the USB stick first.
			VirtualMedia: []metalv1alpha1.DiscoveredMedia{
				{ID: "USBStick1", MediaTypes: []string{"USBStick"}},
				{ID: "VirtualMedia2", MediaTypes: []string{"CD", "DVD"}},
				{ID: "CD9", MediaTypes: []string{"CD"}},
				{ID: "Floppy1", MediaTypes: []string{"Floppy"}},
				{ID: "VirtualMedia10", MediaTypes: []string{"DVD"}},
			},
		},
	}
	host := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Status: metalv1alpha1.HostStatus{ManagedBy: []metalv1alpha1.ManagementPath{
			{EndpointRef: metalv1alpha1.Reference{Name: "ep"}, SystemID: "1", Reachable: true},
		}},
	}
	r := claimReconcilerFor(t, ep)
	v, err := r.endpointShape(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, []string{"VirtualMedia2", "VirtualMedia10"}, v.bootSlots,
		"optical slots only, in the endpoint's order; a slot without an insert mechanism is none")
	assert.Equal(t, []string{"VirtualMedia2", "VirtualMedia10", "USBStick1", "Floppy1"}, v.insertable,
		"a release empties every insertable slot")
}

func TestBootOverrideAsksForUEFIOnlyWhereNeeded(t *testing.T) {
	r := &HostClaimReconciler{OperationsNamespace: "pk-metal"}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	config := &osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "node-config"}}
	installer := &osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "installer"}}
	for served, want := range map[string]string{"Legacy": "UEFI", "UEFI": "", "": ""} {
		plan := provisionPlan{Slots: []string{"VirtualMedia1", "VirtualMedia2"}, Target: "UsbCd", Mode: overrideMode(served)}
		op := r.provisionOp(testClaim("node"), "host-a", "op", plan, node, config, installer)
		var got *metalv1alpha1.SetBootOverrideAction
		for _, a := range op.Spec.Actions {
			if a.SetBootOverride != nil {
				got = a.SetBootOverride
			}
		}
		require.NotNil(t, got)
		assert.Equal(t, want, got.Mode, "system serves %q", served)
	}
}

func TestInstallerImageBelongsToOneCluster(t *testing.T) {
	cluster := func(name string) *osv1alpha1.TalosCluster {
		return &osv1alpha1.TalosCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
			Spec:       osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.0"},
		}
	}
	a, b := cluster("alpha"), cluster("beta")
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	r := claimReconcilerFor(t, a, b, node)

	fromA, err := r.ensureInstallerAsset(t.Context(), node, a)
	require.NoError(t, err)
	fromB, err := r.ensureInstallerAsset(t.Context(), node, b)
	require.NoError(t, err)
	assert.NotEqual(t, fromA.Name, fromB.Name, "the same image, one asset per cluster")
	assert.True(t, ownedBy(fromA, a) && !ownedBy(fromA, b))
	assert.True(t, ownedBy(fromB, b) && !ownedBy(fromB, a), "collecting one cluster never takes the other's image")

	again, err := r.ensureInstallerAsset(t.Context(), node, a)
	require.NoError(t, err)
	assert.Equal(t, fromA.Name, again.Name, "found again, not duplicated")
}

func TestDroppedIntentStopsARunningBoot(t *testing.T) {
	r := claimReconcilerFor(t, bootOp("boot", "node-b", false, true))
	require.NoError(t, r.provision(t.Context(), testClaim(""), "host-a"))
	ops := &metalv1alpha1.HostOperationList{}
	require.NoError(t, r.List(t.Context(), ops))
	assert.Empty(t, ops.Items, "the boot for the dropped intent is gone")
}
