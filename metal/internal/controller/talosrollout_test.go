package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

func step(node string, role osv1alpha1.NodeRole) osv1alpha1.RolloutStep {
	return osv1alpha1.RolloutStep{Kind: osv1alpha1.RolloutStepNode, Node: node, Host: "box-" + node, Role: role}
}

// approvedRollout is a rollout with a plan, approved for it.
func approvedRollout(kind osv1alpha1.RolloutKind, plan ...osv1alpha1.RolloutStep) *osv1alpha1.TalosRollout {
	ro := &osv1alpha1.TalosRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "r"},
		Spec:       osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"}, Kind: kind},
	}
	ro.Status.Plan = plan
	ro.Status.PlanHash = planHashOf(ro, plan)
	ro.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: "bob", PlanHash: ro.Status.PlanHash}
	ro.Status.Steps = make([]osv1alpha1.RolloutStepStatus, len(plan))
	for i := range ro.Status.Steps {
		ro.Status.Steps[i].State = osv1alpha1.RolloutNodePending
	}
	return ro
}

func TestRolloutActions(t *testing.T) {
	cp, w := step("cp", osv1alpha1.RoleControlPlane), step("w", osv1alpha1.RoleWorker)
	keys := func(kind osv1alpha1.RolloutKind, st osv1alpha1.RolloutStep) []string {
		return actionKeys(rolloutActions(&osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{Kind: kind}}, st))
	}
	assert.Equal(t, []string{"drain", "talosUpgrade", "waitNodeReady", "waitEtcdHealthy", "uncordon"}, keys(osv1alpha1.RolloutTalosUpgrade, cp))
	assert.Equal(t, []string{"drain", "talosReboot", "waitNodeReady", "uncordon"}, keys(osv1alpha1.RolloutReboot, w))
	assert.Equal(t, []string{"talosApplyConfig", "waitNodeReady"}, keys(osv1alpha1.RolloutConfig, w),
		"a config the node takes live is neither drained nor uncordoned")
	w.RebootRequired = true
	assert.Equal(t, []string{"drain", "talosApplyConfig", "waitNodeReady", "uncordon"}, keys(osv1alpha1.RolloutKubernetesUpgrade, w))

	ro := &osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{Kind: osv1alpha1.RolloutConfig}}
	a := rolloutActions(ro, w)
	assert.Equal(t, "Reboot", a[1].TalosApplyConfig.Mode)
	upgrade := rolloutActions(&osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{Kind: osv1alpha1.RolloutTalosUpgrade}},
		osv1alpha1.RolloutStep{Node: "cp", To: "v1.14.6", Image: "i@sha256:x", Role: osv1alpha1.RoleControlPlane})
	assert.Equal(t, "v1.14.6", upgrade[2].WaitNodeReady.Version, "an upgrade waits for the version it installs")
}

func TestSameActionsIgnoresAPIDefaults(t *testing.T) {
	ro := &osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{Kind: osv1alpha1.RolloutTalosUpgrade}}
	built := rolloutActions(ro, step("cp", osv1alpha1.RoleControlPlane))
	stored := withDefaults(built)
	stored[1].TalosUpgrade.RebootMode = "" // as if the API server had not defaulted it
	assert.True(t, sameActions(stored, built))

	changed := withDefaults(built)
	changed[1].TalosUpgrade.Image = "other@sha256:y"
	assert.False(t, sameActions(changed, built))
	assert.False(t, sameActions(built[:2], built))
}

func TestRolloutPlanHash(t *testing.T) {
	ro := approvedRollout(osv1alpha1.RolloutReboot, step("a", osv1alpha1.RoleWorker))
	h := ro.Status.PlanHash
	assert.Equal(t, h, planHashOf(ro, ro.Status.Plan), "stable")
	ro.Spec.MaxUnavailableWorkers = 3
	assert.NotEqual(t, h, planHashOf(ro, ro.Status.Plan), "the worker budget is part of what is approved")
	ro.Spec.MaxUnavailableWorkers = 0
	plan := append([]osv1alpha1.RolloutStep(nil), ro.Status.Plan...)
	plan[0].Host = "box-b"
	assert.NotEqual(t, h, planHashOf(ro, plan))
}

