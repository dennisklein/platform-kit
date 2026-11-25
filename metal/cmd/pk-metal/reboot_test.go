package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func talosNodeObj(name string, role osv1alpha1.NodeRole) *osv1alpha1.TalosNode {
	return &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}, Role: role, Address: "10.0.0.2"},
	}
}

func hostRunning(name, node string) *metalv1alpha1.Host {
	return &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: node}}},
	}
}

func TestProposeReboot(t *testing.T) {
	ctx := context.Background()
	cl := fakeClient(talosNodeObj("w-1", osv1alpha1.RoleWorker), talosNodeObj("cp-2", osv1alpha1.RoleControlPlane),
		hostRunning("pk0004", "w-1"), hostRunning("pk0002", "cp-2"))
	opts := rebootOptions{User: "alice", DrainTimeout: 15 * time.Minute}

	var out bytes.Buffer
	opts.Node = "w-1"
	op, err := proposeReboot(ctx, cl, opts, &out)
	require.NoError(t, err)
	assert.Equal(t, "pk0004", op.Spec.HostRef.Name)
	assert.Equal(t, "alice", op.Spec.ProposedBy, "admission checks the proposer is the caller")
	assert.True(t, *op.Spec.Approval.Required)
	assert.Equal(t, []string{"cordon", "drain", "talosReboot", "waitNodeReady", "uncordon"}, keys(op.Spec.Actions))
	assert.Equal(t, int32(900), op.Spec.Actions[1].Drain.TimeoutSeconds)
	assert.Contains(t, out.String(), "pk-metal approve "+op.Namespace+"/"+op.Name)

	out.Reset()
	opts.Node, opts.PowerCycle, opts.GracePeriod = "cp-2", true, ptr.To[int32](30)
	op, err = proposeReboot(ctx, cl, opts, &out)
	require.NoError(t, err)
	assert.Equal(t, []string{"cordon", "drain", "talosReboot", "waitNodeReady", "waitEtcdHealthy", "uncordon"}, keys(op.Spec.Actions),
		"a control plane is uncordoned only once etcd is healthy again")
	assert.Equal(t, "PowerCycle", op.Spec.Actions[2].TalosReboot.Mode)
	assert.Equal(t, int32(30), *op.Spec.Actions[1].Drain.GracePeriodSeconds)
	assert.Contains(t, out.String(), "control plane of cluster lab")

	opts.Node = "w-9"
	_, err = proposeReboot(ctx, cl, opts, &out)
	assert.ErrorContains(t, err, "no TalosNode w-9")
	require.NoError(t, cl.Create(ctx, talosNodeObj("w-2", osv1alpha1.RoleWorker)))
	opts.Node = "w-2"
	_, err = proposeReboot(ctx, cl, opts, &out)
	assert.ErrorContains(t, err, "no host runs TalosNode w-2")
}

func keys(actions []metalv1alpha1.HostOperationAction) []string {
	var out []string
	for _, a := range actions {
		out = append(out, a.Key())
	}
	return out
}

func TestAbort(t *testing.T) {
	ctx := context.Background()
	op, objs := bootPlan()
	cl := fakeClient(objs...)
	var out bytes.Buffer

	require.NoError(t, abort(ctx, cl, op.Namespace, op.Name, "alice", "wrong host", &out))
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(op), got))
	assert.Equal(t, "alice: wrong host", got.Annotations[metalv1alpha1.AnnotationAbort])
	assert.Contains(t, out.String(), "has not started, so it ends now")

	out.Reset()
	require.NoError(t, abort(ctx, cl, op.Namespace, op.Name, "bob", "also wrong", &out))
	assert.Contains(t, out.String(), "already being aborted (alice: wrong host)")

	got.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionTrue,
		Reason: metalv1alpha1.ReasonStepsSucceeded, LastTransitionTime: metav1.Now()}}
	done := fakeClient(append([]client.Object{got}, objs[1:]...)...)
	assert.ErrorContains(t, abort(ctx, done, op.Namespace, op.Name, "alice", "late", &out), "has finished (Succeeded)")
}

// An abort names who asked and why: admission accepts no other form.
func TestAbortNeedsAReason(t *testing.T) {
	ctx := context.Background()
	op, objs := bootPlan()
	cl := fakeClient(objs...)
	var out bytes.Buffer
	assert.ErrorContains(t, abort(ctx, cl, op.Namespace, op.Name, "alice", "  ", &out), "say why")
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(op), got))
	assert.NotContains(t, got.Annotations, metalv1alpha1.AnnotationAbort, "nothing was written")
}

