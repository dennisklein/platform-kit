// Package phase derives status.phase and status.blockedOn — pure functions of
// conditions + spec-derived facts, nothing else (ADR 0012: conditions are truth, phase
// is a human-facing summary, written but never read). The Stamp helpers are the ONLY
// write sites: reconcilers call them, and TestPhaseWriteSites (mise run lint:phase)
// fails on any other write anywhere in the module and on any read in a reconciler.
//
// The derivation is a precedence list, not a state machine — there is no memory of a
// previous phase, every call re-derives from observations. Phases whose inputs are
// not observations (claimed, releasing, maintenance) hang off HostFacts booleans the
// owning reconcilers compute from spec and metadata.
package phase

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// ForEndpoint derives the ManagementEndpoint phase. Precedence: hard stops first
// (Unreachable, Mismatch), then the pipeline position in observation order
// (Pending → Connecting → Inventorying → Verifying → Configuring → Ready).
func ForEndpoint(conds []metav1.Condition) (metalv1alpha1.EndpointPhase, string) {
	blocked := blockedOn(conds)

	reach := meta.FindStatusCondition(conds, metalv1alpha1.ConditionReachable)
	switch {
	case reach == nil && len(conds) == 0:
		return metalv1alpha1.EndpointPending, blocked
	case reach == nil || reach.Status == metav1.ConditionUnknown:
		return metalv1alpha1.EndpointConnecting, blocked
	case reach.Status == metav1.ConditionFalse:
		return metalv1alpha1.EndpointUnreachable, blocked
	}

	if v := meta.FindStatusCondition(conds, metalv1alpha1.ConditionVerified); v != nil && v.Status == metav1.ConditionFalse {
		return metalv1alpha1.EndpointMismatch, blocked
	}
	if !meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionInventoried) {
		return metalv1alpha1.EndpointInventorying, blocked
	}
	if !meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionVerified) {
		return metalv1alpha1.EndpointVerifying, blocked
	}
	// Configured is profile-driven: absent means no EndpointProfile matched — that is
	// Ready, not an eternal Configuring.
	if c := meta.FindStatusCondition(conds, metalv1alpha1.ConditionConfigured); c != nil && c.Status != metav1.ConditionTrue {
		return metalv1alpha1.EndpointConfiguring, blocked
	}
	return metalv1alpha1.EndpointReady, blocked
}

// HostFacts are the spec-derived inputs to the Host derivation — booleans the owning
// reconcilers compute (Claimed from spec.claimRef, Releasing from the claim's
// deletion, Maintenance from the metal.pk.io/maintenance annotation). They keep the function total: every phase in the enum is reachable
// and tested.
type HostFacts struct {
	// Claimed: a HostClaim is bound to this host.
	Claimed bool
	// Releasing: the claim is gone and cleanup (wipe, eject, unbind) is underway.
	Releasing bool
	// Maintenance: a person parked the machine outside the lifecycle
	// (annotation metal.pk.io/maintenance).
	Maintenance bool
}

// ForHost derives the Host phase. Precedence: parked states first (Maintenance,
// Releasing), then a failed OS-ladder observation (Status=False means attempted and
// failed; Unknown means in flight; absent means not attempted), then the ladder
// downwards (Provisioned → Provisioning → Assigned → Available → Inspecting →
// Enrolling).
func ForHost(conds []metav1.Condition, f HostFacts) (metalv1alpha1.HostPhase, string) {
	blocked := blockedOn(conds)

	switch {
	case f.Maintenance:
		// An alarm (a failed verification) still outranks the person's
		// own reason for the stop.
		if blocked == "" {
			blocked = metalv1alpha1.ReasonHostInMaintenance
		}
		return metalv1alpha1.HostMaintenance, blocked
	case f.Releasing:
		return metalv1alpha1.HostReleasing, blocked
	}

	for _, t := range []string{metalv1alpha1.ConditionBooted, metalv1alpha1.ConditionOSReady} {
		if c := meta.FindStatusCondition(conds, t); c != nil && c.Status == metav1.ConditionFalse {
			return metalv1alpha1.HostFailed, blocked
		}
	}

	osReady := meta.FindStatusCondition(conds, metalv1alpha1.ConditionOSReady)
	switch {
	case osReady != nil && osReady.Status == metav1.ConditionTrue:
		return metalv1alpha1.HostProvisioned, blocked
	case f.Claimed && (osReady != nil || meta.FindStatusCondition(conds, metalv1alpha1.ConditionBooted) != nil):
		return metalv1alpha1.HostProvisioning, blocked
	case f.Claimed:
		return metalv1alpha1.HostAssigned, blocked
	}

	if meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionInspected) &&
		meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionVerified) {
		return metalv1alpha1.HostAvailable, blocked
	}
	if meta.FindStatusCondition(conds, metalv1alpha1.ConditionInspected) != nil ||
		meta.IsStatusConditionTrue(conds, metalv1alpha1.ConditionReachable) {
		return metalv1alpha1.HostInspecting, blocked
	}
	return metalv1alpha1.HostEnrolling, blocked
}

