package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// TestStepBudgetCountsFromTheFirstLook: an operation that sat behind other
// passes for longer than a power step's budget is not failed at its first
// look. It used to be: the budget counted from the previous step's end, so a
// power-on that one settle window did not show failed for good, although
// the operator had looked once.
func TestStepBudgetCountsFromTheFirstLook(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Status = resumedStatus(0)
	op.Status.OffObserved = nil
	// Taken up ten minutes ago; nothing looked at it since.
	op.Status.Baseline.CapturedAt = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	st.SetFault(sim.FaultStalePower, true)
	key := client.ObjectKeyFromObject(op)

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	require.False(t, got.Finished(), "one look is not a verdict: %+v", got.Status)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionForceOff}, rec.powers)

	// Watched for longer than the budget, it gives up.
	r.PowerTimeout = time.Millisecond
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), key, got))
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metav1.ConditionFalse, ex.Status)
	assert.Contains(t, ex.Message, "gave up")
}

// TestPassesOnOneBMCDoNotOverlap: a pass on another machine behind the same
// BMC holds the BMC; this pass comes back later and touches nothing.
func TestPassesOnOneBMCDoNotOverlap(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}})
	r, _, rec := executorFor(t, "huawei-2288h-v5", op)
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "bmc-1"}, SystemID: "1", Reachable: true}}
	require.NoError(t, r.Update(t.Context(), host))
	key := client.ObjectKeyFromObject(op)

	other := []string{"host/host-b", "endpoint/bmc-1"}
	require.True(t, r.locks.tryLock(other))
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "it comes back")
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	assert.Nil(t, got.Status.Baseline, "not started")
	assert.Empty(t, rec.powers)

	r.locks.unlock(other)
	require.True(t, opSucceeded(runOp(t, r)))
}

// TestPassesOnTwoHostsRunSideBySide drives two operations on two machines
// behind two BMCs at the same time, as the workers do. Under the race
// detector it checks what the passes share: the reconciler, its locks and
// clocks, the grant table, the API client.
func TestPassesOnTwoHostsRunSideBySide(t *testing.T) {
	a := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	a.Name = "op-a"
	b := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	b.Name, b.Spec.HostRef.Name = "op-b", "host-b"
	r, _, recA := executorFor(t, "huawei-2288h-v5", a, b, &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-b"}})
	f, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	recB := &recordingClient{Client: sim.NewState(f)}
	r.Connect = func(_ context.Context, host *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
		if host.Name == "host-b" {
			return recB, f.Systems[0].ID, "", "", nil
		}
		return recA, f.Systems[0].ID, "", "", nil
	}

	var wg sync.WaitGroup
	for _, name := range []string{"op-a", "op-b"} {
		wg.Go(func() {
			key := client.ObjectKey{Namespace: "pk-metal", Name: name}
			for range 10 {
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
				if !assert.NoError(t, err) {
					return
				}
				op := &metalv1alpha1.HostOperation{}
				if !assert.NoError(t, r.Get(t.Context(), key, op)) || op.Finished() {
					return
				}
			}
		})
	}
	wg.Wait()
	for _, name := range []string{"op-a", "op-b"} {
		assert.True(t, opSucceeded(getOp(t, r, name)), name)
	}
	assert.Equal(t, []bmc.PowerAction{bmc.ActionForceOff, bmc.ActionOn}, recA.powers)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionForceOff, bmc.ActionOn}, recB.powers)
}

// TestGuardedStartsAreOneAtATime: two control planes of the operator's own
// cluster, both allowed, are started by two passes at the same time. The
// guard's verdict and the baseline write are one step across hosts, so the
// second pass sees the first control plane going down and waits. Without
// that, both verdicts could be made before either baseline is written.
func TestGuardedStartsAreOneAtATime(t *testing.T) {
	for round := range 10 {
		objs := guardedObjects()
		var hosts []client.Object
		for _, h := range []struct{ host, node string }{{"host-b", "cp-2"}, {"host-c", "cp-3"}} {
			hosts = append(hosts, &metalv1alpha1.Host{
				ObjectMeta: metav1.ObjectMeta{Name: h.host,
					Annotations: map[string]string{metalv1alpha1.AnnotationAllowSelfDisruption: "true"}},
				Spec: metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: h.node}}},
			})
		}
		opB := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
		opB.Name, opB.Spec.HostRef.Name = "op-b", "host-b"
		opC := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
		opC.Name, opC.Spec.HostRef.Name = "op-c", "host-c"
		r, _, _ := executorFor(t, "huawei-2288h-v5", append(append(objs, hosts...), opB, opC)...)
		r.Self = &SelfGuard{Reader: r.Client, NodeName: "talos-1"}
		f, err := sim.LoadFixture("huawei-2288h-v5")
		require.NoError(t, err)
		sims := map[string]*sim.State{"host-b": sim.NewState(f), "host-c": sim.NewState(f)}
		for _, s := range sims {
			s.SetFault(sim.FaultStalePower, true) // the cycle waits: the first stays in flight
		}
		r.Connect = func(_ context.Context, host *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
			return &recordingClient{Client: sims[host.Name]}, f.Systems[0].ID, "", "", nil
		}

		var wg sync.WaitGroup
		for _, name := range []string{"op-b", "op-c"} {
			wg.Go(func() {
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "pk-metal", Name: name}})
				assert.NoError(t, err)
			})
		}
		wg.Wait()
		started := 0
		for _, name := range []string{"op-b", "op-c"} {
			if getOp(t, r, name).Status.Baseline != nil {
				started++
			}
		}
		require.Equal(t, 1, started, "round %d: one control plane at a time", round)
	}
}
