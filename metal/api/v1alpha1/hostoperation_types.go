package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OperationPhase summarizes an operation's conditions. Proposed means it exists
// but has not been approved; AwaitingApproval that it is waiting on a human;
// Running that steps are executing; Succeeded and Failed are terminal. A failed
// operation stays for inspection — nothing disruptive is retried automatically.
// +kubebuilder:validation:Enum=Proposed;AwaitingApproval;Running;Succeeded;Failed
type OperationPhase string

const (
	OperationProposed         OperationPhase = "Proposed"
	OperationAwaitingApproval OperationPhase = "AwaitingApproval"
	OperationRunning          OperationPhase = "Running"
	OperationSucceeded        OperationPhase = "Succeeded"
	OperationFailed           OperationPhase = "Failed"
)

// HostOperationSpec is one disruptive thing to do to a machine, and the gate a
// human passes to let it happen.
//
// The cluster proposes an operation by creating it; a person approves it by
// patching it. That split works with ordinary RBAC because the target machine and
// the actions are frozen once created — the only thing left to change is the
// approval, so permission to patch is permission to approve. Who may approve, and
// the rule that nobody approves their own proposal in advance, are enforced at
// admission.
type HostOperationSpec struct {
	// HostRef names the Host this operation acts on.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hostRef is frozen after creation"
	HostRef Reference `json:"hostRef"`

	// Reason names the desired-state gate this operation serves (BiosConvergence,
	// FirmwareConvergence, …); empty for a human-requested op. It becomes the
	// Approval(<reason>) domain on the Host's blockedOn — hence the condition-reason
	// shape.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Reason string `json:"reason,omitempty"`

	// PlanHash binds a proposed operation to the desired state it serves — an
	// unchanged hash never spawns a second op (the convergence reconcilers' guard).
	// +optional
	PlanHash string `json:"planHash,omitempty"`

	// Actions run in order; each entry is exactly one action. A failed step fails
	// the operation — disruptive one-shots never auto-retry.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="actions are frozen after creation"
	// +listType=atomic
	Actions []HostOperationAction `json:"actions"`

	// Approval is the human gate. Omitted means required — the safe default is
	// a closed gate.
	// +kubebuilder:default={required: true}
	// +optional
	Approval OperationApproval `json:"approval,omitempty"`

	// TTLSecondsAfterFinished garbage-collects a finished operation Job-style.
	// Unset keeps it forever (human-requested ops stay as their own record);
	// convergence proposals set it — the plan memory on the proposing object,
	// not the operation's existence, is what keeps an unchanged planHash from
	// spawning again.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// HostOperationAction is a one-of union: exactly one action per entry.
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=1
type HostOperationAction struct {
	// PowerCycle restarts the system (ForceRestart; On when powered off). The one
	// action needing a baseline: completion is the reset-time observation advancing
	// past it, and a captured baseline is never re-issued into a box mid-reboot.
	// +optional
	PowerCycle *PowerCycleAction `json:"powerCycle,omitempty"`

	// SetBootOverride sets the source the machine boots from next. The target
	// must be one the endpoint actually offers — machines disagree about what
	// they call a virtual CD, so the value is checked against what was
	// discovered rather than assumed.
	// +optional
	SetBootOverride *SetBootOverrideAction `json:"setBootOverride,omitempty"`

	// InsertMedia mounts an image URL on a virtual-media slot, dispatching the
	// dialect the endpoint advertises (standard InsertMedia vs OEM VmmControl).
	// +optional
	InsertMedia *InsertMediaAction `json:"insertMedia,omitempty"`

	// EjectMedia unmounts a virtual-media slot.
	// +optional
	EjectMedia *EjectMediaAction `json:"ejectMedia,omitempty"`

	// ApplyBios stages a BiosSettings object's desired attributes into the
	// machine's pending (@Redfish.Settings) object. Staging changes nothing —
	// the world moves at the next system reset, so a convergence operation
	// pairs this with a powerCycle.
	// +optional
	ApplyBios *ApplyBiosAction `json:"applyBios,omitempty"`

	// FlashFirmware pushes a firmware image to the management controller and
	// waits for proof that it took: the controller rebooting, and the expected
	// version reading back afterwards. A reported success is not believed on
	// its own.
	// +optional
	FlashFirmware *FlashFirmwareAction `json:"flashFirmware,omitempty"`
}

// PowerCycleAction restarts the machine. It takes no fields: there is only one
// way to do it, forcibly, and a machine that is already off is turned on instead.
type PowerCycleAction struct{}

// SetBootOverrideAction points the next boot at a particular source.
type SetBootOverrideAction struct {
	// Target is the boot source (UsbCd, Cd, Pxe, …) — validated against the
	// endpoint's discovered AllowableValues at execution, not at admission.
	Target string `json:"target"`

	// Once limits the override to the next boot (the default); false is Continuous.
	// +kubebuilder:default=true
	// +optional
	Once *bool `json:"once,omitempty"`
}

// InsertMediaAction mounts an image on one of the machine's virtual-media slots,
// the way you would put a disc in a drive attached to it.
type InsertMediaAction struct {
	// MediaID is the virtual-media slot ("CD", "USBStick", …).
	MediaID string `json:"mediaId"`
	// ImageURL is what to mount. Already-mounted with the same URL is success —
	// steps converge to their goal, so a resumed operation never double-issues.
	ImageURL string `json:"imageUrl"`
}

// EjectMediaAction unmounts a virtual-media slot. Ejecting an empty slot is
// success — the goal is an empty slot, not the act of emptying it.
type EjectMediaAction struct {
	// MediaID is the virtual-media slot to empty.
	MediaID string `json:"mediaId"`
}

// ApplyBiosAction stages a machine's wanted BIOS attributes onto the firmware's
// pending object. Staging on its own changes nothing that is running: BIOS
// settings take effect at the next reset, which is why a converging operation
// pairs this with a powerCycle.
type ApplyBiosAction struct {
	// SettingsRef names the BiosSettings whose spec.attributes to stage. Only
	// the drifted subset is PATCHed; already-staged keys are converged goals,
	// not conflicts.
	SettingsRef Reference `json:"settingsRef"`
}

// FlashFirmwareAction replaces the firmware on a machine's management controller.
// The controller reboots as part of it; the machine it manages keeps running.
type FlashFirmwareAction struct {
	// Image is the digest-pinned OCI reference to fetch and push (the same
	// shape as FirmwareProfile.spec.manager.image).
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Version is the version expected to read back after the flash — the
	// convergence goal: a manager already serving it makes this step a no-op.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

// OperationApproval is the CEL-guarded human gate (a field, not a subresource).
type OperationApproval struct {
	// Required: this operation must be approved before it runs (from the
	// disruption policy; explicit false only where the policy waives the gate).
	// A pointer so that EVERY client that says nothing — YAML omitting the key,
	// a Go zero value — gets the safe default: a closed gate.
	// +kubebuilder:default=true
	// +optional
	Required *bool `json:"required,omitempty"`

	// ApprovedBy is stamped by the approver — the ValidatingAdmissionPolicy
	// enforces that it equals the requesting username, is empty at creation, and
	// is write-once.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// OperationBaseline is the observation captured at execution start — the
// re-derivable "did I already reboot for this request" predicate: completion is
// the world moving past it, never a remembered step counter.
type OperationBaseline struct {
	// +optional
	PowerState string `json:"powerState,omitempty"`
	// LastResetTime as observed before any action ran. Micro precision: the sim
	// resets faster than a second, and second-precision would blind the advance
	// predicate.
	// +optional
	LastResetTime *metav1.MicroTime `json:"lastResetTime,omitempty"`
}

// OperationStep is one executed action's outcome.
type OperationStep struct {
	// Action is the union key that ran (powerCycle, insertMedia, …).
	Action string `json:"action"`
	// +kubebuilder:validation:Enum=Succeeded;Failed
	Phase string `json:"phase"`
	// +optional
	Message string `json:"message,omitempty"`
}

// HostOperationStatus is what happened: which steps ran and how they ended, plus
// the observations that prove the machine really did what was asked.
type HostOperationStatus struct {
	// Baseline is captured (and persisted) before the first mutating call.
	// +optional
	Baseline *OperationBaseline `json:"baseline,omitempty"`

	// ObservedResetTime is the reset-time observation after the last step — the
	// proof the reset happened, and what marks a Host reset-time advance as
	// explained rather than Drifted.
	// +optional
	ObservedResetTime *metav1.MicroTime `json:"observedResetTime,omitempty"`

	// Steps records each action's outcome in execution order.
	// +listType=atomic
	// +optional
	Steps []OperationStep `json:"steps,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the conditions below for people and for kubectl columns.
	// It is a summary, not the contract: automation should read conditions.
	// +optional
	Phase OperationPhase `json:"phase,omitempty"`

	// BlockedOn is derived — why the operation isn't advancing (AwaitingApproval,
	// OperationQueued).
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// HostOperation is one disruptive thing done to one machine, and the record that
// it was done: a power cycle, a BIOS apply, a firmware flash. It is namespaced,
// short-lived, and never retried on its own — if it failed, it stays failed until
// a person looks at it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=metal
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.hostRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Approved",type=string,JSONPath=`.spec.approval.approvedBy`
// +kubebuilder:printcolumn:name="Blocked",type=string,JSONPath=`.status.blockedOn`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HostOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HostOperationSpec `json:"spec"`
	// +optional
	Status HostOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HostOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostOperation `json:"items"`
}
