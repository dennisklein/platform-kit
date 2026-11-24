package controller

import (
	"context"
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

// claimReconcilerFor builds a claim reconciler over a fake API that also
// holds slotlessEndpoint, the endpoint behind onPath.
func claimReconcilerFor(t *testing.T, objs ...client.Object) *HostClaimReconciler {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}).
		WithObjects(append(objs, slotlessEndpoint())...).Build()
	return &HostClaimReconciler{Client: cl, OperationsNamespace: "pk-metal", Proposer: "operator"}
}

// slotlessEndpoint is a reachable, inventoried endpoint whose machine has no
// slot that takes an image: a release reads its slots and ejects none.
func slotlessEndpoint() *metalv1alpha1.ManagementEndpoint {
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc"}}
	ep.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionInventoried, Status: metav1.ConditionTrue,
		Reason: "InventoryGathered", LastTransitionTime: metav1.Now()}}
	return ep
}

// onPath puts the host behind slotlessEndpoint.
func onPath(h *metalv1alpha1.Host) *metalv1alpha1.Host {
	h.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "bmc"}, SystemID: "1", Reachable: true}}
	return h
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
			if tc.want == metalv1alpha1.ReasonHandedOff {
				require.NotNil(t, claim.Status.HandOff, "the operation's success becomes the claim's record")
				assert.Equal(t, "host-a", claim.Status.HandOff.Host)
				assert.Equal(t, "node-b", claim.Status.HandOff.Node)
			} else {
				assert.Nil(t, claim.Status.HandOff)
			}
		})
	}
}

func TestHandOffRecordOutlivesItsOperations(t *testing.T) {
	// The install finished long ago, its operations were tidied away, and the
	// node is down now (no check-in). Nothing is proposed: the record on the
	// claim is what says the machine is installed.
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}
	r := claimReconcilerFor(t, host, node)
	claim := testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", At: metav1.Now()}

	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonHandedOff, c.Reason)
	assert.Contains(t, c.Message, "does not start a new install")
	ops, err := r.claimOps(t.Context(), claim, purposeProvision)
	require.NoError(t, err)
	assert.Empty(t, ops, "no reinstall is proposed")

	// The record of another node is no record of this one.
	claim = testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-a", At: metav1.Now()}
	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	c = meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.NotEqual(t, metalv1alpha1.ReasonHandedOff, c.Reason)
}

func TestCheckInRecordsTheHandOff(t *testing.T) {
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
		Status: osv1alpha1.TalosNodeStatus{ConfigDigest: "sha256:abc", Conditions: []metav1.Condition{{
			Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady,
		}}},
	}
	r := claimReconcilerFor(t, host, node)
	claim := testClaim("node-b")
	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	require.NotNil(t, claim.Status.HandOff)
	assert.Equal(t, "sha256:abc", claim.Status.HandOff.ConfigDigest)
	assert.Empty(t, claim.Status.HandOff.Operation, "found running: no boot of ours touched it")
	at := claim.Status.HandOff.At

	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	assert.Equal(t, at, claim.Status.HandOff.At, "written once, not on every pass")

	// With a boot of ours running into the check-in, the record names it.
	r = claimReconcilerFor(t, host, node, bootOp("boot", "node-b", false, true))
	claim = testClaim("node-b")
	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	require.NotNil(t, claim.Status.HandOff)
	assert.Equal(t, "pk-metal/boot", claim.Status.HandOff.Operation)
}

