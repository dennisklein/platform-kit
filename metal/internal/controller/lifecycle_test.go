package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	talosfake "github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// ---- the pure rules -------------------------------------------------------

// view builds an etcd view: members by hostname, the healthy ones, the
// leader and the learners.
func etcdViewOf(members []string, healthy []string, leader string, learners ...string) etcdView {
	v := etcdView{healthy: map[uint64]bool{}, readFrom: "x"}
	for i, m := range members {
		id := uint64(i + 1)
		v.members = append(v.members, talos.EtcdMember{ID: id, Hostname: m, IsLearner: slices.Contains(learners, m)})
		if slices.Contains(healthy, m) {
			v.healthy[id] = true
		}
		if m == leader {
			v.leader = id
		}
	}
	return v
}

func TestRemovalVerdict(t *testing.T) {
	three := []string{"cp-1", "cp-2", "cp-3"}
	for name, tc := range map[string]struct {
		v       etcdView
		node    string
		leaving []string
		acked   bool
		want    string
	}{
		"3 to 2 leaves an even count": {v: etcdViewOf(three, three, "cp-1"), node: "cp-3", want: metalv1alpha1.ReasonEtcdRiskRefused},
		"3 to 2, acknowledged":        {v: etcdViewOf(three, three, "cp-1"), node: "cp-3", acked: true},
		"4 to 3":                      {v: etcdViewOf([]string{"cp-1", "cp-2", "cp-3", "cp-4"}, []string{"cp-1", "cp-2", "cp-3", "cp-4"}, "cp-1"), node: "cp-4"},
		"2 to 1":                      {v: etcdViewOf([]string{"cp-1", "cp-2"}, []string{"cp-1", "cp-2"}, "cp-1"), node: "cp-2"},
		"the last member":             {v: etcdViewOf([]string{"cp-1"}, []string{"cp-1"}, "cp-1"), node: "cp-1", want: metalv1alpha1.ReasonEtcdRiskRefused},
		"a dead member of 4 leaves 3 healthy of 3": {v: etcdViewOf([]string{"cp-1", "cp-2", "cp-3", "cp-4"}, three, "cp-1"), node: "cp-4"},
		"a healthy member leaves too few healthy": {v: etcdViewOf([]string{"cp-1", "cp-2", "cp-3", "cp-4", "cp-5"}, []string{"cp-1", "cp-2", "cp-3"}, "cp-1"),
			node: "cp-3", want: metalv1alpha1.ReasonEtcdRiskRefused},
		"a learner is pending":            {v: etcdViewOf([]string{"cp-1", "cp-2", "cp-3", "cp-4"}, three, "cp-1", "cp-4"), node: "cp-3", want: metalv1alpha1.ReasonRemovalWaiting},
		"the leader waits for the others": {v: etcdViewOf(three, three, "cp-1"), node: "cp-1", leaving: []string{"cp-1", "cp-2"}, acked: true, want: metalv1alpha1.ReasonRemovalWaiting},
		"a follower goes first":           {v: etcdViewOf(three, three, "cp-1"), node: "cp-2", leaving: []string{"cp-1", "cp-2"}, acked: true},
		"not a member: nothing changes":   {v: etcdViewOf(three, three, "cp-1"), node: "cp-9"},
		"etcd unreadable waits":           {v: etcdView{}, node: "cp-1", want: metalv1alpha1.ReasonRemovalWaiting},
		"etcd unreadable, acknowledged":   {v: etcdView{}, node: "cp-1", acked: true},
	} {
		t.Run(name, func(t *testing.T) {
			reason, msg := removalVerdict(tc.v, "no answer", tc.node, "lab", tc.leaving, tc.acked)
			assert.Equal(t, tc.want, reason, msg)
			if reason == metalv1alpha1.ReasonEtcdRiskRefused {
				assert.Contains(t, msg, metalv1alpha1.AnnotationAckEtcdRisk, "the refusal says how to acknowledge it")
			}
		})
	}
}

