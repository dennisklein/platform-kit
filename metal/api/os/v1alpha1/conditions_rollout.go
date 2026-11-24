package v1alpha1

// Condition types and reasons of day-2 changes: config drift, version skew,
// the secrets bundle guard, and rollouts.
const (
	// ConditionConfigInSync on a TalosNode: a dry-run apply of the rendered
	// config against the machine reports no change. False with reason
	// ConfigDrift says what differs and whether it needs a reboot.
	ConditionConfigInSync = "ConfigInSync"

	// ConditionVersionCurrent on a TalosNode: the machine runs the Talos
	// version the cluster asks for. False with reason VersionSkew while it
	// runs another one; the config is then rendered for the version it runs.
	ConditionVersionCurrent = "VersionCurrent"

	// ConditionVersionsSupported on a TalosCluster: Talos supports the
	// Kubernetes version the spec asks for. False with reason
	// UnsupportedVersionPair otherwise, and nothing renders.
	ConditionVersionsSupported = "VersionsSupported"

	// ConditionVersionsCurrent on a TalosCluster: every machine runs the
	// Talos version and the Kubernetes API runs the Kubernetes version the
	// spec asks for.
	ConditionVersionsCurrent = "VersionsCurrent"

	// ConditionSecretsBundleHeld on a TalosCluster: True while the secrets
	// bundle of the spec is not the bundle that the machines run. The operator
	// then continues to use the bundle that they run (reason BundleChanged).
	ConditionSecretsBundleHeld = "SecretsBundleHeld"

	// ConditionPlanned, ConditionApproved, ConditionProgressing,
	// ConditionPaused and ConditionComplete (declared with the backup kinds,
	// which use it the same way) are a TalosRollout's.
	ConditionPlanned     = "Planned"
	ConditionApproved    = "Approved"
	ConditionProgressing = "Progressing"
	ConditionPaused      = "Paused"
)

// Reasons on ConfigInSync and VersionCurrent.
const (
	ReasonInSync               = "InSync"
	ReasonConfigDrift          = "ConfigDrift"
	ReasonDriftUnknown         = "DriftUnknown"
	ReasonVersionCurrent       = "VersionCurrent"
	ReasonVersionSkew          = "VersionSkew"
	ReasonUnsupportedVersion   = "UnsupportedVersionPair"
	ReasonVersionsSupported    = "VersionsSupported"
	ReasonVersionsBehind       = "VersionsBehind"
	ReasonBundleChanged        = "BundleChanged"
	ReasonBundleApplied        = "BundleApplied"
	ReasonCredentialsPublished = "CredentialsPublished"
)

// Reasons on a TalosRollout's conditions.
const (
	// ReasonPlanReady on Planned=True; ReasonPlanBlocked on Planned=False
	// when a step cannot be planned yet (the message says which and why).
	ReasonPlanReady   = "PlanReady"
	ReasonPlanBlocked = "PlanBlocked"
	// ReasonNothingToDo on Complete=True: no node needs the change.
	ReasonNothingToDo = "NothingToDo"

	// ReasonAwaitingApproval on Approved=False: nobody approved the plan.
	ReasonAwaitingApproval = "AwaitingApproval"
	// ReasonPlanApproved on Approved=True.
	ReasonPlanApproved = "PlanApproved"
	// ReasonPlanChanged on Approved=False: the approval names a planHash
	// the plan no longer has. Nothing runs; approve a new rollout.
	ReasonPlanChanged = "PlanChanged"

	// Progressing reasons.
	ReasonNodeRunning       = "NodeRunning"
	ReasonWaitingForHealth  = "WaitingForHealth"
	ReasonRolloutFailed     = "RolloutFailed"
	ReasonRolloutAborted    = "Aborted"
	ReasonRolloutSucceeded  = "Succeeded"
	ReasonCanaryPause       = "PausedAfterFirstNode"
	ReasonPausedByPerson    = "PausedByPerson"
	ReasonNotPaused         = "NotPaused"
	ReasonRolloutNotStarted = "NotStarted"
)