// ForClaim derives the HostClaim phase from its Bound condition. Reference
// resolution maps 0/1/2+ matches to Pending/Bound/Ambiguous (naming rule 9);
// HostNotDiscovered is in flight, not blocked — the machine just isn't there yet.
func ForClaim(conds []metav1.Condition) (metalv1alpha1.ClaimPhase, string) {
	b := meta.FindStatusCondition(conds, metalv1alpha1.ConditionBound)
	switch {
	case b == nil:
		return metalv1alpha1.ClaimPending, ""
	case b.Status == metav1.ConditionTrue:
		// Bound; whatever stops provisioning is why the claim isn't advancing.
		if p := meta.FindStatusCondition(conds, metalv1alpha1.ConditionProvisioned); p != nil &&
			p.Status == metav1.ConditionFalse && provisioningHolds[p.Reason] {
			return metalv1alpha1.ClaimBound, p.Reason
		}
		return metalv1alpha1.ClaimBound, ""
	case b.Reason == metalv1alpha1.ReasonAmbiguousSystem:
		return metalv1alpha1.ClaimAmbiguous, b.Reason
	case b.Reason == metalv1alpha1.ReasonHostNotDiscovered:
		return metalv1alpha1.ClaimPending, ""
	}
	return metalv1alpha1.ClaimPending, b.Reason
}

// provisioningHolds are the Provisioned=False reasons that need a person —
// the rest are in-flight waits, not blocks.
var provisioningHolds = map[string]bool{
	metalv1alpha1.ReasonSingleMediumUnsupported: true,
	metalv1alpha1.ReasonNoBootableMedia:         true,
	metalv1alpha1.ReasonOperationFailed:         true,
	metalv1alpha1.ReasonReleaseFailed:           true,
	metalv1alpha1.ReasonHeldByUser:              true,
	metalv1alpha1.ReasonHostInMaintenance:       true,
	metalv1alpha1.ReasonSelfDisruption:          true,
}

// ForOperation derives the HostOperation phase from Approved + Executed. A
// finished Executed wins (Succeeded/Failed are terminal); an unstamped gate is
// AwaitingApproval; a queued operation stays Proposed (blocked on its
// predecessor); an approved, unfinished operation is Running.
func ForOperation(conds []metav1.Condition) (metalv1alpha1.OperationPhase, string) {
	ex := meta.FindStatusCondition(conds, metalv1alpha1.ConditionExecuted)
	switch {
	case ex != nil && ex.Status == metav1.ConditionTrue:
		return metalv1alpha1.OperationSucceeded, ""
	case ex != nil && ex.Status == metav1.ConditionFalse:
		return metalv1alpha1.OperationFailed, ""
	}
	ap := meta.FindStatusCondition(conds, metalv1alpha1.ConditionApproved)
	switch {
	case ap == nil:
		return metalv1alpha1.OperationProposed, ""
	case ap.Status != metav1.ConditionTrue:
		return metalv1alpha1.OperationAwaitingApproval, ap.Reason
	}
	if ex != nil && ex.Reason != metalv1alpha1.ReasonExecuting {
		// Approved but held: queued behind a predecessor, or the host/paths are
		// not there — the reason says which.
		return metalv1alpha1.OperationProposed, ex.Reason
	}
	return metalv1alpha1.OperationRunning, ""
}

// blockedOn derives WHY the object isn't advancing — a reason, not a phase, because it
// recurs across phases. Precedence: a failed verification outranks a pending approval.
func blockedOn(conds []metav1.Condition) string {
	if v := meta.FindStatusCondition(conds, metalv1alpha1.ConditionVerified); v != nil && v.Status == metav1.ConditionFalse {
		return v.Reason
	}
	if ap := meta.FindStatusCondition(conds, metalv1alpha1.ConditionApprovalPending); ap != nil && ap.Status == metav1.ConditionTrue {
		// Reason names the gated domain (Bios, Firmware, Reboot…); rendered in the
		// design doc's Approval(bios) shape. Condition reasons cannot carry parens.
		return fmt.Sprintf("Approval(%s)", ap.Reason)
	}
	return ""
}

// StampEndpoint derives and writes phase + blockedOn — the single endpoint write site.
func StampEndpoint(ep *metalv1alpha1.ManagementEndpoint) {
	ep.Status.Phase, ep.Status.BlockedOn = ForEndpoint(ep.Status.Conditions)
}

// StampEndpointOnto derives from src's conditions and writes the result onto
// dst. It exists for server-side apply: an apply object carries only the
// fields its field manager means to own, so it holds a partial condition set —
// but the phase is a summary of ALL of them, and deriving it from the fragment
// would publish a different answer per writer.
func StampEndpointOnto(dst, src *metalv1alpha1.ManagementEndpoint) {
	dst.Status.Phase, dst.Status.BlockedOn = ForEndpoint(src.Status.Conditions)
}

// StampHost derives and writes phase + blockedOn — the single host write site.
func StampHost(h *metalv1alpha1.Host, f HostFacts) {
	h.Status.Phase, h.Status.BlockedOn = ForHost(h.Status.Conditions, f)
}

// StampHostOnto derives from src's conditions and writes onto dst, for the same
// reason StampEndpointOnto exists: an apply object holds only its owner's share
// of the conditions, and the phase summarizes all of them.
func StampHostOnto(dst, src *metalv1alpha1.Host, f HostFacts) {
	dst.Status.Phase, dst.Status.BlockedOn = ForHost(src.Status.Conditions, f)
}

// StampClaim derives and writes phase + blockedOn — the single claim write site.
func StampClaim(c *metalv1alpha1.HostClaim) {
	c.Status.Phase, c.Status.BlockedOn = ForClaim(c.Status.Conditions)
}

// StampOperation derives and writes phase + blockedOn — the single operation
// write site.
func StampOperation(op *metalv1alpha1.HostOperation) {
	op.Status.Phase, op.Status.BlockedOn = ForOperation(op.Status.Conditions)
}
