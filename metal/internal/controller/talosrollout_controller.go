package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/k8supgrade"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// ImageResolver pins a tagged image reference to the digest its registry
// serves now (oci.Resolve in production).
type ImageResolver func(ctx context.Context, ref string) (string, error)

// TalosRolloutReconciler plans and runs TalosRollouts.
//
// Until a person approves it, a rollout is planned on every pass: the plan
// (which nodes, in which order, each node's actions and exact target) and its
// hash go into status, so the person approves what they read. The approval
// names that hash. From then on the plan is frozen and run step by step:
// control planes one at a time, then (for a Kubernetes upgrade) kube-proxy,
// then the workers, at most maxUnavailableWorkers at once. Each node's step
// is a HostOperation the operator creates with no approval of its own; it
// carries rolloutRef{name, planHash}, and the executor runs it only while
// this rollout's approval covers exactly that plan and those actions.
//
// Between nodes it pauses: always after the first node until a person
// resumes (os.pk.io/resume=<planHash>), whenever spec.paused is set, and
// while a live health gate fails (every etcd member healthy and voting,
// every Kubernetes Node of the cluster Ready, read now rather than from a
// status polled minutes ago).
type TalosRolloutReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace is the operator's: config Secrets and credentials live
	// there.
	Namespace string
	// OperationsNamespace receives the rollout's HostOperations.
	OperationsNamespace string
	// Proposer is the operator's own username, stamped as proposedBy.
	Proposer string
	// Factory names installer images; ResolveImage pins them by digest.
	Factory      *render.Factory
	ResolveImage ImageResolver
	// Dial reaches the machines for the etcd gate and the leader.
	Dial talos.Dialer
	// Workload reaches the cluster's Kubernetes API (k8supgrade.Dial in
	// production).
	Workload k8supgrade.Dialer
	// GateInterval paces re-checks while a gate holds or a step runs.
	GateInterval time.Duration

	// resolved caches pinned images by reference for a while, so that an
	// unapproved rollout re-planned every pass does not ask the registry
	// every pass.
	resolved sync.Map
}

const (
	defaultGateInterval = 15 * time.Second
	// replanInterval paces the planning of an unapproved rollout.
	replanInterval = time.Minute
	resolveTTL     = 10 * time.Minute
)

type resolvedImage struct {
	ref string
	at  time.Time
}

// +kubebuilder:rbac:groups=os.pk.io,resources=talosrollouts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=os.pk.io,resources=talosrollouts/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosrollouts/finalizers,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=pk-metal

// Reconcile plans an unapproved rollout and runs an approved one.
func (r *TalosRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ro := &osv1alpha1.TalosRollout{}
	if err := r.Get(ctx, req.NamespacedName, ro); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ro.DeletionTimestamp.IsZero() || rolloutFinished(ro) {
		return ctrl.Result{}, nil
	}
	orig := ro.DeepCopy()
	var res ctrl.Result
	var err error
	if ro.Spec.Approval.ApprovedBy == "" {
		res, err = r.planPass(ctx, ro)
	} else {
		res, err = r.runPass(ctx, ro)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := updateStatusIfChanged(ctx, r.Client, ro, &orig.Status, &ro.Status); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *TalosRolloutReconciler) set(ro *osv1alpha1.TalosRollout, t string, s metav1.ConditionStatus, reason, msg string) {
	setCondition(&ro.Status.Conditions, ro.Generation, t, s, reason, msg)
}

func (r *TalosRolloutReconciler) gateInterval() time.Duration {
	return cmp.Or(r.GateInterval, defaultGateInterval)
}

// planPass writes the current plan of an unapproved rollout.
func (r *TalosRolloutReconciler) planPass(ctx context.Context, ro *osv1alpha1.TalosRollout) (ctrl.Result, error) {
	r.set(ro, osv1alpha1.ConditionApproved, metav1.ConditionFalse, osv1alpha1.ReasonAwaitingApproval,
		"nobody approved the plan yet: pk-metal rollout show "+ro.Name+", then pk-metal rollout approve "+ro.Name)
	r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionFalse, osv1alpha1.ReasonRolloutNotStarted, "waiting for approval")
	if ro.Spec.Abort {
		r.set(ro, osv1alpha1.ConditionComplete, metav1.ConditionFalse, osv1alpha1.ReasonRolloutAborted, "aborted before it was approved")
		return ctrl.Result{}, nil
	}
	plan, blocked, err := r.plan(ctx, ro)
	if err != nil {
		return ctrl.Result{}, err
	}
	ro.Status.Plan = plan
	ro.Status.PlanHash = planHashOf(ro, plan)
	ro.Status.Steps = make([]osv1alpha1.RolloutStepStatus, len(plan))
	for i := range ro.Status.Steps {
		ro.Status.Steps[i].State = osv1alpha1.RolloutNodePending
	}
	switch {
	case blocked != "":
		r.set(ro, osv1alpha1.ConditionPlanned, metav1.ConditionFalse, osv1alpha1.ReasonPlanBlocked, blocked)
	case len(plan) == 0:
		r.set(ro, osv1alpha1.ConditionPlanned, metav1.ConditionTrue, osv1alpha1.ReasonPlanReady, "no node needs this change")
		r.set(ro, osv1alpha1.ConditionComplete, metav1.ConditionTrue, osv1alpha1.ReasonNothingToDo, "no node needs this change")
		return ctrl.Result{}, nil
	default:
		r.set(ro, osv1alpha1.ConditionPlanned, metav1.ConditionTrue, osv1alpha1.ReasonPlanReady,
			fmt.Sprintf("%d steps, plan %s", len(plan), shortDigest(ro.Status.PlanHash)))
	}
	return ctrl.Result{RequeueAfter: replanInterval}, nil
}

