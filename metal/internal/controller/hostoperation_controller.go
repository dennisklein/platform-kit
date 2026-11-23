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
	"github.com/dennisklein/platform-kit/metal/internal/baseline"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/oci"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// opHostIndex joins operations to the Host they act on.
const opHostIndex = ".spec.hostRef.name"

// executeTimeout bounds one whole executor pass — dial, every step (a Huawei
// VmmControl insert polls its task monitor), and the closing observation.
const executeTimeout = time.Minute

// flashTimeout replaces it when the pass flashes firmware: acceptance is not
// completion — the BMC burns and reboots for minutes before the version reads
// back (the sim answers immediately; the budget is for hardware).
const flashTimeout = 15 * time.Minute

// HostOperationReconciler runs approved disruptive one-shots — the model's ONE
// writer to a BMC. The gate itself is admission (VAP: approve as yourself, never
// pre-approved, write-once) and RBAC (the controller may create, humans may
// patch); this reconciler only observes its outcome. At most one operation runs
// per Host (oldest first); a failed step is terminal — one-shots never
// auto-retry. Steps converge to their goal (an inserted image or an advanced
// reset time is success, not a conflict), which makes a resumed execution safe:
// the persisted baseline plus the advance predicate answers "did I already
// reboot", never a remembered step counter.
type HostOperationReconciler struct {
	client.Client
	// CredentialsNamespace is where credential and CA Secrets live.
	CredentialsNamespace string
	// RegistryClient fetches firmware artifacts from the OCI registry; nil uses
	// the default client (system trust store).
	RegistryClient *http.Client
	Recorder       events.EventRecorder
}

func (r *HostOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, req.NamespacedName, op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !op.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if opFinished(op) {
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
			return client.IgnoreNotFound(err)
		}
		orig = op.DeepCopy()
		return nil
	}

	if approvalRequired(op) && op.Spec.Approval.ApprovedBy == "" {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionFalse,
			metalv1alpha1.ReasonAwaitingApproval, "a human must stamp spec.approval.approvedBy")
		return ctrl.Result{}, flush()
	}
	if approvalRequired(op) {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue,
			metalv1alpha1.ReasonApproved, "approved by "+op.Spec.Approval.ApprovedBy)
	} else {
		setCond(metalv1alpha1.ConditionApproved, metav1.ConditionTrue,
			metalv1alpha1.ReasonApprovalNotRequired, "")
	}

	// One active operation per Host: the oldest unfinished one runs, the rest
	// queue and re-enter through the sibling watch when it finishes.
	if pred, err := r.activePredecessor(ctx, op); err != nil {
		return ctrl.Result{}, err
	} else if pred != "" {
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown,
			metalv1alpha1.ReasonOperationQueued, "operation "+pred+" holds host "+op.Spec.HostRef.Name)
		return ctrl.Result{}, flush()
	}

	// One context bounds the whole BMC pass (dial + steps + closing observation):
	// gofish pins the connect context to every later request, so it must live as
	// long as the client does.
	bctx, cancel := context.WithTimeout(ctx, passTimeout(op))
	defer cancel()
	cl, reason, msg, err := r.executorClient(bctx, op)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, reason, msg)
		return ctrl.Result{}, flush()
	}
	defer cl.Close()

	sysID, obs, err := r.observe(bctx, cl)
	if err != nil {
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown,
			metalv1alpha1.ReasonNoReachablePath, err.Error())
		return ctrl.Result{}, flush()
	}

	// Persist the baseline and the Running state BEFORE the first mutating call —
	// the crash rail: a resumed execution re-derives what already happened from
	// the world, starting with this record.
	if op.Status.Baseline == nil {
		op.Status.Baseline = &metalv1alpha1.OperationBaseline{PowerState: string(obs.PowerState)}
		if !obs.LastResetTime.IsZero() {
			op.Status.Baseline.LastResetTime = &metav1.MicroTime{Time: obs.LastResetTime}
		}
		setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown,
			metalv1alpha1.ReasonExecuting, "")
		if err := flush(); err != nil {
			return ctrl.Result{}, err
		}
	}

	op.Status.Steps = nil
	for _, action := range op.Spec.Actions {
		name, err := r.executeAction(bctx, cl, sysID, action, op)
		if err != nil {
			op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{
				Action: name, Phase: "Failed", Message: err.Error(),
			})
			setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse,
				metalv1alpha1.ReasonStepFailed, fmt.Sprintf("step %s: %v", name, err))
			r.Recorder.Eventf(op, nil, corev1.EventTypeWarning, metalv1alpha1.ReasonStepFailed,
				"Execute", "step %s failed: %v", name, err)
			return ctrl.Result{}, flush()
		}
		op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{Action: name, Phase: "Succeeded"})
	}

	// The closing observation: proof of the reset (baseline-and-advance), and what
	// marks the Host's next reset-time advance as explained rather than Drifted.
	if _, after, err := r.observe(bctx, cl); err == nil && !after.LastResetTime.IsZero() {
		op.Status.ObservedResetTime = &metav1.MicroTime{Time: after.LastResetTime}
	}
	setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionTrue,
		metalv1alpha1.ReasonStepsSucceeded, fmt.Sprintf("%d steps", len(op.Spec.Actions)))
	r.Recorder.Eventf(op, nil, corev1.EventTypeNormal, metalv1alpha1.ReasonStepsSucceeded,
		"Execute", "%d steps succeeded on host %s", len(op.Spec.Actions), op.Spec.HostRef.Name)
	return ctrl.Result{}, flush()
}

