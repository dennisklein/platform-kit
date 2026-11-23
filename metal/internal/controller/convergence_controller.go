package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// proposalTTL garbage-collects finished convergence proposals. The plan
// memory on the proposing object, not the operation record, carries the
// no-respawn guard — so history may leave.
const proposalTTL = int32(24 * 60 * 60)

// ConvergenceReconciler drives desired state from the profile classes toward
// the machines, per host: it materializes the layered BiosProfile merge into
// a BiosSettings (and the discovered registry into a BiosSchema), observes
// drift, and PROPOSES a HostOperation — the human gate stays between every
// plan and its execution. Firmware converges before BIOS (the BIOS schema
// depends on firmware). An unchanged planHash never spawns a second
// operation: every hold below is alert-and-hold, unstuck by a profile edit.
//
// It holds its own read-only BMC session for the BIOS observation (attribute
// maps are too big to cache on the endpoint), writes the Host's
// FirmwareConverged/BiosConverged conditions and the firmware plan memory —
// fields the HostReconciler leaves untouched — and is, with the executor, the
// second deliberate opening of the read-only rail.
type ConvergenceReconciler struct {
	client.Client
	// CredentialsNamespace is where credential and CA Secrets live.
	CredentialsNamespace string
	// OperationsNamespace is where proposals are created (the operator's own).
	OperationsNamespace string
	// Proposer is the operator's own username, stamped as proposedBy.
	Proposer string
	// PollInterval is the BIOS re-observation cadence.
	PollInterval time.Duration
}

