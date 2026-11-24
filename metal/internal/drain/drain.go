// Package drain cordons, drains and uncordons a Kubernetes Node through the
// Eviction API, the way kubectl drain does, but one non-blocking pass at a
// time: the executor calls Pass again on its next look, so a drain that waits
// for a PodDisruptionBudget holds no worker and survives a restart of the
// operator.
//
// What a pass leaves alone, and why:
//
//   - DaemonSet pods: the DaemonSet controller would put them straight back
//     on the node, cordoned or not, and they are meant to run on every node.
//   - Mirror pods (static pods the kubelet runs from a file, such as the
//     control-plane components): the API object is only a mirror, evicting it
//     stops nothing.
//
// Everything else is evicted, so that every PodDisruptionBudget holds. A pod
// whose eviction a budget refuses (HTTP 429) is asked again on the next
// pass, never deleted around the budget: the drain waits, and the caller
// decides how long. A pod with emptyDir data is evicted only when the caller
// accepts losing that data; otherwise the pass fails and names the pods.
package drain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MirrorAnnotation marks a mirror pod: the API copy of a static pod.
const MirrorAnnotation = "kubernetes.io/config.mirror"

// NodeNameField is the pod field a drain lists by. A real API server serves
// it as a field selector; a cache or a fake client needs an index under this
// name.
const NodeNameField = "spec.nodeName"

// ErrNodeNotFound: the Kubernetes Node does not exist.
var ErrNodeNotFound = errors.New("node not found")

// Cordon marks node unschedulable. changed reports whether this call did it:
// false when the node was cordoned already, by whoever.
func Cordon(ctx context.Context, c client.Client, node string) (changed bool, err error) {
	return setUnschedulable(ctx, c, node, true)
}

// Uncordon marks node schedulable again. changed is false when it already was.
func Uncordon(ctx context.Context, c client.Client, node string) (changed bool, err error) {
	return setUnschedulable(ctx, c, node, false)
}

func setUnschedulable(ctx context.Context, c client.Client, name string, want bool) (bool, error) {
	n := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, n); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("%w: %s", ErrNodeNotFound, name)
		}
		return false, err
	}
	if n.Spec.Unschedulable == want {
		return false, nil
	}
	base := n.DeepCopy()
	n.Spec.Unschedulable = want
	// The version read is the precondition: a node another writer changed
	// in between is read again on the next pass, not overwritten.
	if err := c.Patch(ctx, n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	return true, nil
}

// Options shape the evictions.
type Options struct {
	// GracePeriodSeconds overrides each pod's own termination grace period;
	// nil keeps the pod's.
	GracePeriodSeconds *int64
	// DeleteEmptyDirData lets the drain evict pods with emptyDir volumes,
	// whose data is lost with the pod.
	DeleteEmptyDirData bool
}

// Progress is what one pass saw.
type Progress struct {
	// Done: no pod the drain evicts is left on the node.
	Done bool
	// Remaining are the pods still on the node (namespace/name), terminating
	// or not, sorted.
	Remaining []string
	// Blocked are the pods whose eviction was refused this pass, with why
	// (a disruption budget, mostly), sorted.
	Blocked []string
	// Evicted counts the evictions this pass asked for.
	Evicted int
}

// Waiting says, in one line, what the drain still waits for.
func (p Progress) Waiting() string {
	if p.Done {
		return ""
	}
	msg := fmt.Sprintf("%d pod(s) still on the node: %s", len(p.Remaining), clip(p.Remaining, 5))
	if len(p.Blocked) > 0 {
		msg += "; eviction refused for " + clip(p.Blocked, 3)
	}
	return msg
}

// EmptyDirError: pods with emptyDir data would be evicted, and the caller
// did not accept losing that data.
type EmptyDirError struct{ Pods []string }

func (e *EmptyDirError) Error() string {
	return fmt.Sprintf("pods with emptyDir data on the node, whose data an eviction deletes: %s "+
		"(allow it with deleteEmptyDirData: true, or move those pods first)", strings.Join(e.Pods, ", "))
}

// Pass runs one drain pass over node: it cordons the node (changed reports
// whether this pass did), asks the Eviction API to evict every pod a drain
// evicts that is not terminating yet, and reports what is left. It never
// waits: call it again until Progress.Done.
func Pass(ctx context.Context, c client.Client, node string, opts Options) (p Progress, cordoned bool, err error) {
	if cordoned, err = Cordon(ctx, c, node); err != nil {
		return Progress{}, false, err
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.MatchingFields{NodeNameField: node}); err != nil {
		return Progress{}, cordoned, fmt.Errorf("list pods on %s: %w", node, err)
	}
	var evict []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != node || skipped(pod) {
			continue
		}
		evict = append(evict, pod)
	}
	if !opts.DeleteEmptyDirData {
		var withData []string
		for _, pod := range evict {
			if pod.DeletionTimestamp.IsZero() && hasEmptyDir(pod) && !finished(pod) {
				withData = append(withData, key(pod))
			}
		}
		if len(withData) > 0 {
			sort.Strings(withData)
			return Progress{}, cordoned, &EmptyDirError{Pods: withData}
		}
	}
	for _, pod := range evict {
		p.Remaining = append(p.Remaining, key(pod))
		if !pod.DeletionTimestamp.IsZero() {
			continue // evicted already; waiting for it to go
		}
		ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
		if opts.GracePeriodSeconds != nil {
			ev.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: opts.GracePeriodSeconds}
		}
		err := c.SubResource("eviction").Create(ctx, pod, ev)
		switch {
		case err == nil:
			p.Evicted++
		case apierrors.IsNotFound(err):
			// Gone between the list and the eviction: what was wanted.
		case refused(err):
			p.Blocked = append(p.Blocked, key(pod)+" ("+reason(err)+")")
		default:
			return Progress{}, cordoned, fmt.Errorf("evict %s: %w", key(pod), err)
		}
	}
	sort.Strings(p.Remaining)
	sort.Strings(p.Blocked)
	p.Done = len(p.Remaining) == 0
	return p, cordoned, nil
}

// skipped: a pod a drain leaves on the node (see the package comment).
func skipped(pod *corev1.Pod) bool {
	if _, mirror := pod.Annotations[MirrorAnnotation]; mirror {
		return true
	}
	if ref := metav1.GetControllerOf(pod); ref != nil && ref.Kind == "DaemonSet" {
		return true
	}
	return false
}

func hasEmptyDir(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.EmptyDir != nil })
}

// finished: the pod's containers ended; nothing is lost with its emptyDir
// that a running workload still needs.
func finished(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// refused: the API server declined this eviction for now. 429 is a
// disruption budget at its limit; the rest are a budget it cannot decide on
// (two budgets match the pod) or a server that is busy. All of them are
// asked again on the next pass.
func refused(err error) bool {
	return apierrors.IsTooManyRequests(err) || apierrors.IsInternalError(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err)
}

// reason is the API server's own words, which name the budget.
func reason(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if d := status.Status().Details; d != nil {
			for _, c := range d.Causes {
				if c.Type == policyv1.DisruptionBudgetCause && c.Message != "" {
					return c.Message
				}
			}
		}
		if m := status.Status().Message; m != "" {
			return m
		}
	}
	return err.Error()
}

func key(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }

// clip joins at most n items and says how many more there are.
func clip(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}
