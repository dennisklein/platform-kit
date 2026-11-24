package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// lab is a running three-node cluster: two control planes Ready on the
// wanted version, one that stopped answering after it had run.
func lab() ([]osv1alpha1.TalosCluster, []osv1alpha1.TalosNode, []metalv1alpha1.Host) {
	at := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	ready := func(s metav1.ConditionStatus, reason, msg string) []metav1.Condition {
		return []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: s, Reason: reason, Message: msg}}
	}
	clusters := []osv1alpha1.TalosCluster{{
		ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec:       osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "1.36.2"},
		Status: osv1alpha1.TalosClusterStatus{
			BootstrappedAt: &at, ControlPlanes: 3, ReadyControlPlanes: 2, KubernetesVersion: "v1.36.2",
			Conditions: []metav1.Condition{
				{Type: osv1alpha1.ConditionBootstrapped, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonEtcdRunning},
				{Type: osv1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonNodesNotReady,
					Message: "2/3 control-plane and 0/0 worker nodes Ready"},
			},
		},
	}}
	node := func(name, version string, conds []metav1.Condition) osv1alpha1.TalosNode {
		return osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"},
				Role: osv1alpha1.RoleControlPlane, Address: "192.0.2." + name[len(name)-1:]},
			Status: osv1alpha1.TalosNodeStatus{Version: version, Stage: "running", Conditions: conds},
		}
	}
	nodes := []osv1alpha1.TalosNode{
		node("node-1", "v1.14.6", ready(metav1.ConditionTrue, osv1alpha1.ReasonNodeReady, "")),
		node("node-2", "v1.14.5", ready(metav1.ConditionTrue, osv1alpha1.ReasonNodeReady, "")),
		node("node-3", "v1.14.6", ready(metav1.ConditionUnknown, osv1alpha1.ReasonAwaitingContact, "no answer since 10:00")),
	}
	hosts := []metalv1alpha1.Host{{
		ObjectMeta: metav1.ObjectMeta{Name: "simx12b000001"},
		Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "node-1"}}},
	}}
	return clusters, nodes, hosts
}

func TestStatusClusterTables(t *testing.T) {
	clusters, nodes, hosts := lab()
	rep := buildReport(hosts, nil, nil, nil, nodes)
	addClusters(&rep, clusters, nodes, hosts, nil)

	require.Len(t, rep.Clusters, 1)
	c := rep.Clusters[0]
	assert.Equal(t, []string{"v1.14.5", "v1.14.6"}, c.TalosVersionsRunning, "every running version, sorted: the skew shows")
	assert.Equal(t, osv1alpha1.ReasonEtcdRunning, c.Etcd, "the cluster controller's Bootstrapped observation")
	assert.Equal(t, "2026-01-02T03:04:05Z", c.BootstrappedAt)
	assert.Empty(t, c.Backup, "no backup controller reported: no backup state is invented")
	assert.True(t, c.NeedsAttention, "a bootstrapped cluster that is not available needs a person")
	assert.Contains(t, c.Message, "2/3 control-plane")

	require.Len(t, rep.Nodes, 3)
	assert.Equal(t, "simx12b000001", rep.Nodes[0].Host)
	assert.False(t, rep.Nodes[0].NeedsAttention)
	assert.True(t, rep.Nodes[2].NeedsAttention, "a node that ran and stopped answering needs a person")
	assert.True(t, rep.NeedsAttention)

	out := renderReport(rep)
	assert.Contains(t, out, "CLUSTER")
	assert.Contains(t, out, "v1.14.6 / v1.14.5,v1.14.6")
	assert.Contains(t, out, "node-3: no answer since 10:00")

	// The backup policies' verdict counts once one reports it.
	clusters[0].Status.Conditions = append(clusters[0].Status.Conditions[:1],
		metav1.Condition{Type: osv1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonClusterAvailable})
	policy := func(name, cluster string, s metav1.ConditionStatus, reason, msg string) osv1alpha1.EtcdBackupPolicy {
		p := osv1alpha1.EtcdBackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.ClusterRef.Name = cluster
		p.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionBackedUp, Status: s, Reason: reason, Message: msg}}
		return p
	}
	stale := policy("lab-daily", "lab", metav1.ConditionFalse, osv1alpha1.ReasonBackupTooOld, "newest snapshot is 30h old")
	rep = buildReport(nil, nil, nil, nil, nil)
	addClusters(&rep, clusters, nil, nil, []osv1alpha1.EtcdBackupPolicy{
		stale, policy("other-hourly", "other", metav1.ConditionTrue, osv1alpha1.ReasonSnapshotRecent, "")})
	assert.Equal(t, osv1alpha1.ReasonBackupTooOld, rep.Clusters[0].Backup, "another cluster's policy does not speak for it")
	assert.Equal(t, "newest snapshot is 30h old", rep.Clusters[0].Message)
	assert.True(t, rep.NeedsAttention)

	// A second policy that is backed up does.
	rep = buildReport(nil, nil, nil, nil, nil)
	addClusters(&rep, clusters, nil, nil, []osv1alpha1.EtcdBackupPolicy{
		stale, policy("lab-hourly", "lab", metav1.ConditionTrue, osv1alpha1.ReasonSnapshotRecent, "lab-hourly-1 taken at ...")})
	assert.Equal(t, osv1alpha1.ReasonSnapshotRecent, rep.Clusters[0].Backup)
	assert.False(t, rep.Clusters[0].NeedsAttention)
}

func TestStatusClustersInJSON(t *testing.T) {
	clusters, nodes, hosts := lab()
	cl := fakeClient(&clusters[0], &nodes[0], &nodes[1], &hosts[0])
	var out bytes.Buffer
	code, err := runStatus(context.Background(), cl, "json", &out)
	require.NoError(t, err)
	assert.Equal(t, exitAttention, code, "the cluster is not available")
	var doc struct {
		Clusters []map[string]any `json:"clusters"`
		Nodes    []map[string]any `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	require.Len(t, doc.Clusters, 1)
	require.Len(t, doc.Nodes, 2)
	assert.Equal(t, "lab", doc.Clusters[0]["name"])
	assert.Equal(t, "node-1", doc.Nodes[0]["name"])
}
