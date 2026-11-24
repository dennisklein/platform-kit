package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func gatherCompare(t *testing.T, now time.Time, objs []client.Object, expect string, names ...string) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(&Collector{Reader: newReader(t, objs...), Now: func() time.Time { return now }}))
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expect), names...))
}

// A node that is still being installed, or whose cluster is not yet
// bootstrapped, is not expected to be Ready: the alert must not page for
// day-1 work that pk-metal status does not flag either.
func TestCollectorExpectedReadyFollowsStatus(t *testing.T) {
	boot := metav1.NewTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	running := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Status: osv1alpha1.TalosClusterStatus{BootstrappedAt: &boot}}
	fresh := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "new"}}
	node := func(name, cluster, version string) *osv1alpha1.TalosNode {
		return &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:   osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: cluster}, Role: osv1alpha1.RoleWorker},
			Status: osv1alpha1.TalosNodeStatus{Version: version}}
	}
	objs := []client.Object{running, fresh,
		node("w-1", "prod", "v1.14.6"), // has answered: expected Ready
		node("w-2", "prod", ""),        // still installing
		node("w-3", "new", "v1.14.6"),  // cluster not bootstrapped
	}
	gatherCompare(t, time.Now(), objs, `
# HELP pk_metal_talosnode_expected_ready 1 when a TalosNode is expected to be Ready: its cluster is bootstrapped and the node reported a version.
# TYPE pk_metal_talosnode_expected_ready gauge
pk_metal_talosnode_expected_ready{cluster="new",node="w-3",role="worker"} 0
pk_metal_talosnode_expected_ready{cluster="prod",node="w-1",role="worker"} 1
pk_metal_talosnode_expected_ready{cluster="prod",node="w-2",role="worker"} 0
# HELP pk_metal_taloscluster_expected_available 1 when a TalosCluster is expected to be available: it is bootstrapped.
# TYPE pk_metal_taloscluster_expected_available gauge
pk_metal_taloscluster_expected_available{cluster="new"} 0
pk_metal_taloscluster_expected_available{cluster="prod"} 1
`, "pk_metal_talosnode_expected_ready", "pk_metal_taloscluster_expected_available")
}

func policy(name, cluster string, interval time.Duration, last *time.Time) *osv1alpha1.EtcdBackupPolicy {
	p := &osv1alpha1.EtcdBackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: osv1alpha1.EtcdBackupPolicySpec{ClusterRef: osv1alpha1.Reference{Name: cluster},
			Interval: metav1.Duration{Duration: interval}}}
	if last != nil {
		p.Status.LastSuccessfulSnapshot = &osv1alpha1.SnapshotReference{Name: name + "-x", TakenAt: metav1.NewTime(*last)}
	}
	return p
}

// The backup series come from the policies: the fresher snapshot and the
// shorter interval of two policies speak for the cluster; a cluster without
// a verified snapshot still reports its interval (the alert finds it by the
// missing age); and a cluster whose policies are gone reports nothing.
func TestCollectorBackupsFromPolicies(t *testing.T) {
	older := time.Unix(1_700_000_000, 0)
	newer := older.Add(3 * time.Hour)
	objs := []client.Object{
		policy("a-s3", "a", 6*time.Hour, &older),
		policy("a-pvc", "a", 24*time.Hour, &newer),
		policy("b-s3", "b", 24*time.Hour, nil),
	}
	gatherCompare(t, time.Now(), objs, `
# HELP pk_metal_etcd_backup_interval_seconds The shortest spec.interval of the EtcdBackupPolicies of a TalosCluster.
# TYPE pk_metal_etcd_backup_interval_seconds gauge
pk_metal_etcd_backup_interval_seconds{cluster="a"} 21600
pk_metal_etcd_backup_interval_seconds{cluster="b"} 86400
# HELP pk_metal_etcd_backup_last_success_timestamp_seconds Unix time of the newest verified etcd snapshot of a TalosCluster, over all its EtcdBackupPolicies.
# TYPE pk_metal_etcd_backup_last_success_timestamp_seconds gauge
pk_metal_etcd_backup_last_success_timestamp_seconds{cluster="a"} 1700010800
`, "pk_metal_etcd_backup_interval_seconds", "pk_metal_etcd_backup_last_success_timestamp_seconds")

	// The policies are deleted: the series go with them, so a stale alert
	// cannot fire for a cluster nobody backs up any more.
	gatherCompare(t, time.Now(), nil, "",
		"pk_metal_etcd_backup_interval_seconds", "pk_metal_etcd_backup_last_success_timestamp_seconds")
}

func rollout(name string, conds ...metav1.Condition) *osv1alpha1.TalosRollout {
	return &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}},
		Status: osv1alpha1.TalosRolloutStatus{Conditions: conds}}
}