func TestJoinVerdict(t *testing.T) {
	one := []string{"cp-1"}
	for name, tc := range map[string]struct {
		v       etcdView
		joining []string
		want    string // a fragment of the message; "" to go ahead
	}{
		"healthy, nobody joining":         {v: etcdViewOf(one, one, "cp-1")},
		"a learner waits for promotion":   {v: etcdViewOf([]string{"cp-1", "cp-2"}, one, "cp-1", "cp-2"), want: "still a learner"},
		"an unhealthy member":             {v: etcdViewOf([]string{"cp-1", "cp-2"}, one, "cp-1"), want: "not healthy"},
		"another control plane joining":   {v: etcdViewOf(one, one, "cp-1"), joining: []string{"cp-2"}, want: "join one at a time"},
		"a stale member of the same name": {v: etcdViewOf([]string{"cp-1", "cp-3"}, []string{"cp-1", "cp-3"}, "cp-1"), want: "still lists a member named cp-3"},
		"etcd unreadable":                 {v: etcdView{}, want: "no answer"},
	} {
		t.Run(name, func(t *testing.T) {
			msg := joinVerdict(tc.v, "no answer", "cp-3", "lab", tc.joining)
			if tc.want == "" {
				assert.Empty(t, msg)
				return
			}
			assert.Contains(t, msg, tc.want)
		})
	}
}

// ---- a cluster of fake machines ------------------------------------------

// lifecycleLab is a bootstrapped cluster "lab" of fake machines sharing one
// etcd, its TalosNodes, and a claim reconciler over a fake API.
type lifecycleLab struct {
	r        *HostClaimReconciler
	fleet    *talosfake.Fleet
	etcd     *talosfake.EtcdCluster
	machines map[string]*talosfake.Machine
}

func labNode(name string, role osv1alpha1.NodeRole, i int) *osv1alpha1.TalosNode {
	n := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: osv1alpha1.TalosNodeSpec{
			ClusterRef: osv1alpha1.Reference{Name: "lab"}, Role: role, Address: fmt.Sprintf("10.0.0.%d", i),
		},
	}
	n.Status.Conditions = []metav1.Condition{
		{Type: osv1alpha1.ConditionMachineReachable, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonMachineAPI, LastTransitionTime: metav1.Now()},
		{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady, LastTransitionTime: metav1.Now()},
	}
	return n
}

// newLifecycleLab runs the control planes cps (members of one etcd, cps[0]
// leading) and the workers, each TalosNode named after its machine; extra
// objects go into the API too.
func newLifecycleLab(t *testing.T, cps, workers []string, extra ...client.Object) *lifecycleLab {
	t.Helper()
	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: "v1.14.6", SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "lab-secrets"},
		},
		Status: osv1alpha1.TalosClusterStatus{BootstrappedAt: ptr.To(metav1.Now())},
	}
	objs := []client.Object{cluster, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "lab-secrets"},
		Data:       map[string][]byte{render.BundleKey: bundle},
	}, slotlessEndpoint()}
	lab := &lifecycleLab{fleet: talosfake.NewFleet(), etcd: talosfake.NewEtcdCluster(), machines: map[string]*talosfake.Machine{}}
	i := 1
	for _, group := range []struct {
		names []string
		role  osv1alpha1.NodeRole
	}{{cps, osv1alpha1.RoleControlPlane}, {workers, osv1alpha1.RoleWorker}} {
		for _, name := range group.names {
			n := labNode(name, group.role, i)
			m := talosfake.NewMachine("v1.14.6")
			m.SetStage("running", true)
			m.Configure([]byte("config of " + name))
			lab.fleet.Add(n.Spec.Address, name, m)
			if group.role == osv1alpha1.RoleControlPlane {
				m.JoinEtcd(lab.etcd, false)
			}
			lab.machines[name] = m
			objs = append(objs, n)
			i++
		}
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &metalv1alpha1.HostClaim{}, &osv1alpha1.TalosNode{}).
		WithObjects(append(objs, extra...)...).Build()
	lab.r = &HostClaimReconciler{
		Client: cl, OperationsNamespace: "pk-metal", Proposer: "operator",
		Dial: lab.fleet.Dialer(), CredentialsNamespace: "pk-metal",
		SupportsAction: func(string) bool { return true },
	}
	return lab
}