// runPass runs an approved rollout one decision further.
func (r *TalosRolloutReconciler) runPass(ctx context.Context, ro *osv1alpha1.TalosRollout) (ctrl.Result, error) {
	if ok, why := rolloutApproved(ro); !ok {
		// The approval is write-once, so a plan that moved after it was
		// shown can never be approved here: this rollout ends, unrun.
		r.set(ro, osv1alpha1.ConditionApproved, metav1.ConditionFalse, osv1alpha1.ReasonPlanChanged,
			why+"; nothing ran. Look at the plan again in a new rollout")
		r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionFalse, osv1alpha1.ReasonPlanChanged, "the approval does not cover the plan")
		r.set(ro, osv1alpha1.ConditionComplete, metav1.ConditionFalse, osv1alpha1.ReasonPlanChanged, why)
		return ctrl.Result{}, nil
	}
	r.set(ro, osv1alpha1.ConditionApproved, metav1.ConditionTrue, osv1alpha1.ReasonPlanApproved,
		fmt.Sprintf("approved by %s for plan %s", ro.Spec.Approval.ApprovedBy, shortDigest(ro.Status.PlanHash)))
	if len(ro.Status.Steps) != len(ro.Status.Plan) {
		ro.Status.Steps = make([]osv1alpha1.RolloutStepStatus, len(ro.Status.Plan))
		for i := range ro.Status.Steps {
			ro.Status.Steps[i].State = osv1alpha1.RolloutNodePending
		}
	}

	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: ro.Spec.ClusterRef.Name}, cluster); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionUnknown, osv1alpha1.ReasonWaitingForHealth,
			"cluster "+ro.Spec.ClusterRef.Name+" does not exist")
		return ctrl.Result{RequeueAfter: r.gateInterval()}, nil
	}

	// Observe the steps that run.
	running, failed := 0, false
	for i := range ro.Status.Plan {
		st := &ro.Status.Steps[i]
		if st.State == osv1alpha1.RolloutNodeRunning {
			if err := r.observeStep(ctx, ro, cluster, i); err != nil {
				return ctrl.Result{}, err
			}
		}
		switch st.State {
		case osv1alpha1.RolloutNodeRunning:
			running++
		case osv1alpha1.RolloutNodeFailed:
			failed = true
		}
	}

	if failed || ro.Spec.Abort {
		reason, what := osv1alpha1.ReasonRolloutFailed, "a step failed"
		if !failed {
			reason, what = osv1alpha1.ReasonRolloutAborted, "aborted by a person"
		}
		for i := range ro.Status.Steps {
			if st := &ro.Status.Steps[i]; st.State == osv1alpha1.RolloutNodePending {
				st.State, st.Message = osv1alpha1.RolloutNodeSkipped, what+" before this step started"
			}
		}
		if running > 0 {
			r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionTrue, reason,
				fmt.Sprintf("%s; waiting for %d running step(s) to finish, nothing else starts", what, running))
			return ctrl.Result{RequeueAfter: r.gateInterval()}, nil
		}
		r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionFalse, reason, what+"; nodes already changed stay as they are")
		r.set(ro, osv1alpha1.ConditionComplete, metav1.ConditionFalse, reason, r.summary(ro))
		logf.FromContext(ctx).Info("rollout ended", "rollout", ro.Name, "reason", reason)
		return ctrl.Result{}, nil
	}

	next := r.nextSteps(ro, running)
	if len(next) == 0 {
		if running == 0 {
			r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionFalse, osv1alpha1.ReasonRolloutSucceeded, r.summary(ro))
			r.set(ro, osv1alpha1.ConditionPaused, metav1.ConditionFalse, osv1alpha1.ReasonNotPaused, "")
			r.set(ro, osv1alpha1.ConditionComplete, metav1.ConditionTrue, osv1alpha1.ReasonRolloutSucceeded, r.summary(ro))
			logf.FromContext(ctx).Info("rollout complete", "rollout", ro.Name)
			return ctrl.Result{}, nil
		}
		r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionTrue, osv1alpha1.ReasonNodeRunning, r.summary(ro))
		return ctrl.Result{RequeueAfter: r.gateInterval()}, nil
	}

	if paused, reason, msg := r.paused(ro); paused {
		r.set(ro, osv1alpha1.ConditionPaused, metav1.ConditionTrue, reason, msg)
		r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionTrue, reason, r.summary(ro))
		if running > 0 {
			return ctrl.Result{RequeueAfter: r.gateInterval()}, nil
		}
		return ctrl.Result{}, nil
	}
	r.set(ro, osv1alpha1.ConditionPaused, metav1.ConditionFalse, osv1alpha1.ReasonNotPaused, "")

	if why, err := r.healthGate(ctx, ro, cluster); err != nil {
		return ctrl.Result{}, err
	} else if why != "" {
		r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionTrue, osv1alpha1.ReasonWaitingForHealth,
			"next step waits: "+why)
		return ctrl.Result{RequeueAfter: r.gateInterval()}, nil
	}

	for _, i := range next {
		if err := r.startStep(ctx, ro, cluster, i); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.set(ro, osv1alpha1.ConditionProgressing, metav1.ConditionTrue, osv1alpha1.ReasonNodeRunning, r.summary(ro))
	return ctrl.Result{RequeueAfter: r.gateInterval()}, nil
}

