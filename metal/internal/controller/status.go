package controller

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Two objects here have their status written by two reconcilers each: the
// ManagementEndpoint, observed by one lane and converged by another, and the
// Host, projected by one and converged by another. Both pairs used to write the
// whole status from a cached read, which meant whichever wrote second erased
// whatever the other had learned since its own read.
//
// For most of these fields that only cost a poll — they are conclusions the
// next pass recomputes. It is not true of the memories: status.identityPushed
// records that a manager accepted a certificate and restarted,
// status.firmwareProposed records an operation already created, and
// status.lastResetTime is the observation the drift check compares against.
// A memory that can be silently dropped is not a memory.
//
// So each writer applies its status server-side under its own field manager and
// names only the fields it owns. The API server keeps the ledger; conditions
// merge per type, which both CRDs already declare as map lists. Two rules follow
// from apply semantics and are easy to get wrong: naming a field CLAIMS it, and
// a field its owner stops naming is DELETED, not left alone.
const (
	fieldOwnerEndpoint        = "pk-metal-endpoint"
	fieldOwnerConvergence     = "pk-metal-endpoint-convergence"
	fieldOwnerHost            = "pk-metal-host"
	fieldOwnerHostConvergence = "pk-metal-host-convergence"
)

// convergenceHostConditions are the host conditions the convergence reconciler
// maintains. The host reconciler maintains every other one, and neither may
// name the other's.
var convergenceHostConditions = []string{
	metalv1alpha1.ConditionBiosConverged,
	metalv1alpha1.ConditionFirmwareConverged,
	metalv1alpha1.ConditionSystemFirmwareConverged,
}

func isConvergenceHostCondition(c metav1.Condition) bool {
	return slices.Contains(convergenceHostConditions, c.Type)
}

// The shares below are what each field manager names when it applies its
// part of a status. The reconcilers stamp the phase onto their share before
// they apply it; ApplyStatusShares writes a whole status at once.

// hostProjectedShare is the host reconciler's part of a host's status: all of
// it but the convergence lane's conditions and plan memory.
func hostProjectedShare(host *metalv1alpha1.Host) *metalv1alpha1.Host {
	apply := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: host.Name},
		Status:     *host.Status.DeepCopy(),
	}
	apply.Status.FirmwareProposed = nil
	apply.Status.SystemFirmwareProposed = nil
	apply.Status.Conditions = slices.DeleteFunc(apply.Status.Conditions, isConvergenceHostCondition)
	return apply
}

// hostConvergenceShare is the convergence lane's part: its two conditions and
// the firmware plan memory.
func hostConvergenceShare(host *metalv1alpha1.Host) *metalv1alpha1.Host {
	apply := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: host.Name},
	}
	apply.Status.FirmwareProposed = host.Status.FirmwareProposed
	apply.Status.SystemFirmwareProposed = host.Status.SystemFirmwareProposed
	for _, t := range convergenceHostConditions {
		if c := meta.FindStatusCondition(host.Status.Conditions, t); c != nil {
			apply.Status.Conditions = append(apply.Status.Conditions, *c)
		}
	}
	return apply
}

// endpointObservedShare is the endpoint observer's part of an endpoint's
// status: all of it but the Configured condition and the push memory.
func endpointObservedShare(ep *metalv1alpha1.ManagementEndpoint) *metalv1alpha1.ManagementEndpoint {
	apply := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: ep.Name},
		Status:     *ep.Status.DeepCopy(),
	}
	apply.Status.IdentityPushed = nil
	apply.Status.Conditions = slices.DeleteFunc(apply.Status.Conditions,
		func(c metav1.Condition) bool { return c.Type == metalv1alpha1.ConditionConfigured })
	return apply
}

// endpointConvergenceShare is the endpoint convergence lane's part: the
// Configured condition and the push memory.
func endpointConvergenceShare(ep *metalv1alpha1.ManagementEndpoint) *metalv1alpha1.ManagementEndpoint {
	apply := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: ep.Name},
	}
	if c := meta.FindStatusCondition(ep.Status.Conditions, metalv1alpha1.ConditionConfigured); c != nil {
		apply.Status.Conditions = []metav1.Condition{*c}
	}
	apply.Status.IdentityPushed = ep.Status.IdentityPushed
	return apply
}

// ApplyStatusShares writes obj's whole status the way the reconcilers write
// it: one server-side apply per field manager, each naming only its own
// fields. pk-metal pivot uses it to copy a status onto another cluster, so
// that the reconcilers there find the field ownership they expect: a field
// owned by the wrong manager would never be removed when its real owner stops
// naming it. It reports false, and writes nothing, for a kind whose status
// has a single writer; such a status is written with an ordinary update.
func ApplyStatusShares(ctx context.Context, c client.Client, obj client.Object) (bool, error) {
	type share struct {
		owner string
		obj   client.Object
	}
	var shares []share
	switch o := obj.(type) {
	case *metalv1alpha1.Host:
		shares = []share{{fieldOwnerHost, hostProjectedShare(o)}, {fieldOwnerHostConvergence, hostConvergenceShare(o)}}
	case *metalv1alpha1.ManagementEndpoint:
		shares = []share{{fieldOwnerEndpoint, endpointObservedShare(o)}, {fieldOwnerConvergence, endpointConvergenceShare(o)}}
		if cond := meta.FindStatusCondition(o.Status.Conditions, metalv1alpha1.ConditionDeletionHeld); cond != nil {
			held := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: o.Name}}
			held.Status.Conditions = []metav1.Condition{*cond}
			shares = append(shares, share{fieldOwnerProtection, held})
		}
	default:
		return false, nil
	}
	for _, s := range shares {
		if err := applyOwnedStatus(ctx, c, s.obj, s.owner); err != nil {
			return true, err
		}
	}
	return true, nil
}

// setCondition is the one way a reconciler sets a condition. It always stamps
// the object's generation, so a condition can never claim to describe a spec it
// did not see.
func setCondition(conds *[]metav1.Condition, gen int64, t string, s metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen,
	})
}

// updateStatusIfChanged writes obj's status when after differs from before,
// and does nothing otherwise. It uses Update, not a patch: these objects have
// one status writer, and a resourceVersion conflict is how a pass learns it
// worked from a stale read.
func updateStatusIfChanged[S any](ctx context.Context, c client.Client, obj client.Object, before, after *S) error {
	if equality.Semantic.DeepEqual(before, after) {
		return nil
	}
	return c.Status().Update(ctx, obj)
}

// applyOwnedStatus sends obj's status as a server-side apply under owner. The
// caller passes an object carrying ONLY what that owner means to own. The
// apiVersion and kind come from the client's scheme, so the caller does not
// spell them out.
func applyOwnedStatus(ctx context.Context, c client.Client, obj client.Object, owner string) error {
	gvk, err := c.GroupVersionKindFor(obj)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	// A status apply carries the whole object and the server reads status from
	// it; the rest is noise on the wire and in the field ledger.
	delete(u, "spec")
	unstructured.RemoveNestedField(u, "metadata", "creationTimestamp")
	return c.Status().Apply(ctx,
		client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}),
		client.FieldOwner(owner), client.ForceOwnership)
}
