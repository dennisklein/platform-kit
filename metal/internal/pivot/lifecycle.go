package pivot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Pause stops the source manager: its Deployment goes to zero replicas, and
// the pause waits until no manager pod runs and the leader lease is free, so
// that no pass of the old manager overlaps the copy. Then it checks again
// that no operation started in the meantime.
func (m *Mover) Pause(ctx context.Context) error {
	o := m.Options
	src := m.Source.Client
	d := &appsv1.Deployment{}
	if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
		return err
	}
	marks := map[string]any{AnnotationMovedTo: m.targetID, AnnotationSource: nil}
	if d.Spec.Replicas == nil || *d.Spec.Replicas > 0 {
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		marks[AnnotationReplicas] = strconv.Itoa(int(replicas))
	}
	if err := m.markNamespace(ctx, m.Source, marks); err != nil {
		return err
	}
	if err := scale(ctx, src, d, 0); err != nil {
		return fmt.Errorf("scale the manager on %s to zero: %w", m.Source.Name, err)
	}
	if err := m.wait(ctx, "the manager pods on "+m.Source.Name+" to stop", func(ctx context.Context) (bool, error) {
		n, err := managerPods(ctx, src, d)
		return n == 0, err
	}); err != nil {
		return err
	}
	if err := m.wait(ctx, "the leader lease on "+m.Source.Name+" to be free", func(ctx context.Context) (bool, error) {
		return leaseFree(ctx, src, o.Namespace, time.Now())
	}); err != nil {
		return err
	}
	ops := &metalv1alpha1.HostOperationList{}
	if err := src.List(ctx, ops); err != nil {
		return err
	}
	if problems := blockingOperations(ops.Items, true); len(problems) > 0 {
		return fmt.Errorf("an operation started while the manager stopped:\n  - %s\n"+
			"Start the manager on %s again and let it finish, then move: %s",
			strings.Join(problems, "\n  - "), m.Source.Name, m.resumeSourceCommand())
	}
	m.logf("paused the manager on %s", m.Source.Name)
	return nil
}

// leaseFree: nobody holds the manager's leader lease, or the holder stopped
// renewing it longer ago than the lease lasts.
func leaseFree(ctx context.Context, c client.Client, namespace string, now time.Time) (bool, error) {
	l := &coordinationv1.Lease{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: leaseName}, l); {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, err
	}
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
		return true, nil
	}
	if l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
		return false, nil
	}
	return l.Spec.RenewTime.Add(time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second).Before(now), nil
}

func scale(ctx context.Context, c client.Client, d *appsv1.Deployment, replicas int32) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": replicas}})
	if err != nil {
		return err
	}
	return c.Patch(ctx, d, client.RawPatch(types.MergePatchType, patch))
}

// Start scales the target manager to the replica count the source ran with.
func (m *Mover) Start(ctx context.Context) error {
	o := m.Options
	ns := &corev1.Namespace{}
	if err := m.Source.Client.Get(ctx, client.ObjectKey{Name: o.Namespace}, ns); err != nil {
		return err
	}
	replicas := int32(1)
	if n, err := strconv.Atoi(ns.Annotations[AnnotationReplicas]); err == nil && n > 0 {
		replicas = int32(n)
	}
	d := &appsv1.Deployment{}
	if err := m.Target.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
		return err
	}
	if err := scale(ctx, m.Target.Client, d, replicas); err != nil {
		return err
	}
	m.logf("started the manager on %s (%d replica)", m.Target.Name, replicas)
	return nil
}

// WaitRunning waits until the target manager is available and holds its
// leader lease. When it gives up, it names a claim that has not bound: the
// usual reason a manager pod does not start on a cluster without storage.
func (m *Mover) WaitRunning(ctx context.Context) error {
	o := m.Options
	err := m.wait(ctx, "the manager on "+m.Target.Name+" to run and lead", func(ctx context.Context) (bool, error) {
		d := &appsv1.Deployment{}
		if err := m.Target.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
			return false, err
		}
		if d.Status.AvailableReplicas < 1 {
			return false, nil
		}
		free, err := leaseFree(ctx, m.Target.Client, o.Namespace, time.Now())
		return !free, err
	})
	if err != nil {
		if hint := m.unboundClaims(context.WithoutCancel(ctx)); hint != "" {
			err = fmt.Errorf("%w\n%s", err, hint)
		}
	}
	return err
}

// unboundClaims names the claims in the operator namespace of the target that
// have not bound, and how to look at them; "" when there are none.
func (m *Mover) unboundClaims(ctx context.Context) string {
	o := m.Options
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := m.Target.Client.List(ctx, pvcs, client.InNamespace(o.Namespace)); err != nil {
		return ""
	}
	var lines []string
	for _, p := range pvcs.Items {
		if p.Status.Phase != corev1.ClaimBound {
			lines = append(lines, fmt.Sprintf("PersistentVolumeClaim %s/%s is %s, and the manager pod does not start until it binds: "+
				"kubectl -n %s describe pvc %s (on %s)", o.Namespace, p.Name, cmp.Or(string(p.Status.Phase), "not bound"), o.Namespace, p.Name, m.Target.Name))
		}
	}
	return strings.Join(lines, "\n")
}