// nextSteps picks the pending steps that may start now. The plan is ordered
// control planes, kube-proxy, workers. A control plane or kube-proxy step
// starts only when nothing else runs, and only after every step before it
// succeeded; workers start together up to maxUnavailableWorkers, once every
// step before the first worker succeeded.
func (r *TalosRolloutReconciler) nextSteps(ro *osv1alpha1.TalosRollout, running int) []int {
	var out []int
	for i, step := range ro.Status.Plan {
		state := ro.Status.Steps[i].State
		if state != osv1alpha1.RolloutNodePending {
			if state != osv1alpha1.RolloutNodeSucceeded && (step.Kind != osv1alpha1.RolloutStepNode || step.Role != osv1alpha1.RoleWorker) {
				return out // a non-worker step still runs: nothing after it starts
			}
			continue
		}
		if step.Kind == osv1alpha1.RolloutStepNode && step.Role == osv1alpha1.RoleWorker {
			if running+len(out) < int(maxUnavailableWorkers(ro)) {
				out = append(out, i)
				continue
			}
			return out
		}
		if running > 0 || len(out) > 0 {
			return out
		}
		return []int{i}
	}
	return out
}

// paused reports whether the rollout may not start another step: a person
// paused it, or its first node step succeeded and nobody resumed it yet.
func (r *TalosRolloutReconciler) paused(ro *osv1alpha1.TalosRollout) (bool, string, string) {
	if ro.Spec.Paused {
		return true, osv1alpha1.ReasonPausedByPerson, "spec.paused is set: pk-metal rollout resume " + ro.Name + " to go on"
	}
	done := 0
	for i, st := range ro.Status.Steps {
		if ro.Status.Plan[i].Kind == osv1alpha1.RolloutStepNode && st.State == osv1alpha1.RolloutNodeSucceeded {
			done++
		}
	}
	if done >= 1 && ro.Annotations[osv1alpha1.AnnotationRolloutResume] != ro.Status.PlanHash {
		return true, osv1alpha1.ReasonCanaryPause, fmt.Sprintf(
			"the first node is done; check it, then pk-metal rollout resume %s (or abort it)", ro.Name)
	}
	return false, "", ""
}

