package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.proposedBy) || (has(self.proposedBy) && self.proposedBy == oldSelf.proposedBy)",message="proposedBy is frozen after creation"
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

	// ProposedBy is the username that created the operation. Admission checks
	// it against the creating request, so it cannot name someone else, and
	// refuses an approval stamped by the same person: nobody approves their
	// own proposal.
	// +kubebuilder:validation:MinLength=1
	ProposedBy string `json:"proposedBy"`

	// PlanHash binds a proposed operation to the desired state it serves — an
	// unchanged hash never spawns a second op (the convergence reconcilers' guard).
	// +optional
	PlanHash string `json:"planHash,omitempty"`

	// Actions run in order; each entry is exactly one action. A failed step fails
	// the operation — disruptive one-shots never auto-retry. At most one power
	// action per operation: the power-cycle proof lives in status once.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:XValidation:rule="self.filter(a, has(a.powerCycle) || has(a.powerOff)).size() <= 1",message="at most one power action per operation"
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
	// PowerCycle restarts the system: off, observed off, on, observed on — or
	// just on when it was off at the baseline. Each half converges on the
	// reported power state, which every vendor serves, so a resumed operation
	// never cycles a machine twice (the iBMC serves no reset time to tell).
	// +optional
	PowerCycle *PowerCycleAction `json:"powerCycle,omitempty"`

	// PowerOff forces the system off and waits until it reports off.
	// +optional
	PowerOff *PowerOffAction `json:"powerOff,omitempty"`

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

	// AwaitCheckIn waits, without touching the machine, until the named
	// TalosNode reports that the machine runs its config — the point after
	// which the boot media are no longer needed. It changes nothing; it keeps
	// the operation (and the grants it issued) alive until the hand-off.
	// +optional
	AwaitCheckIn *AwaitCheckInAction `json:"awaitCheckIn,omitempty"`
}

// PowerCycleAction restarts the machine. It takes no fields: there is only one
// way to do it, forcibly, and a machine that is already off is turned on instead.
type PowerCycleAction struct{}

// PowerOffAction turns the machine off, forcibly. Already off is success.
type PowerOffAction struct{}

// AwaitCheckInAction holds the operation until a TalosNode checks in.
type AwaitCheckInAction struct {
	// NodeRef names the TalosNode (os.pk.io) whose check-in ends the wait:
	// MachineReachable=True over the authenticated machine API, or Ready=True.
	NodeRef Reference `json:"nodeRef"`

	// TimeoutSeconds bounds the wait, counted from the previous step's end.
	// Running out fails the step — the media are ejected and the grants
	// revoked either way.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=1800
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// SetBootOverrideAction points the next boot at a particular source.
type SetBootOverrideAction struct {
	// Target is the boot source (UsbCd, Cd, Pxe, …) — validated against the
	// endpoint's discovered AllowableValues at execution, not at admission.
	Target string `json:"target"`

	// Once limits the override to the next boot (the default); false is Continuous.
	// +kubebuilder:default=true
	// +optional
	Once *bool `json:"once,omitempty"`

	// Mode is the firmware boot mode for the override. Omitted leaves the
	// machine's own setting alone (some firmware rejects the property).
	// +kubebuilder:validation:Enum=UEFI;Legacy
	// +optional
	Mode string `json:"mode,omitempty"`
}

// InsertMediaAction mounts an image on one of the machine's virtual-media slots,
// the way you would put a disc in a drive attached to it. The image is either a
// plain URL or a BootAsset: a secret-free asset mounts from its stable URL, a
// secret-bearing one (configISO) is served by the operator's media service
// under a grant issued at execution time and revoked when the operation ends.
//
// +kubebuilder:validation:XValidation:rule="has(self.imageUrl) != has(self.bootAssetRef)",message="exactly one of imageUrl and bootAssetRef"
// +kubebuilder:validation:XValidation:rule="!has(self.digest) || has(self.bootAssetRef)",message="digest pins a bootAssetRef"
type InsertMediaAction struct {
	// MediaID is the virtual-media slot ("CD", "USBStick", …).
	MediaID string `json:"mediaId"`
	// ImageURL is what to mount. Already-mounted with the same URL is success —
	// steps converge to their goal, so a resumed operation never double-issues.
	// +optional
	ImageURL string `json:"imageUrl,omitempty"`
	// BootAssetRef names the BootAsset (os.pk.io) to mount.
	// +optional
	BootAssetRef *Reference `json:"bootAssetRef,omitempty"`
	// Digest pins the artifact bytes (sha256:<hex>) the approver saw: an
	// artifact rebuilt since the proposal fails the step instead of mounting
	// content nobody approved. configISO assets only.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
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
	// SettingsRef names the BiosSettings the attributes came from — provenance
	// only: the executor never reads it, so an edit after approval cannot
	// change what runs.
	// +optional
	SettingsRef *Reference `json:"settingsRef,omitempty"`

	// Attributes are the values to stage, copied in at proposal time — what
	// the approver saw is exactly what is applied. Only the drifted subset is
	// PATCHed; already-staged keys are converged goals, not conflicts.
	// +kubebuilder:validation:MinProperties=1
	Attributes map[string]apiextensionsv1.JSON `json:"attributes"`
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
	// disruption policy; explicit false only where the policy waives the gate,
	// and admission lets only the operator or an admin create such an
	// operation). A pointer so that EVERY client that says nothing — YAML
	// omitting the key, a Go zero value — gets the safe default: a closed gate.
	// Frozen: flipping it would be an approval without a name on it.
	// +kubebuilder:default=true
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="approval.required is frozen after creation"
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
	// CapturedAt is when the baseline was taken — the start of execution,
	// which bounds how long a power transition may take to show.
	// +optional
	CapturedAt *metav1.Time `json:"capturedAt,omitempty"`
}

// OperationStep is one executed action's outcome.
type OperationStep struct {
	// Action is the union key that ran (powerCycle, insertMedia, …).
	Action string `json:"action"`
	// +kubebuilder:validation:Enum=Succeeded;Failed
	Phase string `json:"phase"`
	// +optional
	Message string `json:"message,omitempty"`
	// FinishedAt is when the step reached its goal (or failed).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
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

	// OffObserved is when the executor saw the machine report off during a
	// power cycle — the first half of the cycle, recorded so that a resumed
	// operation finishes the cycle instead of starting a second one.
	// +optional
	OffObserved *metav1.MicroTime `json:"offObserved,omitempty"`

	// Steps records each action's outcome in execution order, written as each
	// step reaches its goal. A resumed operation continues after the last
	// recorded step: a goal that a later step consumed (a Once boot override
	// used up by the boot that followed) is never re-armed.
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
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Proposed",type=string,JSONPath=`.spec.proposedBy`,priority=1
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
