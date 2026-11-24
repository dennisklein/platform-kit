package controller

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The plan of a rollout, and what each of its steps does to a node. The
// rollout controller builds the HostOperation of a step from this, and the
// executor checks an operation that names a rollout against it: both sides use
// rolloutActions, so the actions a person approved in the plan are the actions
// the operation carries.

// OperationReasonRollout is the spec.reason of a rollout's operations.
const OperationReasonRollout = "TalosRollout"

// planHashOf hashes everything that decides what a rollout does: the
// cluster, the kind, the target, the drain policy and every plan step. JSON
// of Go structs has a fixed field order, so the hash is stable.
func planHashOf(ro *osv1alpha1.TalosRollout, plan []osv1alpha1.RolloutStep) string {
	b, _ := json.Marshal(struct {
		Cluster        string                    `json:"cluster"`
		Kind           osv1alpha1.RolloutKind    `json:"kind"`
		Target         *osv1alpha1.RolloutTarget `json:"target,omitempty"`
		MaxUnavailable int32                     `json:"maxUnavailableWorkers"`
		Drain          int32                     `json:"drainTimeoutSeconds"`
		Plan           []osv1alpha1.RolloutStep  `json:"plan"`
	}{ro.Spec.ClusterRef.Name, ro.Spec.Kind, ro.Spec.Target, maxUnavailableWorkers(ro), drainTimeout(ro), plan})
	return digest.FromBytes(b).String()
}

func maxUnavailableWorkers(ro *osv1alpha1.TalosRollout) int32 {
	return max(ro.Spec.MaxUnavailableWorkers, 1)
}

func drainTimeout(ro *osv1alpha1.TalosRollout) int32 {
	if ro.Spec.DrainTimeoutSeconds > 0 {
		return ro.Spec.DrainTimeoutSeconds
	}
	return 900
}

// The timeouts of the wait actions. The CRD defaults them too; they are set
// here so the operation the API server stores equals what was built.
const (
	waitNodeReadyTimeout   = 1800
	waitEtcdHealthyTimeout = 900
)

// rolloutActions are the actions of one Node step: the change itself between
// the waits that prove the node came back. A step that interrupts the node
// (an upgrade, a reboot, a config that needs one) drains it first and
// uncordons it last; a control plane also waits for etcd to have every member
// healthy before the operation ends, which is the gate the next control plane
// starts on.
func rolloutActions(ro *osv1alpha1.TalosRollout, st osv1alpha1.RolloutStep) []metalv1alpha1.HostOperationAction {
	node := metalv1alpha1.Reference{Name: st.Node}
	var change metalv1alpha1.HostOperationAction
	disrupts := true
	version := ""
	switch ro.Spec.Kind {
	case osv1alpha1.RolloutTalosUpgrade:
		version = st.To
		change.TalosUpgrade = &metalv1alpha1.TalosUpgradeAction{
			NodeRef: node, Image: st.Image, Version: st.To,
			Preserve: ptr.To(true), RebootMode: "Default",
		}
	case osv1alpha1.RolloutReboot:
		change.TalosReboot = &metalv1alpha1.TalosRebootAction{NodeRef: node, Mode: "Default"}
	default: // Config, KubernetesUpgrade
		mode := "NoReboot"
		if st.RebootRequired {
			mode = "Reboot"
		}
		disrupts = st.RebootRequired
		change.TalosApplyConfig = &metalv1alpha1.TalosApplyConfigAction{
			NodeRef: node, ConfigSecretRef: metalv1alpha1.Reference{Name: st.ConfigSecret},
			Digest: st.ConfigDigest, Mode: mode,
		}
	}

	var actions []metalv1alpha1.HostOperationAction
	if disrupts {
		actions = append(actions, metalv1alpha1.HostOperationAction{Drain: &metalv1alpha1.DrainAction{
			NodeRef: node, TimeoutSeconds: drainTimeout(ro), DeleteEmptyDirData: true,
		}})
	}
	actions = append(actions, change, metalv1alpha1.HostOperationAction{WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{
		NodeRef: node, Version: version, TimeoutSeconds: waitNodeReadyTimeout,
	}})
	if st.Role == osv1alpha1.RoleControlPlane {
		actions = append(actions, metalv1alpha1.HostOperationAction{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{
			NodeRef: node, TimeoutSeconds: waitEtcdHealthyTimeout,
		}})
	}
	if disrupts {
		actions = append(actions, metalv1alpha1.HostOperationAction{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: node}})
	}
	return actions
}

