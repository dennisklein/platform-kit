// Package metrics exposes the operator's domain state to Prometheus: host
// phases, operations by phase and action, approvals waiting, BMC
// reachability, Talos node and cluster health, and credential expiry.
//
// Most series are derived at scrape time from the manager's informer cache
// (Collector), never kept as a second copy of the state: a gauge that a
// reconciler forgot to update would show a fact that is no longer true, and a
// value read at scrape time cannot. Two facts are not in any object the
// operator reads back cheaply — when the last etcd backup succeeded and how
// far a rollout got — so their writers report them through the hooks
// SetEtcdBackupLastSuccess and SetRolloutProgress.
//
// The package does not serve HTTP; Register adds everything to the
// controller-runtime registry, which the manager's metrics server already
// exposes.
package metrics

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Namespace prefixes every series this package defines.
const Namespace = "pk_metal"

var (
	etcdBackupLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "etcd_backup_last_success_timestamp_seconds",
		Help:      "Unix time of the newest verified etcd snapshot of a TalosCluster.",
	}, []string{"cluster"})

	rolloutNodesDone = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "rollout_nodes_done",
		Help:      "Nodes a TalosRollout has finished.",
	}, []string{"rollout", "cluster"})

	rolloutNodesTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "rollout_nodes_total",
		Help:      "Nodes in a TalosRollout's approved plan.",
	}, []string{"rollout", "cluster"})
)

// SetEtcdBackupLastSuccess records the time of a cluster's newest verified
// etcd snapshot. The backup controller calls it after each verified snapshot
// (and at start-up from the newest EtcdSnapshot it finds, so a restart does
// not read as a stale backup). A zero time removes the series: a cluster
// without any backup has no age, and the alert rule treats the missing series
// on its own terms.
func SetEtcdBackupLastSuccess(cluster string, at time.Time) {
	if at.IsZero() {
		etcdBackupLastSuccess.DeleteLabelValues(cluster)
		return
	}
	etcdBackupLastSuccess.WithLabelValues(cluster).Set(float64(at.Unix()))
}

// SetRolloutProgress records how many nodes of a rollout's plan are done.
// The rollout controller calls it on every pass.
func SetRolloutProgress(rollout, cluster string, done, total int) {
	rolloutNodesDone.WithLabelValues(rollout, cluster).Set(float64(done))
	rolloutNodesTotal.WithLabelValues(rollout, cluster).Set(float64(total))
}

// ForgetRollout drops a rollout's series once the rollout is gone, so a
// deleted rollout does not stay on the dashboard at its last value.
func ForgetRollout(rollout, cluster string) {
	rolloutNodesDone.DeleteLabelValues(rollout, cluster)
	rolloutNodesTotal.DeleteLabelValues(rollout, cluster)
}

// ForgetCluster drops a cluster's hook-fed series once the cluster is gone.
func ForgetCluster(cluster string) {
	etcdBackupLastSuccess.DeleteLabelValues(cluster)
}

// Register adds the scrape-time collector and the hook-fed series to reg
// (the controller-runtime registry in the manager). reader is the manager's
// cache; credentialsNamespace is where the published cluster credentials
// live. The hook-fed series are process-wide (their writers do not hold a
// registry), so registering them into a registry that already has them is
// not an error.
func Register(reg prometheus.Registerer, reader client.Reader, credentialsNamespace string) error {
	for _, c := range []prometheus.Collector{etcdBackupLastSuccess, rolloutNodesDone, rolloutNodesTotal} {
		if err := reg.Register(c); err != nil && !errors.As(err, &prometheus.AlreadyRegisteredError{}) {
			return err
		}
	}
	return reg.Register(&Collector{Reader: reader, CredentialsNamespace: credentialsNamespace})
}
