package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// clusterStatus is one TalosCluster row of -o json.
type clusterStatus struct {
	Name                     string   `json:"name"`
	TalosVersion             string   `json:"talosVersion"`
	TalosVersionsRunning     []string `json:"talosVersionsRunning"`
	KubernetesVersion        string   `json:"kubernetesVersion"`
	KubernetesVersionRunning string   `json:"kubernetesVersionRunning"`
	ControlPlanes            int32    `json:"controlPlanes"`
	ReadyControlPlanes       int32    `json:"readyControlPlanes"`
	Workers                  int32    `json:"workers"`
	ReadyWorkers             int32    `json:"readyWorkers"`
	Available                string   `json:"available"`
	Etcd                     string   `json:"etcd"`
	BootstrappedAt           string   `json:"bootstrappedAt"`
	Backup                   string   `json:"backup"`
	BackupMessage            string   `json:"backupMessage"`
	Message                  string   `json:"message"`
	NeedsAttention           bool     `json:"needsAttention"`
}

// talosNodeStatus is one TalosNode row of -o json.
type talosNodeStatus struct {
	Name           string `json:"name"`
	Cluster        string `json:"cluster"`
	Role           string `json:"role"`
	Host           string `json:"host"`
	Address        string `json:"address"`
	Version        string `json:"version"`
	Stage          string `json:"stage"`
	Ready          string `json:"ready"`
	Message        string `json:"message"`
	NeedsAttention bool   `json:"needsAttention"`
}

// clusterCond is a condition as status shows it: its reason when it has one,
// else its status, "" when the condition is absent.
func clusterCond(conds []metav1.Condition, t string) (state, msg string, c *metav1.Condition) {
	c = meta.FindStatusCondition(conds, t)
	if c == nil {
		return "", "", nil
	}
	if c.Reason != "" {
		return c.Reason, c.Message, c
	}
	return string(c.Status), c.Message, c
}

// backedUp is a cluster's backup state as its EtcdBackupPolicies report it
// (their BackedUp condition): the freshest policy that is backed up speaks
// for the cluster, else the first one that is not. A cluster without a
// policy has no backup state, and none is invented.
func backedUp(policies []osv1alpha1.EtcdBackupPolicy, cluster string) (state, msg string, c *metav1.Condition) {
	var worst *metav1.Condition
	for i := range policies {
		p := &policies[i]
		if p.Spec.ClusterRef.Name != cluster {
			continue
		}
		pc := meta.FindStatusCondition(p.Status.Conditions, osv1alpha1.ConditionBackedUp)
		switch {
		case pc == nil:
		case pc.Status == metav1.ConditionTrue:
			return clusterCond(p.Status.Conditions, osv1alpha1.ConditionBackedUp)
		case worst == nil:
			worst = pc
		}
	}
	if worst == nil {
		return "", "", nil
	}
	return clusterCond([]metav1.Condition{*worst}, osv1alpha1.ConditionBackedUp)
}