func TestCollectorRolloutCondition(t *testing.T) {
	objs := []client.Object{
		rollout("waiting",
			cond(osv1alpha1.ConditionPlanned, "True", osv1alpha1.ReasonPlanReady),
			cond(osv1alpha1.ConditionApproved, "False", osv1alpha1.ReasonAwaitingApproval),
			cond(osv1alpha1.ConditionProgressing, "False", osv1alpha1.ReasonRolloutNotStarted)),
		rollout("blocked",
			cond(osv1alpha1.ConditionPlanned, "False", osv1alpha1.ReasonPlanBlocked),
			cond(osv1alpha1.ConditionApproved, "False", osv1alpha1.ReasonAwaitingApproval)),
		rollout("canary",
			cond(osv1alpha1.ConditionApproved, "True", osv1alpha1.ReasonPlanApproved),
			cond(osv1alpha1.ConditionPaused, "True", osv1alpha1.ReasonCanaryPause),
			cond(osv1alpha1.ConditionProgressing, "True", osv1alpha1.ReasonCanaryPause)),
		rollout("health",
			cond(osv1alpha1.ConditionApproved, "True", osv1alpha1.ReasonPlanApproved),
			cond(osv1alpha1.ConditionPaused, "False", osv1alpha1.ReasonNotPaused),
			cond(osv1alpha1.ConditionProgressing, "True", osv1alpha1.ReasonWaitingForHealth)),
		rollout("failed",
			cond(osv1alpha1.ConditionApproved, "True", osv1alpha1.ReasonPlanApproved),
			cond(osv1alpha1.ConditionProgressing, "False", osv1alpha1.ReasonRolloutFailed),
			cond(osv1alpha1.ConditionComplete, "False", osv1alpha1.ReasonRolloutFailed)),
		rollout("done",
			cond(osv1alpha1.ConditionComplete, "True", osv1alpha1.ReasonRolloutSucceeded)),
		rollout("new"),
	}
	gatherCompare(t, time.Now(), objs, `
# HELP pk_metal_rollout_condition 1 for each TalosRollout that has not succeeded, with the reason it is where it is (AwaitingApproval, PlanBlocked, WaitingForHealth, PausedAfterFirstNode, RolloutFailed, …).
# TYPE pk_metal_rollout_condition gauge
pk_metal_rollout_condition{cluster="c",reason="AwaitingApproval",rollout="waiting"} 1
pk_metal_rollout_condition{cluster="c",reason="NotStarted",rollout="new"} 1
pk_metal_rollout_condition{cluster="c",reason="PausedAfterFirstNode",rollout="canary"} 1
pk_metal_rollout_condition{cluster="c",reason="PlanBlocked",rollout="blocked"} 1
pk_metal_rollout_condition{cluster="c",reason="RolloutFailed",rollout="failed"} 1
pk_metal_rollout_condition{cluster="c",reason="WaitingForHealth",rollout="health"} 1
`, "pk_metal_rollout_condition")

	st := RolloutStateOf(objs[4].(*osv1alpha1.TalosRollout))
	assert.True(t, st.Ended)
	assert.False(t, st.Succeeded)
}

// A running operation reports the step it is in and for how long: from the
// end of the step before, or from the start of execution for the first.
func TestCollectorOperationRunningSeconds(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	actions := []metalv1alpha1.HostOperationAction{
		{PowerCycle: &metalv1alpha1.PowerCycleAction{}}, {ApplyBios: &metalv1alpha1.ApplyBiosAction{}},
	}
	op := func(name string, st metalv1alpha1.HostOperationStatus) *metalv1alpha1.HostOperation {
		st.Phase = metalv1alpha1.OperationRunning
		return &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: name},
			Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "node-1"}, Actions: actions,
				Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true), ApprovedBy: "alice"}},
			Status: st}
	}
	started := metav1.NewTime(now.Add(-time.Hour))
	finished := metav1.NewTime(now.Add(-10 * time.Minute))
	objs := []client.Object{
		op("first", metalv1alpha1.HostOperationStatus{Baseline: &metalv1alpha1.OperationBaseline{CapturedAt: &started}}),
		op("second", metalv1alpha1.HostOperationStatus{
			Baseline: &metalv1alpha1.OperationBaseline{CapturedAt: &started},
			Steps:    []metalv1alpha1.OperationStep{{Action: "powerCycle", Phase: "Succeeded", FinishedAt: &finished}}}),
		op("unknown", metalv1alpha1.HostOperationStatus{}),
	}
	gatherCompare(t, now, objs, `
# HELP pk_metal_operation_running_seconds How long each running HostOperation has been in its current step (step is the action's key).
# TYPE pk_metal_operation_running_seconds gauge
pk_metal_operation_running_seconds{host="node-1",namespace="pk-metal",operation="first",step="powerCycle"} 3600
pk_metal_operation_running_seconds{host="node-1",namespace="pk-metal",operation="second",step="applyBios"} 600
`, "pk_metal_operation_running_seconds")
}