// actionKeys lists the keys of actions, for the plan a person reads.
func actionKeys(actions []metalv1alpha1.HostOperationAction) []string {
	keys := make([]string, len(actions))
	for i, a := range actions {
		keys[i] = a.Key()
	}
	return keys
}

// sameActions compares an operation's actions with the ones a step builds,
// after filling in what the API server defaults, so that a default the CRD
// adds does not read as a difference.
func sameActions(got, want []metalv1alpha1.HostOperationAction) bool {
	return reflect.DeepEqual(withDefaults(got), withDefaults(want))
}

// withDefaults fills the CRD defaults of the actions a rollout builds.
func withDefaults(in []metalv1alpha1.HostOperationAction) []metalv1alpha1.HostOperationAction {
	out := make([]metalv1alpha1.HostOperationAction, len(in))
	for i := range in {
		a := *in[i].DeepCopy()
		switch {
		case a.Drain != nil && a.Drain.TimeoutSeconds == 0:
			a.Drain.TimeoutSeconds = 900
		case a.WaitNodeReady != nil && a.WaitNodeReady.TimeoutSeconds == 0:
			a.WaitNodeReady.TimeoutSeconds = waitNodeReadyTimeout
		case a.WaitEtcdHealthy != nil && a.WaitEtcdHealthy.TimeoutSeconds == 0:
			a.WaitEtcdHealthy.TimeoutSeconds = waitEtcdHealthyTimeout
		case a.TalosApplyConfig != nil && a.TalosApplyConfig.Mode == "":
			a.TalosApplyConfig.Mode = "NoReboot"
		case a.TalosReboot != nil && a.TalosReboot.Mode == "":
			a.TalosReboot.Mode = "Default"
		case a.TalosUpgrade != nil:
			if a.TalosUpgrade.Preserve == nil {
				a.TalosUpgrade.Preserve = ptr.To(true)
			}
			if a.TalosUpgrade.RebootMode == "" {
				a.TalosUpgrade.RebootMode = "Default"
			}
		}
		out[i] = a
	}
	return out
}

// rolloutFinished: the rollout reached an end (complete, failed, aborted, or
// its approval no longer matches its plan) and starts nothing any more.
func rolloutFinished(ro *osv1alpha1.TalosRollout) bool {
	c := meta.FindStatusCondition(ro.Status.Conditions, osv1alpha1.ConditionComplete)
	return c != nil && c.Status != metav1.ConditionUnknown
}

// rolloutApproved reports whether the rollout's approval covers its current
// plan, and why not otherwise.
func rolloutApproved(ro *osv1alpha1.TalosRollout) (bool, string) {
	a := ro.Spec.Approval
	switch {
	case a.ApprovedBy == "":
		return false, "nobody approved the plan yet: pk-metal rollout approve " + ro.Name
	case ro.Status.PlanHash == "" || a.PlanHash != ro.Status.PlanHash:
		return false, fmt.Sprintf("approved by %s for plan %s, but the plan is %s", a.ApprovedBy,
			shortDigest(a.PlanHash), shortDigest(ro.Status.PlanHash))
	case planHashOf(ro, ro.Status.Plan) != ro.Status.PlanHash:
		return false, "the stored plan does not hash to its planHash"
	}
	return true, ""
}

// rolloutStepFor finds the plan step of node, -1 when the plan has none.
func rolloutStepFor(ro *osv1alpha1.TalosRollout, node string) int {
	for i, st := range ro.Status.Plan {
		if st.Kind == osv1alpha1.RolloutStepNode && st.Node == node {
			return i
		}
	}
	return -1
}
