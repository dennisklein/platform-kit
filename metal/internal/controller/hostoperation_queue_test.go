package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// opNamed is a pre-cleared operation on host-a, created at the given time.
func opNamed(name string, created time.Time, actions ...metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	op := preClearedOp(actions...)
	op.Name = name
	op.CreationTimestamp = metav1.Time{Time: created}
	// The fake client leaves UIDs empty, and the queue tells operations
	// apart by UID.
	op.UID = types.UID("uid-" + name)
	return op
}

func getOp(t *testing.T, r *HostOperationReconciler, name string) *metalv1alpha1.HostOperation {
	t.Helper()
	op := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: "pk-metal", Name: name}, op))
	return op
}

func opCond(t *testing.T, r *HostOperationReconciler, name, typ string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(getOp(t, r, name).Status.Conditions, typ)
	require.NotNil(t, c, "%s has no %s condition", name, typ)
	return c
}

func reconcileOp(t *testing.T, r *HostOperationReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "pk-metal", Name: name}})
	require.NoError(t, err)
}

func TestStartedOperationIsNotPreemptedByAnOlderOneApprovedLater(t *testing.T) {
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	// older waits for a person; newer is pre-cleared and starts at once.
	older := opNamed("older", created,
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	older.Spec.Approval = metalv1alpha1.OperationApproval{Required: ptr.To(true)}
	newer := opNamed("newer", created.Add(time.Minute),
		metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: 300}})
	r, _, rec := executorFor(t, "huawei-2288h-v5", older, newer)
	// The OS ignores the power button: newer stays in its graceful window.
	rec.ignoreGraceful = true

	reconcileOp(t, r, "older")
	require.Equal(t, metalv1alpha1.ReasonAwaitingApproval, opCond(t, r, "older", metalv1alpha1.ConditionApproved).Reason)
	reconcileOp(t, r, "newer")
	require.NotNil(t, getOp(t, r, "newer").Status.Baseline, "newer started")
	require.Equal(t, []bmc.PowerAction{bmc.ActionGracefulShutdown}, rec.powers)

	// A person approves the older operation while newer is mid-run.
	o := getOp(t, r, "older")
	o.Spec.Approval.ApprovedBy = "alice"
	require.NoError(t, r.Update(t.Context(), o))

	reconcileOp(t, r, "older")
	ex := opCond(t, r, "older", metalv1alpha1.ConditionExecuted)
	assert.Equal(t, metalv1alpha1.ReasonOperationQueued, ex.Reason, "the started operation keeps the host")
	assert.Contains(t, ex.Message, "pk-metal/newer")
	assert.Nil(t, getOp(t, r, "older").Status.Baseline, "older never touched the machine")
	assert.Equal(t, []bmc.PowerAction{bmc.ActionGracefulShutdown}, rec.powers, "no power action from older")

	reconcileOp(t, r, "newer")
	assert.Equal(t, metalv1alpha1.ReasonExecuting, opCond(t, r, "newer", metalv1alpha1.ConditionExecuted).Reason,
		"newer is not queued mid-run")

	// The window runs out and newer forces the machine off; then older runs.
	n := getOp(t, r, "newer")
	n.Status.ShutdownRequested = &metav1.MicroTime{Time: time.Now().Add(-301 * time.Second)}
	require.NoError(t, r.Status().Update(t.Context(), n))
	for range 3 {
		reconcileOp(t, r, "newer")
	}
	require.Equal(t, metav1.ConditionTrue, opCond(t, r, "newer", metalv1alpha1.ConditionExecuted).Status)
	for range 5 {
		reconcileOp(t, r, "older")
	}
	assert.Equal(t, metav1.ConditionTrue, opCond(t, r, "older", metalv1alpha1.ConditionExecuted).Status)
	// older's cycle found the machine off, so it only turns it on.
	assert.Equal(t, []bmc.PowerAction{bmc.ActionGracefulShutdown, bmc.ActionForceOff, bmc.ActionOn},
		rec.powers, "newer's actions all come before older's")
}