// Verify compares the target with what the copy left there, and lists what
// is wrong: an operation proposed after the move (a move must never cause a
// reinstall), a claim with a hand-off record that does not read as handed
// off. pending lists what is not done yet: a host the new manager has not
// looked at since the copy.
func (m *Mover) Verify(ctx context.Context, rep *Report) (failures, pending []string, err error) {
	dst := m.Target.Client
	ops := &metalv1alpha1.HostOperationList{}
	if err := dst.List(ctx, ops); err != nil {
		return nil, nil, err
	}
	for _, op := range ops.Items {
		if ref := op.Namespace + "/" + op.Name; !rep.Operations[ref] {
			failures = append(failures, fmt.Sprintf("operation %s (%s on host %s) was proposed after the move; "+
				"it waits for approval if its claim asks for that. Look at it: pk-metal status", ref, op.Spec.Reason, op.Spec.HostRef.Name))
		}
	}
	for _, ref := range rep.HandedOff {
		ns, name, _ := strings.Cut(ref, "/")
		claim := &metalv1alpha1.HostClaim{}
		if err := dst.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, claim); err != nil {
			failures = append(failures, fmt.Sprintf("claim %s: %v", ref, err))
			continue
		}
		c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
		if c == nil || c.Reason != metalv1alpha1.ReasonHandedOff {
			got := "no Provisioned condition"
			if c != nil {
				got = c.Reason + ": " + c.Message
			}
			failures = append(failures, fmt.Sprintf("claim %s was handed off on %s but reads %s", ref, m.Source.Name, got))
		}
	}
	hosts := &metalv1alpha1.HostList{}
	if err := dst.List(ctx, hosts); err != nil {
		return nil, nil, err
	}
	// A host pass writes the conditions it computes with the generation it
	// read; some conditions (Drifted) are written only when they change. So
	// one condition at the current generation shows a pass on the target.
	for _, h := range hosts.Items {
		if !slices.ContainsFunc(h.Status.Conditions, func(c metav1.Condition) bool { return c.ObservedGeneration == h.Generation }) {
			pending = append(pending, fmt.Sprintf("host %s: the manager has not looked at generation %d yet", h.Name, h.Generation))
		}
	}
	return failures, pending, nil
}

// Settle watches the new manager for the settle time: it fails at the first
// failure Verify finds, and succeeds when nothing failed for the whole time
// and nothing is pending at its end.
func (m *Mover) Settle(ctx context.Context, rep *Report) error {
	deadline := time.Now().Add(m.Options.Settle)
	var pending []string
	for {
		failures, p, err := m.Verify(ctx, rep)
		if err != nil {
			return err
		}
		if len(failures) > 0 {
			return fmt.Errorf("the manager on %s does not settle as it should:\n  - %s", m.Target.Name, strings.Join(failures, "\n  - "))
		}
		pending = p
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.Options.Poll):
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("the manager on %s has not looked at everything within %s:\n  - %s\nRun the move again to keep watching",
			m.Target.Name, m.Options.Settle, strings.Join(pending, "\n  - "))
	}
	m.logf("the manager on %s settled: no new operation in %s", m.Target.Name, m.Options.Settle)
	return nil
}

// resumeSourceCommand is how a person gives the source its manager back.
func (m *Mover) resumeSourceCommand() string {
	return fmt.Sprintf("kubectl -n %s scale deployment %s --replicas=1 (on %s)", m.Options.Namespace, m.Options.Deployment, m.Source.Name)
}

// Run is the whole move: preflight, pause, install, copy, start, verify.
// Rerun after a failure, it continues where the last run stopped.
func (m *Mover) Run(ctx context.Context) (*Report, error) {
	plan, err := m.Preflight(ctx)
	if err != nil {
		return nil, err
	}
	for _, line := range plan.Reachable {
		m.logf("must be reachable: %s", line)
	}
	if !plan.Resume {
		if err := m.Pause(ctx); err != nil {
			return nil, err
		}
		if err := m.Install(ctx); err != nil {
			return nil, m.stopped(err)
		}
	}
	// A resumed move copies again: nothing changed on the paused source,
	// and the copy is how the verification learns what it moved.
	if plan.Resume {
		plan.Prune = false
	}
	rep, err := m.copyForRun(ctx, plan)
	if err != nil {
		return nil, m.stopped(err)
	}
	if !plan.Resume {
		if err := m.Start(ctx); err != nil {
			return nil, m.stopped(err)
		}
	}
	if err := m.WaitRunning(ctx); err != nil {
		return rep, err
	}
	return rep, m.Settle(ctx, rep)
}

// copyForRun copies, or on a resumed move only reads what was moved: the
// target manager runs, and its state is its own now.
func (m *Mover) copyForRun(ctx context.Context, plan *Plan) (*Report, error) {
	if !plan.Resume {
		return m.Copy(ctx, plan)
	}
	return m.Moved(ctx)
}

// Moved rebuilds the report of a copy that already happened from the paused
// source: which operations and hand-offs it moved.
func (m *Mover) Moved(ctx context.Context) (*Report, error) {
	rep := &Report{Copied: map[string]int{}, Operations: map[string]bool{}}
	ops := &metalv1alpha1.HostOperationList{}
	if err := m.Source.Client.List(ctx, ops); err != nil {
		return nil, err
	}
	for _, op := range ops.Items {
		rep.Operations[op.Namespace+"/"+op.Name] = true
	}
	claims := &metalv1alpha1.HostClaimList{}
	if err := m.Source.Client.List(ctx, claims); err != nil {
		return nil, err
	}
	for _, c := range claims.Items {
		if c.Status.HandOff != nil {
			rep.HandedOff = append(rep.HandedOff, c.Namespace+"/"+c.Name)
		}
	}
	return rep, nil
}

// stopped says how to go on after a move broke off before the target
// manager started.
func (m *Mover) stopped(err error) error {
	return fmt.Errorf("%w\nThe manager on %s is paused and the one on %s has not started. Fix the cause and run the move again, "+
		"or give the source its manager back: %s", err, m.Source.Name, m.Target.Name, m.resumeSourceCommand())
}
