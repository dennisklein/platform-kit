package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// boundHost is host-a bound to the test claim, behind slotlessEndpoint.
func boundHost(claim *metalv1alpha1.HostClaim) *metalv1alpha1.Host {
	return onPath(&metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec: metalv1alpha1.HostSpec{
			ClaimRef: &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID},
			OS:       &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "node-b"}},
		},
	})
}

// releasePass runs one release pass with the host as the API holds it now.
func releasePass(t *testing.T, r *HostClaimReconciler, claim *metalv1alpha1.HostClaim) bool {
	t.Helper()
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	var bound []metalv1alpha1.Host
	if host.Spec.ClaimRef != nil {
		bound = append(bound, *host)
	}
	done, err := r.release(t.Context(), claim, bound)
	require.NoError(t, err)
	return done
}

func hostBound(t *testing.T, r *HostClaimReconciler) bool {
	t.Helper()
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	return host.Spec.ClaimRef != nil
}

func releaseOps(t *testing.T, r *HostClaimReconciler, claim *metalv1alpha1.HostClaim) []metalv1alpha1.HostOperation {
	t.Helper()
	ops, err := r.claimOps(t.Context(), claim, purposeRelease)
	require.NoError(t, err)
	return ops
}

func failOp(t *testing.T, r *HostClaimReconciler, op *metalv1alpha1.HostOperation, msg string) {
	t.Helper()
	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
		Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
		Reason: metalv1alpha1.ReasonStepFailed, Message: msg,
	})
	require.NoError(t, r.Status().Update(t.Context(), op))
}

func succeedOp(t *testing.T, r *HostClaimReconciler, op *metalv1alpha1.HostOperation) {
	t.Helper()
	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
		Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionTrue,
		Reason: metalv1alpha1.ReasonStepsSucceeded, Message: "done",
	})
	require.NoError(t, r.Status().Update(t.Context(), op))
}

func provisionedCond(t *testing.T, claim *metalv1alpha1.HostClaim) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	return c
}

func TestFailedReleaseKeepsTheMachineBound(t *testing.T) {
	claim := testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	r := claimReconcilerFor(t, boundHost(claim))

	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	assert.Equal(t, metalv1alpha1.ReasonReleasing, provisionedCond(t, claim).Reason)

	failOp(t, r, &rel[0], "step 1/1 powerOff: BMC refused the request")
	for range 2 {
		require.False(t, releasePass(t, r, claim), "a failed release never completes on its own")
		c := provisionedCond(t, claim)
		assert.Equal(t, metalv1alpha1.ReasonReleaseFailed, c.Reason)
		assert.Contains(t, c.Message, "BMC refused the request", "the message says why")
		assert.Contains(t, c.Message, "pk-metal retry pk-metal/"+rel[0].Name, "and how to try again")
		assert.True(t, hostBound(t, r), "the machine stays bound")
		assert.Len(t, releaseOps(t, r, claim), 1, "nothing new is proposed while the failure stands")
	}

	// A retry deletes the failed operation; the hand-off record makes the
	// next pass propose the power-off again.
	require.NoError(t, r.Delete(t.Context(), &rel[0]))
	require.False(t, releasePass(t, r, claim))
	rel = releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	assert.True(t, hostBound(t, r))

	// It succeeds: only now is the machine handed back.
	succeedOp(t, r, &rel[0])
	require.True(t, releasePass(t, r, claim))
	assert.False(t, hostBound(t, r))
}

func TestHoldWithdrawsAPendingReleaseProposal(t *testing.T) {
	claim := testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	host := boundHost(claim)
	// A running node: the release waits for a person.
	host.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionOSReady, Status: metav1.ConditionTrue,
		Reason: "NodeReady", LastTransitionTime: metav1.Now()}}
	r := claimReconcilerFor(t, host)

	require.False(t, releasePass(t, r, claim))
	require.Len(t, releaseOps(t, r, claim), 1, "the release is proposed")

	claim.Annotations = map[string]string{metalv1alpha1.AnnotationHold: "keep it running for the demo"}
	require.False(t, releasePass(t, r, claim))
	assert.Empty(t, releaseOps(t, r, claim), "a hold takes the proposal back")
	c := provisionedCond(t, claim)
	assert.Equal(t, metalv1alpha1.ReasonHeldByUser, c.Reason)
	assert.Contains(t, c.Message, "keep it running for the demo")
	assert.True(t, hostBound(t, r))

	claim.Annotations = nil
	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1, "lifting the hold proposes it again")
	assert.True(t, *rel[0].Spec.Approval.Required)
}

