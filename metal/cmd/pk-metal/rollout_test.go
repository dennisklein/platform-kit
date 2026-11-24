package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

const planHash = "sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// planned is a Talos upgrade the operator proposed and planned: two control
// planes, then a worker.
func planned() *osv1alpha1.TalosRollout {
	step := func(node, host string, role osv1alpha1.NodeRole) osv1alpha1.RolloutStep {
		return osv1alpha1.RolloutStep{Kind: osv1alpha1.RolloutStepNode, Node: node, Host: host, Role: role,
			From: "v1.14.6", To: "v1.14.7", RebootRequired: true,
			Actions: []string{"drain", "talosUpgrade", "waitNodeReady", "uncordon"}}
	}
	return &osv1alpha1.TalosRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-talosupgrade-1a2b3c4d", UID: "u1",
			CreationTimestamp: metav1.NewTime(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))},
		Spec: osv1alpha1.TalosRolloutSpec{
			ClusterRef: osv1alpha1.Reference{Name: "lab"}, Kind: osv1alpha1.RolloutTalosUpgrade,
			Target: &osv1alpha1.RolloutTarget{TalosVersion: "v1.14.7"}, ProposedBy: operator,
		},
		Status: osv1alpha1.TalosRolloutStatus{
			Plan: []osv1alpha1.RolloutStep{
				step("cp-1", "pk0001", osv1alpha1.RoleControlPlane),
				step("cp-2", "pk0002", osv1alpha1.RoleControlPlane),
				step("worker-1", "pk0003", osv1alpha1.RoleWorker),
			},
			PlanHash: planHash,
			Conditions: []metav1.Condition{{Type: osv1alpha1.ConditionPlanned, Status: metav1.ConditionTrue,
				Reason: osv1alpha1.ReasonPlanReady, Message: "3 nodes"}},
		},
	}
}

func TestRenderRollout(t *testing.T) {
	out := renderRollout(planned())
	assert.Contains(t, out, "TalosUpgrade on cluster lab to Talos v1.14.7")
	assert.Contains(t, out, "1. cp-1 (controlplane, host pk0001): v1.14.6 -> v1.14.7, REBOOTS")
	assert.Contains(t, out, "3. worker-1")
	assert.Contains(t, out, "pauses after the first node")
	assert.Contains(t, out, "state AwaitingApproval")
	assert.Less(t, strings.Index(out, "cp-2"), strings.Index(out, "worker-1"), "the order is the plan's order")
}

func TestListRollouts(t *testing.T) {
	cl := fakeClient(planned())
	var out bytes.Buffer
	require.NoError(t, listRollouts(context.Background(), cl, &out, time.Date(2030, 1, 1, 13, 0, 0, 0, time.UTC)))
	assert.Contains(t, out.String(), "lab-talosupgrade-1a2b3c4d")
	assert.Contains(t, out.String(), "0/3")
	assert.Contains(t, out.String(), "AwaitingApproval")
}

func TestApproveRollout(t *testing.T) {
	ctx := context.Background()
	t.Run("binds the approval to the plan shown", func(t *testing.T) {
		cl := fakeClient(planned())
		var out bytes.Buffer
		require.NoError(t, approveRollout(ctx, cl, rolloutApproval{Name: "lab-talosupgrade-1a2b3c4d",
			User: "alice", In: strings.NewReader("y\n"), Interactive: true, Out: &out}))
		ro := &osv1alpha1.TalosRollout{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "lab-talosupgrade-1a2b3c4d"}, ro))
		assert.Equal(t, "alice", ro.Spec.Approval.ApprovedBy)
		assert.Equal(t, planHash, ro.Spec.Approval.PlanHash)
		assert.Contains(t, out.String(), "cp-1")
	})
	t.Run("no means nothing changes", func(t *testing.T) {
		cl := fakeClient(planned())
		err := approveRollout(ctx, cl, rolloutApproval{Name: "lab-talosupgrade-1a2b3c4d",
			User: "alice", In: strings.NewReader("n\n"), Interactive: true, Out: &bytes.Buffer{}})
		require.ErrorIs(t, err, errDeclined)
		ro := &osv1alpha1.TalosRollout{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "lab-talosupgrade-1a2b3c4d"}, ro))
		assert.Empty(t, ro.Spec.Approval.ApprovedBy)
	})
	t.Run("refusals", func(t *testing.T) {
		own := planned()
		assert.ErrorContains(t, approvableRollout(own, operator), "another person")
		blocked := planned()
		blocked.Status.Conditions[0].Status = metav1.ConditionFalse
		blocked.Status.Conditions[0].Message = "node worker-1 has no fresh dry run"
		assert.ErrorContains(t, approvableRollout(blocked, "alice"), "no fresh dry run")
		empty := planned()
		empty.Status.PlanHash = ""
		assert.ErrorContains(t, approvableRollout(empty, "alice"), "no plan yet")
		done := planned()
		done.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: "bob", PlanHash: planHash}
		assert.ErrorContains(t, approvableRollout(done, "alice"), "already approved by bob")
	})
	t.Run("no terminal and no --yes", func(t *testing.T) {
		cl := fakeClient(planned())
		err := approveRollout(ctx, cl, rolloutApproval{Name: "lab-talosupgrade-1a2b3c4d", User: "alice", Out: &bytes.Buffer{}})
		require.ErrorContains(t, err, "--yes")
	})
}

