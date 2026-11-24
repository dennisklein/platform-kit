package pivot

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Plan is what the preflight found out about the move.
type Plan struct {
	// Resume: the target already runs the manager this move started; only
	// the verification is left to do.
	Resume bool
	// Prune: the target holds an older copy of the operator's objects (a
	// move back, or a copy that broke off). Objects the source no longer has
	// are removed from it.
	Prune bool
	// Reachable lists what must be reachable at the target, for the
	// person to check: the move cannot test it.
	Reachable []string
	// Claims are the PersistentVolumeClaims the manager mounts, and how
	// each gets to the target.
	Claims []ClaimMove
}

// opState is how far one operation got, from its conditions and baseline.
type opState int

const (
	opDone opState = iota
	opRunning
	opWaiting
	opApproved // approved, not started: it starts wherever the manager runs
)

func operationState(op *metalv1alpha1.HostOperation) opState {
	if c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted); c != nil &&
		c.Status != metav1.ConditionUnknown {
		return opDone
	}
	if op.Status.Baseline != nil {
		return opRunning
	}
	required := op.Spec.Approval.Required == nil || *op.Spec.Approval.Required
	if required && op.Spec.Approval.ApprovedBy == "" {
		return opWaiting
	}
	return opApproved
}

// blockingOperations says which operations stop a move: one that has
// started and not finished would be cut off halfway, and one waiting for
// approval would be approved on a cluster nobody looks at any more.
func blockingOperations(ops []metalv1alpha1.HostOperation, forceWaiting bool) []string {
	var out []string
	for i := range ops {
		op := &ops[i]
		ref := op.Namespace + "/" + op.Name
		switch operationState(op) {
		case opRunning:
			out = append(out, fmt.Sprintf("operation %s is running on host %s: wait until it has finished (pk-metal status)",
				ref, op.Spec.HostRef.Name))
		case opWaiting:
			if !forceWaiting {
				out = append(out, fmt.Sprintf("operation %s waits for approval: approve it and let it finish (pk-metal approve %s), "+
					"delete it, or move it as it is with --force-waiting", ref, ref))
			}
		case opDone, opApproved:
		}
	}
	return out
}

// Preflight checks that the move can start, and finds out whether it is a
// rerun. It changes nothing.
func (m *Mover) Preflight(ctx context.Context) (*Plan, error) {
	o := m.Options
	src, dst := m.Source.Client, m.Target.Client
	srcNS := &corev1.Namespace{}
	if err := src.Get(ctx, client.ObjectKey{Name: o.Namespace}, srcNS); err != nil {
		return nil, fmt.Errorf("no namespace %s on %s, so no pk-metal operator to move: %w", o.Namespace, m.Source.Name, err)
	}
	if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, &appsv1.Deployment{}); err != nil {
		return nil, fmt.Errorf("no Deployment %s/%s on %s: %w", o.Namespace, o.Deployment, m.Source.Name, err)
	}

	dstNS := &corev1.Namespace{}
	if err := dst.Get(ctx, client.ObjectKey{Name: o.Namespace}, dstNS); client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	fromHere := dstNS.Annotations[AnnotationSource] == m.sourceID
	backHere := dstNS.Annotations[AnnotationMovedTo] == m.sourceID
	running, err := managerRuns(ctx, dst, o.Namespace, o.Deployment)
	if err != nil {
		return nil, err
	}
	if running {
		if fromHere && srcNS.Annotations[AnnotationMovedTo] == m.targetID {
			return &Plan{Resume: true}, nil
		}
		return nil, fmt.Errorf("%s already runs a pk-metal manager (Deployment %s/%s): two managers would act on the same machines. "+
			"Move from that cluster instead, or scale it to zero first: kubectl -n %s scale deployment %s --replicas=0",
			m.Target.Name, o.Namespace, o.Deployment, o.Namespace, o.Deployment)
	}

	var problems []string
	ops := &metalv1alpha1.HostOperationList{}
	if err := src.List(ctx, ops); err != nil {
		return nil, err
	}
	problems = append(problems, blockingOperations(ops.Items, o.ForceWaiting)...)
	deleting, err := deletingObjects(ctx, src, m.movedKinds())
	if err != nil {
		return nil, err
	}
	problems = append(problems, deleting...)
	busy, err := inFlight(ctx, src, m.movedKinds())
	if err != nil {
		return nil, err
	}
	problems = append(problems, busy...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("the operator cannot move now:\n  - %s", strings.Join(problems, "\n  - "))
	}

	plan := &Plan{}
	held, err := holdsObjects(ctx, dst, m.movedKinds())
	if err != nil {
		return nil, err
	}
	if held != "" {
		if !fromHere && !backHere {
			return nil, fmt.Errorf("%s already holds pk-metal objects (%s) that were not moved there from %s. "+
				"A move does not merge two fleets: remove them first, or move from the cluster they belong to",
				m.Target.Name, held, m.Source.Name)
		}
		plan.Prune = true
	}
	if plan.Claims, err = m.claimMoves(ctx); err != nil {
		return nil, err
	}
	if plan.Reachable, err = m.reachability(ctx); err != nil {
		return nil, err
	}
	return plan, nil
}