// summary counts the steps by state.
func (r *TalosRolloutReconciler) summary(ro *osv1alpha1.TalosRollout) string {
	counts := map[osv1alpha1.RolloutNodeState]int{}
	var running []string
	for i, st := range ro.Status.Steps {
		counts[st.State]++
		if st.State == osv1alpha1.RolloutNodeRunning {
			running = append(running, stepName(ro.Status.Plan[i]))
		}
	}
	s := fmt.Sprintf("%d/%d steps succeeded", counts[osv1alpha1.RolloutNodeSucceeded], len(ro.Status.Steps))
	if n := counts[osv1alpha1.RolloutNodeFailed]; n > 0 {
		s += fmt.Sprintf(", %d failed", n)
	}
	if n := counts[osv1alpha1.RolloutNodeSkipped]; n > 0 {
		s += fmt.Sprintf(", %d skipped", n)
	}
	if len(running) > 0 {
		s += "; running: " + strings.Join(running, ", ")
	}
	return s
}

func stepName(st osv1alpha1.RolloutStep) string {
	if st.Kind == osv1alpha1.RolloutStepKubeProxy {
		return "kube-proxy"
	}
	return st.Node
}

// rolloutOpName is the deterministic name of a step's operation: creating it
// twice finds the first.
func rolloutOpName(ro *osv1alpha1.TalosRollout, st osv1alpha1.RolloutStep) string {
	name := fmt.Sprintf("%s-%s-%s", ro.Name, hash8(ro.Status.PlanHash), st.Node)
	if len(name) > 253 {
		name = name[:253]
	}
	return strings.TrimRight(name, "-.")
}

// startStep starts one step: a Node step creates its HostOperation, a
// KubeProxy step patches the DaemonSet.
func (r *TalosRolloutReconciler) startStep(ctx context.Context, ro *osv1alpha1.TalosRollout, cluster *osv1alpha1.TalosCluster, i int) error {
	step := ro.Status.Plan[i]
	st := &ro.Status.Steps[i]
	st.State, st.StartedAt, st.Message = osv1alpha1.RolloutNodeRunning, ptr.To(metav1.Now()), ""
	if step.Kind == osv1alpha1.RolloutStepKubeProxy {
		return r.observeKubeProxy(ctx, ro, cluster, i)
	}
	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: rolloutOpName(ro, step), Namespace: r.OperationsNamespace,
			Labels: map[string]string{osv1alpha1.LabelRollout: ro.Name},
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: step.Host},
			ProposedBy: r.Proposer,
			Reason:     OperationReasonRollout,
			PlanHash:   ro.Status.PlanHash,
			Actions:    rolloutActions(ro, step),
			// The rollout's approval covers this operation; the executor
			// checks rolloutRef against it before it runs anything.
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(false)},
			RolloutRef: &metalv1alpha1.RolloutReference{Name: ro.Name, PlanHash: ro.Status.PlanHash},
		},
	}
	if err := controllerutil.SetControllerReference(ro, op, r.Scheme); err != nil {
		return err
	}
	err := r.Create(ctx, op)
	if apierrors.IsAlreadyExists(err) {
		err = nil // an earlier pass created it and its status write was lost
	}
	if err != nil {
		return fmt.Errorf("create operation for %s: %w", step.Node, err)
	}
	st.Operation = op.Namespace + "/" + op.Name
	logf.FromContext(ctx).Info("rollout step started", "rollout", ro.Name, "node", step.Node, "operation", st.Operation)
	return nil
}

