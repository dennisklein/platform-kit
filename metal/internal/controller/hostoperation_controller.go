package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	// passMargin is what a pass keeps in reserve beyond one power settle
	// window before it starts another step: a step begun with less would run
	// into the pass deadline halfway and fail an operation that was only
	// slow, not broken.
	passMargin = 10 * time.Second
	// passResume paces the next pass after one ran out of budget.
	passResume = time.Second
)

// HostOperationReconciler runs approved disruptive one-shots — the model's ONE
// writer to a BMC. The gate itself is admission (VAP: approve as yourself, never
// your own proposal, never pre-approved, write-once) and RBAC (the controller
// may create, humans may patch); this reconciler only observes its outcome. At
// most one operation runs per Host (a started one first, then the oldest); a
// failed step is terminal — one-shots never auto-retry.
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
	// PassTimeout overrides the budget of one pass. Zero budgets by what the
	// operation runs. A test seam.
	PassTimeout time.Duration
	// APIReader reads straight from the API server, bypassing the informer
	// cache, for the queue check before an operation starts. Defaults to the
	// manager's API reader in SetupWithManager.
	APIReader client.Reader
}

// reader is the uncached reader when one is wired, the client otherwise.
func (r *HostOperationReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
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
// on the world (with what it waits for). hint is added to the message only
// when the wait runs out: where a person looks next.
type stepState struct {
	done    bool
	waiting string
	hint    string
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
		setCondition(&op.Status.Conditions, op.Generation, t, s, reason, msg)
	}
	flush := func() error {
		phase.StampOperation(op)
		if err := updateStatusIfChanged(ctx, r.Client, op, &orig.Status, &op.Status); err != nil {
			return err
		}
		orig = op.DeepCopy()
		return nil
	}

	if approvalRequired(op) && op.Spec.Approval.ApprovedBy == "" {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionFalse,
			metalv1alpha1.ReasonAwaitingApproval, awaitingApprovalMessage(op))
		return ctrl.Result{}, client.IgnoreNotFound(flush())
	}
	if approvalRequired(op) {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue,
			metalv1alpha1.ReasonApproved, "approved by "+op.Spec.Approval.ApprovedBy)
	} else {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue,
			metalv1alpha1.ReasonApprovalNotRequired, "proposed by "+op.Spec.ProposedBy)
	}

	// One active operation per Host (see hostHolder): the holder runs, the rest
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

	// A host in maintenance belongs to a person: an operation that has not
	// started waits until it comes back. One already running finishes —
	// stopping between a mount and its boot leaves the machine worse off.
	if op.Status.Baseline == nil {
		if _, ok := inMaintenance(host); ok {
			setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonHostInMaintenance,
				maintenanceNote(host)+"; this operation starts when it is given back: "+endMaintenanceCommand(host))
			return ctrl.Result{}, client.IgnoreNotFound(flush())
		}
	}

	// One context bounds the whole BMC pass (dial + steps + closing observation):
	// gofish pins the connect context to every later request, so it must live as
	// long as the client does.
	budget := r.PassTimeout
	if budget == 0 {
		budget = passTimeout(op)
	}
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	x := &exec{r: r, op: op, host: host, flush: flush}
	defer x.close()

	// Persist the baseline and the Running state BEFORE the first mutating call —
	// the crash rail: a resumed execution re-derives what already happened from
	// the world, starting with this record.
	if op.Status.Baseline == nil {
		// Starting is the one step that must not race another operation:
		// confirm the queue against the API server, not the cache.
		if pred, err := r.activePredecessorUncached(ctx, op); err != nil {
			return ctrl.Result{}, err
		} else if pred != "" {
			setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown,
				metalv1alpha1.ReasonOperationQueued, "operation "+pred+" holds host "+op.Spec.HostRef.Name)
			return ctrl.Result{}, client.IgnoreNotFound(flush())
		}
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

	// fail records step i as failed — terminal, one-shots never auto-retry.
	fail := func(i int, err error) (ctrl.Result, error) {
		name := actionName(op.Spec.Actions[i])
		op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{
			Action: name, Phase: "Failed", Message: err.Error(), FinishedAt: ptrTime(metav1.Now()),
		})
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse,
			metalv1alpha1.ReasonStepFailed, fmt.Sprintf("step %d/%d %s: %v", i+1, len(op.Spec.Actions), name, err))
		r.Recorder.Eventf(op, nil, corev1.EventTypeWarning, metalv1alpha1.ReasonStepFailed,
			"Execute", "step %s failed: %v", name, err)
		r.revokeGrants(req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(flush())
	}
	// wait leaves step i in flight and comes back after the given pause.
	wait := func(i int, msg string, after time.Duration) (ctrl.Result, error) {
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonExecuting,
			fmt.Sprintf("step %d/%d %s: %s", i+1, len(op.Spec.Actions), actionName(op.Spec.Actions[i]), msg))
		if err := flush(); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{RequeueAfter: after}, nil
	}
	// held reports an error that says the machine could not be reached this
	// pass — a dial that failed, a deadline that ran out — as a wait. The
	// step did not run to a verdict, and every step re-checks its goal when
	// it is entered again, so trying later is safe where failing for good
	// would end an operation over a network hiccup.
	held := func(i int, err error) (ctrl.Result, error) {
		return wait(i, "BMC unavailable, retrying: "+err.Error(), r.waitInterval())
	}

	start := recordedSteps(op)
	if start < len(op.Spec.Actions) {
		if err := x.remountGrants(bctx, start); err != nil {
			if transient(bctx, err) {
				return held(start, err)
			}
			return fail(start, err)
		}
	}
	for i := start; i < len(op.Spec.Actions); i++ {
		// A pass can outlive its operation: a release deletes an unfinished
		// provisioning operation, and the steps after that must not run.
		if gone, err := r.opGone(ctx, req.NamespacedName); err != nil || gone {
			r.revokeGrants(req.NamespacedName)
			return ctrl.Result{}, err
		}
		// A pass that cannot fit another power settle stops between steps
		// and the next pass continues; at least one step runs per pass.
		if i > start && !budgetLeft(bctx, r.powerSettle()+passMargin) {
			return wait(i, "pass budget spent, continuing", passResume)
		}
		name := actionName(op.Spec.Actions[i])
		st, err := x.step(bctx, i)
		if perr := (*persistError)(nil); errors.As(err, &perr) {
			// The machine did what was asked; only the record of it did
			// not land. That is no verdict on the step: end the pass, and
			// the next one enters the step again, which checks its goal
			// against the machine first.
			return ctrl.Result{}, client.IgnoreNotFound(perr.err)
		}
		if err != nil && stepTransient(bctx, op.Spec.Actions[i], err) {
			return held(i, err)
		}
		if err == nil && st.waiting != "" && time.Since(x.stepStart(i)) > x.stepBudget(i) {
			err = fmt.Errorf("%s (gave up after %s)%s", st.waiting, x.stepBudget(i), st.hint)
		}
		switch {
		case err != nil:
			return fail(i, err)
		case !st.done:
			return wait(i, st.waiting, r.waitInterval())
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

// awaitingApprovalMessage says who may let the operation run and how; for an
// operation serving a claim, also how to say no.
func awaitingApprovalMessage(op *metalv1alpha1.HostOperation) string {
	ref := op.Namespace + "/" + op.Name
	msg := fmt.Sprintf("waits for a person other than %s to approve it: pk-metal approve %s", op.Spec.ProposedBy, ref)
	if l := op.Labels; l[labelClaimName] != "" {
		msg += fmt.Sprintf(". To say no for now: pk-metal hold %s/%s", l[labelClaimNamespace], l[labelClaimName])
	}
	return msg
}

// budgetLeft: the pass deadline is at least need away.
func budgetLeft(ctx context.Context, need time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= need
}

// persistError is a status write that failed in the middle of a step, after
// the machine acted (the off half of a power cycle seen, a graceful shutdown
// asked for).
type persistError struct{ err error }

func (e *persistError) Error() string { return "record progress: " + e.err.Error() }
func (e *persistError) Unwrap() error { return e.err }

// holdError is a connection hold — no reachable path, credentials not
// readable — met by a step that needed the machine.
type holdError struct{ reason, msg string }

func (e *holdError) Error() string { return e.reason + ": " + e.msg }

// transient: the error says this pass could not talk to the machine, not that
// the machine refused — a hold, the pass deadline, or a network failure
// (refused or reset connections, dial and read timeouts).
func transient(passCtx context.Context, err error) bool {
	var hold *holdError
	var nerr net.Error
	return errors.As(err, &hold) || passCtx.Err() != nil ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &nerr)
}

// stepTransient is transient for a step. A flash is the exception for
// everything but a hold: its deadline is the verdict that the upload was
// accepted and never took, and a retry would push the image again.
func stepTransient(passCtx context.Context, a metalv1alpha1.HostOperationAction, err error) bool {
	if a.FlashFirmware != nil {
		var hold *holdError
		return errors.As(err, &hold)
	}
	return transient(passCtx, err)
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

// opStarted: the operation captured its baseline, so it may have touched the
// machine already.
func opStarted(op *metalv1alpha1.HostOperation) bool { return op.Status.Baseline != nil }

// hostHolder picks the one operation that may run on a host from all the
// operations on it, or nil when none is active. A started operation holds the
// host until it finishes: an older one approved later waits, because stopping
// a run between its mount and its boot leaves the machine half-done. Among
// operations that have not started, the oldest wins. Namespace and name break
// ties, so every pass picks the same one.
func hostHolder(ops []metalv1alpha1.HostOperation) *metalv1alpha1.HostOperation {
	active := slices.DeleteFunc(slices.Clone(ops), func(o metalv1alpha1.HostOperation) bool {
		return !opActive(&o) || !o.DeletionTimestamp.IsZero()
	})
	if len(active) == 0 {
		return nil
	}
	h := slices.MinFunc(active, func(a, b metalv1alpha1.HostOperation) int {
		if sa, sb := opStarted(&a), opStarted(&b); sa != sb {
			if sa {
				return -1
			}
			return 1
		}
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return &h
}

// activePredecessor names the operation that holds the op's host, if it is
// another one.
func (r *HostOperationReconciler) activePredecessor(ctx context.Context, op *metalv1alpha1.HostOperation) (string, error) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops, client.MatchingFields{opHostIndex: op.Spec.HostRef.Name}); err != nil {
		return "", err
	}
	return holderOtherThan(ops.Items, op), nil
}

// activePredecessorUncached is activePredecessor over the operations as the
// API server holds them. The cache can lag behind another operation's
// baseline write; deciding from it would let two operations start on one
// host. Reconciles of operations run one at a time (SetupWithManager), so a
// pass that confirms here and then writes its baseline is seen by the next.
func (r *HostOperationReconciler) activePredecessorUncached(ctx context.Context, op *metalv1alpha1.HostOperation) (string, error) {
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.reader().List(ctx, ops); err != nil {
		return "", err
	}
	onHost := slices.DeleteFunc(ops.Items, func(o metalv1alpha1.HostOperation) bool {
		return o.Spec.HostRef.Name != op.Spec.HostRef.Name
	})
	return holderOtherThan(onHost, op), nil
}

// holderOtherThan names the holder of the host when it is not op.
func holderOtherThan(ops []metalv1alpha1.HostOperation, op *metalv1alpha1.HostOperation) string {
	h := hostHolder(ops)
	if h == nil || h.UID == op.UID {
		return ""
	}
	return h.Namespace + "/" + h.Name
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
		return nil, "", &holdError{reason: reason, msg: msg}
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
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
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
		// One pass at a time: the uncached queue check before a start is
		// only a guard when no other pass can start between it and the
		// baseline write.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
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
