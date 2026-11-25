package v1alpha1

// ReasonWaiverRefused on Executed=False: the operator proposed the operation
// with no approval of its own, but it is not one of the kinds the operator
// may run without a person: the node step of an approved rollout, the
// install of a claim set to Automatic, or a release. The executor refuses
// it before it acts on the machine. A request with the operator's token
// that the operator code did not make ends here.
const ReasonWaiverRefused = "WaiverRefused"
