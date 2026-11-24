package phase_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

func c(t string, s metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: reason}
}

func TestForEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		conds   []metav1.Condition
		phase   metalv1alpha1.EndpointPhase
		blocked string
	}{
		{name: "no observations", phase: metalv1alpha1.EndpointPending},
		{name: "connect in flight",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionReachable, metav1.ConditionUnknown, "Connecting")},
			phase: metalv1alpha1.EndpointConnecting},
		{name: "unreachable",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionReachable, metav1.ConditionFalse, "DialTimeout")},
			phase: metalv1alpha1.EndpointUnreachable},
		{name: "reachable, not yet inventoried",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp")},
			phase: metalv1alpha1.EndpointInventorying},
		{name: "inventoried, verification pending",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp"),
				c(metalv1alpha1.ConditionInventoried, metav1.ConditionTrue, "FactsGathered"),
			},
			phase: metalv1alpha1.EndpointVerifying},
		{name: "serial mismatch is alert-and-hold",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp"),
				c(metalv1alpha1.ConditionInventoried, metav1.ConditionTrue, "FactsGathered"),
				c(metalv1alpha1.ConditionVerified, metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch),
			},
			phase: metalv1alpha1.EndpointMismatch, blocked: "SerialMismatch"},
		{name: "profile applying",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp"),
				c(metalv1alpha1.ConditionInventoried, metav1.ConditionTrue, "FactsGathered"),
				c(metalv1alpha1.ConditionVerified, metav1.ConditionTrue, "SerialMatch"),
				c(metalv1alpha1.ConditionConfigured, metav1.ConditionFalse, "RotatingCredentials"),
			},
			phase: metalv1alpha1.EndpointConfiguring},
		{name: "ready without any profile matched (Configured absent)",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp"),
				c(metalv1alpha1.ConditionInventoried, metav1.ConditionTrue, "FactsGathered"),
				c(metalv1alpha1.ConditionVerified, metav1.ConditionTrue, "SerialMatch"),
			},
			phase: metalv1alpha1.EndpointReady},
		{name: "ready with profile applied",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp"),
				c(metalv1alpha1.ConditionInventoried, metav1.ConditionTrue, "FactsGathered"),
				c(metalv1alpha1.ConditionVerified, metav1.ConditionTrue, "SerialMatch"),
				c(metalv1alpha1.ConditionConfigured, metav1.ConditionTrue, "ProfileApplied"),
			},
			phase: metalv1alpha1.EndpointReady},
		{name: "mismatch outranks unreached inventory stage",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp"),
				c(metalv1alpha1.ConditionVerified, metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch),
			},
			phase: metalv1alpha1.EndpointMismatch, blocked: "SerialMismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b := phase.ForEndpoint(tt.conds)
			assert.Equal(t, tt.phase, p)
			assert.Equal(t, tt.blocked, b)
		})
	}
}