func TestReleasePowersOffOnlyWhatABootTouched(t *testing.T) {
	for name, tc := range map[string]struct {
		executed    bool
		handedOff   string // the record's operation; "-" for no record
		wantRelease bool
	}{
		"a boot proposed, never taken up": {executed: false, handedOff: "-", wantRelease: false},
		"a boot that ran":                 {executed: true, handedOff: "-", wantRelease: true},
		// The boot's operation was deleted since; the claim's record remains.
		"an install recorded on the claim": {executed: false, handedOff: "pk-metal/boot", wantRelease: true},
		// The machine already ran the node when the claim found it: it is
		// still a node of the cluster, and the release takes it out.
		"a node found running": {executed: false, handedOff: "", wantRelease: true},
	} {
		t.Run(name, func(t *testing.T) {
			claim := testClaim("node-b")
			if tc.handedOff != "-" {
				claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: tc.handedOff, At: metav1.Now()}
			}
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
			r := claimReconcilerFor(t, onPath(host), boot)

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

func TestReleaseOfARunningNodeWaitsForAPerson(t *testing.T) {
	osReady := []metav1.Condition{{Type: metalv1alpha1.ConditionOSReady, Status: metav1.ConditionTrue,
		Reason: osv1alpha1.ReasonNodeReady, LastTransitionTime: metav1.Now()}}
	for name, tc := range map[string]struct {
		running   bool
		automatic bool
		required  bool
	}{
		"a running node":                  {running: true, required: true},
		"a running node, approval waived": {running: true, automatic: true, required: false},
		"a boot that never finished":      {running: false, required: false},
	} {
		t.Run(name, func(t *testing.T) {
			claim := testClaim("node-b")
			if tc.automatic {
				claim.Spec.Provisioning = &metalv1alpha1.ClaimProvisioning{Approval: metalv1alpha1.ProvisioningApprovalAutomatic}
			}
			// A node that came up is on record; a boot that never finished
			// (it ran, it did not check in) is not.
			var objs []client.Object
			if tc.running {
				claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
			} else {
				objs = append(objs, bootOp("boot", "node-b", false, true))
			}
			host := &metalv1alpha1.Host{
				ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
				Spec: metalv1alpha1.HostSpec{
					ClaimRef: &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
				},
			}
			if tc.running {
				host.Status.Conditions = osReady
			}
			r := claimReconcilerFor(t, append(objs, onPath(host))...)

			done, err := r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
			require.NoError(t, err)
			assert.False(t, done)
			release, err := r.claimOps(t.Context(), claim, purposeRelease)
			require.NoError(t, err)
			require.Len(t, release, 1)
			op := release[0]
			assert.Equal(t, tc.required, *op.Spec.Approval.Required)
			require.NotNil(t, op.Spec.Actions[0].PowerOff)
			assert.EqualValues(t, 300, op.Spec.Actions[0].PowerOff.GracefulTimeoutSeconds,
				"the OS gets the default five minutes to shut down")

			c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
			require.NotNil(t, c)
			if tc.required {
				assert.Contains(t, c.Message, "pk-metal approve pk-metal/"+op.Name, "the message says how to go ahead")
			} else {
				assert.Contains(t, c.Message, "powering off host host-a")
			}
		})
	}
}

func TestHoldWithdrawsAndStopsProposals(t *testing.T) {
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}
	waiting := bootOp("boot", "node-b", false, false)
	waiting.Spec.Approval = metalv1alpha1.OperationApproval{Required: ptr.To(true)}
	r := claimReconcilerFor(t, host, node, waiting)
	claim := testClaim("node-b")
	claim.Annotations = map[string]string{metalv1alpha1.AnnotationHold: "change freeze until Monday"}

	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonHeldByUser, c.Reason)
	assert.Contains(t, c.Message, "change freeze until Monday", "the reason is shown")
	assert.Contains(t, c.Message, "kubectl annotate hostclaim -n default claim metal.pk.io/hold-", "and how to lift it")
	ops, err := r.claimOps(t.Context(), claim, purposeProvision)
	require.NoError(t, err)
	assert.Empty(t, ops, "the proposal waiting for approval is withdrawn, and none is made")
}

func TestHoldKeepsADeletedClaimsMachine(t *testing.T) {
	claim := testClaim("node-b")
	claim.Annotations = map[string]string{metalv1alpha1.AnnotationHold: ""}
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	host := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec: metalv1alpha1.HostSpec{
			ClaimRef: &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
		},
	}
	r := claimReconcilerFor(t, onPath(host))

	done, err := r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
	require.NoError(t, err)
	assert.False(t, done, "held: the machine is not handed back")
	release, err := r.claimOps(t.Context(), claim, purposeRelease)
	require.NoError(t, err)
	assert.Empty(t, release, "no release is proposed while held")
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonHeldByUser, c.Reason)

	// Lifted, the release is proposed.
	claim.Annotations = nil
	done, err = r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
	require.NoError(t, err)
	assert.False(t, done)
	release, err = r.claimOps(t.Context(), claim, purposeRelease)
	require.NoError(t, err)
	assert.Len(t, release, 1)
}