// claimOn is a claim that runs node on host-a, recorded as installed by us.
func claimOn(node string) (*metalv1alpha1.HostClaim, *metalv1alpha1.Host) {
	claim := testClaim(node)
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: node, Operation: "pk-metal/boot", At: metav1.Now()}
	host := boundHost(claim)
	host.Spec.OS.NodeRef.Name = node
	return claim, host
}

func opActionKeys(op *metalv1alpha1.HostOperation) []string {
	return actionKeys(op.Spec.Actions)
}

func TestReadEtcdSeesStaleAndLearnerMembers(t *testing.T) {
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2"}, nil)
	stale := lab.etcd.AddStale("cp-9")
	cluster := &osv1alpha1.TalosCluster{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "lab"}, cluster))

	v, why, err := lab.r.etcdOf(t.Context(), cluster)
	require.NoError(t, err)
	require.Empty(t, why)
	require.Len(t, v.members, 3, "a member no machine runs is still a member")
	voting, healthy := v.voting()
	assert.Equal(t, 3, voting)
	assert.Equal(t, 2, healthy, "nobody answers for the stale member")
	assert.Contains(t, v.unhealthy(), "cp-9")
	assert.Equal(t, lab.etcd.Leader(), v.leader)
	assert.NotNil(t, v.member("cp-9"))
	assert.Equal(t, stale, v.member("cp-9").ID)
}

// ---- removal on release ---------------------------------------------------

func TestReleaseOfAnsweringControlPlaneLeavesEtcdAndWipes(t *testing.T) {
	claim, host := claimOn("cp-3")
	claim.Annotations = map[string]string{metalv1alpha1.AnnotationAckEtcdRisk: "scaling 3 to 1"}
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2", "cp-3"}, nil, host)

	require.False(t, releasePass(t, lab.r, claim))
	rel := releaseOps(t, lab.r, claim)
	require.Len(t, rel, 1)
	assert.Equal(t, []string{"cordon", "drain", "etcdForfeitLeadership", "etcdLeave", "talosReset", "deleteKubernetesNode", "powerOff"},
		opActionKeys(&rel[0]), "leave etcd, wipe, then power off; no slot to eject behind this endpoint")
	reset := rel[0].Spec.Actions[4].TalosReset
	assert.False(t, *reset.Graceful, "the steps before did what a graceful reset does")
	assert.Empty(t, reset.Mode, "a release wipes every disk")
	assert.Empty(t, rel[0].Annotations[metalv1alpha1.AnnotationDiskNotWiped])
	assert.Contains(t, provisionedCond(t, claim).Message, "takes it out of etcd")

	got := &metalv1alpha1.Host{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, got))
	assert.NotContains(t, got.Annotations, metalv1alpha1.AnnotationDiskNotWiped)

	succeedOp(t, lab.r, &rel[0])
	require.True(t, releasePass(t, lab.r, claim))
	assert.False(t, hostBound(t, lab.r))
}

func TestReleaseOfTheOnlyControlPlanePlansNoEtcdStep(t *testing.T) {
	claim, host := claimOn("cp-1")
	claim.Annotations = map[string]string{metalv1alpha1.AnnotationAckEtcdRisk: "the cluster ends"}
	lab := newLifecycleLab(t, []string{"cp-1"}, nil, host)

	require.False(t, releasePass(t, lab.r, claim))
	rel := releaseOps(t, lab.r, claim)
	require.Len(t, rel, 1)
	assert.Equal(t, []string{"cordon", "drain", "talosReset", "powerOff"},
		opActionKeys(&rel[0]), "the only member has no peer to leave to, and once it is wiped no API is left to delete its Node")
}