func TestHoldDoesNotStopAnApprovedRelease(t *testing.T) {
	claim := testClaim("node-b")
	// The boot ran and never checked in: no hand-off, no check-in on record.
	r := claimReconcilerFor(t, boundHost(claim), bootOp("boot", "node-b", false, true))

	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	require.False(t, *rel[0].Spec.Approval.Required, "pre-cleared: the node never came up")

	claim.Annotations = map[string]string{metalv1alpha1.AnnotationHold: ""}
	require.False(t, releasePass(t, r, claim))
	assert.Len(t, releaseOps(t, r, claim), 1, "one already cleared to run finishes")
}

func TestReleaseSkipsAHostOfAnotherClaimWithTheSameName(t *testing.T) {
	claim := testClaim("node-b")
	// A claim of the same namespace and name, deleted and created again, holds
	// the host now.
	host := boundHost(claim)
	host.Spec.ClaimRef.UID = "new-claim-uid"
	r := claimReconcilerFor(t, host, bootOp("boot", "node-b", false, true))

	require.True(t, releasePass(t, r, claim), "nothing of this claim's is bound")
	assert.Empty(t, releaseOps(t, r, claim), "the other claim's machine is not powered off")
	got := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, got))
	require.NotNil(t, got.Spec.ClaimRef)
	assert.EqualValues(t, "new-claim-uid", got.Spec.ClaimRef.UID, "and stays bound to it")
}

func TestReleaseApprovalIsDecidedOnceAtCreation(t *testing.T) {
	claim := testClaim("node-b")
	r := claimReconcilerFor(t, boundHost(claim), bootOp("boot", "node-b", false, true))

	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	require.False(t, *rel[0].Spec.Approval.Required, "the boot never finished: pre-cleared")

	// The node comes up before the release ran. The release is not re-gated:
	// deleting the claim was the decision, and approval.required is frozen.
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionOSReady, Status: metav1.ConditionTrue,
		Reason: "NodeReady", LastTransitionTime: metav1.Now()}}
	require.NoError(t, r.Update(t.Context(), host))

	require.False(t, releasePass(t, r, claim))
	rel = releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	assert.False(t, *rel[0].Spec.Approval.Required)
	assert.Contains(t, provisionedCond(t, claim).Message, "powering off host host-a")
}

func TestReleaseEjectsEveryInsertableSlot(t *testing.T) {
	claim := testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	host := boundHost(claim)
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "ep"}, SystemID: "1", Reachable: true}}
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep"}}
	ep.Status.VirtualMedia = []metalv1alpha1.DiscoveredMedia{
		{ID: "CD", MediaTypes: []string{"CD", "DVD"}},
		{ID: "USB", MediaTypes: []string{"USBStick"}},
		{ID: "Floppy", MediaTypes: []string{"Floppy"}},
	}
	ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{MediaDialects: map[string]string{
		"CD": string(bmc.DialectStandard), "USB": string(bmc.DialectVmmControl), "Floppy": string(bmc.DialectNone),
	}}
	ep.Status.Conditions = slotlessEndpoint().Status.Conditions
	r := claimReconcilerFor(t, host, ep)

	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	var ejects []string
	for _, a := range rel[0].Spec.Actions[1:] {
		require.NotNil(t, a.EjectMedia)
		ejects = append(ejects, a.EjectMedia.MediaID)
	}
	require.NotNil(t, rel[0].Spec.Actions[0].PowerOff, "off first, then the slots")
	assert.Equal(t, []string{"CD", "USB"}, ejects, "every slot a boot could have used")
}

