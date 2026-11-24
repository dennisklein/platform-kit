package main

import (
	"fmt"
	"slices"
	"strings"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/metrics"
)

// rolloutStatus is one TalosRollout row of -o json.
type rolloutStatus struct {
	Name           string `json:"name"`
	Cluster        string `json:"cluster"`
	Kind           string `json:"kind"`
	NodesDone      int    `json:"nodesDone"`
	NodesTotal     int    `json:"nodesTotal"`
	State          string `json:"state"`
	ApprovedBy     string `json:"approvedBy"`
	Message        string `json:"message"`
	NeedsAttention bool   `json:"needsAttention"`
}

// rolloutNeedsPerson are the states in which a rollout does not go on until
// somebody acts: approve it, resume it after its first node, or read why it
// cannot plan or why it ended. Running a node, waiting for health between
// nodes and a pause a person set are the plan working; the alert
// PkMetalRolloutStalled covers one that waits for too long.
var rolloutNeedsPerson = []string{
	osv1alpha1.ReasonAwaitingApproval, osv1alpha1.ReasonPlanBlocked, osv1alpha1.ReasonPlanChanged,
	osv1alpha1.ReasonCanaryPause, osv1alpha1.ReasonRolloutFailed, osv1alpha1.ReasonRolloutAborted,
}

// addRollouts fills the ROLLOUT rows. The state is read the same way as the
// metric pk_metal_rollout_condition, so the table and the alerts agree.
func addRollouts(rep *statusReport, rollouts []osv1alpha1.TalosRollout) {
	for i := range rollouts {
		ro := &rollouts[i]
		st := metrics.RolloutStateOf(ro)
		row := rolloutStatus{Name: ro.Name, Cluster: ro.Spec.ClusterRef.Name, Kind: string(ro.Spec.Kind),
			NodesTotal: len(ro.Status.Plan), State: st.Reason, ApprovedBy: ro.Spec.Approval.ApprovedBy, Message: st.Message}
		for _, s := range ro.Status.Steps {
			if s.State == osv1alpha1.RolloutNodeSucceeded {
				row.NodesDone++
			}
		}
		row.NeedsAttention = !st.Succeeded && slices.Contains(rolloutNeedsPerson, st.Reason)
		rep.Rollouts = append(rep.Rollouts, row)
		rep.NeedsAttention = rep.NeedsAttention || row.NeedsAttention
	}
	slices.SortFunc(rep.Rollouts, func(a, b rolloutStatus) int { return strings.Compare(a.Name, b.Name) })
}

// renderRollouts prints the rollouts that have not succeeded; a finished
// one is history, as a succeeded operation is.
func renderRollouts(rep statusReport, section func(string, [][]string, []string)) {
	var rows [][]string
	var notes []string
	for _, r := range rep.Rollouts {
		if r.State == osv1alpha1.ReasonRolloutSucceeded || r.State == osv1alpha1.ReasonNothingToDo {
			continue
		}
		rows = append(rows, []string{r.Name, orDash(r.Cluster), orDash(r.Kind),
			fmt.Sprintf("%d/%d", r.NodesDone, r.NodesTotal), orDash(r.State), orDash(r.ApprovedBy)})
		if r.NeedsAttention {
			notes = append(notes, note(r.Name, r.Message, r.State))
		}
	}
	if len(rows) > 0 {
		section("ROLLOUT\tCLUSTER\tKIND\tNODES\tSTATE\tAPPROVED", rows, notes)
	}
}