// A hold that takes back a release proposal does not lose the record that a
// boot ran: the boot operations went when the release was made, and the
// power-off decision on the host is what the passes after the hold read.
func TestHoldKeepsTheLastRecordOfABoot(t *testing.T) {
	claim := testClaim("node-b")
	host := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec: metalv1alpha1.HostSpec{
			ClaimRef: &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
		},
	}
	// The node checked in after the claim was deleted: it runs, so its
	// release waits for a person, and no hand-off was recorded.
	host.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionOSReady,
		Status: metav1.ConditionTrue, Reason: "NodeReady", LastTransitionTime: metav1.Now()}}
	r := claimReconcilerFor(t, host, bootOp("boot", "node-b", false, true))

	// The first pass makes the release and stops the boot.
	done, err := r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
	require.NoError(t, err)
	require.False(t, done)
	boots, err := r.claimOps(t.Context(), claim, purposeProvision)
	require.NoError(t, err)
	require.Empty(t, boots, "precondition: the boot record is gone")

	// A person holds the claim; two passes later the machine is still bound.
	claim.Annotations = map[string]string{metalv1alpha1.AnnotationHold: ""}
	got := &metalv1alpha1.Host{}
	for range 2 {
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(host), got))
		done, err = r.release(t.Context(), claim, []metalv1alpha1.Host{*got})
		require.NoError(t, err)
		assert.False(t, done, "held: the machine is not handed back")
	}
	// The hold lifted, the release still powers the machine off first.
	claim.Annotations = nil
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(host), got))
	done, err = r.release(t.Context(), claim, []metalv1alpha1.Host{*got})
	require.NoError(t, err)
	assert.False(t, done, "the machine goes back only after its power-off")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(host), got))
	assert.NotNil(t, got.Spec.ClaimRef, "still bound, waiting for the power-off")
	assert.True(t, powerOffDecided(got, claim), "the power-off decision outlives the withdrawn proposal")
}

func TestMaintenanceStopsProposals(t *testing.T) {
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a",
		Annotations: map[string]string{metalv1alpha1.AnnotationMaintenance: "replacing DIMM B2"}}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}
	waiting := bootOp("boot", "node-b", false, false)
	waiting.Spec.Approval = metalv1alpha1.OperationApproval{Required: ptr.To(true)}
	r := claimReconcilerFor(t, host, node, waiting)
	claim := testClaim("node-b")

	require.NoError(t, r.provision(t.Context(), claim, "host-a"))
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonHostInMaintenance, c.Reason)
	assert.Contains(t, c.Message, "replacing DIMM B2")
	assert.Contains(t, c.Message, "kubectl annotate host host-a metal.pk.io/maintenance-")
	ops, err := r.claimOps(t.Context(), claim, purposeProvision)
	require.NoError(t, err)
	assert.Empty(t, ops, "the waiting proposal is withdrawn, and none is made")

	// A deleted claim's machine is not released while in maintenance.
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	host.Spec.ClaimRef = &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID}
	done, err := r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
	require.NoError(t, err)
	assert.False(t, done)
	release, err := r.claimOps(t.Context(), claim, purposeRelease)
	require.NoError(t, err)
	assert.Empty(t, release)
}

func TestMessagesSayHowToApproveRefuseAndRetry(t *testing.T) {
	claim := testClaim("node-b")
	op := bootOp("provision-host-a-12345678", "node-b", false, false)
	msg := awaitsApprovalMessage(claim, op, "boot", "install")
	assert.Contains(t, msg, "pk-metal approve pk-metal/provision-host-a-12345678")
	assert.Contains(t, msg, "pk-metal hold default/claim")
	assert.Contains(t, awaitingApprovalMessage(op), "pk-metal hold default/claim", "the operation itself says it too")
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

func TestConfigMediumTakesAnyOtherSlot(t *testing.T) {
	cases := []struct {
		name         string
		view         endpointView
		boot, config string
	}{
		{"optical and USB stick", endpointView{insertable: []string{"2", "1"}, bootSlots: []string{"2"}}, "2", "1"},
		{"two optical", endpointView{insertable: []string{"CD1", "CD2"}, bootSlots: []string{"CD1", "CD2"}}, "CD1", "CD2"},
		{"one optical only", endpointView{insertable: []string{"CD"}, bootSlots: []string{"CD"}}, "", ""},
		{"USB sticks only", endpointView{insertable: []string{"U1", "U2"}}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			boot, config := c.view.mediaSlots()
			assert.Equal(t, c.boot, boot)
			assert.Equal(t, c.config, config)
		})
	}
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

// A release decides from the API server, not from a cache that lags behind:
// a boot that has just captured its baseline is seen as started, and a host
// bound a moment ago is seen as bound. The second used to read the cache, and
// the finalizer went while the host still carried the claim.
func TestReleaseReadsPastAStaleCache(t *testing.T) {
	// stale wires r like the manager does: writes and the API reader go to
	// the API server (the reconciler's own fake); lists through the client
	// answer from a cache that shows the objects given here.
	stale := func(r *HostClaimReconciler, cached ...client.Object) {
		r.APIReader = r.Client
		r.Client = &staleLists{Client: r.Client, cache: claimReconcilerFor(t, cached...).Client}
	}
	deleting := func() *metalv1alpha1.HostClaim {
		c := testClaim("node-b")
		c.Finalizers = []string{releaseFinalizer}
		c.DeletionTimestamp = ptr.To(metav1.Now())
		return c
	}
	bound := func(c *metalv1alpha1.HostClaim) *metalv1alpha1.Host {
		return &metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
			Spec: metalv1alpha1.HostSpec{
				ClaimRef: &metalv1alpha1.ClaimReference{Namespace: c.Namespace, Name: c.Name, UID: c.UID}},
		}
	}

	t.Run("a boot that started is seen", func(t *testing.T) {
		claim := deleting()
		host := bound(claim)
		r := claimReconcilerFor(t, claim, host, bootOp("boot", "node-b", false, true))
		stale(r, claim, host, bootOp("boot", "node-b", false, false))

		_, err := r.finishRelease(t.Context(), claim)
		require.NoError(t, err)
		got := &metalv1alpha1.Host{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(host), got))
		assert.NotNil(t, got.Spec.ClaimRef, "still bound until the power-off ran")
		assert.True(t, powerOffDecided(got, claim), "the started boot is powered off before the machine goes back")
	})

	t.Run("a host just bound is seen", func(t *testing.T) {
		claim := deleting()
		host := bound(claim)
		r := claimReconcilerFor(t, claim, host)
		stale(r, claim, &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}})

		_, err := r.finishRelease(t.Context(), claim)
		require.NoError(t, err)
		got := &metalv1alpha1.Host{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(host), got))
		assert.Nil(t, got.Spec.ClaimRef, "the host is handed back before the claim goes")
	})
}