// opFinished: Succeeded or Failed are terminal — conditions, not phase (lint-phase).
func opFinished(op *metalv1alpha1.HostOperation) bool {
	c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	return c != nil && c.Status != metav1.ConditionUnknown
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
		return !opActive(&o)
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

// executorClient connects to the host's first reachable management path with a
// WRITE-capable client — one of the two places the read-only rail is
// deliberately opened (the other is the convergence reconciler's read pass).
// A hold reason (host missing, no path, credentials) comes back as reason+msg.
func (r *HostOperationReconciler) executorClient(ctx context.Context, op *metalv1alpha1.HostOperation) (bmc.Client, string, string, error) {
	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, client.ObjectKey{Name: op.Spec.HostRef.Name}, host); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, metalv1alpha1.ReasonHostNotFound,
				fmt.Sprintf("host %s not found", op.Spec.HostRef.Name), nil
		}
		return nil, "", "", err
	}
	return hostPathClient(ctx, r.Client, r.CredentialsNamespace, host, false)
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

// observe reads the current reset observables through the executor's path.
func (r *HostOperationReconciler) observe(ctx context.Context, cl bmc.Client) (string, baseline.Observation, error) {
	pc := cl.(*pathClient)
	systems, err := cl.Systems(ctx)
	if err != nil {
		return "", baseline.Observation{}, err
	}
	for _, sys := range systems {
		if sys.ID == pc.systemID {
			return sys.ID, baseline.FromSystem(sys), nil
		}
	}
	return "", baseline.Observation{}, fmt.Errorf("path system %q not among endpoint systems", pc.systemID)
}

// executeAction converges ONE action to its goal. Goal-observably-met is success
// (already-inserted media, an already-advanced reset time), which is what makes
// re-entry after a crash or a conflict idempotent.
func (r *HostOperationReconciler) executeAction(ctx context.Context, cl bmc.Client, systemID string, a metalv1alpha1.HostOperationAction, op *metalv1alpha1.HostOperation) (string, error) {
	log := logf.FromContext(ctx)
	switch {
	case a.PowerCycle != nil:
		base := baseline.Observation{}
		if b := op.Status.Baseline; b != nil && b.LastResetTime != nil {
			base.LastResetTime = b.LastResetTime.Time
		}
		if _, cur, err := r.observe(ctx, cl); err == nil && baseline.Advanced(base, cur) {
			log.Info("power cycle already happened (baseline advanced)", "host", op.Spec.HostRef.Name)
			return "powerCycle", nil
		}
		state, err := cl.Power(ctx, systemID)
		if err != nil {
			return "powerCycle", err
		}
		action := bmc.ActionForceRestart
		if state != bmc.PowerOn {
			action = bmc.ActionOn
		}
		return "powerCycle", cl.SetPower(ctx, systemID, action)

	case a.SetBootOverride != nil:
		once := a.SetBootOverride.Once == nil || *a.SetBootOverride.Once
		return "setBootOverride", cl.SetBootOverride(ctx, systemID, a.SetBootOverride.Target, once)

	case a.InsertMedia != nil:
		media, err := cl.Media(ctx)
		if err != nil {
			return "insertMedia", err
		}
		for _, m := range media {
			if m.ID == a.InsertMedia.MediaID && m.Inserted && m.ImageURL == a.InsertMedia.ImageURL {
				return "insertMedia", nil // goal already met
			}
		}
		// A proposed operation carries a bare URL; granted (credentialed) mounts
		// belong to the provisioning flow, which issues them itself.
		return "insertMedia", cl.InsertMedia(ctx, a.InsertMedia.MediaID, a.InsertMedia.ImageURL, nil)

	case a.EjectMedia != nil:
		media, err := cl.Media(ctx)
		if err != nil {
			return "ejectMedia", err
		}
		for _, m := range media {
			if m.ID == a.EjectMedia.MediaID && m.Inserted {
				return "ejectMedia", cl.EjectMedia(ctx, a.EjectMedia.MediaID)
			}
		}
		return "ejectMedia", nil // goal already met

	case a.ApplyBios != nil:
		return "applyBios", r.applyBios(ctx, cl, systemID, a.ApplyBios)

	case a.FlashFirmware != nil:
		return "flashFirmware", r.flashFirmware(ctx, cl, a.FlashFirmware)
	}
	return "unknown", fmt.Errorf("action carries no known key")
}