// observeStep updates a running step from the world.
func (r *TalosRolloutReconciler) observeStep(ctx context.Context, ro *osv1alpha1.TalosRollout, cluster *osv1alpha1.TalosCluster, i int) error {
	step := ro.Status.Plan[i]
	st := &ro.Status.Steps[i]
	if step.Kind == osv1alpha1.RolloutStepKubeProxy {
		return r.observeKubeProxy(ctx, ro, cluster, i)
	}
	ns, name, _ := strings.Cut(st.Operation, "/")
	if name == "" {
		ns, name = r.OperationsNamespace, rolloutOpName(ro, step)
		st.Operation = ns + "/" + name
	}
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, op); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		// The cache may not show an operation created moments ago: only a
		// step that started a while back has lost its operation.
		if st.StartedAt != nil && time.Since(st.StartedAt.Time) > time.Minute {
			st.State, st.FinishedAt = osv1alpha1.RolloutNodeFailed, ptr.To(metav1.Now())
			st.Message = "operation " + st.Operation + " was deleted before it finished"
		}
		return nil
	}
	c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	switch {
	case c == nil || c.Status == metav1.ConditionUnknown:
		if c != nil {
			st.Message = c.Message
		}
	case c.Status == metav1.ConditionTrue:
		st.State, st.FinishedAt, st.Message = osv1alpha1.RolloutNodeSucceeded, ptr.To(metav1.Now()), ""
		logf.FromContext(ctx).Info("rollout step succeeded", "rollout", ro.Name, "node", step.Node)
	default:
		st.State, st.FinishedAt = osv1alpha1.RolloutNodeFailed, ptr.To(metav1.Now())
		st.Message = "operation " + st.Operation + " failed: " + c.Message
		logf.FromContext(ctx).Info("rollout step failed", "rollout", ro.Name, "node", step.Node, "message", c.Message)
	}
	return nil
}

// observeKubeProxy moves the kube-proxy DaemonSet and reads its rollout.
func (r *TalosRolloutReconciler) observeKubeProxy(ctx context.Context, ro *osv1alpha1.TalosRollout, cluster *osv1alpha1.TalosCluster, i int) error {
	st := &ro.Status.Steps[i]
	w, msg, err := r.workload(ctx, cluster)
	if err != nil || w == nil {
		st.Message = msg
		return err
	}
	done, msg, err := w.SyncKubeProxy(ctx, ro.Status.Plan[i].Image)
	if err != nil {
		st.Message = err.Error()
		return nil
	}
	st.Message = msg
	if done {
		st.State, st.FinishedAt = osv1alpha1.RolloutNodeSucceeded, ptr.To(metav1.Now())
	}
	return nil
}

// workload opens the cluster's Kubernetes API with the published admin
// kubeconfig. It returns a nil Workload and why when it cannot yet.
func (r *TalosRolloutReconciler) workload(ctx context.Context, cluster *osv1alpha1.TalosCluster) (k8supgrade.Workload, string, error) {
	if r.Workload == nil {
		return nil, "no Kubernetes client configured", nil
	}
	ref := cluster.Status.KubeconfigSecretRef
	if ref == nil {
		return nil, "the cluster has no published kubeconfig yet", nil
	}
	s := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, "", err
		}
		return nil, "kubeconfig Secret " + ref.Namespace + "/" + ref.Name + " not found", nil
	}
	w, err := r.Workload(s.Data[KubeconfigKey])
	if err != nil {
		return nil, "open the cluster's Kubernetes API: " + err.Error(), nil
	}
	return w, "", nil
}

// healthGate checks, live, that the cluster can take the next step: every
// etcd member answers, votes and reports no error, and every Kubernetes Node
// of the cluster that no running step has taken down is Ready. It returns
// why not, or "".
func (r *TalosRolloutReconciler) healthGate(ctx context.Context, ro *osv1alpha1.TalosRollout, cluster *osv1alpha1.TalosCluster) (string, error) {
	nodes, err := clusterMembers(ctx, r.Client, cluster.Name)
	if err != nil {
		return "", err
	}
	busy := map[string]bool{}
	for i, st := range ro.Status.Steps {
		if st.State == osv1alpha1.RolloutNodeRunning {
			busy[ro.Status.Plan[i].Node] = true
		}
	}
	handed, err := handedOffNodes(ctx, r.Client)
	if err != nil {
		return "", err
	}

	if why, err := r.etcdGate(ctx, cluster, nodes, handed); err != nil || why != "" {
		return why, err
	}

	w, why, err := r.workload(ctx, cluster)
	if err != nil || w == nil {
		return why, err
	}
	ready, err := w.NodesReady(ctx)
	if err != nil {
		return "the Kubernetes API does not answer: " + err.Error(), nil
	}
	var notReady []string
	for _, n := range nodes {
		if !handed[n.Name] || busy[n.Name] {
			continue
		}
		if !ready[n.Name] {
			notReady = append(notReady, n.Name)
		}
	}
	if len(notReady) > 0 {
		return "Kubernetes Node(s) not Ready: " + strings.Join(notReady, ", "), nil
	}
	return "", nil
}