func TestReleaseOfUnreachableControlPlaneRemovesItsMemberByID(t *testing.T) {
	claim, host := claimOn("cp-4")
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2", "cp-3", "cp-4"}, nil, host)
	// cp-4's machine is dead: its node does not answer, and its member is
	// not healthy.
	lab.machines["cp-4"].SetUnreachable(true)
	node := &osv1alpha1.TalosNode{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-4"}, node))
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{Type: osv1alpha1.ConditionMachineReachable,
		Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonDialFailed})
	require.NoError(t, lab.r.Status().Update(t.Context(), node))
	var id uint64
	for _, m := range lab.etcd.Members() {
		if m.Hostname == "cp-4" {
			id = m.ID
		}
	}

	require.False(t, releasePass(t, lab.r, claim))
	rel := releaseOps(t, lab.r, claim)
	require.Len(t, rel, 1)
	assert.Equal(t, []string{"etcdRemoveMember", "deleteKubernetesNode", "powerOff"}, opActionKeys(&rel[0]))
	assert.Equal(t, memberIDHex(id), rel[0].Spec.Actions[0].EtcdRemoveMember.MemberID, "the member read at proposal is frozen in")
	assert.Positive(t, rel[0].Spec.Actions[2].PowerOff.GracefulTimeoutSeconds, "the OS may still be up: ask it first")

	got := &metalv1alpha1.Host{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, got))
	assert.Equal(t, "lab/cp-4", got.Annotations[metalv1alpha1.AnnotationDiskNotWiped], "nothing wiped the disk")

	succeedOp(t, lab.r, &rel[0])
	require.True(t, releasePass(t, lab.r, claim))
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, got))
	assert.Nil(t, got.Spec.ClaimRef, "released to the pool")
	assert.Equal(t, "lab/cp-4", got.Annotations[metalv1alpha1.AnnotationDiskNotWiped], "and marked until a wipe")
}

func TestReleaseRefusesTwoToOneWithoutLeavingAnEvenCount(t *testing.T) {
	// 3 to 2 is refused without an acknowledgement.
	claim, host := claimOn("cp-3")
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2", "cp-3"}, nil, host)
	require.False(t, releasePass(t, lab.r, claim))
	assert.Empty(t, releaseOps(t, lab.r, claim))
	c := provisionedCond(t, claim)
	assert.Equal(t, metalv1alpha1.ReasonEtcdRiskRefused, c.Reason)
	assert.Contains(t, c.Message, "even number")
	assert.True(t, hostBound(t, lab.r))
}

func TestOneControlPlaneLeavesAtATime(t *testing.T) {
	claim, host := claimOn("cp-4")
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2", "cp-3", "cp-4", "cp-5"}, nil, host,
		// cp-5's removal is proposed and not finished yet.
		&metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "release-host-b", Namespace: "pk-metal"},
			Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "host-b"}, ProposedBy: "operator",
				Actions: []metalv1alpha1.HostOperationAction{{EtcdLeave: &metalv1alpha1.EtcdLeaveAction{NodeRef: metalv1alpha1.Reference{Name: "cp-5"}}}}},
		})
	require.False(t, releasePass(t, lab.r, claim))
	assert.Empty(t, releaseOps(t, lab.r, claim))
	c := provisionedCond(t, claim)
	assert.Equal(t, metalv1alpha1.ReasonRemovalWaiting, c.Reason)
	assert.Contains(t, c.Message, "release-host-b")
}

func TestReleaseOfAWorkerWithoutTalosStepsMarksTheHost(t *testing.T) {
	claim, host := claimOn("w-1")
	lab := newLifecycleLab(t, []string{"cp-1"}, []string{"w-1"}, host)
	lab.r.SupportsAction = nil // the executor's own list: no Talos steps yet
	require.False(t, releasePass(t, lab.r, claim))
	rel := releaseOps(t, lab.r, claim)
	require.Len(t, rel, 1)
	if implementedActions["talosReset"] {
		t.Skip("the executor runs the Talos steps: covered by the tests above")
	}
	assert.Equal(t, []string{"powerOff"}, opActionKeys(&rel[0]))
	got := &metalv1alpha1.Host{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, got))
	assert.Equal(t, "lab/w-1", got.Annotations[metalv1alpha1.AnnotationDiskNotWiped])
}

// ---- a deleted TalosNode --------------------------------------------------

