package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/baseline"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// opHostIndex joins operations to the Host they act on.
const opHostIndex = ".spec.hostRef.name"

// executeTimeout bounds one whole executor pass — dial, every step (a Huawei
// VmmControl insert polls its task monitor), and the closing observation.
// Steps that wait on the world (power transitions, a check-in) do not block a
// pass for their whole duration: they report waiting and the pass requeues.
const executeTimeout = time.Minute

// flashTimeout replaces it when the pass flashes firmware: acceptance is not
// completion — the BMC burns and reboots for minutes before the version reads
// back (the sim answers immediately; the budget is for hardware).
const flashTimeout = 15 * time.Minute

// Defaults for the waiting steps. A power transition that has not shown after
// powerTimeout fails the step: a BMC whose reported state never moves is
// lying or wedged, and either way a person should look.
const (
	defaultPowerSettle  = 20 * time.Second
	defaultPowerTimeout = 5 * time.Minute
	defaultWaitInterval = 5 * time.Second
	// grantMargin outlives the longest wait an operation can still be in when
	// its grant is minted, so a mount never dies under a machine still reading.
	grantMargin = 15 * time.Minute
)

// HostOperationReconciler runs approved disruptive one-shots — the model's ONE
// writer to a BMC. The gate itself is admission (VAP: approve as yourself, never
// your own proposal, never pre-approved, write-once) and RBAC (the controller
// may create, humans may patch); this reconciler only observes its outcome. At
// most one operation runs per Host (oldest first); a failed step is terminal —
// one-shots never auto-retry.
//
// Execution is resumable. Each step is written to status as it reaches its
// goal, and a resumed pass continues after the last recorded step, so a goal a
// later step consumed (a Once boot override used up by the boot that followed)
// is never re-armed. The step in flight when a pass died is re-entered, which
// is safe because every step checks its goal against the world before acting:
// an inserted image, a set override, a power state already reached are
// success, not conflicts.
type HostOperationReconciler struct {
	client.Client
	// CredentialsNamespace is where credential and CA Secrets live.
	CredentialsNamespace string
	// RegistryClient fetches firmware artifacts from the OCI registry; nil uses
	// the default client (system trust store).
	RegistryClient *http.Client
	// Media serves granted boot media. Nil leaves the executor unable to mount
	// a secret-bearing BootAsset (the step fails saying so).
	Media    *asset.Service
	Recorder events.EventRecorder

	// Connect opens a write-capable session to the host's machine; nil dials
	// the first reachable management path. A test seam.
	Connect func(ctx context.Context, host *metalv1alpha1.Host) (cl bmc.Client, systemID, reason, msg string, err error)
	// PowerSettle is how long one pass waits for a power transition to show
	// before requeueing; PowerTimeout fails the step. Zero values default.
	PowerSettle  time.Duration
	PowerTimeout time.Duration
	// WaitInterval paces the requeue of a waiting step. Zero defaults.
	WaitInterval time.Duration
}

// exec is one pass over one operation: the operation, its BMC session (opened
// lazily — a pass that only waits on a check-in never dials), and the flush
// that persists progress.
type exec struct {
	r     *HostOperationReconciler
	op    *metalv1alpha1.HostOperation
	host  *metalv1alpha1.Host
	flush func() error

	cl       bmc.Client
	systemID string
}

// stepState is what a step reports besides an error: done, or still waiting
// on the world (with what it waits for).
type stepState struct {
	done    bool
	waiting string
}

var stepDone = stepState{done: true}

