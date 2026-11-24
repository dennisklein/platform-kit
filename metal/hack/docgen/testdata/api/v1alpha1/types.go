package v1alpha1

// AnnotationHold on a claim stops new proposals.
const AnnotationHold = "metal.pk.io/hold"

// OperationPhase summarizes an operation's conditions.
// +kubebuilder:validation:Enum=Proposed;Running
type OperationPhase string

const (
	// OperationProposed: it exists.
	OperationProposed OperationPhase = "Proposed"
	OperationRunning  OperationPhase = "Running"
)

// Condition types.
const (
	// ConditionApproved: the human gate.
	ConditionApproved = "Approved"
	// ConditionExecuted: the outcome.
	ConditionExecuted = "Executed"
)

// Reasons on the operation's Approved condition.
const (
	// ReasonAwaitingApproval on Approved=False: the gate is required.
	ReasonAwaitingApproval = "AwaitingApproval"
	// ReasonApproved: a human approved it. Also on Executed=Unknown while it waits.
	ReasonApproved = "Approved"
)

// OperationReasonBios is the spec.reason of a BIOS proposal.
const OperationReasonBios = "Bios"

// Reference names an object.
type Reference struct {
	Name string `json:"name"`
}

// HostOperationSpec is what to do.
type HostOperationSpec struct {
	Actions []HostOperationAction `json:"actions"`
}

// HostOperationStatus is what happened.
type HostOperationStatus struct {
	Phase OperationPhase `json:"phase,omitempty"`
}

// HostOperationAction is a union: exactly one member is set.
type HostOperationAction struct {
	// PowerOff turns the system off.
	// +optional
	PowerOff *PowerOffAction `json:"powerOff,omitempty"`

	// Wait waits for the node.
	// +optional
	Wait *WaitAction `json:"wait,omitempty"`
}

// PowerOffAction turns the machine off. Already off is success.
type PowerOffAction struct {
	// GracefulTimeoutSeconds asks the OS first.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +optional
	GracefulTimeoutSeconds int32 `json:"gracefulTimeoutSeconds,omitempty"`
}

// WaitAction waits, changing nothing.
type WaitAction struct {
	// NodeRef names the node.
	NodeRef Reference `json:"nodeRef"`
}