func TestClaimDeletedMidBootProposesTheReleaseBeforeStoppingTheBoot(t *testing.T) {
	boot := []metalv1alpha1.HostOperationAction{
		{InsertMedia: &metalv1alpha1.InsertMediaAction{MediaID: "CD", ImageURL: "https://203.0.113.5/installer.iso"}},
		{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd"}},
		{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{NodeRef: metalv1alpha1.Reference{Name: "node-b"}}},
	}
	for done := range len(boot) {
		t.Run(actionName(boot[done]), func(t *testing.T) {
			claim := testClaim("node-b")
			op := bootOp("boot", "node-b", false, true)
			op.Spec.Actions = boot
			// The steps before this one are recorded; this one is in flight.
			for _, a := range boot[:done] {
				op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{Action: actionName(a), Phase: "Succeeded"})
			}
			var writes []string
			cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}).
				WithObjects(slotlessEndpoint(), boundHost(claim), op).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						writes = append(writes, "create "+obj.GetName())
						return c.Create(ctx, obj, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						writes = append(writes, "delete "+obj.GetName())
						return c.Delete(ctx, obj, opts...)
					},
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						writes = append(writes, "update "+obj.GetName())
						return c.Update(ctx, obj, opts...)
					},
				}).Build()
			r := &HostClaimReconciler{Client: cl, OperationsNamespace: "pk-metal", Proposer: "operator"}

			require.False(t, releasePass(t, r, claim))
			rel := releaseOps(t, r, claim)
			require.Len(t, rel, 1)
			assert.Equal(t, []string{"update host-a", "create " + rel[0].Name, "delete boot"}, writes,
				"the decision is recorded and the release exists before the running boot is stopped")
			host := &metalv1alpha1.Host{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
			assert.NotNil(t, host.Spec.ClaimRef, "the host is not unbound")
			assert.Equal(t, string(claim.UID), host.Annotations[annotationReleasePowerOff])
			assert.False(t, *rel[0].Spec.Approval.Required, "a machine mid-boot runs no node: pre-cleared")
		})
	}
}
func TestFailedReleaseOfAMidBootMachineIsRetriedNotDropped(t *testing.T) {
	claim := testClaim("node-b")
	// The claim is deleted while its boot runs: no hand-off yet.
	r := claimReconcilerFor(t, boundHost(claim), bootOp("boot", "node-b", false, true))

	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1, "the boot ran: the machine is powered off first")

	// The power-off fails.
	failOp(t, r, &rel[0], "step 1/1 powerOff: BMC refused the request")
	require.False(t, releasePass(t, r, claim))
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonReleaseFailed, c.Reason)
	assert.Contains(t, c.Message, "BMC refused the request")
	assert.Contains(t, c.Message, "pk-metal retry pk-metal/"+rel[0].Name)
	assert.True(t, hostBound(t, r), "a failed release keeps the machine bound")

	// pk-metal retry deletes the failed operation (its TTL does the same a
	// day later). The next pass must propose the power-off again, not hand
	// back a machine that is still mid-boot.
	require.NoError(t, r.Delete(t.Context(), &rel[0]))
	require.False(t, releasePass(t, r, claim), "the release is not done")
	assert.True(t, hostBound(t, r), "still bound")
	rel = releaseOps(t, r, claim)
	require.Len(t, rel, 1, "the release is proposed again")

	succeedOp(t, r, &rel[0])
	require.True(t, releasePass(t, r, claim))
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	assert.Nil(t, host.Spec.ClaimRef, "handed back once it is off")
	assert.NotContains(t, host.Annotations, annotationReleasePowerOff, "the record goes with the bind")
}

func TestStaleCacheDoesNotHideABootThatRan(t *testing.T) {
	claim := testClaim("node-b")
	// The API server holds the boot's baseline; the informer cache has not
	// seen that write yet, and lists the operation as never taken up.
	live := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}).
		WithObjects(slotlessEndpoint(), boundHost(claim), bootOp("boot", "node-b", false, true)).Build()
	cached := interceptor.NewClient(live, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if ops, ok := list.(*metalv1alpha1.HostOperationList); ok {
				for i := range ops.Items {
					ops.Items[i].Status = metalv1alpha1.HostOperationStatus{}
				}
			}
			return nil
		},
	})
	r := &HostClaimReconciler{Client: cached, APIReader: live, OperationsNamespace: "pk-metal", Proposer: "operator"}

	require.False(t, releasePass(t, r, claim), "the boot ran: the machine is powered off first")
	assert.True(t, hostBound(t, r), "not unbound mid-boot")
	assert.Len(t, releaseOps(t, r, claim), 1)
}

