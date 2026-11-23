package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

const operator = "system:serviceaccount:pk-metal:pk-metal-manager"

// bootPlan is the operation the operator proposes to install a node, with
// everything the plan names.
func bootPlan() (*metalv1alpha1.HostOperation, []client.Object) {
	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "provision-node1-1a2b3c4d",
			CreationTimestamp: metav1.NewTime(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)),
			Labels:            map[string]string{labelClaimNamespace: "default", labelClaimName: "node-1"}},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: "pk0001"},
			Reason:     "Provisioning",
			ProposedBy: operator,
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(true)},
			Actions: []metalv1alpha1.HostOperationAction{
				{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "CD"}},
				{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "USBStick"}},
				{InsertMedia: &metalv1alpha1.InsertMediaAction{MediaID: "CD",
					BootAssetRef: &metalv1alpha1.Reference{Name: "installer-v1.14.6"}}},
				{InsertMedia: &metalv1alpha1.InsertMediaAction{MediaID: "USBStick",
					BootAssetRef: &metalv1alpha1.Reference{Name: "node-1-config"},
					Digest:       "sha256:0123456789abcdef0123456789abcdef"}},
				{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd", Once: ptr.To(true), Mode: "UEFI"}},
				{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
				{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{NodeRef: metalv1alpha1.Reference{Name: "node-1"}, TimeoutSeconds: 1800}},
				{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "USBStick"}},
				{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "CD"}},
			},
		},
		Status: metalv1alpha1.HostOperationStatus{
			Phase: metalv1alpha1.OperationAwaitingApproval, BlockedOn: metalv1alpha1.ReasonAwaitingApproval,
			Conditions: []metav1.Condition{{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionFalse,
				Reason: metalv1alpha1.ReasonAwaitingApproval}}},
	}
	host := &metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "pk0001"},
		Status: metalv1alpha1.HostStatus{
			Identity:  &metalv1alpha1.HostIdentity{Serial: "PK0001", Vendor: "QEMU"},
			ManagedBy: []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "node-1-bmc"}, SystemID: "1", Reachable: true}},
		},
	}
	ep := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-bmc"},
		Spec:       metalv1alpha1.ManagementEndpointSpec{AssetRef: metalv1alpha1.Reference{Name: "node-1"}, Address: "https://10.0.0.1:8441"},
	}
	installer := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "installer-v1.14.6"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: "v1.14.6"},
	}
	config := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-config"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node-1"}},
	}
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec: osv1alpha1.TalosNodeSpec{Role: osv1alpha1.RoleControlPlane, Address: "10.0.0.11",
			InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "disk-0001"}},
	}
	return op, []client.Object{op, host, ep, installer, config, node}
}

func fakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).Build()
}

func TestRenderPlan(t *testing.T) {
	op, objs := bootPlan()
	cl := fakeClient(objs...)
	pc, err := loadPlanContext(context.Background(), cl, op)
	require.NoError(t, err)

	want := `operation pk-metal/provision-node1-1a2b3c4d
  why:         Provisioning
  proposed by: system:serviceaccount:pk-metal:pk-metal-manager
host pk0001 (asset node-1, serial PK0001)
  BMC: https://10.0.0.1:8441 (endpoint node-1-bmc)
steps, in this order:
   1. eject whatever is mounted on virtual media slot CD
   2. eject whatever is mounted on virtual media slot USBStick
   3. mount the Talos v1.14.6 installer (boot asset installer-v1.14.6) on virtual media slot CD
   4. mount the machine config of TalosNode node-1 (boot asset node-1-config) on virtual media slot USBStick (content sha256:0123456789ab…)
   5. boot from Cd at the next start only, in UEFI mode
   6. power cycle: if the machine is on, force it off (no clean shutdown) and on again; if it is off, power it on
   7. wait up to 30m for TalosNode node-1 to report in at 10.0.0.11
   8. eject whatever is mounted on virtual media slot USBStick
   9. eject whatever is mounted on virtual media slot CD
boots TalosNode node-1: address 10.0.0.11, role controlplane
  installs Talos onto the disk that matches serial=disk-0001; everything on that disk is overwritten
`
	assert.Equal(t, want, renderPlan(op, pc))
}

// TestRenderPlanMissingObjects: a plan whose host or node is gone still
// renders, and says what it cannot know.
func TestRenderPlanMissingObjects(t *testing.T) {
	op, _ := bootPlan()
	pc, err := loadPlanContext(context.Background(), fakeClient(op), op)
	require.NoError(t, err)
	out := renderPlan(op, pc)
	assert.Contains(t, out, "host pk0001 (not found in the cluster)")
	assert.Contains(t, out, "boot asset installer-v1.14.6 (not found in the cluster)")
	assert.Contains(t, out, "boots TalosNode node-1 (not found in the cluster: the disk it installs onto is unknown)")
}