func waitingOn(format string, args ...any) stepState {
	return stepState{waiting: fmt.Sprintf(format, args...)}
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations/status,verbs=update
// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=managementendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=biossettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets;talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=pk-metal
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile runs an approved operation's steps against the BMC. Executing needs
// neither create nor patch on operations, and approving is a patch, so this
// reconciler's rules cannot pass the approval gate.
func (r *HostOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, req.NamespacedName, op); err != nil {
		if apierrors.IsNotFound(err) {
			// A deleted operation takes its grants with it: whatever it
			// mounted stops answering.
			r.revokeGrants(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !op.DeletionTimestamp.IsZero() {
		r.revokeGrants(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	if opFinished(op) {
		r.revokeGrants(req.NamespacedName)
		return r.expire(ctx, op)
	}

	orig := op.DeepCopy()
	setCond := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: op.Generation,
		})
	}
	flush := func() error {
		phase.StampOperation(op)
		if equality.Semantic.DeepEqual(orig.Status, op.Status) {
			return nil
		}
		if err := r.Status().Update(ctx, op); err != nil {
			return err
		}
		orig = op.DeepCopy()
		return nil
	}

	if approvalRequired(op) && op.Spec.Approval.ApprovedBy == "" {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionFalse,
			metalv1alpha1.ReasonAwaitingApproval, "a human must stamp spec.approval.approvedBy")
		return ctrl.Result{}, client.IgnoreNotFound(flush())
	}
	if approvalRequired(op) {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue,
			metalv1alpha1.ReasonApproved, "approved by "+op.Spec.Approval.ApprovedBy)
	} else {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue,
			metalv1alpha1.ReasonApprovalNotRequired, "proposed by "+op.Spec.ProposedBy)
	}

	// One active operation per Host: the oldest unfinished one runs, the rest
	// queue and re-enter through the sibling watch when it finishes.
	if pred, err := r.activePredecessor(ctx, op); err != nil {
		return ctrl.Result{}, err
	} else if pred != "" {
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown,
			metalv1alpha1.ReasonOperationQueued, "operation "+pred+" holds host "+op.Spec.HostRef.Name)
		return ctrl.Result{}, client.IgnoreNotFound(flush())
	}

	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, client.ObjectKey{Name: op.Spec.HostRef.Name}, host); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown,
			metalv1alpha1.ReasonHostNotFound, fmt.Sprintf("host %s not found", op.Spec.HostRef.Name))
		return ctrl.Result{}, client.IgnoreNotFound(flush())
	}

	// One context bounds the whole BMC pass (dial + steps + closing observation):
	// gofish pins the connect context to every later request, so it must live as
	// long as the client does.
	bctx, cancel := context.WithTimeout(ctx, passTimeout(op))
	defer cancel()
	x := &exec{r: r, op: op, host: host, flush: flush}
	defer x.close()

	// Persist the baseline and the Running state BEFORE the first mutating call —
	// the crash rail: a resumed execution re-derives what already happened from
	// the world, starting with this record.
	if op.Status.Baseline == nil {
		obs, hold, err := x.observe(bctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if hold != "" {
			setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonNoReachablePath, hold)
			return ctrl.Result{}, client.IgnoreNotFound(flush())
		}
		op.Status.Baseline = &metalv1alpha1.OperationBaseline{
			PowerState: string(obs.PowerState),
			CapturedAt: ptrTime(metav1.Now()),
		}
		if !obs.LastResetTime.IsZero() {
			op.Status.Baseline.LastResetTime = &metav1.MicroTime{Time: obs.LastResetTime}
		}
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonExecuting, "")
		if err := flush(); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	for i := recordedSteps(op); i < len(op.Spec.Actions); i++ {
		// A pass can outlive its operation: a release deletes an unfinished
		// provisioning operation, and the steps after that must not run.
		if gone, err := r.opGone(ctx, req.NamespacedName); err != nil || gone {
			r.revokeGrants(req.NamespacedName)
			return ctrl.Result{}, err
		}
		name := actionName(op.Spec.Actions[i])
		st, err := x.step(bctx, i)
		if err == nil && st.waiting != "" && time.Since(x.stepStart(i)) > x.stepBudget(i) {
			err = fmt.Errorf("%s (gave up after %s)", st.waiting, x.stepBudget(i))
		}
		switch {
		case err != nil:
			op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{
				Action: name, Phase: "Failed", Message: err.Error(), FinishedAt: ptrTime(metav1.Now()),
			})
			setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse,
				metalv1alpha1.ReasonStepFailed, fmt.Sprintf("step %d/%d %s: %v", i+1, len(op.Spec.Actions), name, err))
			r.Recorder.Eventf(op, nil, corev1.EventTypeWarning, metalv1alpha1.ReasonStepFailed,
				"Execute", "step %s failed: %v", name, err)
			r.revokeGrants(req.NamespacedName)
			return ctrl.Result{}, client.IgnoreNotFound(flush())
		case !st.done:
			setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonExecuting,
				fmt.Sprintf("step %d/%d %s: %s", i+1, len(op.Spec.Actions), name, st.waiting))
			if err := flush(); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{RequeueAfter: r.waitInterval()}, nil
		}
		op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{
			Action: name, Phase: "Succeeded", FinishedAt: ptrTime(metav1.Now()),
		})
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonExecuting,
			fmt.Sprintf("step %d/%d %s done", i+1, len(op.Spec.Actions), name))
		// Progress is persisted before the next mutating call — the record a
		// resumed pass continues from.
		if err := flush(); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// The closing observation: proof of the reset (baseline-and-advance), and what
	// marks the Host's next reset-time advance as explained rather than Drifted.
	if x.cl != nil {
		if after, hold, err := x.observe(bctx); err == nil && hold == "" && !after.LastResetTime.IsZero() {
			op.Status.ObservedResetTime = &metav1.MicroTime{Time: after.LastResetTime}
		}
	}
	r.revokeGrants(req.NamespacedName)
	setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionTrue,
		metalv1alpha1.ReasonStepsSucceeded, fmt.Sprintf("%d steps", len(op.Spec.Actions)))
	r.Recorder.Eventf(op, nil, corev1.EventTypeNormal, metalv1alpha1.ReasonStepsSucceeded,
		"Execute", "%d steps succeeded on host %s", len(op.Spec.Actions), op.Spec.HostRef.Name)
	return ctrl.Result{}, client.IgnoreNotFound(flush())
}