func TestReleaseFencesABootThatStartsWhileItDecides(t *testing.T) {
	claim := testClaim("node-b")
	boot := bootOp("boot", "node-b", false, false)
	live := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}).
		WithObjects(slotlessEndpoint(), boundHost(claim), boot).Build()
	// The executor captures the boot's baseline right after the release's
	// uncached read saw none.
	started := false
	reader := interceptor.NewClient(live, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*metalv1alpha1.HostOperationList); ok && !started {
				started = true
				op := &metalv1alpha1.HostOperation{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(boot), op))
				now := metav1.Now()
				op.Status.Baseline = &metalv1alpha1.OperationBaseline{PowerState: "On", CapturedAt: &now}
				require.NoError(t, c.Status().Update(ctx, op))
			}
			return nil
		},
	})
	r := &HostClaimReconciler{Client: live, APIReader: reader, OperationsNamespace: "pk-metal", Proposer: "operator"}

	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	_, err := r.release(t.Context(), claim, []metalv1alpha1.Host{*host})
	require.Error(t, err, "the boot changed under the decision: the pass is retried")
	assert.True(t, hostBound(t, r), "not unbound while the boot runs")

	// The next pass sees the boot that ran and powers the machine off.
	require.False(t, releasePass(t, r, claim))
	assert.True(t, hostBound(t, r))
	assert.Len(t, releaseOps(t, r, claim), 1)
}

func TestReleaseDeletesAnUnstartedBootBeforeItUnbinds(t *testing.T) {
	claim := testClaim("node-b")
	var writes []string
	live := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}).
		WithObjects(slotlessEndpoint(), boundHost(claim), bootOp("boot", "node-b", false, false)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				writes = append(writes, "delete "+obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				writes = append(writes, "update "+obj.GetName())
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &HostClaimReconciler{Client: live, OperationsNamespace: "pk-metal", Proposer: "operator"}

	require.True(t, releasePass(t, r, claim))
	require.NotEmpty(t, writes)
	assert.Equal(t, []string{"delete boot", "update host-a"}, writes[:2],
		"the boot can no longer start once the host is handed back")
	assert.False(t, hostBound(t, r))
}

func TestReleaseWaitsForTheMediaSlotsBeforeItIsProposed(t *testing.T) {
	claim := testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	host := boundHost(claim)
	// The only path to the machine is down: its slots cannot be read.
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "ep"}, SystemID: "1", Reachable: false}}
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep"}}
	ep.Status.VirtualMedia = []metalv1alpha1.DiscoveredMedia{{ID: "CD", MediaTypes: []string{"CD"}}}
	ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{MediaDialects: map[string]string{"CD": string(bmc.DialectStandard)}}
	ep.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionInventoried, Status: metav1.ConditionTrue,
		Reason: "InventoryGathered", LastTransitionTime: metav1.Now()}}
	r := claimReconcilerFor(t, host, ep)

	require.False(t, releasePass(t, r, claim))
	assert.Empty(t, releaseOps(t, r, claim), "no release without its eject list")
	c := provisionedCond(t, claim)
	assert.Equal(t, metalv1alpha1.ReasonReleasing, c.Reason)
	assert.Contains(t, c.Message, "media slots")
	assert.True(t, hostBound(t, r))

	// The path comes back: the release is proposed with every slot.
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Status.ManagedBy[0].Reachable = true
	require.NoError(t, r.Update(t.Context(), host))
	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	require.Len(t, rel[0].Spec.Actions, 2)
	require.NotNil(t, rel[0].Spec.Actions[1].EjectMedia)
	assert.Equal(t, "CD", rel[0].Spec.Actions[1].EjectMedia.MediaID)
}

func TestReleaseOfAMachineWithoutSlotsIsProposedAtOnce(t *testing.T) {
	claim := testClaim("node-b")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-b", Operation: "pk-metal/boot", At: metav1.Now()}
	host := boundHost(claim)
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "ep"}, SystemID: "1", Reachable: true}}
	// Inventoried, and no slot can take an image.
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep"}}
	ep.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionInventoried, Status: metav1.ConditionTrue,
		Reason: "InventoryGathered", LastTransitionTime: metav1.Now()}}
	r := claimReconcilerFor(t, host, ep)

	require.False(t, releasePass(t, r, claim))
	rel := releaseOps(t, r, claim)
	require.Len(t, rel, 1)
	assert.Len(t, rel[0].Spec.Actions, 1, "power-off only: nothing to eject")
}
