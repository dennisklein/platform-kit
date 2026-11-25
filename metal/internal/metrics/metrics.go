// Package metrics exposes the operator's domain state to Prometheus: host
// phases, operations by phase and action, approvals waiting, BMC
// reachability, Talos node and cluster health, rollouts, etcd backups, and
// credential expiry.
//
// Most series are derived at scrape time from the manager's informer cache
// (Collector), never kept as a second copy of the state: a gauge that a
// reconciler forgot to update would show a fact that is no longer true, and a
// value read at scrape time cannot. One fact is not in any object the
// operator reads back cheaply — how far a rollout got — so its writer reports
// it through the hook SetRolloutProgress.
//
// The package does not serve HTTP; Register adds everything to the
// controller-runtime registry, which the manager's metrics server already
// exposes.
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Namespace prefixes every series this package defines.
const Namespace = "pk_metal"

var (
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

	gateDegraded = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "admission_gate_degraded",
		Help: "1 while an admission policy of the approval gate, or its binding, is missing or does not deny; " +
			"0 while it is in place. The settings controller checks each policy on a timer.",
	}, []string{"policy"})
)

// SetGateDegraded records whether the named admission policy of the
// approval gate is degraded. The settings controller calls it on every
// check.
func SetGateDegraded(policy string, degraded bool) {
	v := 0.0
	if degraded {
		v = 1
	}
	gateDegraded.WithLabelValues(policy).Set(v)
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

// Register adds the scrape-time collector and the hook-fed series to reg
// (the controller-runtime registry in the manager). reader is the manager's
// cache; credentialsNamespace is where the published cluster credentials
// live. The hook-fed series are process-wide (their writers do not hold a
// registry), so registering them into a registry that already has them is
// not an error.
func Register(reg prometheus.Registerer, reader client.Reader, credentialsNamespace string) error {
	for _, c := range []prometheus.Collector{rolloutNodesDone, rolloutNodesTotal, gateDegraded} {
		if err := reg.Register(c); err != nil && !errors.As(err, &prometheus.AlreadyRegisteredError{}) {
			return err
		}
	}
	return reg.Register(&Collector{Reader: reader, CredentialsNamespace: credentialsNamespace})
}