// etcdGate asks the control planes about etcd: every control plane that a
// Host runs must be a voting member, and each must report no error.
func (r *TalosRolloutReconciler) etcdGate(ctx context.Context, cluster *osv1alpha1.TalosCluster,
	nodes []osv1alpha1.TalosNode, handed map[string]bool) (string, error) {
	if r.Dial == nil {
		return "", nil
	}
	bundle, err := loadBundle(ctx, r.Client, r.Namespace, cluster)
	if err != nil {
		if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
			return unavailable.msg, nil
		}
		return "", err
	}
	var cps []osv1alpha1.TalosNode
	for _, n := range nodes {
		if n.Spec.Role == osv1alpha1.RoleControlPlane && handed[n.Name] {
			cps = append(cps, n)
		}
	}
	if len(cps) == 0 {
		return "no control plane runs", nil
	}
	var members []talos.EtcdMember
	var problems []string
	for _, n := range cps {
		addr := render.NodeAddress(&n)
		tcfg, err := render.Talosconfig(bundle, cluster.Name, []string{addr}, checkinCertTTL)
		if err != nil {
			return "", err
		}
		derr := talos.With(ctx, r.Dial, talos.Target{Endpoint: addr, Talosconfig: tcfg}, func(c talos.Client) error {
			st, err := c.EtcdStatus(ctx)
			if err != nil {
				return err
			}
			if len(st.Errors) > 0 {
				problems = append(problems, n.Name+": "+strings.Join(st.Errors, "; "))
			}
			if st.LeaderID == 0 {
				problems = append(problems, n.Name+" follows no leader")
			}
			if members == nil {
				members, err = c.EtcdMembers(ctx)
			}
			return err
		})
		if derr != nil {
			problems = append(problems, n.Name+": "+derr.Error())
		}
	}
	if len(problems) > 0 {
		return "etcd is not healthy: " + strings.Join(problems, "; "), nil
	}
	voting := map[string]bool{}
	for _, m := range members {
		if m.IsLearner {
			return "etcd member " + m.Hostname + " is still a learner", nil
		}
		voting[m.Hostname] = true
	}
	for _, n := range cps {
		if !voting[n.Name] {
			return "control plane " + n.Name + " is not an etcd member", nil
		}
	}
	return "", nil
}

// handedOffNodes is the set of TalosNodes some Host runs.
func handedOffNodes(ctx context.Context, c client.Reader) (map[string]bool, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := c.List(ctx, hosts); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range hosts.Items {
		if os := hosts.Items[i].Spec.OS; os != nil {
			out[os.NodeRef.Name] = true
		}
	}
	return out, nil
}

// hostsByNode maps each handed-off TalosNode to its Host.
func hostsByNode(ctx context.Context, c client.Reader) (map[string]string, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := c.List(ctx, hosts); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range hosts.Items {
		if os := hosts.Items[i].Spec.OS; os != nil {
			out[os.NodeRef.Name] = hosts.Items[i].Name
		}
	}
	return out, nil
}