func TestForHost(t *testing.T) {
	inspected := []metav1.Condition{
		c(metalv1alpha1.ConditionInspected, metav1.ConditionTrue, "InventoryGathered"),
		c(metalv1alpha1.ConditionVerified, metav1.ConditionTrue, "SerialMatch"),
	}
	tests := []struct {
		name    string
		conds   []metav1.Condition
		facts   phase.HostFacts
		phase   metalv1alpha1.HostPhase
		blocked string
	}{
		{name: "no observations", phase: metalv1alpha1.HostEnrolling},
		{name: "reachable starts inspection",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp")},
			phase: metalv1alpha1.HostInspecting},
		{name: "inspection in flight",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionInspected, metav1.ConditionUnknown, "Gathering")},
			phase: metalv1alpha1.HostInspecting},
		{name: "inspected + verified = available", conds: inspected, phase: metalv1alpha1.HostAvailable},
		{name: "verification failed holds availability",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionInspected, metav1.ConditionTrue, "InventoryGathered"),
				c(metalv1alpha1.ConditionVerified, metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch),
			},
			phase: metalv1alpha1.HostInspecting, blocked: "SerialMismatch"},
		{name: "claimed, no disruptive work yet",
			conds: inspected, facts: phase.HostFacts{Claimed: true},
			phase: metalv1alpha1.HostAssigned},
		{name: "claimed with approval gate open",
			conds: append(append([]metav1.Condition{}, inspected...),
				c(metalv1alpha1.ConditionApprovalPending, metav1.ConditionTrue, "Bios")),
			facts: phase.HostFacts{Claimed: true},
			phase: metalv1alpha1.HostAssigned, blocked: "Approval(Bios)"},
		{name: "boot underway",
			conds: append(append([]metav1.Condition{}, inspected...),
				c(metalv1alpha1.ConditionBooted, metav1.ConditionUnknown, "MediaInserted")),
			facts: phase.HostFacts{Claimed: true},
			phase: metalv1alpha1.HostProvisioning},
		{name: "os ready = provisioned",
			conds: append(append([]metav1.Condition{}, inspected...),
				c(metalv1alpha1.ConditionBooted, metav1.ConditionTrue, "BootProgressOK"),
				c(metalv1alpha1.ConditionOSReady, metav1.ConditionTrue, "MachineReady")),
			facts: phase.HostFacts{Claimed: true},
			phase: metalv1alpha1.HostProvisioned},
		{name: "boot attempted and failed",
			conds: append(append([]metav1.Condition{}, inspected...),
				c(metalv1alpha1.ConditionBooted, metav1.ConditionFalse, "BootTimeout")),
			facts: phase.HostFacts{Claimed: true},
			phase: metalv1alpha1.HostFailed},
		{name: "releasing outranks the ladder",
			conds: inspected, facts: phase.HostFacts{Releasing: true},
			phase: metalv1alpha1.HostReleasing},
		{name: "maintenance outranks everything",
			conds: inspected, facts: phase.HostFacts{Maintenance: true, Claimed: true},
			phase: metalv1alpha1.HostMaintenance, blocked: "HostInMaintenance"},
		{name: "a failed verification still shows in maintenance",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionVerified, metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch)},
			facts: phase.HostFacts{Maintenance: true},
			phase: metalv1alpha1.HostMaintenance, blocked: "SerialMismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b := phase.ForHost(tt.conds, tt.facts)
			assert.Equal(t, tt.phase, p)
			assert.Equal(t, tt.blocked, b)
		})
	}
}

func TestForClaim(t *testing.T) {
	tests := []struct {
		name    string
		conds   []metav1.Condition
		phase   metalv1alpha1.ClaimPhase
		blocked string
	}{
		{name: "no observations", phase: metalv1alpha1.ClaimPending},
		{name: "bound",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionTrue, metalv1alpha1.ReasonHostBound)},
			phase: metalv1alpha1.ClaimBound},
		{name: "bind retained under a stale assetRef is still bound",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionTrue, metalv1alpha1.ReasonBindRetained)},
			phase: metalv1alpha1.ClaimBound},
		{name: "host not discovered is in flight, not blocked",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionFalse, metalv1alpha1.ReasonHostNotDiscovered)},
			phase: metalv1alpha1.ClaimPending},
		{name: "ambiguous system needs a systemRef",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionFalse, metalv1alpha1.ReasonAmbiguousSystem)},
			phase: metalv1alpha1.ClaimAmbiguous, blocked: "AmbiguousSystem"},
		{name: "lost race holds at the contention reason",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionFalse, metalv1alpha1.ReasonHostAlreadyClaimed)},
			phase: metalv1alpha1.ClaimPending, blocked: "HostAlreadyClaimed"},
		{name: "missing asset blocks",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionFalse, metalv1alpha1.ReasonAssetNotFound)},
			phase: metalv1alpha1.ClaimPending, blocked: "AssetNotFound"},
		{name: "an asset with no endpoint blocks",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionBound, metav1.ConditionFalse, metalv1alpha1.ReasonAssetNotMaterialized)},
			phase: metalv1alpha1.ClaimPending, blocked: "AssetNotMaterialized"},
		{name: "a hold blocks a bound claim",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionBound, metav1.ConditionTrue, metalv1alpha1.ReasonHostBound),
				c(metalv1alpha1.ConditionProvisioned, metav1.ConditionFalse, metalv1alpha1.ReasonHeldByUser),
			},
			phase: metalv1alpha1.ClaimBound, blocked: "HeldByUser"},
		{name: "waiting on an operation is not a block",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionBound, metav1.ConditionTrue, metalv1alpha1.ReasonHostBound),
				c(metalv1alpha1.ConditionProvisioned, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingOperation),
			},
			phase: metalv1alpha1.ClaimBound},
		{name: "an install that waits for its approval blocks on a person",
			conds: []metav1.Condition{
				c(metalv1alpha1.ConditionBound, metav1.ConditionTrue, metalv1alpha1.ReasonHostBound),
				c(metalv1alpha1.ConditionProvisioned, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingApproval),
			},
			phase: metalv1alpha1.ClaimBound, blocked: "Approval(Provisioning)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b := phase.ForClaim(tt.conds)
			assert.Equal(t, tt.phase, p)
			assert.Equal(t, tt.blocked, b)
		})
	}
}