func TestDescribeOtherActions(t *testing.T) {
	pc := planContext{}
	assert.Equal(t, "change 2 BIOS setting(s): BootMode=\"Uefi\", SriovGlobalEnable=true (takes effect at the next start)",
		describeAction(metalv1alpha1.HostOperationAction{ApplyBios: &metalv1alpha1.ApplyBiosAction{
			Attributes: map[string]apiextensionsv1.JSON{"SriovGlobalEnable": {Raw: []byte("true")}, "BootMode": {Raw: []byte(`"Uefi"`)}},
		}}, pc))
	assert.Equal(t, "power the machine off (forced; the OS is not asked to shut down)",
		describeAction(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}}, pc))
	assert.Equal(t, "boot from Pxe at every start, until changed",
		describeAction(metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Pxe", Once: ptr.To(false)}}, pc))
	assert.Equal(t, "flash firmware version 1.2.3 from https://repo.example/bmc.bin",
		describeAction(metalv1alpha1.HostOperationAction{FlashFirmware: &metalv1alpha1.FlashFirmwareAction{Version: "1.2.3", Image: "https://repo.example/bmc.bin"}}, pc))
	assert.Equal(t, "model=SAMSUNG* and busPath=/pci0000:00/*", diskSelector(osv1alpha1.DiskSelector{Model: "SAMSUNG*", BusPath: "/pci0000:00/*"}))
	assert.Equal(t, "no selector (Talos picks a disk)", diskSelector(osv1alpha1.DiskSelector{}))
}

func TestSplitRef(t *testing.T) {
	for _, tc := range []struct {
		arg, ns  string
		nsSet    bool
		wantNS   string
		wantName string
		wantErr  string
	}{
		{arg: "op", ns: "pk-metal", wantNS: "pk-metal", wantName: "op"},
		{arg: "op", ns: "other", nsSet: true, wantNS: "other", wantName: "op"},
		{arg: "pk-metal/op", ns: "default", wantNS: "pk-metal", wantName: "op"},
		{arg: "pk-metal/op", ns: "pk-metal", nsSet: true, wantNS: "pk-metal", wantName: "op"},
		{arg: "pk-metal/op", ns: "default", nsSet: true, wantErr: "-n says default"},
		{arg: "/op", wantErr: "not a name"},
		{arg: "ns/", wantErr: "not a name"},
		{arg: "a/b/c", wantErr: "not a name"},
		{arg: "", wantErr: "empty"},
	} {
		ns, name, err := splitRef(tc.arg, tc.ns, tc.nsSet)
		if tc.wantErr != "" {
			assert.ErrorContains(t, err, tc.wantErr, tc.arg)
			continue
		}
		require.NoError(t, err, tc.arg)
		assert.Equal(t, tc.wantNS, ns, tc.arg)
		assert.Equal(t, tc.wantName, name, tc.arg)
	}
}

func TestNotFoundHint(t *testing.T) {
	op, _ := bootPlan()
	twin := op.DeepCopy()
	twin.Namespace, twin.ResourceVersion = "lab", ""
	ctx := context.Background()

	err := getNamed(ctx, fakeClient(op), "operation", &metalv1alpha1.HostOperation{}, &metalv1alpha1.HostOperationList{},
		"default", op.Name)
	assert.EqualError(t, err, "no operation provision-node1-1a2b3c4d in namespace default; "+
		"it is in namespace pk-metal: use pk-metal/provision-node1-1a2b3c4d")

	err = getNamed(ctx, fakeClient(op, twin), "operation", &metalv1alpha1.HostOperation{}, &metalv1alpha1.HostOperationList{},
		"default", op.Name)
	assert.ErrorContains(t, err, "it is in namespaces lab, pk-metal")

	err = getNamed(ctx, fakeClient(), "operation", &metalv1alpha1.HostOperation{}, &metalv1alpha1.HostOperationList{},
		"pk-metal", "nope")
	assert.EqualError(t, err, "no operation nope in namespace pk-metal, and in no other namespace")
}

func approvedBy(t *testing.T, cl client.Client, op *metalv1alpha1.HostOperation) string {
	t.Helper()
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(op), got))
	return got.Spec.Approval.ApprovedBy
}