// TestEveryActionIsDescribed: the approver reads every step in plain words;
// none falls back to "read it in spec.actions".
func TestEveryActionIsDescribed(t *testing.T) {
	node := metalv1alpha1.Reference{Name: "cp-2"}
	digest := "sha256:" + strings.Repeat("a", 64)
	all := metalv1alpha1.HostOperationAction{
		PowerCycle: &metalv1alpha1.PowerCycleAction{}, PowerOff: &metalv1alpha1.PowerOffAction{},
		SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd"},
		InsertMedia:     &metalv1alpha1.InsertMediaAction{MediaID: "Cd", ImageURL: "https://x/i.iso"},
		EjectMedia:      &metalv1alpha1.EjectMediaAction{MediaID: "Cd"},
		ApplyBios:       &metalv1alpha1.ApplyBiosAction{}, FlashFirmware: &metalv1alpha1.FlashFirmwareAction{},
		AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{NodeRef: node},
		Cordon:       &metalv1alpha1.CordonAction{NodeRef: node}, Drain: &metalv1alpha1.DrainAction{NodeRef: node},
		Uncordon:      &metalv1alpha1.UncordonAction{NodeRef: node},
		TalosReboot:   &metalv1alpha1.TalosRebootAction{NodeRef: node},
		TalosShutdown: &metalv1alpha1.TalosShutdownAction{NodeRef: node},
		TalosUpgrade:  &metalv1alpha1.TalosUpgradeAction{NodeRef: node, Image: "factory.talos.dev/installer/x:v1.14.7@" + digest, Version: "v1.14.7"},
		TalosApplyConfig: &metalv1alpha1.TalosApplyConfigAction{NodeRef: node,
			ConfigSecretRef: metalv1alpha1.Reference{Name: "machineconfig-cp-2"}, Digest: digest, Mode: "Reboot"},
		EtcdLeave:        &metalv1alpha1.EtcdLeaveAction{NodeRef: node},
		EtcdRemoveMember: &metalv1alpha1.EtcdRemoveMemberAction{NodeRef: node, MemberID: "8e9e05c52164694d", Hostname: "cp-2"},
		TalosReset:       &metalv1alpha1.TalosResetAction{NodeRef: node, Graceful: ptr.To(false), UserDisks: []string{"/dev/sdb"}},
		WaitNodeReady:    &metalv1alpha1.WaitNodeReadyAction{NodeRef: node, Version: "v1.14.7"},
		WaitEtcdHealthy:  &metalv1alpha1.WaitEtcdHealthyAction{NodeRef: node},
	}
	pc := planContext{Nodes: map[string]*osv1alpha1.TalosNode{"cp-2": talosNodeObj("cp-2", osv1alpha1.RoleControlPlane)}}
	// One action at a time: copy each set field into an otherwise empty union.
	v := reflect.ValueOf(all)
	for i := range v.NumField() {
		one := metalv1alpha1.HostOperationAction{}
		reflect.ValueOf(&one).Elem().Field(i).Set(v.Field(i))
		got := describeAction(one, pc)
		assert.NotContains(t, got, "not described", one.Key())
		if talosNodeRef(one) != "" && one.AwaitCheckIn == nil {
			assert.Contains(t, got, "node cp-2 (control plane of cluster lab)", one.Key())
		}
	}
	assert.Equal(t, len(metalv1alpha1.ActionKeys()), v.NumField(), "the test sets every action")

	reset := describeAction(metalv1alpha1.HostOperationAction{TalosReset: all.TalosReset}, pc)
	assert.Contains(t, reset, "WIPE the system disk and the user disks /dev/sdb of")
	bare := describeAction(metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node}}, pc)
	assert.Contains(t, bare, "WIPE the system disk and no user disk of", "a reset that names no user disk says so")
	assert.Contains(t, reset, "not graceful")
	upgrade := describeAction(metalv1alpha1.HostOperationAction{TalosUpgrade: all.TalosUpgrade}, pc)
	assert.Contains(t, upgrade, "UPGRADE Talos on node cp-2 (control plane of cluster lab) to v1.14.7 from factory.talos.dev/installer/x:v1.14.7")
	drain := describeAction(metalv1alpha1.HostOperationAction{Drain: all.Drain}, pc)
	assert.Contains(t, drain, "wait up to 15m")
	assert.Contains(t, drain, "a pod with emptyDir data fails the step")
}