// managerRuns reports whether a manager runs, or is meant to run, in the
// cluster: the Deployment asks for a replica or still has one, or a manager
// pod exists.
func managerRuns(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	d := &appsv1.Deployment{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, d); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	if (d.Spec.Replicas == nil || *d.Spec.Replicas > 0) || d.Status.Replicas > 0 {
		return true, nil
	}
	n, err := managerPods(ctx, c, d)
	return n > 0, err
}

func managerPods(ctx context.Context, c client.Client, d *appsv1.Deployment) (int, error) {
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels) == 0 {
		return 0, fmt.Errorf("deployment %s/%s has no label selector to find its pods by", d.Namespace, d.Name)
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(d.Namespace), client.MatchingLabels(d.Spec.Selector.MatchLabels)); err != nil {
		return 0, err
	}
	return len(pods.Items), nil
}

// deletingObjects lists moved objects that are being deleted: their
// finalizer's work (a release) is the source manager's, and it must finish
// there.
func deletingObjects(ctx context.Context, c client.Client, kinds []movedKind) ([]string, error) {
	var out []string
	for _, k := range kinds {
		if k.core {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
		if err := c.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return nil, err
		}
		for _, u := range list.Items {
			if u.GetDeletionTimestamp() != nil {
				out = append(out, fmt.Sprintf("%s %s is being deleted: let the operator finish that first", k.gvk.Kind, objectRef(&u)))
			}
		}
	}
	return out, nil
}

// holdsObjects names a few of the operator's objects the cluster holds, or ""
// for none. A cluster without the CRDs holds none.
func holdsObjects(ctx context.Context, c client.Client, kinds []movedKind) (string, error) {
	var names []string
	for _, k := range kinds {
		if k.core {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
		if err := c.List(ctx, list, client.Limit(1)); err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		if len(list.Items) > 0 {
			names = append(names, k.gvk.Kind+" "+objectRef(&list.Items[0]))
		}
	}
	return strings.Join(names, ", "), nil
}

func objectRef(o client.Object) string {
	if o.GetNamespace() != "" {
		return o.GetNamespace() + "/" + o.GetName()
	}
	return o.GetName()
}

// reachability lists what the operator needs to reach from its new place,
// and what needs to reach it. The move cannot test it: the BMC network is
// usually not reachable from the workstation that runs the move.
func (m *Mover) reachability(ctx context.Context) ([]string, error) {
	o := m.Options
	src, dst := m.Source.Client, m.Target.Client
	var out []string

	eps := &metalv1alpha1.ManagementEndpointList{}
	if err := src.List(ctx, eps); err != nil {
		return nil, err
	}
	var bmcs []string
	for _, ep := range eps.Items {
		if u, err := url.Parse(ep.Spec.Address); err == nil && u.Host != "" {
			bmcs = append(bmcs, u.Host)
		}
	}
	slices.Sort(bmcs)
	bmcs = slices.Compact(bmcs)
	if len(bmcs) > 0 {
		out = append(out, fmt.Sprintf("the BMCs, over HTTPS, from every node of %s: %s", m.Target.Name, strings.Join(bmcs, ", ")))
	}

	nodes := &osv1alpha1.TalosNodeList{}
	if err := src.List(ctx, nodes); err != nil {
		return nil, err
	}
	var talos []string
	for i := range nodes.Items {
		if a := talosAddress(&nodes.Items[i]); a != "" {
			talos = append(talos, net.JoinHostPort(a, "50000"))
		}
	}
	if len(talos) > 0 {
		out = append(out, fmt.Sprintf("the Talos API of the machines it installs, from every node of %s: %s", m.Target.Name, strings.Join(talos, ", ")))
	}

	media := o.MediaURL
	if media == "" {
		d := &appsv1.Deployment{}
		if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
			return nil, err
		}
		media = argValue(d, "--media-url")
	}
	var addrs []string
	k8sNodes := &corev1.NodeList{}
	if err := dst.List(ctx, k8sNodes); err != nil {
		return nil, err
	}
	for _, n := range k8sNodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				addrs = append(addrs, a.Address)
			}
		}
	}
	line := fmt.Sprintf("the media service, from every BMC: NodePort 30443 on the nodes of %s (%s)", m.Target.Name, strings.Join(addrs, ", "))
	if media == "" {
		line += "; the manager has no --media-url, so it installs nothing"
	} else {
		line += "; the manager tells the BMCs to fetch from " + media
		if o.MediaURL == "" {
			line += ", the source's address: if that does not lead to the new cluster, give --media-url"
		}
	}
	out = append(out, line)
	return out, nil
}

// talosAddress is the address the operator dials a TalosNode at.
func talosAddress(n *osv1alpha1.TalosNode) string {
	if n.Spec.Address != "" {
		return n.Spec.Address
	}
	for _, l := range n.Spec.Network.Links {
		for _, a := range l.Addresses {
			host, _, _ := strings.Cut(a, "/")
			return host
		}
	}
	return n.Status.Address
}

// argValue is the value of a --name=value argument of the manager container.
func argValue(d *appsv1.Deployment, name string) string {
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, a := range c.Args {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v
			}
		}
	}
	return ""
}