func (r *ConvergenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &metalv1alpha1.Host{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		// Host gone — the owned BiosSettings/BiosSchema follow via GC.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	biosProfiles, fwProfiles, err := r.matchedProfiles(ctx, host)
	if err != nil {
		return ctrl.Result{}, err
	}

	orig := host.DeepCopy()
	setCond := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: host.Generation,
		})
	}

	fwConverged, err := r.firmware(ctx, host, fwProfiles, setCond)
	if err != nil {
		return ctrl.Result{}, err
	}
	requeue, err := r.bios(ctx, host, biosProfiles, fwConverged, setCond)
	if err != nil {
		return ctrl.Result{}, err
	}

	facts := hostFacts(ctx, r.Client, host)
	phase.StampHost(host, facts)
	if !equality.Semantic.DeepEqual(orig.Status, host.Status) {
		if err := r.applyStatus(ctx, host, facts); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if requeue {
		return ctrl.Result{RequeueAfter: r.PollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// applyStatus declares only what this lane decides: its two conditions and the
// firmware plan memory. The projected facts belong to the host reconciler and
// are left out entirely.
//
// Unlike the endpoint's identity lane, this one needs no uncached read. Losing
// the plan memory costs a re-proposal, and a re-proposal is harmless by
// construction: the operation is created before the memory is recorded and its
// name is derived from the plan hash, so the second attempt is an AlreadyExists
// and nothing is done twice.
func (r *ConvergenceReconciler) applyStatus(ctx context.Context, host *metalv1alpha1.Host, facts phase.HostFacts) error {
	apply := &metalv1alpha1.Host{
		TypeMeta: metav1.TypeMeta{
			APIVersion: metalv1alpha1.GroupVersion.String(),
			Kind:       "Host",
		},
		ObjectMeta: metav1.ObjectMeta{Name: host.Name},
	}
	apply.Status.FirmwareProposed = host.Status.FirmwareProposed
	for _, t := range convergenceHostConditions {
		if c := meta.FindStatusCondition(host.Status.Conditions, t); c != nil {
			apply.Status.Conditions = append(apply.Status.Conditions, *c)
		}
	}
	phase.StampHostOnto(apply, host, facts)
	return applyOwnedStatus(ctx, r.Client, apply, fieldOwnerHostConvergence)
}

// matchedProfiles selects both profile kinds against the host's labels.
func (r *ConvergenceReconciler) matchedProfiles(ctx context.Context, host *metalv1alpha1.Host) ([]metalv1alpha1.BiosProfile, []metalv1alpha1.FirmwareProfile, error) {
	bios := &metalv1alpha1.BiosProfileList{}
	if err := r.List(ctx, bios); err != nil {
		return nil, nil, err
	}
	fw := &metalv1alpha1.FirmwareProfileList{}
	if err := r.List(ctx, fw); err != nil {
		return nil, nil, err
	}
	var matchedBios []metalv1alpha1.BiosProfile
	for _, p := range bios.Items {
		ok, err := selectorMatches(p.Spec.Match, host.Labels)
		if err != nil {
			return nil, nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		if ok {
			matchedBios = append(matchedBios, p)
		}
	}
	var matchedFw []metalv1alpha1.FirmwareProfile
	for _, p := range fw.Items {
		ok, err := selectorMatches(p.Spec.Match, host.Labels)
		if err != nil {
			return nil, nil, fmt.Errorf("profile %s: %w", p.Name, err)
		}
		if ok {
			matchedFw = append(matchedFw, p)
		}
	}
	return matchedBios, matchedFw, nil
}

// firmware converges the manager firmware and reports whether the BIOS lane
// may proceed (converged, or nothing desired). The observation is a pure
// cluster-state join — the endpoint poll already serves the manager version.
func (r *ConvergenceReconciler) firmware(ctx context.Context, host *metalv1alpha1.Host, profiles []metalv1alpha1.FirmwareProfile, setCond func(string, metav1.ConditionStatus, string, string)) (bool, error) {
	if len(profiles) == 0 {
		if err := r.withdrawObsolete(ctx, host.Status.FirmwareProposed); err != nil {
			return false, err
		}
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionFirmwareConverged)
		host.Status.FirmwareProposed = nil
		return true, nil
	}
	target, conflict := mergeFirmware(profiles)
	if conflict != "" {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionFalse,
			metalv1alpha1.ReasonProfileConflict, conflict)
		return false, nil
	}

	versions := map[string]string{}
	push := false
	for _, p := range host.Status.ManagedBy {
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: p.EndpointRef.Name}, ep); err != nil {
			continue
		}
		versions[ep.Name] = ep.Status.FirmwareVersion
		if caps := ep.Status.Capabilities; caps != nil && caps.UpdateDialect == string(bmc.DialectMultipartPush) {
			push = true
		}
	}
	if len(versions) == 0 {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionUnknown,
			metalv1alpha1.ReasonNoReachablePath, "no endpoint observation to compare against")
		return false, nil
	}
	drifted := ""
	for name, v := range versions {
		if v != target.Version {
			drifted = fmt.Sprintf("endpoint %s serves %s, want %s", name, v, target.Version)
			break
		}
	}
	if drifted == "" {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionTrue,
			metalv1alpha1.ReasonInSync, "manager "+target.Version)
		// The plan is spent: forgetting it is what lets a later drift to the
		// same desired state be proposed again instead of read as this
		// plan's failure.
		host.Status.FirmwareProposed = nil
		return true, nil
	}

	hash := planHash(target)
	if plan := host.Status.FirmwareProposed; plan != nil && plan.PlanHash == hash {
		reason, msg, err := r.planVerdict(ctx, plan)
		if err != nil {
			return false, err
		}
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionFalse, reason, msg)
		return false, nil
	}
	if !push {
		setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionFalse,
			metalv1alpha1.ReasonNoUpdateTransport,
			drifted+" — no path advertises the proven multipart-push transport")
		return false, nil
	}
	if err := r.withdrawObsolete(ctx, host.Status.FirmwareProposed); err != nil {
		return false, err
	}
	op := r.proposal(host.Name, "firmware", hash, "FirmwareConvergence", []metalv1alpha1.HostOperationAction{
		{FlashFirmware: &metalv1alpha1.FlashFirmwareAction{Image: target.Image, Version: target.Version}},
	})
	// Create before recording: a crash between the two re-proposes into
	// AlreadyExists (benign), the reverse order would hold on a ghost.
	if err := r.propose(ctx, op); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("proposed operation", "operation", op.Namespace+"/"+op.Name, "host", host.Name, "reason", op.Spec.Reason)
	host.Status.FirmwareProposed = &metalv1alpha1.ProposedPlan{
		PlanHash: hash, Operation: op.Namespace + "/" + op.Name,
	}
	setCond(metalv1alpha1.ConditionFirmwareConverged, metav1.ConditionFalse,
		metalv1alpha1.ReasonAwaitingOperation,
		fmt.Sprintf("%s; operation %s/%s proposed", drifted, op.Namespace, op.Name))
	return false, nil
}

