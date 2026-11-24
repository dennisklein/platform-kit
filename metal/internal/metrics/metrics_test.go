package metrics

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/testpki"
)

func newReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	s, err := scheme.New()
	require.NoError(t, err)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func cond(t, s, reason string) metav1.Condition {
	return metav1.Condition{Type: t, Status: metav1.ConditionStatus(s), Reason: reason, LastTransitionTime: metav1.Now()}
}

func TestCollectorDerivesDomainSeries(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	certPEM, _ := testpki.SelfSigned(t, "admin", nil, nil)
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://192.0.2.1:6443"}}]
users: [{name: admin, user: {client-certificate-data: %s}}]
contexts: [{name: c, context: {cluster: c, user: admin}}]
current-context: c
`, base64.StdEncoding.EncodeToString(certPEM))
	talosconfig := fmt.Sprintf("context: c\ncontexts:\n  c:\n    crt: %s\n", base64.StdEncoding.EncodeToString(certPEM))

	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: metalv1alpha1.HostStatus{Phase: metalv1alpha1.HostProvisioned, BlockedOn: "Approval(BiosConvergence)"}}
	waiting := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "bios", CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour))},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "node-1"}, Reason: "BiosConvergence",
			Actions: []metalv1alpha1.HostOperationAction{
				{ApplyBios: &metalv1alpha1.ApplyBiosAction{}}, {PowerCycle: &metalv1alpha1.PowerCycleAction{}},
			}},
		Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationAwaitingApproval},
	}
	failed := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "flash"},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "node-1"}, Reason: "FirmwareConvergence",
			Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true), ApprovedBy: "alice"},
			Actions:  []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}}},
		Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationFailed,
			Conditions: []metav1.Condition{cond(metalv1alpha1.ConditionExecuted, "False", "StepFailed")}},
	}
	held := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "held"},
		Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "node-1"},
			Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true), ApprovedBy: "alice"},
			Actions:  []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}}},
		Status: metalv1alpha1.HostOperationStatus{Phase: metalv1alpha1.OperationProposed, BlockedOn: "OutsideMaintenanceWindow"},
	}
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc-1"},
		Status: metalv1alpha1.ManagementEndpointStatus{Conditions: []metav1.Condition{
			cond(metalv1alpha1.ConditionReachable, "False", "Unreachable")}}}
	unprobed := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc-2"}}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "cp-1"},
		Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}, Role: osv1alpha1.RoleControlPlane},
		Status: osv1alpha1.TalosNodeStatus{Version: "v1.14.6",
			Conditions: []metav1.Condition{cond(osv1alpha1.ConditionReady, "True", "Ready")}}}
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Status: osv1alpha1.TalosClusterStatus{ControlPlanes: 3, ReadyControlPlanes: 2, Workers: 1, ReadyWorkers: 1,
			KubeconfigSecretRef:  &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "c-kubeconfig"},
			TalosconfigSecretRef: &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "c-talosconfig"}}}
	secrets := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "c-kubeconfig"},
			Data: map[string][]byte{"kubeconfig": []byte(kubeconfig)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "c-talosconfig"},
			Data: map[string][]byte{"talosconfig": []byte(talosconfig)}},
	}

	c := &Collector{
		Reader:               newReader(t, append(secrets, host, waiting, failed, held, ep, unprobed, node, cluster)...),
		CredentialsNamespace: "pk-metal",
		Now:                  func() time.Time { return now },
	}
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(c))

	expect := `
# HELP pk_metal_host_phase 1 for the phase each Host is in.
# TYPE pk_metal_host_phase gauge
pk_metal_host_phase{host="node-1",phase="Provisioned"} 1
# HELP pk_metal_host_blocked 1 while a Host is blocked, with the reason (status.blockedOn).
# TYPE pk_metal_host_blocked gauge
pk_metal_host_blocked{host="node-1",reason="Approval(BiosConvergence)"} 1
# HELP pk_metal_operation_awaiting_approval_seconds How long each HostOperation has been waiting for approval.
# TYPE pk_metal_operation_awaiting_approval_seconds gauge
pk_metal_operation_awaiting_approval_seconds{host="node-1",namespace="pk-metal",operation="bios",reason="BiosConvergence"} 7200
# HELP pk_metal_operation_failed 1 for each HostOperation that failed and still waits for a person (retry or delete).
# TYPE pk_metal_operation_failed gauge
pk_metal_operation_failed{host="node-1",namespace="pk-metal",operation="flash",reason="FirmwareConvergence"} 1
# HELP pk_metal_operation_blocked 1 for each approved HostOperation that has not started, with the reason it waits (OperationQueued, OutsideMaintenanceWindow, …).
# TYPE pk_metal_operation_blocked gauge
pk_metal_operation_blocked{host="node-1",namespace="pk-metal",operation="held",reason="OutsideMaintenanceWindow"} 1
# HELP pk_metal_operations HostOperations by phase and action; an operation counts once for each distinct action it carries.
# TYPE pk_metal_operations gauge
pk_metal_operations{action="applyBios",phase="AwaitingApproval"} 1
pk_metal_operations{action="powerCycle",phase="AwaitingApproval"} 1
pk_metal_operations{action="powerCycle",phase="Failed"} 1
pk_metal_operations{action="powerCycle",phase="Proposed"} 1
# HELP pk_metal_bmc_reachable 1 when a ManagementEndpoint answers Redfish, 0 when it does not; absent while not yet probed.
# TYPE pk_metal_bmc_reachable gauge
pk_metal_bmc_reachable{endpoint="bmc-1"} 0
# HELP pk_metal_talosnode_ready 1 when a TalosNode reports Ready, 0 otherwise.
# TYPE pk_metal_talosnode_ready gauge
pk_metal_talosnode_ready{cluster="c",node="cp-1",role="controlplane"} 1
# HELP pk_metal_talosnode_info 1 per TalosNode, labelled with the Talos version it reports.
# TYPE pk_metal_talosnode_info gauge
pk_metal_talosnode_info{cluster="c",node="cp-1",version="v1.14.6"} 1
# HELP pk_metal_taloscluster_available 1 when a TalosCluster's Kubernetes API answers and every node is Ready.
# TYPE pk_metal_taloscluster_available gauge
pk_metal_taloscluster_available{cluster="c"} 0
# HELP pk_metal_taloscluster_nodes TalosCluster nodes by role and readiness.
# TYPE pk_metal_taloscluster_nodes gauge
pk_metal_taloscluster_nodes{cluster="c",ready="false",role="controlplane"} 1
pk_metal_taloscluster_nodes{cluster="c",ready="false",role="worker"} 0
pk_metal_taloscluster_nodes{cluster="c",ready="true",role="controlplane"} 2
pk_metal_taloscluster_nodes{cluster="c",ready="true",role="worker"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expect),
		"pk_metal_host_phase", "pk_metal_host_blocked", "pk_metal_operation_awaiting_approval_seconds",
		"pk_metal_operation_failed", "pk_metal_operation_blocked", "pk_metal_operations", "pk_metal_bmc_reachable",
		"pk_metal_talosnode_ready", "pk_metal_talosnode_info", "pk_metal_taloscluster_available",
		"pk_metal_taloscluster_nodes"))

	// The credential expiry and issue time are the certificate's own
	// NotAfter and NotBefore, for both kinds: the alert reads the lifetime
	// from the two, so both must be there.
	mfs, err := reg.Gather()
	require.NoError(t, err)
	series := map[string]map[string]float64{}
	for _, mf := range mfs {
		name := mf.GetName()
		if name != "pk_metal_credentials_expiry_timestamp_seconds" && name != "pk_metal_credentials_issued_timestamp_seconds" {
			continue
		}
		series[name] = map[string]float64{}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "kind" {
					series[name][l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	expiry, issued := series["pk_metal_credentials_expiry_timestamp_seconds"], series["pk_metal_credentials_issued_timestamp_seconds"]
	require.Len(t, expiry, 2, "both published credentials must report an expiry")
	require.Len(t, issued, 2, "both published credentials must report an issue time")
	v, ok := kubeconfigValidity([]byte(kubeconfig))
	require.True(t, ok)
	for _, kind := range []string{"kubeconfig", "talosconfig"} {
		assert.InDelta(t, float64(v.NotAfter.Unix()), expiry[kind], 0, kind)
		assert.InDelta(t, float64(v.NotBefore.Unix()), issued[kind], 0, kind)
	}
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), v.NotAfter, time.Hour)
	assert.True(t, v.NotBefore.Before(v.NotAfter) && !v.NotBefore.After(time.Now()),
		"the issue time is the certificate's own, before its expiry: %v", v)
}

func TestHooksFeedTheirSeries(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, Register(reg, newReader(t), "pk-metal"))
	require.NoError(t, Register(prometheus.NewRegistry(), newReader(t), "pk-metal"),
		"a second registry must take the process-wide hook series too")

	SetRolloutProgress("upgrade-1", "c", 1, 3)
	assert.InDelta(t, 1, testutil.ToFloat64(rolloutNodesDone.WithLabelValues("upgrade-1", "c")), 0)
	assert.InDelta(t, 3, testutil.ToFloat64(rolloutNodesTotal.WithLabelValues("upgrade-1", "c")), 0)

	ForgetRollout("upgrade-1", "c")
	assert.Equal(t, 0, testutil.CollectAndCount(rolloutNodesDone))
}