// staleLists answers lists from a cache that lags behind the API server;
// everything else goes to the API server.
type staleLists struct {
	client.Client
	cache client.Client
}

func (c *staleLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.cache.List(ctx, list, opts...)
}

func TestDroppedIntentStopsARunningBoot(t *testing.T) {
	r := claimReconcilerFor(t, bootOp("boot", "node-b", false, true))
	require.NoError(t, r.provision(t.Context(), testClaim(""), "host-a"))
	ops := &metalv1alpha1.HostOperationList{}
	require.NoError(t, r.List(t.Context(), ops))
	assert.Empty(t, ops.Items, "the boot for the dropped intent is gone")
}

// The boot medium carries the operator's config argument exactly once, for
// every combination of node and cluster schematic, and building it never
// changes the schematics a person wrote.
func TestBootSchematic(t *testing.T) {
	schematics := map[string]*osv1alpha1.Schematic{
		"none":  nil,
		"empty": {},
		"console": {Customization: osv1alpha1.SchematicCustomization{
			ExtraKernelArgs: []string{"console=ttyS0"}}},
		"already carries it": {Customization: osv1alpha1.SchematicCustomization{
			ExtraKernelArgs: []string{bootConfigKernelArg, "console=ttyS0"}}},
		"extensions": {Customization: osv1alpha1.SchematicCustomization{
			SystemExtensions: osv1alpha1.SystemExtensions{OfficialExtensions: []string{"siderolabs/iscsi-tools"}}}},
	}
	for cn, cs := range schematics {
		for nn, ns := range schematics {
			t.Run("cluster "+cn+", node "+nn, func(t *testing.T) {
				cluster := &osv1alpha1.TalosCluster{Spec: osv1alpha1.TalosClusterSpec{Schematic: cs.DeepCopy()}}
				node := &osv1alpha1.TalosNode{Spec: osv1alpha1.TalosNodeSpec{Schematic: ns.DeepCopy()}}
				before := []any{cluster.DeepCopy(), node.DeepCopy()}

				got := bootSchematic(node, cluster)
				n := 0
				for _, a := range got.Customization.ExtraKernelArgs {
					if a == bootConfigKernelArg {
						n++
					}
				}
				assert.Equal(t, 1, n, "args %v", got.Customization.ExtraKernelArgs)
				assert.Equal(t, before, []any{cluster, node}, "the written schematics stay as they are")

				eff := ns
				if eff == nil {
					eff = cs
				}
				if eff != nil {
					assert.Equal(t, eff.Customization.SystemExtensions, got.Customization.SystemExtensions)
					for _, a := range eff.Customization.ExtraKernelArgs {
						assert.Contains(t, got.Customization.ExtraKernelArgs, a, "the person's arguments stay")
					}
				}
			})
		}
	}
}
