package v1alpha1

// ReasonRolloutRefused on Executed=False: the operation names a TalosRollout
// (spec.rolloutRef) whose approval does not cover it: the rollout is gone,
// not approved, aborted, approved for another planHash, or its plan gives
// this node other actions. Nothing was done to the machine.
const ReasonRolloutRefused = "RolloutRefused"