// addClusters fills the cluster and node tables. Hosts give the NODE table
// its HOST column (the Host whose spec.os names the node); the backup
// policies its BACKUP column.
func addClusters(rep *statusReport, clusters []osv1alpha1.TalosCluster, nodes []osv1alpha1.TalosNode, hosts []metalv1alpha1.Host,
	policies []osv1alpha1.EtcdBackupPolicy) {
	hostOf := map[string]string{}
	for _, h := range hosts {
		if h.Spec.OS != nil {
			hostOf[h.Spec.OS.NodeRef.Name] = h.Name
		}
	}
	running := map[string][]string{}
	bootstrapped := map[string]bool{}
	for _, c := range clusters {
		bootstrapped[c.Name] = c.Status.BootstrappedAt != nil
	}

	for _, n := range nodes {
		cluster := n.Spec.ClusterRef.Name
		row := talosNodeStatus{Name: n.Name, Cluster: cluster, Role: string(n.Spec.Role), Host: hostOf[n.Name],
			Address: nodeAddress(&n), Version: n.Status.Version, Stage: n.Status.Stage}
		var ready *metav1.Condition
		row.Ready, row.Message, ready = clusterCond(n.Status.Conditions, osv1alpha1.ConditionReady)
		if ready != nil && ready.Status == metav1.ConditionTrue {
			row.Ready = "Ready"
		}
		// A node of a cluster that runs, and that has answered before (it
		// reported a version), is expected to be Ready. One still being
		// installed is not, and is no alarm.
		row.NeedsAttention = bootstrapped[cluster] && n.Status.Version != "" &&
			(ready == nil || ready.Status != metav1.ConditionTrue)
		if n.Status.Version != "" && !slices.Contains(running[cluster], n.Status.Version) {
			running[cluster] = append(running[cluster], n.Status.Version)
		}
		rep.Nodes = append(rep.Nodes, row)
	}

	for _, c := range clusters {
		st := c.Status
		row := clusterStatus{Name: c.Name, TalosVersion: c.Spec.TalosVersion, TalosVersionsRunning: running[c.Name],
			KubernetesVersion: c.Spec.KubernetesVersion, KubernetesVersionRunning: st.KubernetesVersion,
			ControlPlanes: st.ControlPlanes, ReadyControlPlanes: st.ReadyControlPlanes,
			Workers: st.Workers, ReadyWorkers: st.ReadyWorkers}
		if row.TalosVersionsRunning == nil {
			row.TalosVersionsRunning = []string{}
		}
		slices.Sort(row.TalosVersionsRunning)
		if st.BootstrappedAt != nil {
			row.BootstrappedAt = st.BootstrappedAt.UTC().Format(time.RFC3339)
		}
		avail, availMsg, availCond := clusterCond(st.Conditions, osv1alpha1.ConditionAvailable)
		row.Available = avail
		// Etcd as the cluster controller last observed it: its Bootstrapped
		// condition says EtcdRunning, or EtcdDataLost when every control
		// plane lost its data (docs site: operate/day-2/restore-etcd).
		etcd, etcdMsg, etcdCond := clusterCond(st.Conditions, osv1alpha1.ConditionBootstrapped)
		row.Etcd = etcd
		backup, backupMsg, backupCond := backedUp(policies, c.Name)
		// The backup controller's own sentence carries when the newest
		// verified snapshot was taken; the condition's transition time is
		// not that time, so it is not shown as one.
		row.Backup, row.BackupMessage = backup, backupMsg

		bad := func(c *metav1.Condition) bool { return c != nil && c.Status == metav1.ConditionFalse }
		switch {
		case st.BootstrappedAt != nil && bad(etcdCond):
			row.NeedsAttention, row.Message = true, etcdMsg
		case st.BootstrappedAt != nil && bad(availCond):
			row.NeedsAttention, row.Message = true, availMsg
		case bad(backupCond):
			row.NeedsAttention, row.Message = true, backupMsg
		}
		rep.Clusters = append(rep.Clusters, row)
	}

	slices.SortFunc(rep.Clusters, func(a, b clusterStatus) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(rep.Nodes, func(a, b talosNodeStatus) int {
		return cmpJoin(a.Cluster, a.Name, b.Cluster, b.Name)
	})
	for _, r := range rep.Clusters {
		rep.NeedsAttention = rep.NeedsAttention || r.NeedsAttention
	}
	for _, r := range rep.Nodes {
		rep.NeedsAttention = rep.NeedsAttention || r.NeedsAttention
	}
}

func cmpJoin(a1, a2, b1, b2 string) int {
	if c := strings.Compare(a1, b1); c != 0 {
		return c
	}
	return strings.Compare(a2, b2)
}

// renderClusters prints the CLUSTER and NODE tables, or nothing when there
// is no cluster and no node (a rack without the OS kinds in use).
func renderClusters(rep statusReport, section func(string, [][]string, []string)) {
	if len(rep.Clusters) > 0 {
		var rows [][]string
		var notes []string
		for _, c := range rep.Clusters {
			running := strings.Join(c.TalosVersionsRunning, ",")
			bootstrapped := "-"
			if c.BootstrappedAt != "" {
				bootstrapped = c.BootstrappedAt
			}
			rows = append(rows, []string{c.Name,
				orDash(c.TalosVersion) + " / " + orDash(running),
				orDash(c.KubernetesVersion) + " / " + orDash(c.KubernetesVersionRunning),
				fmt.Sprintf("%d/%d", c.ReadyControlPlanes, c.ControlPlanes),
				fmt.Sprintf("%d/%d", c.ReadyWorkers, c.Workers),
				orDash(c.Etcd), bootstrapped, orDash(c.Backup), orDash(c.Available)})
			if c.NeedsAttention {
				notes = append(notes, note(c.Name, c.Message, "needs attention"))
			}
		}
		section("CLUSTER\tTALOS WANT/RUN\tK8S WANT/RUN\tCP\tWORKERS\tETCD\tBOOTSTRAPPED\tBACKUP\tAVAILABLE", rows, notes)
	}
	if len(rep.Nodes) > 0 {
		var rows [][]string
		var notes []string
		for _, n := range rep.Nodes {
			rows = append(rows, []string{n.Name, orDash(n.Cluster), orDash(n.Role), orDash(n.Host), orDash(n.Address),
				orDash(n.Version), orDash(n.Stage), orDash(n.Ready)})
			if n.NeedsAttention {
				notes = append(notes, note(n.Name, n.Message, "not Ready"))
			}
		}
		section("NODE\tCLUSTER\tROLE\tHOST\tADDRESS\tVERSION\tSTAGE\tREADY", rows, notes)
	}
}