func TestResumePauseAbort(t *testing.T) {
	ctx := context.Background()
	ro := planned()
	ro.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: "alice", PlanHash: planHash}
	ro.Spec.Paused = true
	cl := fakeClient(ro)
	get := func() *osv1alpha1.TalosRollout {
		got := &osv1alpha1.TalosRollout{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: ro.Name}, got))
		return got
	}
	require.NoError(t, resumeRollout(ctx, cl, ro.Name, &bytes.Buffer{}))
	assert.False(t, get().Spec.Paused)
	assert.Equal(t, planHash, get().Annotations[osv1alpha1.AnnotationRolloutResume], "resume names the plan it lets go on")

	require.NoError(t, pauseRollout(ctx, cl, ro.Name, &bytes.Buffer{}))
	assert.True(t, get().Spec.Paused)

	require.ErrorContains(t, abortRollout(ctx, cl, ro.Name, false, nil, false, &bytes.Buffer{}), "--yes")
	assert.False(t, get().Spec.Abort)
	require.NoError(t, abortRollout(ctx, cl, ro.Name, true, nil, false, &bytes.Buffer{}))
	assert.True(t, get().Spec.Abort)

	unapproved := planned()
	unapproved.Name = "other"
	require.NoError(t, cl.Create(ctx, unapproved))
	require.ErrorContains(t, resumeRollout(ctx, cl, "other", &bytes.Buffer{}), "approve it first")
}

func TestUpgrade(t *testing.T) {
	ctx := context.Background()
	cluster := func() *osv1alpha1.TalosCluster {
		return &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"},
			Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2"}}
	}
	t.Run("sets the spec and shows the proposed plan", func(t *testing.T) {
		cl := fakeClient(cluster(), planned())
		var out bytes.Buffer
		require.NoError(t, upgrade(ctx, cl, "talos", "lab", "v1.14.7", time.Second, time.Millisecond, &out))
		c := &osv1alpha1.TalosCluster{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "lab"}, c))
		assert.Equal(t, "v1.14.7", c.Spec.TalosVersion)
		assert.Contains(t, out.String(), "lab-talosupgrade-1a2b3c4d")
		assert.Contains(t, out.String(), "pk-metal rollout approve")
	})
	t.Run("refuses a downgrade and a bad version", func(t *testing.T) {
		cl := fakeClient(cluster())
		require.ErrorContains(t, upgrade(ctx, cl, "talos", "lab", "v1.14.5", 0, time.Millisecond, &bytes.Buffer{}), "not an upgrade")
		require.ErrorContains(t, upgrade(ctx, cl, "kubernetes", "lab", "1.36", 0, time.Millisecond, &bytes.Buffer{}), "v1.2.3")
		c := &osv1alpha1.TalosCluster{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "lab"}, c))
		assert.Equal(t, "v1.14.6", c.Spec.TalosVersion)
		assert.Equal(t, "v1.36.2", c.Spec.KubernetesVersion)
	})
	t.Run("times out when nothing is proposed", func(t *testing.T) {
		cl := fakeClient(cluster())
		err := upgrade(ctx, cl, "kubernetes", "lab", "v1.36.3", 5*time.Millisecond, time.Millisecond, &bytes.Buffer{})
		require.ErrorContains(t, err, "no rollout planned")
	})
}
