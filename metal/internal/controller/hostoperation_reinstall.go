package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// annotationObserve on a TalosNode asks its reconciler to look at the
// machine now. The value only has to change; it names the operation and the
// time that asked.
const annotationObserve = "os.pk.io/observe"

// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch;patch

// staleCheckIn reports whether a reinstall's awaitCheckIn sees the check-in
// of the install it replaced. A reinstall boots a node that was Ready a
// moment ago, and its TalosNode is polled only every few minutes while it
// is Ready: the old Ready can outlive the reset that ended it. Only a
// check-in observed after this operation began counts, and while there is
// none the node is asked to look again, so that the old one is replaced by
// what the machine says now.
func (x *exec) staleCheckIn(ctx context.Context, node *osv1alpha1.TalosNode) (bool, error) {
	if x.op.Annotations[annotationReinstallOf] == "" || x.op.Status.Baseline == nil || x.op.Status.Baseline.CapturedAt == nil {
		return false, nil
	}
	began := x.op.Status.Baseline.CapturedAt.Time
	for _, t := range []string{osv1alpha1.ConditionReady, osv1alpha1.ConditionMachineReachable} {
		if c := meta.FindStatusCondition(node.Status.Conditions, t); c != nil && c.LastTransitionTime.After(began) {
			return false, nil
		}
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[annotationObserve] = x.op.Name + "@" + time.Now().UTC().Format(time.RFC3339)
	return true, client.IgnoreNotFound(x.r.Patch(ctx, node, patch))
}
