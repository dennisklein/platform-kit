package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// talosNodeMemberFinalizer holds a deleted TalosNode until its node is out of
// its cluster. Without it a TalosNode could vanish while its machine still
// runs, with nothing left to say which cluster that machine is a member of,
// and the steps that take it out (drain, etcd leave, reset) would have no
// node to act on.
const talosNodeMemberFinalizer = "os.pk.io/member"

// removalRecheck paces the re-reading of etcd while a deletion waits for a
// member to go.
const removalRecheck = 30 * time.Second

// TalosNodeMemberReconciler keeps the member finalizer on every TalosNode and
// lets a deleted one go once nothing is left of it in its cluster: no claim
// records an install of it, no unfinished operation acts on it, and, for a
// control plane of a running cluster, etcd lists no member under its name.
// It proposes nothing itself: the claim that holds the node's machine
// proposes the removal (HostClaimReconciler.removeNode). While the deletion
// waits, the RemovalBlocked condition says for what. The TalosNode
// reconciler writes no status during deletion, so this one is the only
// status writer then.
type TalosNodeMemberReconciler struct {
	client.Client
	// Dial reads etcd membership; nil skips the etcd check.
	Dial talos.Dialer
	// CredentialsNamespace holds the clusters' secrets bundles.
	CredentialsNamespace string
	// OperationsNamespace holds the operations.
	OperationsNamespace string
}

// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list;watch

// Reconcile adds the finalizer, and removes it from a deleted node whose
// removal is complete.
func (r *TalosNodeMemberReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if node.DeletionTimestamp.IsZero() {
		if controllerutil.AddFinalizer(node, talosNodeMemberFinalizer) {
			return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, node))
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(node, talosNodeMemberFinalizer) {
		return ctrl.Result{}, nil
	}
	reason, msg, err := r.blocked(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		orig := node.Status.DeepCopy()
		setCondition(&node.Status.Conditions, node.Generation, osv1alpha1.ConditionRemovalBlocked, metav1.ConditionTrue, reason, msg)
		if !equality.Semantic.DeepEqual(orig, &node.Status) {
			if err := r.Status().Update(ctx, node); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{RequeueAfter: removalRecheck}, nil
	}
	controllerutil.RemoveFinalizer(node, talosNodeMemberFinalizer)
	if err := r.Update(ctx, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logf.FromContext(ctx).Info("TalosNode left its cluster", "node", node.Name, "cluster", node.Spec.ClusterRef.Name)
	return ctrl.Result{}, nil
}

// blocked says what the deletion of node still waits for, "" for nothing.
func (r *TalosNodeMemberReconciler) blocked(ctx context.Context, node *osv1alpha1.TalosNode) (string, string, error) {
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return "", "", err
	}
	for _, c := range claims.Items {
		if h := c.Status.HandOff; h != nil && h.Node == node.Name {
			return osv1alpha1.ReasonMachineAssigned, fmt.Sprintf(
				"host %s still runs or ran this node for claim %s/%s; the claim removes it (drain, etcd leave, reset) once a person approves: "+
					"pk-metal approve. See: kubectl get hostclaim -n %s %s", h.Host, c.Namespace, c.Name, c.Namespace, c.Name), nil
		}
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.InNamespace(r.OperationsNamespace)); err != nil {
		return "", "", err
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if opFinished(op) {
			continue
		}
		for _, n := range opNodes(op) {
			if n == node.Name {
				return osv1alpha1.ReasonMachineAssigned, fmt.Sprintf(
					"operation %s/%s acts on this node and has not finished", op.Namespace, op.Name), nil
			}
		}
	}
	if node.Spec.Role != osv1alpha1.RoleControlPlane || r.Dial == nil {
		return "", "", nil
	}
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		// A cluster that is gone has no etcd to leave a member in.
		return "", "", client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() || cluster.Status.BootstrappedAt == nil {
		return "", "", nil
	}
	v, why, err := clusterEtcd(ctx, r.Client, r.Dial, r.CredentialsNamespace, cluster)
	if err != nil {
		return "", "", err
	}
	_, acked := node.Annotations[metalv1alpha1.AnnotationAckEtcdRisk]
	switch m := v.member(node.Name); {
	case !v.known() && !acked:
		return osv1alpha1.ReasonEtcdUnreadable, fmt.Sprintf(
			"nothing says whether etcd still lists this node: %s. To let it go anyway: kubectl annotate talosnode %s %s=\"<reason>\"",
			why, node.Name, metalv1alpha1.AnnotationAckEtcdRisk), nil
	case m != nil:
		return osv1alpha1.ReasonEtcdMemberRemains, fmt.Sprintf(
			"etcd of cluster %s still lists member %s (ID %s); a removal of the node takes it out. If no machine runs the node any more, "+
				"remove the member through a healthy control plane: talosctl -n <control-plane> etcd remove-member %s",
			cluster.Name, m.Hostname, memberIDHex(m.ID), memberIDHex(m.ID)), nil
	}
	return "", "", nil
}

func (r *TalosNodeMemberReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.TalosNode{}).
		Watches(&metalv1alpha1.HostClaim{}, handler.EnqueueRequestsFromMapFunc(nodesOfClaim)).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(nodesOfOperation)).
		Named("talosnode-member").
		Complete(r)
}

// nodesOfClaim maps a claim event to the nodes it names and records.
func nodesOfClaim(_ context.Context, obj client.Object) []ctrl.Request {
	c := obj.(*metalv1alpha1.HostClaim)
	var reqs []ctrl.Request
	if c.Spec.OS != nil {
		reqs = append(reqs, requestFor(c.Spec.OS.NodeRef.Name))
	}
	if h := c.Status.HandOff; h != nil {
		reqs = append(reqs, requestFor(h.Node))
	}
	return reqs
}

// nodesOfOperation maps an operation event to the nodes its actions name.
func nodesOfOperation(_ context.Context, obj client.Object) []ctrl.Request {
	var reqs []ctrl.Request
	for _, n := range opNodes(obj.(*metalv1alpha1.HostOperation)) {
		reqs = append(reqs, requestFor(n))
	}
	return reqs
}