func TestRolloutCovers(t *testing.T) {
	ro := approvedRollout(osv1alpha1.RolloutReboot, step("a", osv1alpha1.RoleWorker), step("b", osv1alpha1.RoleWorker))
	op := func() *metalv1alpha1.HostOperation {
		return &metalv1alpha1.HostOperation{Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: "box-a"},
			Actions:    rolloutActions(ro, ro.Status.Plan[0]),
			RolloutRef: &metalv1alpha1.RolloutReference{Name: "r", PlanHash: ro.Status.PlanHash},
		}}
	}
	assert.Empty(t, rolloutCovers(ro, op()))

	o := op()
	o.Spec.RolloutRef.PlanHash = "sha256:other"
	assert.Contains(t, rolloutCovers(ro, o), "made for plan")

	o = op()
	o.Spec.HostRef.Name = "box-b"
	assert.Contains(t, rolloutCovers(ro, o), "targets host box-b")

	o = op()
	o.Spec.Actions = rolloutActions(ro, ro.Status.Plan[1])
	o.Spec.HostRef.Name = "box-b"
	assert.Empty(t, rolloutCovers(ro, o), "b's step on b's host is covered")
	o.Spec.Actions = o.Spec.Actions[1:]
	assert.Contains(t, rolloutCovers(ro, o), "differ")

	aborted := ro.DeepCopy()
	aborted.Spec.Abort = true
	assert.Contains(t, rolloutCovers(aborted, op()), "aborted")

	unapproved := ro.DeepCopy()
	unapproved.Spec.Approval = osv1alpha1.RolloutApproval{}
	assert.Contains(t, rolloutCovers(unapproved, op()), "nobody approved")

	tampered := ro.DeepCopy()
	tampered.Status.Plan[0].Host = "box-z"
	assert.Contains(t, rolloutCovers(tampered, op()), "does not hash")
}

func TestNextSteps(t *testing.T) {
	r := &TalosRolloutReconciler{}
	ro := approvedRollout(osv1alpha1.RolloutReboot,
		step("cp-1", osv1alpha1.RoleControlPlane), step("cp-2", osv1alpha1.RoleControlPlane),
		osv1alpha1.RolloutStep{Kind: osv1alpha1.RolloutStepKubeProxy},
		step("w-1", osv1alpha1.RoleWorker), step("w-2", osv1alpha1.RoleWorker), step("w-3", osv1alpha1.RoleWorker))
	ro.Spec.MaxUnavailableWorkers = 2
	set := func(states ...osv1alpha1.RolloutNodeState) {
		for i, s := range states {
			ro.Status.Steps[i].State = s
		}
	}
	const (
		P = osv1alpha1.RolloutNodePending
		R = osv1alpha1.RolloutNodeRunning
		S = osv1alpha1.RolloutNodeSucceeded
	)
	assert.Equal(t, []int{0}, r.nextSteps(ro, 0), "one control plane first")
	set(R)
	assert.Empty(t, r.nextSteps(ro, 1), "nothing beside a running control plane")
	set(S, P)
	assert.Equal(t, []int{1}, r.nextSteps(ro, 0))
	set(S, S, P)
	assert.Equal(t, []int{2}, r.nextSteps(ro, 0), "kube-proxy after the control planes")
	set(S, S, S, P, P, P)
	assert.Equal(t, []int{3, 4}, r.nextSteps(ro, 0), "workers up to the budget")
	set(S, S, S, R, P, P)
	assert.Equal(t, []int{4}, r.nextSteps(ro, 1))
	set(S, S, S, R, R, P)
	assert.Empty(t, r.nextSteps(ro, 2))
	set(S, S, S, S, R, P)
	assert.Equal(t, []int{5}, r.nextSteps(ro, 1))
}

func TestPausedAfterFirstNode(t *testing.T) {
	r := &TalosRolloutReconciler{}
	ro := approvedRollout(osv1alpha1.RolloutReboot, step("a", osv1alpha1.RoleWorker), step("b", osv1alpha1.RoleWorker))
	paused, _, _ := r.paused(ro)
	assert.False(t, paused, "the first node starts once approved")
	ro.Status.Steps[0].State = osv1alpha1.RolloutNodeSucceeded
	paused, reason, _ := r.paused(ro)
	assert.True(t, paused)
	assert.Equal(t, osv1alpha1.ReasonCanaryPause, reason)
	ro.Annotations = map[string]string{osv1alpha1.AnnotationRolloutResume: "sha256:another-plan"}
	paused, _, _ = r.paused(ro)
	assert.True(t, paused, "a resume for another plan does not count")
	ro.Annotations[osv1alpha1.AnnotationRolloutResume] = ro.Status.PlanHash
	paused, _, _ = r.paused(ro)
	assert.False(t, paused)
	ro.Spec.Paused = true
	paused, reason, _ = r.paused(ro)
	assert.True(t, paused)
	assert.Equal(t, osv1alpha1.ReasonPausedByPerson, reason)
}

func TestSummarizeDryRun(t *testing.T) {
	changed, summary := summarizeDryRun("Dry run summary:\nApplied configuration without a reboot (skipped in dry-run).\n\nConfig diff:\n\nNo changes.")
	assert.False(t, changed)
	assert.Equal(t, "no changes", summary)

	diff := "Dry run summary:\n...\nConfig diff:\n\n--- a\n+++ b\n@@ -1,3 +1,3 @@\n" +
		"-    token: oldsecretvalue\n+    token: newsecretvalue\n+  example.com/rack: r2\n"
	changed, summary = summarizeDryRun(diff)
	assert.True(t, changed)
	assert.Equal(t, "2 lines added, 1 removed; keys: token, example.com/rack", summary)
	assert.NotContains(t, summary, "secretvalue", "values never reach the status")
}