// recordedSteps counts the leading Succeeded steps — where a resumed pass
// continues.
func recordedSteps(op *metalv1alpha1.HostOperation) int {
	n := 0
	for _, s := range op.Status.Steps {
		if s.Phase != "Succeeded" {
			break
		}
		n++
	}
	return min(n, len(op.Spec.Actions))
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func (r *HostOperationReconciler) waitInterval() time.Duration {
	if r.WaitInterval > 0 {
		return r.WaitInterval
	}
	return defaultWaitInterval
}

func (r *HostOperationReconciler) powerSettle() time.Duration {
	if r.PowerSettle > 0 {
		return r.PowerSettle
	}
	return defaultPowerSettle
}

func (r *HostOperationReconciler) powerTimeout() time.Duration {
	if r.PowerTimeout > 0 {
		return r.PowerTimeout
	}
	return defaultPowerTimeout
}

// opGone re-reads the operation between steps; deleted or deleting stops the pass.
func (r *HostOperationReconciler) opGone(ctx context.Context, key types.NamespacedName) (bool, error) {
	cur := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, key, cur); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return !cur.DeletionTimestamp.IsZero(), nil
}

// grantHandle names an operation's grants in the media service's table.
func grantHandle(key types.NamespacedName) string { return "hostoperation:" + key.String() }

func (r *HostOperationReconciler) revokeGrants(key types.NamespacedName) {
	if r.Media == nil {
		return
	}
	if n := r.Media.Grants.RevokeAsset(grantHandle(key)); n > 0 {
		logf.Log.WithName("hostoperation").Info("revoked media grants", "operation", key.String(), "grants", n)
	}
}

// opFinished: Succeeded or Failed are terminal — conditions, not phase (lint-phase).
func opFinished(op *metalv1alpha1.HostOperation) bool {
	c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	return c != nil && c.Status != metav1.ConditionUnknown
}

// opSucceeded: finished with every step at its goal.
func opSucceeded(op *metalv1alpha1.HostOperation) bool {
	return meta.IsStatusConditionTrue(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
}

// expire garbage-collects a finished operation once its TTL elapses,
// Job-style. No TTL keeps it forever — human-requested ops stay as their own
// record; convergence proposals set one (their no-respawn guard is the plan
// memory on the proposing object, not this record).
func (r *HostOperationReconciler) expire(ctx context.Context, op *metalv1alpha1.HostOperation) (ctrl.Result, error) {
	if op.Spec.TTLSecondsAfterFinished == nil {
		return ctrl.Result{}, nil
	}
	c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	expiry := c.LastTransitionTime.Add(time.Duration(*op.Spec.TTLSecondsAfterFinished) * time.Second)
	if wait := time.Until(expiry); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	logf.FromContext(ctx).Info("operation expired", "operation", op.Namespace+"/"+op.Name)
	return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, op))
}

