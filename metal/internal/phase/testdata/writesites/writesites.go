// Package writesites holds the shapes the write-site check must catch. Each line
// marked "want" is one finding. Only the check's own test loads this package.
package writesites

import (
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func direct(h *metalv1alpha1.Host) {
	h.Status.Phase = metalv1alpha1.HostFailed // want write
	h.Status.BlockedOn = "x"                  // want write
}

func alias(h *metalv1alpha1.Host) {
	st := &h.Status
	st.Phase = metalv1alpha1.HostFailed // want write
}

func pointer(op *metalv1alpha1.HostOperation) *metalv1alpha1.OperationPhase {
	return &op.Status.Phase // want write
}

func literal() metalv1alpha1.HostClaimStatus {
	return metalv1alpha1.HostClaimStatus{BlockedOn: "x"} // want write
}

func compound(ep *metalv1alpha1.ManagementEndpoint) {
	ep.Status.BlockedOn += "x" // want write
}

func read(h *metalv1alpha1.Host) bool {
	return h.Status.Phase == metalv1alpha1.HostFailed // want read
}

// A step's phase is not a summary phase: it is the outcome of one action, and
// the executor owns it.
func stepPhase(op *metalv1alpha1.HostOperation) {
	op.Status.Steps = append(op.Status.Steps, metalv1alpha1.OperationStep{Phase: "Succeeded"})
	op.Status.Steps[0].Phase = "Failed"
}