// TestPlanNodeSaysWhenTheNodeRestarts: the plan an approver reads says
// "REBOOTS" for a reboot and for a Talos upgrade, which boots the new image.
// It said "no reboot" for an upgrade.
func TestPlanNodeSaysWhenTheNodeRestarts(t *testing.T) {
	r := &TalosRolloutReconciler{Factory: render.NewFactory(""),
		ResolveImage: func(_ context.Context, ref string) (string, error) {
			return ref + "@sha256:" + strings.Repeat("0", 64), nil
		}}
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	n := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec: osv1alpha1.TalosNodeSpec{Role: osv1alpha1.RoleControlPlane}, Status: osv1alpha1.TalosNodeStatus{Version: "v1.14.5"}}
	for _, kind := range []osv1alpha1.RolloutKind{osv1alpha1.RolloutTalosUpgrade, osv1alpha1.RolloutReboot} {
		ro := &osv1alpha1.TalosRollout{Spec: osv1alpha1.TalosRolloutSpec{Kind: kind}}
		step, why, err := r.planNode(t.Context(), ro, cluster, n, "box-a", osv1alpha1.RolloutTarget{TalosVersion: "v1.14.6"})
		require.NoError(t, err)
		require.Empty(t, why)
		require.NotNil(t, step)
		assert.True(t, step.RebootRequired, "a %s restarts the node", kind)
	}
}

func TestRebootRequired(t *testing.T) {
	assert.False(t, rebootRequired(talos.ApplyResult{Mode: talos.ApplyNoReboot}))
	assert.True(t, rebootRequired(talos.ApplyResult{Mode: talos.ApplyReboot}))
	assert.False(t, rebootRequired(talos.ApplyResult{Details: "Applied configuration without a reboot (skipped in dry-run)"}))
	assert.True(t, rebootRequired(talos.ApplyResult{Details: "something new"}), "unsure means drain and reboot")
}

func TestProposerNeedsUpgradesBeforeConfig(t *testing.T) {
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	drift := []metav1.Condition{{Type: osv1alpha1.ConditionConfigInSync, Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonConfigDrift}}
	nodes := []osv1alpha1.TalosNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: osv1alpha1.TalosNodeStatus{Version: "v1.14.6", Conditions: drift, ConfigDigest: "sha256:1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: osv1alpha1.TalosNodeStatus{Version: "v1.14.5"}},
	}
	hosts := map[string]string{"a": "box-a", "b": "box-b"}

	ro := needed(cluster, nodes, hosts)
	require.NotNil(t, ro)
	assert.Equal(t, osv1alpha1.RolloutTalosUpgrade, ro.Spec.Kind)
	assert.Equal(t, "v1.14.6", ro.Spec.Target.TalosVersion)
	assert.True(t, strings.HasPrefix(ro.Name, "c-talosupgrade-"))

	nodes[1].Status.Version = "v1.14.6"
	cluster.Status.KubernetesVersion = "v1.36.1"
	assert.Equal(t, osv1alpha1.RolloutKubernetesUpgrade, needed(cluster, nodes, hosts).Spec.Kind)

	cluster.Status.KubernetesVersion = "v1.36.2"
	cfg := needed(cluster, nodes, hosts)
	assert.Equal(t, osv1alpha1.RolloutConfig, cfg.Spec.Kind)
	assert.Equal(t, cfg.Name, needed(cluster, nodes, hosts).Name, "the same need, the same name: it never spawns twice")
	nodes[0].Status.ConfigDigest = "sha256:2"
	assert.NotEqual(t, cfg.Name, needed(cluster, nodes, hosts).Name, "another render is another proposal")

	delete(hosts, "a")
	assert.Nil(t, needed(cluster, nodes, hosts), "a node no machine runs needs nothing")
}

func TestConfigSecretName(t *testing.T) {
	assert.Equal(t, "machineconfig-n1-0123456789ab", configSecretNameFor("n1", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"))
	assert.Empty(t, configSecretNameFor("n1", "latest"))
}

// A child operation a maintenance window holds, or a guard holds, is
// Executed=Unknown: the rollout keeps it running and fails nothing.
func TestRolloutStepHeldByAWindowIsStillRunning(t *testing.T) {
	ro := approvedRollout(osv1alpha1.RolloutReboot, step("w-1", osv1alpha1.RoleWorker))
	ro.Status.Steps[0].State = osv1alpha1.RolloutNodeRunning
	ro.Status.Steps[0].Operation = "pk-metal/r-w-1"
	op := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "r-w-1"}}
	op.Status.Conditions = []metav1.Condition{{
		Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown,
		Reason: metalv1alpha1.ReasonOutsideMaintenanceWindow, Message: "waits for window nights",
		LastTransitionTime: metav1.Now(),
	}}
	r := &TalosRolloutReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(op).Build()}
	require.NoError(t, r.observeStep(t.Context(), ro, &osv1alpha1.TalosCluster{}, 0))
	assert.Equal(t, osv1alpha1.RolloutNodeRunning, ro.Status.Steps[0].State)
	assert.Equal(t, "waits for window nights", ro.Status.Steps[0].Message, "the rollout says why it waits")
}
