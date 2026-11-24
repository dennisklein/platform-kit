package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// RolloutProposer proposes, and never starts, the rollout a cluster needs:
// a TalosUpgrade when a machine runs another Talos version than the spec
// asks for, then a KubernetesUpgrade when the API runs another Kubernetes
// version, then a Config rollout for the nodes whose dry run reports drift.
// One rollout per cluster at a time: while one is unfinished nothing new is
// proposed. A proposal is named after what it proposes, so the same need
// never spawns twice; a failed or aborted rollout stays until a person
// deletes it, which is how they ask for the proposal again.
type RolloutProposer struct {
	client.Client
	// Proposer is the operator's own username, stamped as proposedBy.
	Proposer string
}

// labelProposal marks a rollout the operator proposed with the hash of what
// it proposes.
const labelProposal = "os.pk.io/proposal"

// maxProposalRounds bounds how often one need is proposed again after
// earlier rounds completed (an out-of-band change undone by a Config rollout,
// then made again).
const maxProposalRounds = 20

// +kubebuilder:rbac:groups=os.pk.io,resources=talosrollouts,verbs=get;list;watch;create

// Reconcile proposes the rollout a cluster needs, if any.
func (r *RolloutProposer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Day 0 is the provisioning path's: nothing runs yet to roll through.
	if !cluster.DeletionTimestamp.IsZero() || cluster.Status.BootstrappedAt == nil {
		return ctrl.Result{}, nil
	}
	rollouts := &osv1alpha1.TalosRolloutList{}
	if err := r.List(ctx, rollouts); err != nil {
		return ctrl.Result{}, err
	}
	existing := map[string]*osv1alpha1.TalosRollout{}
	for i := range rollouts.Items {
		ro := &rollouts.Items[i]
		if ro.Spec.ClusterRef.Name != cluster.Name {
			continue
		}
		if !rolloutFinished(ro) {
			return ctrl.Result{}, nil
		}
		existing[ro.Name] = ro
	}
	members, err := clusterMembers(ctx, r.Client, cluster.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	hosts, err := hostsByNode(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	ro := needed(cluster, members, hosts)
	if ro == nil {
		return ctrl.Result{}, nil
	}
	ro.Spec.ProposedBy = r.Proposer
	base := ro.Name
	for round := 1; round <= maxProposalRounds; round++ {
		if round > 1 {
			ro.Name = fmt.Sprintf("%s-%d", base, round)
		}
		prev, ok := existing[ro.Name]
		if !ok {
			break
		}
		// A round that completed, or whose plan moved under its approval
		// (nothing ran), is proposed again. One that failed or was aborted
		// stays: a person decides what next, and deleting it asks again.
		planChanged := meta.FindStatusCondition(prev.Status.Conditions, osv1alpha1.ConditionComplete).Reason == osv1alpha1.ReasonPlanChanged
		again := meta.IsStatusConditionTrue(prev.Status.Conditions, osv1alpha1.ConditionComplete) || planChanged
		if prev.Spec.Abort || !again {
			return ctrl.Result{}, nil
		}
		if round == maxProposalRounds {
			return ctrl.Result{}, nil
		}
	}
	if err := r.Create(ctx, ro); client.IgnoreAlreadyExists(err) != nil {
		return ctrl.Result{}, fmt.Errorf("propose rollout %s: %w", ro.Name, err)
	}
	logf.FromContext(ctx).Info("proposed rollout", "cluster", cluster.Name, "rollout", ro.Name, "kind", ro.Spec.Kind)
	return ctrl.Result{}, nil
}

// needed is the rollout the cluster needs first, nil when none.
func needed(cluster *osv1alpha1.TalosCluster, members []osv1alpha1.TalosNode, hosts map[string]string) *osv1alpha1.TalosRollout {
	var skewed, drifted []string
	for _, n := range members {
		if hosts[n.Name] == "" {
			continue
		}
		if v := n.Status.Version; v != "" && v != cluster.Spec.TalosVersion {
			skewed = append(skewed, n.Name)
		}
		if c := meta.FindStatusCondition(n.Status.Conditions, osv1alpha1.ConditionConfigInSync); c != nil &&
			c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonConfigDrift {
			drifted = append(drifted, n.Name+"="+n.Status.ConfigDigest)
		}
	}
	mk := func(kind osv1alpha1.RolloutKind, target *osv1alpha1.RolloutTarget, what string) *osv1alpha1.TalosRollout {
		h := digest.FromString(cluster.Name + "/" + string(kind) + "/" + what).Encoded()[:8]
		return &osv1alpha1.TalosRollout{
			ObjectMeta: metav1.ObjectMeta{
				Name:   fmt.Sprintf("%s-%s-%s", cluster.Name, strings.ToLower(string(kind)), h),
				Labels: map[string]string{labelProposal: h},
			},
			Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: cluster.Name}, Kind: kind, Target: target},
		}
	}
	switch {
	case len(skewed) > 0:
		return mk(osv1alpha1.RolloutTalosUpgrade, &osv1alpha1.RolloutTarget{TalosVersion: cluster.Spec.TalosVersion},
			cluster.Spec.TalosVersion)
	case cluster.Status.KubernetesVersion != "" && cluster.Status.KubernetesVersion != cluster.Spec.KubernetesVersion:
		return mk(osv1alpha1.RolloutKubernetesUpgrade, &osv1alpha1.RolloutTarget{KubernetesVersion: cluster.Spec.KubernetesVersion},
			cluster.Spec.KubernetesVersion)
	case len(drifted) > 0:
		slices.Sort(drifted)
		return mk(osv1alpha1.RolloutConfig, nil, strings.Join(drifted, ","))
	}
	return nil
}

func (r *RolloutProposer) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.TalosCluster{}).
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(clusterForNode)).
		Watches(&osv1alpha1.TalosRollout{}, handler.EnqueueRequestsFromMapFunc(clusterForRollout)).
		Named("rolloutproposer").
		Complete(r)
}

// rolloutLane wires the rollout controller and its proposer together.
type rolloutLane struct {
	run     *TalosRolloutReconciler
	propose *RolloutProposer
}

func (l rolloutLane) SetupWithManager(mgr ctrl.Manager) error {
	if err := l.run.SetupWithManager(mgr); err != nil {
		return err
	}
	return l.propose.SetupWithManager(mgr)
}