func TestApprove(t *testing.T) {
	ctx := context.Background()
	req := func(op *metalv1alpha1.HostOperation, out *bytes.Buffer) approval {
		return approval{Namespace: op.Namespace, Name: op.Name, User: "alice", Out: out}
	}

	t.Run("no terminal and no --yes refuses after printing the plan", func(t *testing.T) {
		op, objs := bootPlan()
		cl := fakeClient(objs...)
		var out bytes.Buffer
		err := approve(ctx, cl, req(op, &out))
		assert.ErrorContains(t, err, "rerun with --yes")
		assert.Contains(t, out.String(), "steps, in this order:")
		assert.Empty(t, approvedBy(t, cl, op))
	})

	t.Run("a person who answers no changes nothing", func(t *testing.T) {
		op, objs := bootPlan()
		cl := fakeClient(objs...)
		var out bytes.Buffer
		a := req(op, &out)
		a.Interactive, a.In = true, strings.NewReader("\n")
		assert.ErrorIs(t, approve(ctx, cl, a), errDeclined)
		assert.Contains(t, out.String(), "Approve as alice? [y/N] ")
		assert.Empty(t, approvedBy(t, cl, op))
	})

	t.Run("a person who answers yes approves", func(t *testing.T) {
		op, objs := bootPlan()
		cl := fakeClient(objs...)
		var out bytes.Buffer
		a := req(op, &out)
		a.Interactive, a.In = true, strings.NewReader("y\n")
		require.NoError(t, approve(ctx, cl, a))
		assert.Equal(t, "alice", approvedBy(t, cl, op))
	})

	t.Run("--yes approves without asking", func(t *testing.T) {
		op, objs := bootPlan()
		cl := fakeClient(objs...)
		var out bytes.Buffer
		a := req(op, &out)
		a.Yes = true
		require.NoError(t, approve(ctx, cl, a))
		assert.Equal(t, "alice", approvedBy(t, cl, op))
		assert.NotContains(t, out.String(), "[y/N]")
	})

	t.Run("any user name reaches the patch intact", func(t *testing.T) {
		// Go's %q and JSON escape differently: \x01 and \a are Go escapes
		// that JSON does not have, so such a name broke the patch.
		const user = "oidc:Alice \"A\" O'Neil\x01\a "
		op, objs := bootPlan()
		cl := fakeClient(objs...)
		var out bytes.Buffer
		a := req(op, &out)
		a.User, a.Yes = user, true
		require.NoError(t, approve(ctx, cl, a))
		assert.Equal(t, user, approvedBy(t, cl, op))
	})

	t.Run("the proposer cannot approve", func(t *testing.T) {
		op, objs := bootPlan()
		var out bytes.Buffer
		a := req(op, &out)
		a.User, a.Yes = operator, true
		assert.ErrorContains(t, approve(ctx, fakeClient(objs...), a), "another person must approve it")
	})

	t.Run("an approved operation stays as it is", func(t *testing.T) {
		op, objs := bootPlan()
		op.Spec.Approval.ApprovedBy = "bob"
		var out bytes.Buffer
		a := req(op, &out)
		a.Yes = true
		assert.ErrorContains(t, approve(ctx, fakeClient(objs...), a), "already approved by bob")
	})

	t.Run("a finished operation has nothing to approve", func(t *testing.T) {
		op, objs := bootPlan()
		op.Status.Conditions = append(op.Status.Conditions, executed(metav1.ConditionFalse))
		var out bytes.Buffer
		a := req(op, &out)
		a.Yes = true
		assert.ErrorContains(t, approve(ctx, fakeClient(objs...), a), "has finished (Failed)")
	})

	t.Run("the conditions decide, not a stale phase", func(t *testing.T) {
		// The phase is written after the conditions and can lag behind them.
		op, objs := bootPlan()
		op.Status.Phase = metalv1alpha1.OperationFailed
		var out bytes.Buffer
		a := req(op, &out)
		a.Yes = true
		require.NoError(t, approve(ctx, fakeClient(objs...), a), "no Executed outcome: the operation still waits")

		op, objs = bootPlan()
		op.Status.Conditions = append(op.Status.Conditions, executed(metav1.ConditionTrue))
		out.Reset()
		a = req(op, &out)
		a.Yes = true
		assert.ErrorContains(t, approve(ctx, fakeClient(objs...), a), "has finished (Succeeded)",
			"an AwaitingApproval phase does not hide a recorded outcome")
	})
}

func TestListPending(t *testing.T) {
	op, objs := bootPlan()
	auto := op.DeepCopy()
	auto.Name, auto.ResourceVersion = "auto", ""
	auto.Spec.Approval.Required = ptr.To(false)
	done := op.DeepCopy()
	done.Name, done.ResourceVersion = "done", ""
	done.Status.Conditions = append(done.Status.Conditions, executed(metav1.ConditionTrue))
	cl := fakeClient(append(objs, auto, done)...)

	var out bytes.Buffer
	require.NoError(t, listPending(context.Background(), cl, &out, op.CreationTimestamp.Add(90*time.Minute)))
	assert.Regexp(t, `(?m)^OPERATION\s+HOST\s+WHY\s+STEPS\s+PROPOSED BY\s+AGE$`, out.String())
	assert.Regexp(t, `(?m)^pk-metal/provision-node1-1a2b3c4d\s+pk0001\s+Provisioning\s+9\s+`+operator+`\s+1h30m$`, out.String())
	assert.NotContains(t, out.String(), "auto", "an operation that needs no approval is not pending")
	assert.NotContains(t, out.String(), "pk-metal/done")

	out.Reset()
	require.NoError(t, listPending(context.Background(), fakeClient(), &out, time.Now()))
	assert.Equal(t, "no operation waits for approval\n", out.String())
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 30 * time.Minute: "30m", 90 * time.Minute: "1h30m",
		2 * time.Hour: "2h", 72 * time.Hour: "3d",
	} {
		assert.Equal(t, want, shortDuration(d))
	}
}

// executed is the executor's outcome condition.
func executed(status metav1.ConditionStatus) metav1.Condition {
	reason := metalv1alpha1.ReasonStepFailed
	if status == metav1.ConditionTrue {
		reason = metalv1alpha1.ReasonStepsSucceeded
	}
	return metav1.Condition{Type: metalv1alpha1.ConditionExecuted, Status: status, Reason: reason}
}
