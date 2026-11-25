package controller

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// managerProfiles keeps the profiles that say something about the manager's
// firmware; a profile for system components only is no manager profile.
func managerProfiles(profiles []metalv1alpha1.FirmwareProfile) []metalv1alpha1.FirmwareProfile {
	var out []metalv1alpha1.FirmwareProfile
	for _, p := range profiles {
		if p.Spec.Manager != nil {
			out = append(out, p)
		}
	}
	return out
}

// mergeSystemFirmware resolves the system targets of the matched profiles,
// per component, with the same layering as everything else: (layer, name)
// order, a later layer overrides the whole target, two profiles on one
// layer that disagree on a component are a conflict.
func mergeSystemFirmware(profiles []metalv1alpha1.FirmwareProfile) ([]metalv1alpha1.SystemFirmwareTarget, []string) {
	sorted := slices.Clone(profiles)
	slices.SortFunc(sorted, func(a, b metalv1alpha1.FirmwareProfile) int {
		if c := int(a.Spec.Layer) - int(b.Spec.Layer); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	type won struct {
		target  metalv1alpha1.SystemFirmwareTarget
		profile string
		layer   int32
	}
	by := map[string]won{}
	var conflicts []string
	for _, p := range sorted {
		for _, t := range p.Spec.System {
			if prev, ok := by[t.Component]; ok && prev.layer == p.Spec.Layer && prev.target != t {
				conflicts = append(conflicts, fmt.Sprintf("profiles %s and %s disagree on component %s on layer %d",
					prev.profile, p.Name, t.Component, p.Spec.Layer))
				continue
			}
			by[t.Component] = won{t, p.Name, p.Spec.Layer}
		}
	}
	out := make([]metalv1alpha1.SystemFirmwareTarget, 0, len(by))
	for _, w := range by {
		out = append(out, w.target)
	}
	slices.SortFunc(out, func(a, b metalv1alpha1.SystemFirmwareTarget) int { return strings.Compare(a.Component, b.Component) })
	slices.Sort(conflicts)
	return out, conflicts
}

// systemStage is one stageFirmware action of a plan: a target and the
// inventory entries it updates.
type systemStage struct {
	Component string   `json:"component"`
	Version   string   `json:"version"`
	Image     string   `json:"image"`
	Entries   []string `json:"entries"`
}

// systemDrift joins the desired targets with the inventory. It returns the
// stages a plan needs (only the entries off their version), what drifts in
// words, and why nothing can be planned (a pattern matching nothing).
func systemDrift(targets []metalv1alpha1.SystemFirmwareTarget, inventory []metalv1alpha1.FirmwareInventoryItem) (stages []systemStage, drifted []string, hold string) {
	for _, t := range targets {
		re, err := regexp.Compile("^(?:" + t.Component + ")$")
		if err != nil {
			return nil, nil, fmt.Sprintf("component %q is not a valid pattern: %v", t.Component, err)
		}
		st := systemStage{Component: t.Component, Version: t.Version, Image: t.Image}
		matched := 0
		for _, e := range inventory {
			if !re.MatchString(e.ID) {
				continue
			}
			matched++
			if e.Version == t.Version {
				continue
			}
			if !e.Updateable {
				return nil, nil, fmt.Sprintf("inventory entry %s serves %s, want %s, and the BMC marks it not updateable",
					e.ID, orUnknown(e.Version), t.Version)
			}
			st.Entries = append(st.Entries, e.ID)
			drifted = append(drifted, fmt.Sprintf("%s serves %s, want %s", e.ID, orUnknown(e.Version), t.Version))
		}
		if matched == 0 {
			ids := make([]string, 0, len(inventory))
			for _, e := range inventory {
				ids = append(ids, e.ID)
			}
			return nil, nil, fmt.Sprintf("component %q matches no inventory entry (the BMC lists %s)",
				t.Component, strings.Join(ids, ", "))
		}
		if len(st.Entries) > 0 {
			slices.Sort(st.Entries)
			stages = append(stages, st)
		}
	}
	return stages, drifted, ""
}

func orUnknown(v string) string {
	if v == "" {
		return "no version"
	}
	return v
}

// systemFirmware converges the system components (BIOS/UEFI, NIC, RAID) and
// reports whether the BIOS-settings lane may proceed. The observation is the
// FirmwareInventory the endpoint poll already serves; the change is staged
// to apply at the next reset, inside the plan disruptivePlan builds, and the
// inventory reading back the version afterwards is the only proof taken.
func (r *ConvergenceReconciler) systemFirmware(ctx context.Context, host *metalv1alpha1.Host, profiles []metalv1alpha1.FirmwareProfile,
	managerConverged bool, setCond func(string, metav1.ConditionStatus, string, string)) (bool, error) {
	targets, conflicts := mergeSystemFirmware(profiles)
	cond := metalv1alpha1.ConditionSystemFirmwareConverged
	if len(targets) == 0 && len(conflicts) == 0 {
		if err := r.withdrawObsolete(ctx, host.Status.SystemFirmwareProposed); err != nil {
			return false, err
		}
		meta.RemoveStatusCondition(&host.Status.Conditions, cond)
		host.Status.SystemFirmwareProposed = nil
		return true, nil
	}
	if len(conflicts) > 0 {
		setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonProfileConflict, strings.Join(conflicts, "; "))
		return false, nil
	}

	var inventory []metalv1alpha1.FirmwareInventoryItem
	seen := map[string]bool{}
	push, observed := false, false
	for _, p := range host.Status.ManagedBy {
		ep := &metalv1alpha1.ManagementEndpoint{}
		if err := r.Get(ctx, client.ObjectKey{Name: p.EndpointRef.Name}, ep); err != nil {
			continue
		}
		if len(ep.Status.Firmware) > 0 {
			observed = true
		}
		for _, e := range ep.Status.Firmware {
			if !seen[e.ID] {
				seen[e.ID] = true
				inventory = append(inventory, e)
			}
		}
		if caps := ep.Status.Capabilities; caps != nil && caps.UpdateDialect == string(bmc.DialectMultipartPush) {
			push = true
		}
	}
	if !observed {
		setCond(cond, metav1.ConditionUnknown, metalv1alpha1.ReasonNoReachablePath, "no firmware inventory observed yet")
		return false, nil
	}
	stages, drifted, hold := systemDrift(targets, inventory)
	if hold != "" {
		setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonNoMatchingComponent, hold)
		return false, nil
	}
	if len(stages) == 0 {
		setCond(cond, metav1.ConditionTrue, metalv1alpha1.ReasonInSync, fmt.Sprintf("%d component(s)", len(targets)))
		host.Status.SystemFirmwareProposed = nil // the plan is spent, see the manager lane
		return true, nil
	}
	what := strings.Join(drifted, "; ")
	if !managerConverged {
		setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingFirmware, what+" — the manager's firmware converges first")
		return false, nil
	}

	node, nodeHold, err := r.installedNode(ctx, host)
	if err != nil {
		return false, err
	}
	if nodeHold != "" {
		if err := r.withdrawPlan(ctx, &host.Status.SystemFirmwareProposed); err != nil {
			return false, err
		}
		setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonInstalledNodeMissing, what+"; "+nodeHold)
		return false, nil
	}
	hash := disruptiveHash(stages, node)
	if plan := host.Status.SystemFirmwareProposed; plan != nil && plan.PlanHash == hash {
		reason, msg, again, err := r.planVerdict(ctx, plan)
		if err != nil {
			return false, err
		}
		if !again {
			setCond(cond, metav1.ConditionFalse, reason, msg)
			return false, nil
		}
		host.Status.SystemFirmwareProposed = nil
	}
	if !push {
		setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonNoUpdateTransport,
			what+" — no path advertises the proven multipart-push transport")
		return false, nil
	}
	if err := r.withdrawObsolete(ctx, host.Status.SystemFirmwareProposed); err != nil {
		return false, err
	}
	// Applying it reboots the machine, like a BIOS change.
	guard, err := r.Self.Check(ctx, host)
	if err != nil {
		return false, err
	}
	if guard != "" {
		setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonSelfDisruption, what+"; no firmware change is proposed: "+guard)
		return false, nil
	}
	stage := make([]metalv1alpha1.HostOperationAction, 0, len(stages))
	for _, st := range stages {
		stage = append(stage, metalv1alpha1.HostOperationAction{StageFirmware: &metalv1alpha1.StageFirmwareAction{
			Image: st.Image, Version: st.Version, Targets: st.Entries,
		}})
	}
	op := r.proposal(host.Name, "sysfw", hash, metalv1alpha1.OperationReasonSystemFirmwareConvergence, disruptivePlan(node, stage))
	op.Spec.RequestedBy = profileRequesters(profiles)
	if err := r.propose(ctx, op); err != nil {
		return false, err
	}
	logf.FromContext(ctx).Info("proposed operation", "operation", op.Namespace+"/"+op.Name, "host", host.Name, "reason", op.Spec.Reason)
	host.Status.SystemFirmwareProposed = &metalv1alpha1.ProposedPlan{PlanHash: hash, Operation: op.Namespace + "/" + op.Name}
	setCond(cond, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingOperation,
		fmt.Sprintf("%s; operation %s/%s proposed", what, op.Namespace, op.Name))
	return false, nil
}