// plan computes the rollout's plan from the world as it is now. blocked says
// why a plan cannot be made yet (a node not ready to be planned); the steps
// planned so far are still returned for the person to read.
func (r *TalosRolloutReconciler) plan(ctx context.Context, ro *osv1alpha1.TalosRollout) ([]osv1alpha1.RolloutStep, string, error) {
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: ro.Spec.ClusterRef.Name}, cluster); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, "", err
		}
		return nil, "cluster " + ro.Spec.ClusterRef.Name + " does not exist", nil
	}
	members, err := clusterMembers(ctx, r.Client, cluster.Name)
	if err != nil {
		return nil, "", err
	}
	hosts, err := hostsByNode(ctx, r.Client)
	if err != nil {
		return nil, "", err
	}
	var target osv1alpha1.RolloutTarget
	if ro.Spec.Target != nil {
		target = *ro.Spec.Target
	}
	switch ro.Spec.Kind {
	case osv1alpha1.RolloutTalosUpgrade:
		if target.TalosVersion == "" {
			return nil, "a TalosUpgrade needs spec.target.talosVersion", nil
		}
		if err := render.SupportedPair(target.TalosVersion, cluster.Spec.KubernetesVersion); err != nil {
			return nil, err.Error(), nil
		}
	case osv1alpha1.RolloutKubernetesUpgrade:
		if target.KubernetesVersion == "" {
			return nil, "a KubernetesUpgrade needs spec.target.kubernetesVersion", nil
		}
		if cluster.Spec.KubernetesVersion != target.KubernetesVersion {
			return nil, fmt.Sprintf("the cluster's spec.kubernetesVersion is %s, this rollout targets %s: "+
				"the configs it applies are rendered from the spec", cluster.Spec.KubernetesVersion, target.KubernetesVersion), nil
		}
	}

	var bundle *secrets.Bundle // only for the leader lookup
	if r.Dial != nil {
		bundle, _ = loadBundle(ctx, r.Client, r.Namespace, cluster)
	}
	leader := r.etcdLeader(ctx, cluster, members, hosts, bundle)

	var cps, workers []osv1alpha1.RolloutStep
	var blocked []string
	for i := range members {
		n := &members[i]
		if len(ro.Spec.Nodes) > 0 && !slices.Contains(ro.Spec.Nodes, n.Name) {
			continue
		}
		host, ok := hosts[n.Name]
		if !ok {
			continue // no machine runs it: nothing to change
		}
		step, why, err := r.planNode(ctx, ro, cluster, n, host, target)
		if err != nil {
			return nil, "", err
		}
		if why != "" {
			blocked = append(blocked, n.Name+": "+why)
			continue
		}
		if step == nil {
			continue
		}
		step.Actions = actionKeys(rolloutActions(ro, *step))
		if n.Spec.Role == osv1alpha1.RoleControlPlane {
			cps = append(cps, *step)
		} else {
			workers = append(workers, *step)
		}
	}
	// The etcd leader goes last among the control planes: one election,
	// not one per node.
	slices.SortStableFunc(cps, func(a, b osv1alpha1.RolloutStep) int {
		return cmpBool(a.Node == leader, b.Node == leader)
	})
	plan := cps
	if ro.Spec.Kind == osv1alpha1.RolloutKubernetesUpgrade && len(cps)+len(workers) > 0 {
		image, why, err := r.kubeProxyImage(ctx, members)
		if err != nil {
			return nil, "", err
		}
		switch {
		case why != "":
			blocked = append(blocked, why)
		case image != "":
			plan = append(plan, osv1alpha1.RolloutStep{Kind: osv1alpha1.RolloutStepKubeProxy, To: target.KubernetesVersion, Image: image})
		}
	}
	plan = append(plan, workers...)
	return plan, strings.Join(blocked, "; "), nil
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// planNode plans one node, or returns nil when it needs no change, or why it
// cannot be planned yet.
func (r *TalosRolloutReconciler) planNode(ctx context.Context, ro *osv1alpha1.TalosRollout, cluster *osv1alpha1.TalosCluster,
	n *osv1alpha1.TalosNode, host string, target osv1alpha1.RolloutTarget) (*osv1alpha1.RolloutStep, string, error) {
	step := &osv1alpha1.RolloutStep{Kind: osv1alpha1.RolloutStepNode, Node: n.Name, Host: host, Role: n.Spec.Role}
	switch ro.Spec.Kind {
	case osv1alpha1.RolloutReboot:
		step.From, step.To = n.Status.Version, n.Status.Version
		return step, "", nil
	case osv1alpha1.RolloutTalosUpgrade:
		running := n.Status.Version
		if running == "" {
			return nil, "its Talos version is not known (is it up?)", nil
		}
		if running == target.TalosVersion {
			return nil, "", nil
		}
		if err := render.UpgradeableFrom(running, target.TalosVersion); err != nil {
			return nil, err.Error(), nil
		}
		ref, err := r.Factory.InstallerImage(render.EffectiveSchematic(cluster, n), target.TalosVersion)
		if err != nil {
			return nil, err.Error(), nil
		}
		pinned, err := r.resolve(ctx, ref)
		if err != nil {
			return nil, "pin the installer image: " + err.Error(), nil
		}
		step.From, step.To, step.Image = running, target.TalosVersion, pinned
		return step, "", nil
	default: // Config, KubernetesUpgrade
		if !meta.IsStatusConditionTrue(n.Status.Conditions, osv1alpha1.ConditionRendered) {
			return nil, "its config does not render (see its Rendered condition)", nil
		}
		d := n.Status.Drift
		if d == nil || d.ConfigDigest != n.Status.ConfigDigest || n.Status.ConfigSecretRef == nil {
			return nil, "waiting for the dry run of its current render", nil
		}
		if !d.Changed {
			return nil, "", nil
		}
		step.From = n.Status.AppliedConfigDigest
		step.To = n.Status.ConfigDigest
		step.ConfigSecret = n.Status.ConfigSecretRef.Name
		step.ConfigDigest = n.Status.ConfigDigest
		step.RebootRequired = d.RebootRequired
		return step, "", nil
	}
}

