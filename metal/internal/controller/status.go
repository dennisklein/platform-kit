package controller

import (
	"context"
	"slices"

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
}

func isConvergenceHostCondition(c metav1.Condition) bool {
	return slices.Contains(convergenceHostConditions, c.Type)
}

// applyOwnedStatus sends obj's status as a server-side apply under owner. The
// caller passes an object carrying ONLY what that owner means to own.
func applyOwnedStatus(ctx context.Context, c client.Client, obj client.Object, owner string) error {
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