func TestDeletedTalosNodeIsRemovedWhileTheClaimKeepsTheMachine(t *testing.T) {
	claim, host := claimOn("w-1")
	lab := newLifecycleLab(t, []string{"cp-1"}, []string{"w-1"}, host, claim)
	node := &osv1alpha1.TalosNode{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "w-1"}, node))
	node.Finalizers = []string{talosNodeMemberFinalizer}
	require.NoError(t, lab.r.Update(t.Context(), node))
	require.NoError(t, lab.r.Delete(t.Context(), node))

	require.NoError(t, lab.r.provision(t.Context(), claim, "host-a"))
	ops, err := lab.r.claimOps(t.Context(), claim, purposeRemove)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, []string{"cordon", "drain", "talosReset", "deleteKubernetesNode", "powerOff"}, opActionKeys(&ops[0]))
	assert.Equal(t, "w-1", ops[0].Labels[labelNode])
	assert.Equal(t, metalv1alpha1.ReasonRemovingNode, provisionedCond(t, claim).Reason)

	// The finalizer waits for the claim's record of the install.
	fin := &TalosNodeMemberReconciler{Client: lab.r.Client, Dial: lab.fleet.Dialer(), CredentialsNamespace: "pk-metal", OperationsNamespace: "pk-metal"}
	reason, msg, err := fin.blocked(t.Context(), node)
	require.NoError(t, err)
	assert.Equal(t, osv1alpha1.ReasonMachineAssigned, reason, msg)

	succeedOp(t, lab.r, &ops[0])
	require.NoError(t, lab.r.provision(t.Context(), claim, "host-a"))
	assert.Nil(t, claim.Status.HandOff, "the install is gone, and so is its record")
	assert.Equal(t, metalv1alpha1.ReasonNodeRemoved, provisionedCond(t, claim).Reason)
	assert.True(t, hostBound(t, lab.r), "the claim keeps the machine")

	fresh := &metalv1alpha1.HostClaim{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKeyFromObject(claim), fresh))
	fresh.Status = claim.Status
	require.NoError(t, lab.r.Status().Update(t.Context(), fresh))
	res, err := fin.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "w-1"}})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	err = lab.r.Get(t.Context(), client.ObjectKey{Name: "w-1"}, node)
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "the finalizer let the node go")
}

func TestFinalizerWaitsForTheEtcdMember(t *testing.T) {
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2"}, nil)
	node := &osv1alpha1.TalosNode{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node))
	fin := &TalosNodeMemberReconciler{Client: lab.r.Client, Dial: lab.fleet.Dialer(), CredentialsNamespace: "pk-metal", OperationsNamespace: "pk-metal"}

	_, err := fin.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cp-2"}})
	require.NoError(t, err)
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node))
	require.Contains(t, node.Finalizers, talosNodeMemberFinalizer, "every node gets the finalizer")

	require.NoError(t, lab.r.Delete(t.Context(), node))
	res, err := fin.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cp-2"}})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node))
	c := meta.FindStatusCondition(node.Status.Conditions, osv1alpha1.ConditionRemovalBlocked)
	require.NotNil(t, c)
	assert.Equal(t, osv1alpha1.ReasonEtcdMemberRemains, c.Reason)

	// The member leaves (a removal ran): the deletion finishes.
	for _, m := range lab.etcd.Members() {
		if m.Hostname == "cp-2" {
			c, err := lab.fleet.Dialer()(t.Context(), talos.Target{Endpoint: "10.0.0.1", Talosconfig: []byte("x")})
			require.NoError(t, err)
			require.NoError(t, c.EtcdRemoveMemberByID(t.Context(), m.ID))
		}
	}
	_, err = fin.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cp-2"}})
	require.NoError(t, err)
	assert.Error(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node), "gone")
}

// ---- joining ---------------------------------------------------------------