// resolve pins an image reference, through the cache.
func (r *TalosRolloutReconciler) resolve(ctx context.Context, ref string) (string, error) {
	if v, ok := r.resolved.Load(ref); ok && time.Since(v.(resolvedImage).at) < resolveTTL {
		return v.(resolvedImage).ref, nil
	}
	if r.ResolveImage == nil {
		return "", fmt.Errorf("no image resolver configured")
	}
	pinned, err := r.ResolveImage(ctx, ref)
	if err != nil {
		return "", err
	}
	r.resolved.Store(ref, resolvedImage{ref: pinned, at: time.Now()})
	return pinned, nil
}

// kubeProxyImage reads the kube-proxy image from a control plane's current
// render. "" with no reason: the cluster deploys no kube-proxy.
func (r *TalosRolloutReconciler) kubeProxyImage(ctx context.Context, members []osv1alpha1.TalosNode) (string, string, error) {
	for _, n := range members {
		if n.Spec.Role != osv1alpha1.RoleControlPlane || n.Status.ConfigSecretRef == nil {
			continue
		}
		s := &corev1.Secret{}
		ref := n.Status.ConfigSecretRef
		if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return "", "", err
			}
			continue
		}
		image, enabled, err := render.KubeProxyImage(s.Data[asset.ConfigFilename])
		if err != nil {
			return "", "kube-proxy image: " + err.Error(), nil
		}
		if !enabled {
			return "", "", nil
		}
		return image, "", nil
	}
	return "", "no control plane has a rendered config to read the kube-proxy image from", nil
}

// etcdLeader names the control plane that leads etcd now, "" when unknown.
func (r *TalosRolloutReconciler) etcdLeader(ctx context.Context, cluster *osv1alpha1.TalosCluster,
	members []osv1alpha1.TalosNode, hosts map[string]string, bundle *secrets.Bundle) string {
	if bundle == nil {
		return ""
	}
	for _, n := range members {
		if n.Spec.Role != osv1alpha1.RoleControlPlane || hosts[n.Name] == "" {
			continue
		}
		addr := render.NodeAddress(&n)
		tcfg, err := render.Talosconfig(bundle, cluster.Name, []string{addr}, checkinCertTTL)
		if err != nil {
			return ""
		}
		var leader string
		if talos.With(ctx, r.Dial, talos.Target{Endpoint: addr, Talosconfig: tcfg}, func(c talos.Client) error {
			st, err := c.EtcdStatus(ctx)
			if err != nil {
				return err
			}
			ms, err := c.EtcdMembers(ctx)
			if err != nil {
				return err
			}
			for _, m := range ms {
				if m.ID == st.LeaderID {
					leader = m.Hostname
				}
			}
			return nil
		}) == nil {
			return leader
		}
	}
	return ""
}

func (r *TalosRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.TalosRollout{}).
		Owns(&metalv1alpha1.HostOperation{}).
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(r.rolloutsForNode)).
		Named("talosrollout").
		Complete(r)
}

// rolloutsForNode re-plans the unapproved rollouts of a node's cluster when
// the node moves (its drift check, its version).
func (r *TalosRolloutReconciler) rolloutsForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster := obj.(*osv1alpha1.TalosNode).Spec.ClusterRef.Name
	rollouts := &osv1alpha1.TalosRolloutList{}
	if err := r.List(ctx, rollouts); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range rollouts.Items {
		ro := &rollouts.Items[i]
		if ro.Spec.ClusterRef.Name == cluster && ro.Spec.Approval.ApprovedBy == "" && !rolloutFinished(ro) {
			reqs = append(reqs, requestFor(ro.Name))
		}
	}
	return reqs
}