// passTimeout budgets one executor pass by what it runs.
func passTimeout(op *metalv1alpha1.HostOperation) time.Duration {
	for _, a := range op.Spec.Actions {
		if a.FlashFirmware != nil {
			return flashTimeout
		}
	}
	return executeTimeout
}

// approvalRequired: nil is a closed gate — the safe default for every client
// that says nothing (the schema default writes true, but unpersisted or
// hand-built objects must land the same way).
func approvalRequired(op *metalv1alpha1.HostOperation) bool {
	return op.Spec.Approval.Required == nil || *op.Spec.Approval.Required
}

// opAwaitsApproval: the gate is required and unstamped, and the op is unfinished.
func opAwaitsApproval(op *metalv1alpha1.HostOperation) bool {
	return approvalRequired(op) && op.Spec.Approval.ApprovedBy == "" && !opFinished(op)
}

// opActive: approved and unfinished — holds (or is about to hold) the host.
func opActive(op *metalv1alpha1.HostOperation) bool {
	return !opFinished(op) && !opAwaitsApproval(op)
}

// activePredecessor names an older active operation on the same host, if any.
// Oldest creation wins; namespace/name breaks ties.
func (r *HostOperationReconciler) activePredecessor(ctx context.Context, op *metalv1alpha1.HostOperation) (string, error) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.MatchingFields{opHostIndex: op.Spec.HostRef.Name}); err != nil {
		return "", err
	}
	active := slices.DeleteFunc(ops.Items, func(o metalv1alpha1.HostOperation) bool {
		return !opActive(&o) || !o.DeletionTimestamp.IsZero()
	})
	slices.SortFunc(active, func(a, b metalv1alpha1.HostOperation) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(active) == 0 || active[0].UID == op.UID {
		return "", nil
	}
	return active[0].Namespace + "/" + active[0].Name, nil
}

// client opens the write-capable session on first use. A hold (no reachable
// path, credentials missing) is an error to the step that needed it: it
// cannot proceed, and a step without the machine is not a step.
func (x *exec) client(ctx context.Context) (bmc.Client, string, error) {
	if x.cl != nil {
		return x.cl, x.systemID, nil
	}
	connect := x.r.Connect
	if connect == nil {
		connect = x.r.dialHost
	}
	cl, sysID, reason, msg, err := connect(ctx, x.host)
	if err != nil {
		return nil, "", err
	}
	if reason != "" {
		return nil, "", fmt.Errorf("%s: %s", reason, msg)
	}
	x.cl, x.systemID = cl, sysID
	return cl, sysID, nil
}

func (x *exec) close() {
	if x.cl != nil {
		_ = x.cl.Close()
	}
}

// dialHost connects to the host's first reachable management path with a
// WRITE-capable client — one of the two places the read-only rail is
// deliberately opened (the other is the convergence reconciler's read pass).
func (r *HostOperationReconciler) dialHost(ctx context.Context, host *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
	cl, reason, msg, err := hostPathClient(ctx, r.Client, r.CredentialsNamespace, host, false)
	if err != nil || reason != "" {
		return nil, "", reason, msg, err
	}
	return cl, cl.(*pathClient).systemID, "", "", nil
}

// observe reads the current reset observables; a connection hold comes back
// as a message, not an error, so the caller can surface it.
func (x *exec) observe(ctx context.Context) (baseline.Observation, string, error) {
	cl, sysID, err := x.client(ctx)
	if err != nil {
		return baseline.Observation{}, err.Error(), nil
	}
	sys, err := systemOf(ctx, cl, sysID)
	if err != nil {
		return baseline.Observation{}, err.Error(), nil
	}
	return baseline.FromSystem(sys), "", nil
}

// systemOf reads one system's current view.
func systemOf(ctx context.Context, cl bmc.Client, systemID string) (bmc.System, error) {
	systems, err := cl.Systems(ctx)
	if err != nil {
		return bmc.System{}, err
	}
	for _, sys := range systems {
		if sys.ID == systemID {
			return sys, nil
		}
	}
	return bmc.System{}, fmt.Errorf("path system %q not among endpoint systems", systemID)
}