func TestControlPlanesJoinOneAtATime(t *testing.T) {
	// cp-2 and cp-3 are written, and nothing runs them yet.
	fresh := func(name string, i int) *osv1alpha1.TalosNode {
		n := labNode(name, osv1alpha1.RoleControlPlane, i)
		n.Status.Conditions = nil
		return n
	}
	lab := newLifecycleLab(t, []string{"cp-1"}, nil, fresh("cp-2", 7), fresh("cp-3", 8))
	cluster := &osv1alpha1.TalosCluster{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "lab"}, cluster))
	get := func(name string) *osv1alpha1.TalosNode {
		n := &osv1alpha1.TalosNode{}
		require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: name}, n))
		return n
	}

	msg, err := lab.r.joinGate(t.Context(), get("cp-2"), cluster, "")
	require.NoError(t, err)
	assert.Empty(t, msg, "one healthy member, nobody joining: cp-2 may go")

	// cp-2's boot is under way: cp-3 waits for it.
	boot := bootOp("provision-cp-2", "cp-2", false, true)
	require.NoError(t, lab.r.Create(t.Context(), boot))
	msg, err = lab.r.joinGate(t.Context(), get("cp-3"), cluster, "")
	require.NoError(t, err)
	assert.Contains(t, msg, "provision-cp-2")

	// It checked in and joined as a learner: cp-3 still waits.
	succeedOp(t, lab.r, boot)
	m2 := talosfake.NewMachine("v1.14.6")
	m2.Configure([]byte("x"))
	lab.fleet.Add("10.0.0.7", "cp-2", m2)
	m2.JoinEtcd(lab.etcd, true)
	msg, err = lab.r.joinGate(t.Context(), get("cp-3"), cluster, "")
	require.NoError(t, err)
	assert.Contains(t, msg, "learner")

	// Promoted: cp-3's turn.
	lab.etcd.Promote("cp-2")
	msg, err = lab.r.joinGate(t.Context(), get("cp-3"), cluster, "")
	require.NoError(t, err)
	assert.Empty(t, msg)
}

// ---- reinstall -------------------------------------------------------------

func TestReinstallRequestNamesOneInstall(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	claim := testClaim("cp-1")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "cp-1", At: at}
	assert.False(t, reinstallRequested(claim, "host-a", "cp-1"))
	claim.Annotations = map[string]string{metalv1alpha1.AnnotationReinstall: "2026-11-01T10:00:00Z"}
	assert.True(t, reinstallRequested(claim, "host-a", "cp-1"))
	assert.False(t, reinstallRequested(claim, "host-b", "cp-1"), "another host's install")
	claim.Status.HandOff.At = metav1.Now()
	assert.False(t, reinstallRequested(claim, "host-a", "cp-1"), "the install it named was replaced: it asks for nothing")
}

func TestReinstallLeavesEtcdBeforeTheBoot(t *testing.T) {
	claim, host := claimOn("cp-2")
	lab := newLifecycleLab(t, []string{"cp-1", "cp-2", "cp-3"}, nil, host)
	cluster := &osv1alpha1.TalosCluster{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "lab"}, cluster))
	node := &osv1alpha1.TalosNode{}
	require.NoError(t, lab.r.Get(t.Context(), client.ObjectKey{Name: "cp-2"}, node))

	// 3 to 2 for the length of the reinstall: refused without a word.
	_, reason, _, err := lab.r.bootPrefix(t.Context(), claim, host, node, cluster, "cp-2", "provision-x")
	require.NoError(t, err)
	assert.Equal(t, metalv1alpha1.ReasonEtcdRiskRefused, reason)

	claim.Annotations = map[string]string{metalv1alpha1.AnnotationAckEtcdRisk: "disk swap"}
	steps, reason, msg, err := lab.r.bootPrefix(t.Context(), claim, host, node, cluster, "cp-2", "provision-x")
	require.NoError(t, err)
	require.Empty(t, reason, msg)
	op := &metalv1alpha1.HostOperation{Spec: metalv1alpha1.HostOperationSpec{Actions: steps}}
	assert.Equal(t, []string{"cordon", "drain", "etcdForfeitLeadership", "etcdLeave", "talosReset", "deleteKubernetesNode"}, opActionKeys(op))
	assert.Equal(t, "SystemDisk", steps[4].TalosReset.Mode, "a reinstall keeps the user disks")
}

// ---- wipe ------------------------------------------------------------------