func TestForOperation(t *testing.T) {
	approved := c(metalv1alpha1.ConditionApproved, metav1.ConditionTrue, metalv1alpha1.ReasonApproved)
	tests := []struct {
		name    string
		conds   []metav1.Condition
		phase   metalv1alpha1.OperationPhase
		blocked string
	}{
		{name: "no observations", phase: metalv1alpha1.OperationProposed},
		{name: "gate unstamped",
			conds: []metav1.Condition{c(metalv1alpha1.ConditionApproved, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingApproval)},
			phase: metalv1alpha1.OperationAwaitingApproval, blocked: "AwaitingApproval"},
		{name: "approved, executor not yet entered",
			conds: []metav1.Condition{approved},
			phase: metalv1alpha1.OperationRunning},
		{name: "queued behind a predecessor",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonOperationQueued)},
			phase: metalv1alpha1.OperationProposed, blocked: "OperationQueued"},
		{name: "host not materialized holds",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonHostNotFound)},
			phase: metalv1alpha1.OperationProposed, blocked: "HostNotFound"},
		{name: "executing",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonExecuting)},
			phase: metalv1alpha1.OperationRunning},
		{name: "succeeded",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionTrue, metalv1alpha1.ReasonStepsSucceeded)},
			phase: metalv1alpha1.OperationSucceeded},
		{name: "a failed step is terminal",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse, metalv1alpha1.ReasonStepFailed)},
			phase: metalv1alpha1.OperationFailed},
		{name: "an abort is terminal and is not a failure",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse, metalv1alpha1.ReasonAborted)},
			phase: metalv1alpha1.OperationAborted},
		{name: "held by the cluster guard before it starts",
			conds: []metav1.Condition{approved,
				c(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonClusterDisruption)},
			phase: metalv1alpha1.OperationProposed, blocked: "ClusterDisruption"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b := phase.ForOperation(tt.conds)
			assert.Equal(t, tt.phase, p)
			assert.Equal(t, tt.blocked, b)
		})
	}
}

// The stamp helpers are the single write site (TestPhaseWriteSites finds any
// other) — prove they derive from the object's own conditions.
func TestStamp(t *testing.T) {
	ep := &metalv1alpha1.ManagementEndpoint{}
	ep.Status.Conditions = []metav1.Condition{c(metalv1alpha1.ConditionReachable, metav1.ConditionFalse, "DialTimeout")}
	phase.StampEndpoint(ep)
	assert.Equal(t, metalv1alpha1.EndpointUnreachable, ep.Status.Phase)
	assert.Empty(t, ep.Status.BlockedOn)

	h := &metalv1alpha1.Host{}
	h.Status.Conditions = []metav1.Condition{
		c(metalv1alpha1.ConditionInspected, metav1.ConditionTrue, "InventoryGathered"),
		c(metalv1alpha1.ConditionVerified, metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch),
	}
	phase.StampHost(h, phase.HostFacts{})
	assert.Equal(t, metalv1alpha1.HostInspecting, h.Status.Phase)
	assert.Equal(t, "SerialMismatch", h.Status.BlockedOn)

	claim := &metalv1alpha1.HostClaim{}
	claim.Status.Conditions = []metav1.Condition{
		c(metalv1alpha1.ConditionBound, metav1.ConditionFalse, metalv1alpha1.ReasonHostAlreadyClaimed)}
	phase.StampClaim(claim)
	assert.Equal(t, metalv1alpha1.ClaimPending, claim.Status.Phase)
	assert.Equal(t, "HostAlreadyClaimed", claim.Status.BlockedOn)

	op := &metalv1alpha1.HostOperation{}
	op.Status.Conditions = []metav1.Condition{
		c(metalv1alpha1.ConditionApproved, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingApproval)}
	phase.StampOperation(op)
	assert.Equal(t, metalv1alpha1.OperationAwaitingApproval, op.Status.Phase)
	assert.Equal(t, "AwaitingApproval", op.Status.BlockedOn)
}