// hostPathClient connects to the host's first reachable management path —
// shared by the executor (write-capable) and the convergence observer
// (read-only). ctx is the caller's pass-bounded context: gofish pins it to
// the client's whole lifetime, so no narrower dial timeout here.
func hostPathClient(ctx context.Context, c client.Client, credentialsNamespace string, host *metalv1alpha1.Host, readOnly bool) (bmc.Client, string, string, error) {
	for _, path := range host.Status.ManagedBy {
		if !path.Reachable {
			continue
		}
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := c.Get(ctx, client.ObjectKey{Name: path.EndpointRef.Name}, ep); err != nil {
			continue
		}
		cfg, reason, err := endpointClientConfig(ctx, c, credentialsNamespace, ep, readOnly)
		if err != nil {
			return nil, reason, err.Error(), nil
		}
		cl, err := gofishclient.Connect(ctx, cfg)
		if err != nil {
			continue
		}
		return &pathClient{Client: cl, systemID: path.SystemID}, "", "", nil
	}
	return nil, metalv1alpha1.ReasonNoReachablePath,
		fmt.Sprintf("host %s has no reachable management path", host.Name), nil
}

// pathClient pins the chosen path's system id alongside the connection.
type pathClient struct {
	bmc.Client
	systemID string
}

// actionName is the union key of one action — the step name in status.
func actionName(a metalv1alpha1.HostOperationAction) string {
	switch {
	case a.PowerCycle != nil:
		return "powerCycle"
	case a.PowerOff != nil:
		return "powerOff"
	case a.SetBootOverride != nil:
		return "setBootOverride"
	case a.InsertMedia != nil:
		return "insertMedia"
	case a.EjectMedia != nil:
		return "ejectMedia"
	case a.ApplyBios != nil:
		return "applyBios"
	case a.FlashFirmware != nil:
		return "flashFirmware"
	case a.AwaitCheckIn != nil:
		return "awaitCheckIn"
	}
	return "unknown"
}

// approvalDomain names what the Host is blocked on while this operation awaits
// its gate — the spec reason (BiosConvergence, Provisioning) or, for
// human-requested ops, the first action key in Approval(<domain>) shape.
func approvalDomain(op *metalv1alpha1.HostOperation) string {
	if op.Spec.Reason != "" {
		return op.Spec.Reason
	}
	if len(op.Spec.Actions) > 0 {
		if n := actionName(op.Spec.Actions[0]); n != "unknown" {
			return strings.ToUpper(n[:1]) + n[1:]
		}
	}
	return "Operation"
}

func (r *HostOperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&metalv1alpha1.HostOperation{}, opHostIndex,
		func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.HostOperation).Spec.HostRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.HostOperation{}).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(r.siblingOps)).
		Watches(&metalv1alpha1.Host{}, handler.EnqueueRequestsFromMapFunc(r.opsForHost)).
		Named("hostoperation").
		Complete(r)
}

// siblingOps fans an operation event out to the other operations on the same
// host — a finished predecessor is what un-queues its successors.
func (r *HostOperationReconciler) siblingOps(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.opsFor(ctx, obj.(*metalv1alpha1.HostOperation).Spec.HostRef.Name, obj.GetUID())
}

// opsForHost re-enters operations when their host appears or its paths change.
func (r *HostOperationReconciler) opsForHost(ctx context.Context, obj client.Object) []ctrl.Request {
	return r.opsFor(ctx, obj.GetName(), "")
}

func (r *HostOperationReconciler) opsFor(ctx context.Context, hostName string, skip types.UID) []ctrl.Request {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.MatchingFields{opHostIndex: hostName}); err != nil {
		logf.FromContext(ctx).Error(err, "list operations for host", "host", hostName)
		return nil
	}
	var reqs []ctrl.Request
	for i := range ops.Items {
		if op := &ops.Items[i]; op.UID != skip && !opFinished(op) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: op.Namespace, Name: op.Name,
			}})
		}
	}
	return reqs
}
