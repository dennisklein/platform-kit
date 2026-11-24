package v1alpha1

// ReasonRolloutRefused on Executed=False: the operation names a TalosRollout
// (spec.rolloutRef), but the approval of the rollout does not cover it. The
// rollout is gone, not approved, aborted or approved for another planHash, or
// its plan gives this node other actions. The machine did not change.
const ReasonRolloutRefused = "RolloutRefused"