func TestHostHolderOrder(t *testing.T) {
	t0 := time.Now().Truncate(time.Second)
	op := func(ns, name string, created time.Time, mut ...func(*metalv1alpha1.HostOperation)) metalv1alpha1.HostOperation {
		o := opNamed(name, created)
		o.Namespace = ns
		o.UID = types.UID(ns + "/" + name)
		for _, m := range mut {
			m(o)
		}
		return *o
	}
	started := func(o *metalv1alpha1.HostOperation) { o.Status = resumedStatus(0) }
	unapproved := func(o *metalv1alpha1.HostOperation) {
		o.Spec.Approval = metalv1alpha1.OperationApproval{Required: ptr.To(true)}
	}
	finished := func(o *metalv1alpha1.HostOperation) {
		o.Status = resumedStatus(0)
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{
			Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse, Reason: metalv1alpha1.ReasonStepFailed,
		})
	}
	deleting := func(o *metalv1alpha1.HostOperation) {
		o.DeletionTimestamp = &metav1.Time{Time: t0}
		o.Finalizers = []string{"test"}
	}
	type ops = []metalv1alpha1.HostOperation

	for name, tc := range map[string]struct {
		ops  ops
		want string
	}{
		"none":                 {want: ""},
		"oldest first":         {ops: ops{op("ns", "b", t0.Add(time.Second)), op("ns", "a", t0.Add(2*time.Second))}, want: "ns/b"},
		"same second, by ns":   {ops: ops{op("zz", "a", t0), op("aa", "z", t0)}, want: "aa/z"},
		"same second, by name": {ops: ops{op("ns", "b", t0), op("ns", "a", t0)}, want: "ns/a"},
		"started beats older":  {ops: ops{op("ns", "old", t0), op("ns", "new", t0.Add(time.Hour), started)}, want: "ns/new"},
		"oldest started wins":  {ops: ops{op("ns", "b", t0.Add(time.Hour), started), op("ns", "a", t0, started)}, want: "ns/a"},
		"unapproved skipped":   {ops: ops{op("ns", "old", t0, unapproved), op("ns", "new", t0.Add(time.Hour))}, want: "ns/new"},
		"finished skipped":     {ops: ops{op("ns", "old", t0, finished), op("ns", "new", t0.Add(time.Hour))}, want: "ns/new"},
		"deleting skipped":     {ops: ops{op("ns", "old", t0, started, deleting), op("ns", "new", t0.Add(time.Hour))}, want: "ns/new"},
		"only inactive":        {ops: ops{op("ns", "a", t0, unapproved), op("ns", "b", t0, finished)}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			// Every rotation of the input picks the same holder.
			for i := range max(1, len(tc.ops)) {
				in := append(slices.Clone(tc.ops[i:]), tc.ops[:i]...)
				got := ""
				if h := hostHolder(in); h != nil {
					got = h.Namespace + "/" + h.Name
				}
				assert.Equal(t, tc.want, got, "rotation %d", i)
			}
		})
	}
}

// staleOpCache serves operation lists as an informer cache that has not seen
// any baseline write yet; everything else, and the API reader, sees the store.
func staleOpCache(t *testing.T, r *HostOperationReconciler) {
	t.Helper()
	live := r.Client.(client.WithWatch)
	r.APIReader = live
	r.Client = interceptor.NewClient(live, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if ops, ok := list.(*metalv1alpha1.HostOperationList); ok {
				for i := range ops.Items {
					ops.Items[i].Status.Baseline = nil
				}
			}
			return nil
		},
	})
}

func TestStaleCacheDoesNotLetTwoOperationsStart(t *testing.T) {
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	older := opNamed("older", created, metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	// newer has started; the cache has not seen its baseline yet.
	newer := opNamed("newer", created.Add(time.Minute),
		metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: 300}})
	newer.Status = resumedStatus(0)
	r, _, rec := executorFor(t, "huawei-2288h-v5", older, newer)
	staleOpCache(t, r)

	reconcileOp(t, r, "older")
	assert.Nil(t, getOp(t, r, "older").Status.Baseline, "older does not start")
	assert.Empty(t, rec.powers)
	ex := opCond(t, r, "older", metalv1alpha1.ConditionExecuted)
	assert.Equal(t, metalv1alpha1.ReasonOperationQueued, ex.Reason)
	assert.Contains(t, ex.Message, "pk-metal/newer")
}