// bios materializes the desired BIOS state, observes the machine, and
// converges — mirroring the settings' verdict onto the Host. Returns whether
// a re-observation requeue is wanted.
func (r *ConvergenceReconciler) bios(ctx context.Context, host *metalv1alpha1.Host, profiles []metalv1alpha1.BiosProfile, fwConverged bool, setCond func(string, metav1.ConditionStatus, string, string)) (bool, error) {
	if len(profiles) == 0 {
		settings := &metalv1alpha1.BiosSettings{}
		switch err := r.Get(ctx, client.ObjectKey{Name: host.Name}, settings); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, err
		default:
			if err := r.withdrawObsolete(ctx, settings.Status.Proposed); err != nil {
				return false, err
			}
			if err := r.Delete(ctx, settings); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		meta.RemoveStatusCondition(&host.Status.Conditions, metalv1alpha1.ConditionBiosConverged)
		return false, nil
	}

	attrs, provenance, conflicts := mergeBios(profiles)
	settings := &metalv1alpha1.BiosSettings{ObjectMeta: metav1.ObjectMeta{Name: host.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, settings, func() error {
		if err := controllerutil.SetOwnerReference(host, settings, r.Scheme()); err != nil {
			return err
		}
		// A conflicted merge holds the previous rendering whole — desired state
		// under dispute is never partially applied.
		if len(conflicts) == 0 {
			settings.Spec.Attributes = attrs
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("materialize biossettings %s: %w", host.Name, err)
	}

	origSettings := settings.DeepCopy()
	status, reason, msg := metav1.ConditionFalse, "", ""
	requeue := true
	switch {
	case len(conflicts) > 0:
		reason, msg = metalv1alpha1.ReasonProfileConflict, strings.Join(conflicts, "; ")
	default:
		settings.Status.Provenance = provenance
		state, schema, obsErr := r.observeBios(ctx, host)
		if obsErr != nil {
			status, reason, msg = metav1.ConditionUnknown, metalv1alpha1.ReasonNoReachablePath, obsErr.Error()
			break
		}
		if err := r.materializeSchema(ctx, host, state.Registry, schema); err != nil {
			return false, err
		}
		if violations := validateBios(settings.Spec.Attributes, schema); len(violations) > 0 {
			reason, msg = metalv1alpha1.ReasonSchemaViolation, strings.Join(violations, "; ")
			break
		}

		desired := decodeAttrs(settings.Spec.Attributes)
		settings.Status.Observed = subsetJSON(state.Attributes, desired)
		settings.Status.Pending = subsetJSON(state.Pending, desired)
		drifted := driftedKeys(desired, state.Attributes, state.Pending)
		if len(drifted) == 0 {
			status, reason = metav1.ConditionTrue, metalv1alpha1.ReasonInSync
			msg = fmt.Sprintf("%d attributes", len(desired))
			// The plan is spent — see the firmware lane.
			settings.Status.Proposed = nil
			break
		}
		if !fwConverged {
			reason = metalv1alpha1.ReasonAwaitingFirmware
			msg = "drift on " + strings.Join(drifted, ", ") + " — firmware converges first"
			break
		}
		hash := planHash(desired)
		if plan := settings.Status.Proposed; plan != nil && plan.PlanHash == hash {
			var err error
			if reason, msg, err = r.planVerdict(ctx, plan); err != nil {
				return false, err
			}
			break
		}
		if err := r.withdrawObsolete(ctx, settings.Status.Proposed); err != nil {
			return false, err
		}
		op := r.proposal(host.Name, "bios", hash, "BiosConvergence", []metalv1alpha1.HostOperationAction{
			// The attributes travel inside the operation: the approver sees
			// exactly what will be staged, and a later edit of the settings
			// cannot change an approved operation.
			{ApplyBios: &metalv1alpha1.ApplyBiosAction{
				SettingsRef: &metalv1alpha1.Reference{Name: settings.Name},
				Attributes:  settings.Spec.Attributes,
			}},
			{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		})
		if err := r.propose(ctx, op); err != nil {
			return false, err
		}
		logf.FromContext(ctx).Info("proposed operation", "operation", op.Namespace+"/"+op.Name, "host", host.Name, "reason", op.Spec.Reason)
		settings.Status.Proposed = &metalv1alpha1.ProposedPlan{
			PlanHash: hash, Operation: op.Namespace + "/" + op.Name,
		}
		reason = metalv1alpha1.ReasonAwaitingOperation
		msg = fmt.Sprintf("drift on %s; operation %s/%s proposed", strings.Join(drifted, ", "), op.Namespace, op.Name)
	}

	meta.SetStatusCondition(&settings.Status.Conditions, metav1.Condition{
		Type: metalv1alpha1.ConditionConverged, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: settings.Generation,
	})
	if !equality.Semantic.DeepEqual(origSettings.Status, settings.Status) {
		if err := r.Status().Update(ctx, settings); err != nil {
			return false, client.IgnoreNotFound(err)
		}
	}
	setCond(metalv1alpha1.ConditionBiosConverged, status, reason, msg)
	return requeue, nil
}

// withdrawObsolete deletes a superseded proposal that never got its gate —
// when the desired state moves on (a profile edit, a transient partial merge
// completing), the controller retracts its own un-actioned request instead of
// stranding it in front of the approver. An approved, running, or finished
// operation is not touched: it is active work or history.
func (r *ConvergenceReconciler) withdrawObsolete(ctx context.Context, plan *metalv1alpha1.ProposedPlan) error {
	if plan == nil {
		return nil
	}
	ns, name, _ := strings.Cut(plan.Operation, "/")
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, op); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !opAwaitsApproval(op) {
		return nil
	}
	logf.FromContext(ctx).Info("withdrew obsolete proposal", "operation", plan.Operation)
	return client.IgnoreNotFound(r.Delete(ctx, op))
}

// planVerdict derives a held plan's condition live from the operation or its
// absence — the outcome is never remembered, only that the proposal was made.
func (r *ConvergenceReconciler) planVerdict(ctx context.Context, plan *metalv1alpha1.ProposedPlan) (string, string, error) {
	ns, name, _ := strings.Cut(plan.Operation, "/")
	op := &metalv1alpha1.HostOperation{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, op); err != nil {
		if apierrors.IsNotFound(err) {
			return metalv1alpha1.ReasonOperationGone,
				"proposal " + plan.Operation + " no longer exists — change the profile to re-propose", nil
		}
		return "", "", err
	}
	c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	switch {
	case c == nil || c.Status == metav1.ConditionUnknown:
		return metalv1alpha1.ReasonAwaitingOperation, "operation " + plan.Operation + " in flight", nil
	case c.Status == metav1.ConditionFalse:
		return metalv1alpha1.ReasonOperationFailed,
			"operation " + plan.Operation + " failed: " + c.Message, nil
	}
	return metalv1alpha1.ReasonConvergenceFailed,
		"operation " + plan.Operation + " succeeded but the observed state did not converge", nil
}

// propose creates the proposal, or adopts it when it already exists. The
// deterministic name dedupes a double-propose race; a same-named operation that
// is already finished belongs to an earlier round of the same plan (drift came
// back after the plan converged), so the proposal moves to the next round's
// name instead of mistaking that record for itself.
func (r *ConvergenceReconciler) propose(ctx context.Context, op *metalv1alpha1.HostOperation) error {
	base := op.Name
	for round := 1; round <= 64; round++ {
		if round > 1 {
			op.Name = fmt.Sprintf("%s-%d", base, round)
		}
		err := r.Create(ctx, op)
		if err == nil || !apierrors.IsAlreadyExists(err) {
			return err
		}
		existing := &metalv1alpha1.HostOperation{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(op), existing); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !opFinished(existing) {
			return nil // this round's proposal — a double-propose race
		}
	}
	return fmt.Errorf("proposal %s: too many finished rounds of the same plan", base)
}

// proposal builds a convergence HostOperation. The deterministic name (host,
// domain, plan-hash prefix) makes proposing create-if-absent — the apiserver
// dedupes a double-propose race.
func (r *ConvergenceReconciler) proposal(hostName, domain, hash, reason string, actions []metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", hostName, domain, hash8(hash)),
			Namespace: r.OperationsNamespace,
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:                 metalv1alpha1.Reference{Name: hostName},
			ProposedBy:              r.Proposer,
			Reason:                  reason,
			PlanHash:                hash,
			Actions:                 actions,
			Approval:                metalv1alpha1.OperationApproval{Required: ptr.To(true)},
			TTLSecondsAfterFinished: ptr.To(proposalTTL),
		},
	}
}

// observeBios reads the machine's BIOS state and registry over a read-only
// session on the first reachable path.
func (r *ConvergenceReconciler) observeBios(ctx context.Context, host *metalv1alpha1.Host) (bmc.BiosState, []bmc.BiosAttribute, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	cl, _, holdMsg, err := hostPathClient(cctx, r.Client, r.CredentialsNamespace, host, true)
	if err != nil {
		return bmc.BiosState{}, nil, err
	}
	if holdMsg != "" {
		return bmc.BiosState{}, nil, errors.New(holdMsg)
	}
	defer cl.Close() //nolint:errcheck // read-only session
	pc := cl.(*pathClient)
	state, err := cl.Bios(cctx, pc.systemID)
	if err != nil {
		return bmc.BiosState{}, nil, err
	}
	schema, err := cl.BiosSchema(cctx, pc.systemID)
	if err != nil {
		return bmc.BiosState{}, nil, err
	}
	return state, schema, nil
}

// materializeSchema renders the discovered registry into the host's
// BiosSchema. Serving no registry materializes nothing — absence is data, and
// an earlier discovery is not erased by it.
func (r *ConvergenceReconciler) materializeSchema(ctx context.Context, host *metalv1alpha1.Host, registry string, schema []bmc.BiosAttribute) error {
	if schema == nil {
		return nil
	}
	attrs := make([]metalv1alpha1.BiosSchemaAttribute, 0, len(schema))
	for _, a := range schema {
		attrs = append(attrs, metalv1alpha1.BiosSchemaAttribute{
			Name: a.Name, Type: a.Type, AllowedValues: a.AllowedValues,
		})
	}
	slices.SortFunc(attrs, func(a, b metalv1alpha1.BiosSchemaAttribute) int {
		return strings.Compare(a.Name, b.Name)
	})
	obj := &metalv1alpha1.BiosSchema{ObjectMeta: metav1.ObjectMeta{Name: host.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := controllerutil.SetOwnerReference(host, obj, r.Scheme()); err != nil {
			return err
		}
		obj.Spec.Registry = registry
		obj.Spec.Attributes = attrs
		return nil
	})
	if err != nil {
		return fmt.Errorf("materialize biosschema %s: %w", host.Name, err)
	}
	return nil
}

// driftedKeys lists the managed keys off their desired value, sorted. Three
// ways to drift: the observed value differs; the machine does not serve the
// key at all (the registry validated it, so staging is the honest attempt —
// the vendor verdict lands on the operation); or a CONTRADICTORY value sits
// staged in the pending object — current may match, but the next reboot will
// flip it, so it is future drift on a schedule and must be converged now.
func driftedKeys(desired, observed, pending map[string]any) []string {
	var drifted []string
	for k, want := range desired {
		got, ok := observed[k]
		staged, stagedOK := pending[k]
		if !ok || !jsonEqual(got, want) || (stagedOK && !jsonEqual(staged, want)) {
			drifted = append(drifted, k)
		}
	}
	slices.Sort(drifted)
	return drifted
}

func (r *ConvergenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.BiosSettings{}).
		Watches(&metalv1alpha1.Host{}, handler.EnqueueRequestsFromMapFunc(sameName)).
		Watches(&metalv1alpha1.BiosProfile{}, handler.EnqueueRequestsFromMapFunc(r.allHosts)).
		Watches(&metalv1alpha1.FirmwareProfile{}, handler.EnqueueRequestsFromMapFunc(r.allHosts)).
		Watches(&metalv1alpha1.ManagementEndpoint{}, handler.EnqueueRequestsFromMapFunc(r.hostsBehindEndpoint)).
		Watches(&metalv1alpha1.HostOperation{}, handler.EnqueueRequestsFromMapFunc(hostForOperation)).
		Named("convergence").
		Complete(r)
}

// sameName: the reconcile key IS the host name (BiosSettings share it).
func sameName(_ context.Context, obj client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
}

// allHosts fans a profile event out to every host — matching is per-host.
func (r *ConvergenceReconciler) allHosts(ctx context.Context, _ client.Object) []ctrl.Request {
	hosts := &metalv1alpha1.HostList{}
	if err := r.List(ctx, hosts); err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(hosts.Items))
	for i := range hosts.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: hosts.Items[i].Name}})
	}
	return reqs
}

// hostsBehindEndpoint re-converges when an endpoint's observations move — the
// firmware version after a flash arrives through the endpoint poll.
func (r *ConvergenceReconciler) hostsBehindEndpoint(_ context.Context, obj client.Object) []ctrl.Request {
	names := hostNames(obj.(*metalv1alpha1.ManagementEndpoint))
	reqs := make([]ctrl.Request, 0, len(names))
	for _, n := range names {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: n}})
	}
	return reqs
}