// applyBios stages the settings' drifted attributes into the pending object.
// Converge-to-goal at key granularity: an attribute already current or already
// staged with the desired value is done, not a conflict — a resumed operation
// PATCHes only what is left, and a fully-staged machine PATCHes nothing (the
// paired powerCycle then applies it).
func (r *HostOperationReconciler) applyBios(ctx context.Context, cl bmc.Client, systemID string, a *metalv1alpha1.ApplyBiosAction) error {
	settings := &metalv1alpha1.BiosSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: a.SettingsRef.Name}, settings); err != nil {
		return fmt.Errorf("settings %s: %w", a.SettingsRef.Name, err)
	}
	state, err := cl.Bios(ctx, systemID)
	if err != nil {
		return err
	}
	stage := map[string]any{}
	for k, want := range decodeAttrs(settings.Spec.Attributes) {
		staged, stagedOK := state.Pending[k]
		switch {
		case stagedOK && jsonEqual(staged, want):
			continue // already staged right
		case stagedOK:
			// A contradictory value sits pending (a stale earlier staging) —
			// overwrite it, or the paired reboot applies the wrong one.
			stage[k] = want
		case jsonEqual(state.Attributes[k], want):
			continue // already current, nothing pending
		default:
			stage[k] = want
		}
	}
	if len(stage) == 0 {
		return nil // goal already met (or fully staged)
	}
	return cl.SetBiosAttributes(ctx, systemID, stage)
}

// flashFirmware pushes the image and waits for the observable proof — the
// version reading back after the manager's own reboot. Acceptance is NOT
// completion (this firmware answers success to actions it silently declines),
// so the upload's return value proves nothing; the goal check does.
func (r *HostOperationReconciler) flashFirmware(ctx context.Context, cl bmc.Client, a *metalv1alpha1.FlashFirmwareAction) error {
	before, err := cl.Manager(ctx)
	if err != nil {
		return err
	}
	if before.FirmwareVersion == a.Version {
		return nil // goal already met
	}
	image, err := oci.Pull(ctx, r.RegistryClient, a.Image)
	if err != nil {
		return err
	}
	if err := cl.UpdateFirmware(ctx, image); err != nil {
		return err
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		// Mid-flash reads fail while the BMC reboots — expected, keep polling.
		m, err := cl.Manager(ctx)
		if err == nil && m.FirmwareVersion == a.Version &&
			(before.LastResetTime.IsZero() || m.LastResetTime.After(before.LastResetTime)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("upload accepted but version %s never read back: %w", a.Version, ctx.Err())
		case <-tick.C:
		}
	}
}

// approvalDomain names what the Host is blocked on while this operation awaits
// its gate — the spec reason (BiosConvergence) or, for human-requested ops, the
// first action key in Approval(<domain>) shape.
func approvalDomain(op *metalv1alpha1.HostOperation) string {
	if op.Spec.Reason != "" {
		return op.Spec.Reason
	}
	if len(op.Spec.Actions) > 0 {
		a := op.Spec.Actions[0]
		switch {
		case a.PowerCycle != nil:
			return "PowerCycle"
		case a.SetBootOverride != nil:
			return "SetBootOverride"
		case a.InsertMedia != nil:
			return "InsertMedia"
		case a.EjectMedia != nil:
			return "EjectMedia"
		case a.ApplyBios != nil:
			return "ApplyBios"
		case a.FlashFirmware != nil:
			return "FlashFirmware"
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
