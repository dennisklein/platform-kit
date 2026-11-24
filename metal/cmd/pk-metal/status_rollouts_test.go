package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

func statusRollout(name string, steps []osv1alpha1.RolloutNodeState, conds ...metav1.Condition) *osv1alpha1.TalosRollout {
	ro := &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}, Kind: osv1alpha1.RolloutKind("Upgrade")}}
	for _, s := range steps {
		ro.Status.Plan = append(ro.Status.Plan, osv1alpha1.RolloutStep{})
		ro.Status.Steps = append(ro.Status.Steps, osv1alpha1.RolloutStepStatus{State: s})
	}
	for _, c := range conds {
		c.LastTransitionTime = metav1.Now()
		ro.Status.Conditions = append(ro.Status.Conditions, c)
	}
	return ro
}

func TestStatusRolloutTable(t *testing.T) {
	ok, pending := osv1alpha1.RolloutNodeSucceeded, osv1alpha1.RolloutNodePending
	canary := statusRollout("upgrade-1", []osv1alpha1.RolloutNodeState{ok, pending, pending},
		metav1.Condition{Type: osv1alpha1.ConditionApproved, Status: "True", Reason: osv1alpha1.ReasonPlanApproved},
		metav1.Condition{Type: osv1alpha1.ConditionPaused, Status: "True", Reason: osv1alpha1.ReasonCanaryPause,
			Message: "the first node is done; check it, then pk-metal rollout resume upgrade-1 (or abort it)"})
	canary.Spec.Approval.ApprovedBy = "alice"
	health := statusRollout("config-2", []osv1alpha1.RolloutNodeState{ok, pending},
		metav1.Condition{Type: osv1alpha1.ConditionApproved, Status: "True", Reason: osv1alpha1.ReasonPlanApproved},
		metav1.Condition{Type: osv1alpha1.ConditionProgressing, Status: "True", Reason: osv1alpha1.ReasonWaitingForHealth,
			Message: "next step waits: node cp-2 is not Ready"})
	done := statusRollout("old", []osv1alpha1.RolloutNodeState{ok},
		metav1.Condition{Type: osv1alpha1.ConditionComplete, Status: "True", Reason: osv1alpha1.ReasonRolloutSucceeded})

	rep := buildReport(nil, nil, nil, nil, nil)
	addRollouts(&rep, []osv1alpha1.TalosRollout{*health, *done})
	assert.False(t, rep.NeedsAttention, "waiting for health between nodes is the plan working; the alert covers a long wait")
	out := renderReport(rep)
	assert.Contains(t, out, "ROLLOUT")
	assert.Contains(t, out, "WaitingForHealth")
	assert.NotContains(t, out, "old ", "a succeeded rollout is history")

	rep = buildReport(nil, nil, nil, nil, nil)
	addRollouts(&rep, []osv1alpha1.TalosRollout{*canary, *health, *done})
	assert.True(t, rep.NeedsAttention, "a rollout paused after its first node waits for a person")
	out = renderReport(rep)
	assert.Regexp(t, `upgrade-1\s+lab\s+Upgrade\s+1/3\s+PausedAfterFirstNode\s+alice`, out)
	assert.Contains(t, out, "upgrade-1: the first node is done; check it, then pk-metal rollout resume upgrade-1")
}

func TestStatusRolloutsInJSONAndExitCode(t *testing.T) {
	waiting := statusRollout("upgrade-1", []osv1alpha1.RolloutNodeState{osv1alpha1.RolloutNodePending},
		metav1.Condition{Type: osv1alpha1.ConditionPlanned, Status: "True", Reason: osv1alpha1.ReasonPlanReady},
		metav1.Condition{Type: osv1alpha1.ConditionApproved, Status: "False", Reason: osv1alpha1.ReasonAwaitingApproval,
			Message: "nobody approved the plan yet"})
	var out bytes.Buffer
	code, err := runStatus(context.Background(), fakeClient(waiting), "json", &out)
	require.NoError(t, err)
	assert.Equal(t, exitAttention, code, "a rollout waiting for approval needs a person, like an operation does")
	var doc struct {
		Rollouts []rolloutStatus `json:"rollouts"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	require.Len(t, doc.Rollouts, 1)
	assert.Equal(t, rolloutStatus{Name: "upgrade-1", Cluster: "lab", Kind: "Upgrade", NodesTotal: 1,
		State: osv1alpha1.ReasonAwaitingApproval, Message: "nobody approved the plan yet", NeedsAttention: true}, doc.Rollouts[0])
}
