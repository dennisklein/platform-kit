package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
)

// Bounds of the steps a running node's disruptive proposal adds. The approver
// sees them in the operation and can ask for another proposal by changing
// nothing but these: they are part of the plan, not of the desired state.
const (
	// disruptDrainTimeout bounds the drain, budget waits included. A
	// PodDisruptionBudget that holds longer fails the step, and the node
	// stays cordoned for a person to look at.
	disruptDrainTimeout = int32(900)
	// disruptReadyTimeout bounds the wait for the node, and on a control
	// plane for etcd, after the machine is back on. A firmware update can
	// add several reboots of its own to the first POST.
	disruptReadyTimeout = int32(3600)
	// talosRebootPowerCycle is TalosRebootAction.mode for a reboot through
	// the firmware.
	talosRebootPowerCycle = "PowerCycle"
)

// installedNode returns the TalosNode the host runs, when it runs one: the
// host's claim holds the hand-off record of this host to that node. A host
// with no claim, a claim still installing, or a claim of another host runs
// nothing the operator knows of, and gets the plain proposal as before.
//
// hold is non-empty when the record names a node that is not there: the
// machine runs something, and without its TalosNode the operator cannot drain
// it, so no disruptive change is proposed for it.
func (r *ConvergenceReconciler) installedNode(ctx context.Context, host *metalv1alpha1.Host) (node *osv1alpha1.TalosNode, hold string, err error) {
	ref := host.Spec.ClaimRef
	if ref == nil {
		return nil, "", nil
	}
	claim := &metalv1alpha1.HostClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, claim); err != nil {
		return nil, "", client.IgnoreNotFound(err)
	}
	h := claim.Status.HandOff
	if h == nil || h.Host != host.Name || h.Node == "" {
		return nil, "", nil
	}
	node = &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, client.ObjectKey{Name: h.Node}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("host %s runs TalosNode %s (claim %s/%s), which no longer exists; "+
				"the node cannot be drained, so no change that reboots it is proposed", host.Name, h.Node, ref.Namespace, ref.Name), nil
		}
		return nil, "", err
	}
	return node, "", nil
}

// disruptivePlan wraps the actions that stage a change (BIOS attributes,
// system firmware) into the sequence that applies it to a machine at a
// reset. Without a node it is the staging and a power cycle, as for a
// machine that runs nothing yet. With one the node's workloads are moved off
// first and the node is waited for afterwards:
//
//	stage …                  staging changes nothing running, and a BMC that
//	                         refuses it fails the operation before any pod moves
//	waitEtcdHealthy          control plane only: never take a member out of an
//	                         etcd that is already short of one
//	cordon, drain            PodDisruptionBudgets hold; a refusal is waited out
//	talosReboot PowerCycle   Talos stops services and unmounts in order, then
//	                         restarts through the firmware. Its default reboot
//	                         can be a kexec, which never reaches the firmware
//	                         and would leave the staged change pending
//	waitNodeReady            Talos running and ready, Kubernetes Node Ready
//	waitEtcdHealthy          control plane only: its member is back and voting
//	uncordon
//
// The node is part of the plan: the same change on a host that now runs
// another node (or none) is another plan, with another hash, so a proposal
// made for the old situation is withdrawn instead of approved by mistake.
func disruptivePlan(node *osv1alpha1.TalosNode, stage []metalv1alpha1.HostOperationAction) []metalv1alpha1.HostOperationAction {
	if node == nil {
		return append(stage, metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	}
	ref := metalv1alpha1.Reference{Name: node.Name}
	cp := node.Spec.Role == osv1alpha1.RoleControlPlane
	etcd := metalv1alpha1.HostOperationAction{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{
		NodeRef: ref, TimeoutSeconds: disruptReadyTimeout}}

	actions := append([]metalv1alpha1.HostOperationAction(nil), stage...)
	if cp {
		actions = append(actions, etcd)
	}
	actions = append(actions,
		metalv1alpha1.HostOperationAction{Cordon: &metalv1alpha1.CordonAction{NodeRef: ref}},
		metalv1alpha1.HostOperationAction{Drain: &metalv1alpha1.DrainAction{NodeRef: ref, TimeoutSeconds: disruptDrainTimeout}},
		metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref, Mode: talosRebootPowerCycle}},
		metalv1alpha1.HostOperationAction{WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{NodeRef: ref, TimeoutSeconds: disruptReadyTimeout}},
	)
	if cp {
		actions = append(actions, etcd)
	}
	return append(actions, metalv1alpha1.HostOperationAction{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: ref}})
}

// disruptiveHash is the plan hash of a change applied through
// disruptivePlan. Without a node it is the desired state's own hash, the
// hash proposals had before nodes were drained, so an existing plan memory
// still matches; with one it binds the node and its role as well.
func disruptiveHash(desired any, node *osv1alpha1.TalosNode) string {
	if node == nil {
		return planHash(desired)
	}
	return planHash(struct {
		Desired any    `json:"desired"`
		Node    string `json:"node"`
		Role    string `json:"role"`
	}{desired, node.Name, string(node.Spec.Role)})
}

// withdrawPlan takes back a plan's proposal while nobody approved it, and
// forgets the plan when it did: the plan was made for a situation that is
// gone, and a plan memory pointing at a withdrawn proposal would later read
// as a person's refusal. An approved, running or finished operation stays,
// and so does the memory of it.
func (r *ConvergenceReconciler) withdrawPlan(ctx context.Context, plan **metalv1alpha1.ProposedPlan) error {
	if *plan == nil {
		return nil
	}
	ns, name, _ := strings.Cut((*plan).Operation, "/")
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, op); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !op.AwaitsApproval() {
		return nil
	}
	logf.FromContext(ctx).Info("withdrew proposal for a node that is gone", "operation", (*plan).Operation)
	if err := deleteOp(ctx, r.Client, r.Audit, op, audit.DeleteWithdrawn,
		"the node it was planned for is gone before anyone approved it", client.Preconditions{UID: &op.UID}); client.IgnoreNotFound(err) != nil {
		return err
	}
	*plan = nil
	return nil
}

// hostOfClaim re-converges the host a claim is bound to: the hand-off record
// on the claim decides whether a change is applied to a running node.
func hostOfClaim(_ context.Context, obj client.Object) []ctrl.Request {
	claim := obj.(*metalv1alpha1.HostClaim)
	var reqs []ctrl.Request
	if claim.Status.HostRef != nil {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Status.HostRef.Name}})
	}
	if h := claim.Status.HandOff; h != nil && (claim.Status.HostRef == nil || h.Host != claim.Status.HostRef.Name) {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: h.Host}})
	}
	return reqs
}

// hostsRunningNode re-converges the hosts whose claim records the hand-off of
// this node: a node created, deleted or moved to another role changes the
// plan.
func (r *ConvergenceReconciler) hostsRunningNode(ctx context.Context, obj client.Object) []ctrl.Request {
	claims := &metalv1alpha1.HostClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, c := range claims.Items {
		if h := c.Status.HandOff; h != nil && h.Node == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: h.Host}})
		}
	}
	return reqs
}