func TestWipeIsProposedOnRequestAndClearsTheMark(t *testing.T) {
	host := onPath(&metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a", Annotations: map[string]string{
		metalv1alpha1.AnnotationDiskNotWiped: "lab/w-1", metalv1alpha1.AnnotationWipe: "v1.14.6",
		metalv1alpha1.AnnotationRequestedBy: "alice",
	}}})
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc"}}
	ep.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionInventoried, Status: metav1.ConditionTrue, Reason: "x", LastTransitionTime: metav1.Now()}}
	ep.Status.VirtualMedia = []metalv1alpha1.DiscoveredMedia{{ID: "CD", MediaTypes: []string{"CD", "DVD"}}}
	ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{MediaDialects: map[string]string{"CD": "insert-media"}}
	ep.Status.Systems = []metalv1alpha1.DiscoveredSystem{{ID: "1", BootTargets: []string{"Cd", "Pxe"}}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}, &osv1alpha1.BootAsset{}).
		WithObjects(host, ep).Build()
	w := &hostWipeReconciler{&HostClaimReconciler{Client: cl, OperationsNamespace: "pk-metal", Proposer: "operator"}}
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: "host-a"}}

	_, err := w.Reconcile(t.Context(), req)
	require.NoError(t, err)
	assets := &osv1alpha1.BootAssetList{}
	require.NoError(t, cl.List(t.Context(), assets))
	require.Len(t, assets.Items, 1)
	a := &assets.Items[0]
	assert.Contains(t, a.Spec.Schematic.Customization.ExtraKernelArgs, wipeKernelArg)
	a.Status.URL = "https://factory.example/image.iso"
	a.Status.Digest, a.Status.BuildKey = "sha256:"+strings.Repeat("ab", 32), "k"
	a.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "x", LastTransitionTime: metav1.Now()}}
	require.NoError(t, cl.Status().Update(t.Context(), a))

	_, err = w.Reconcile(t.Context(), req)
	require.NoError(t, err)
	ops := &metalv1alpha1.HostOperationList{}
	require.NoError(t, cl.List(t.Context(), ops))
	require.Len(t, ops.Items, 1)
	op := &ops.Items[0]
	assert.Equal(t, []string{"ejectMedia", "insertMedia", "setBootOverride", "powerCycle"}, opActionKeys(op))
	assert.Equal(t, a.Status.Digest, op.Spec.Actions[1].InsertMedia.Digest, "the wipe image is pinned to what the operator downloaded")
	assert.True(t, *op.Spec.Approval.Required, "erasing a disk is a person's decision")
	assert.Equal(t, []string{"alice"}, op.Spec.RequestedBy, "the person who asked for the wipe is named")

	meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{Type: metalv1alpha1.ConditionExecuted,
		Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonStepsSucceeded})
	require.NoError(t, cl.Status().Update(t.Context(), op))
	_, err = w.Reconcile(t.Context(), req)
	require.NoError(t, err)
	got := &metalv1alpha1.Host{}
	require.NoError(t, cl.Get(t.Context(), req.NamespacedName, got))
	assert.NotContains(t, got.Annotations, metalv1alpha1.AnnotationDiskNotWiped)
	assert.NotContains(t, got.Annotations, metalv1alpha1.AnnotationWipe)
}

func TestNotWipedHostTakesNoOtherInstall(t *testing.T) {
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a",
		Annotations: map[string]string{metalv1alpha1.AnnotationDiskNotWiped: "lab/w-1"}}}
	assert.Contains(t, notWipedHold(host, "lab", "w-2"), "pk-metal wipe host-a")
	assert.Empty(t, notWipedHold(host, "lab", "w-1"), "the node's own install is replaced by its boot")
	assert.NotEmpty(t, notWipedPowerHold(host))
	projectDiskWiped(host)
	c := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionDiskWiped)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonInstalledNodeRemains, c.Reason)
}

// TestClaimHeldForEtcdLooksAgain: what a claim held for etcd waits for (a
// member turning voter, another claim's operation finishing) is no event of
// its own, so the claim asks to be looked at again; other holds do not.
func TestClaimHeldForEtcdLooksAgain(t *testing.T) {
	held := func(reason string) *metalv1alpha1.HostClaim {
		c := testClaim("cp-3")
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type: metalv1alpha1.ConditionProvisioned, Status: metav1.ConditionFalse, Reason: reason, Message: "x",
		})
		return c
	}
	assert.Equal(t, etcdWaitPoll, etcdWaitResult(held(metalv1alpha1.ReasonEtcdJoinWaiting)).RequeueAfter)
	assert.Equal(t, etcdWaitPoll, etcdWaitResult(held(metalv1alpha1.ReasonRemovalWaiting)).RequeueAfter)
	assert.Zero(t, etcdWaitResult(held(metalv1alpha1.ReasonAwaitingOperation)).RequeueAfter,
		"an operation of its own wakes the claim through the watch")
	assert.Zero(t, etcdWaitResult(testClaim("cp-3")).RequeueAfter)
}
