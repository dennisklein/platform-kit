package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// selfNode is a TalosNode of cluster "mgmt" at 10.0.0.<n>, checked in.
func selfNode(name string, role osv1alpha1.NodeRole, n byte) osv1alpha1.TalosNode {
	node := osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: osv1alpha1.TalosNodeSpec{
			ClusterRef: osv1alpha1.Reference{Name: "mgmt"}, Role: role,
			Network: osv1alpha1.NodeNetwork{Links: []osv1alpha1.Link{{
				Name: "lan", HardwareAddr: "52:54:00:00:00:01",
				Addresses: []string{"10.0.0." + string('0'+n) + "/24"},
			}}},
		},
	}
	node.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: osv1alpha1.ReasonNodeReady, LastTransitionTime: metav1.Now()}}
	return node
}

func runsNodeHost(name, node string) metalv1alpha1.Host {
	return metalv1alpha1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: node}}},
	}
}

func k8sNodeAt(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: name},
			{Type: corev1.NodeInternalIP, Address: ip},
		}},
	}
}

func TestLocateSelf(t *testing.T) {
	nodes := []osv1alpha1.TalosNode{
		selfNode("cp-1", osv1alpha1.RoleControlPlane, 1),
		selfNode("cp-2", osv1alpha1.RoleControlPlane, 2),
	}
	withSpecAddress := selfNode("w-1", osv1alpha1.RoleWorker, 9)
	withSpecAddress.Spec.Network.Links = nil
	withSpecAddress.Spec.Address = "10.0.0.7"
	nodes = append(nodes, withSpecAddress)

	for name, tc := range map[string]struct {
		nodeName string
		k8s      *corev1.Node
		want     string
		how      string
	}{
		"by link address":          {nodeName: "talos-abc-def", k8s: k8sNodeAt("talos-abc-def", "10.0.0.2"), want: "cp-2", how: "address 10.0.0.2"},
		"by spec address":          {nodeName: "talos-x", k8s: k8sNodeAt("talos-x", "10.0.0.7"), want: "w-1", how: "address 10.0.0.7"},
		"by name, Node unread":     {nodeName: "cp-1", want: "cp-1", how: `node name "cp-1"`},
		"a kind node matches none": {nodeName: "pk-metal-control-plane", k8s: k8sNodeAt("pk-metal-control-plane", "172.18.0.2")},
		"no node name":             {k8s: k8sNodeAt("x", "10.0.0.1")},
	} {
		t.Run(name, func(t *testing.T) {
			got := locateSelf(tc.nodeName, tc.k8s, nodes)
			if tc.want == "" {
				assert.Nil(t, got.node)
				return
			}
			require.NotNil(t, got.node)
			assert.Equal(t, tc.want, got.node.Name)
			assert.Equal(t, tc.how, got.how)
		})
	}
}

func TestDisruptionVerdict(t *testing.T) {
	cp1 := selfNode("cp-1", osv1alpha1.RoleControlPlane, 1)
	cp2 := selfNode("cp-2", osv1alpha1.RoleControlPlane, 2)
	cp3 := selfNode("cp-3", osv1alpha1.RoleControlPlane, 3)
	w1 := selfNode("w-1", osv1alpha1.RoleWorker, 4)
	fresh := selfNode("cp-4", osv1alpha1.RoleControlPlane, 5)
	fresh.Status.Conditions = nil
	other := selfNode("o-1", osv1alpha1.RoleControlPlane, 6)
	other.Spec.ClusterRef.Name = "work"
	nodes := []osv1alpha1.TalosNode{cp1, cp2, cp3, w1, fresh, other}
	cluster := func(total, ready int32) *osv1alpha1.TalosCluster {
		c := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "mgmt"}}
		c.Status.ControlPlanes, c.Status.ReadyControlPlanes = total, ready
		return c
	}
	allowed := func(h metalv1alpha1.Host) metalv1alpha1.Host {
		h.Annotations = map[string]string{metalv1alpha1.AnnotationAllowSelfDisruption: "true"}
		return h
	}
	running := func(host, name string) metalv1alpha1.HostOperation {
		op := metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: name},
			Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: host},
				Actions: []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}}},
		}
		now := metav1.Now()
		op.Status.Baseline = &metalv1alpha1.OperationBaseline{CapturedAt: &now}
		return op
	}
	hosts := []metalv1alpha1.Host{runsNodeHost("h-1", "cp-1"), runsNodeHost("h-2", "cp-2"), runsNodeHost("h-3", "cp-3")}
	onCP1 := selfLocation{node: &cp1, how: "address 10.0.0.1"}
	onW1 := selfLocation{node: &w1, how: "address 10.0.0.4"}

	for name, tc := range map[string]struct {
		in   disruptionInput
		want []string // empty: allowed
	}{
		"nothing known about where the operator runs": {
			in: disruptionInput{host: ptr.To(runsNodeHost("h-1", "cp-1")), nodes: nodes},
		},
		"a host of another cluster": {
			in: disruptionInput{host: ptr.To(runsNodeHost("h-9", "o-1")), self: onCP1, nodes: nodes, cluster: cluster(3, 3)},
		},
		"a host without an OS": {
			in: disruptionInput{host: &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h-0"}}, self: onCP1, nodes: nodes},
		},
		"a fresh control plane joining the operator's cluster": {
			in: disruptionInput{host: ptr.To(runsNodeHost("h-4", "cp-4")), self: onCP1, nodes: nodes, cluster: cluster(3, 3)},
		},
		"a worker of the operator's cluster it does not run on": {
			in: disruptionInput{host: ptr.To(runsNodeHost("h-5", "w-1")), self: onCP1, nodes: nodes, cluster: cluster(3, 3)},
		},
		"the machine the operator runs on": {
			in:   disruptionInput{host: ptr.To(runsNodeHost("h-5", "w-1")), self: onW1, nodes: nodes, cluster: cluster(3, 3)},
			want: []string{"this operator runs on it (address 10.0.0.4)", "kubectl annotate host h-5 metal.pk.io/allow-self-disruption=true", "pk-metal pivot"},
		},
		"the machine the operator runs on, allowed": {
			in: disruptionInput{host: ptr.To(allowed(runsNodeHost("h-5", "w-1"))), self: onW1, nodes: nodes, cluster: cluster(3, 3)},
		},
		"a control plane of the operator's cluster": {
			in:   disruptionInput{host: ptr.To(runsNodeHost("h-2", "cp-2")), self: onCP1, nodes: nodes, cluster: cluster(3, 3), hosts: hosts},
			want: []string{"a control plane of cluster mgmt", "kubectl annotate host h-2"},
		},
		"a control plane, allowed, with a majority left": {
			in: disruptionInput{host: ptr.To(allowed(runsNodeHost("h-2", "cp-2"))), self: onCP1, nodes: nodes, cluster: cluster(3, 3), hosts: hosts},
		},
		"a control plane, allowed, without a majority left": {
			in:   disruptionInput{host: ptr.To(allowed(runsNodeHost("h-2", "cp-2"))), self: onCP1, nodes: nodes, cluster: cluster(3, 2), hosts: hosts},
			want: []string{"2 of 3 control planes ready", "1 are left and etcd needs 2"},
		},
		"the only control plane, allowed": {
			in:   disruptionInput{host: ptr.To(allowed(runsNodeHost("h-1", "cp-1"))), self: onCP1, nodes: nodes, cluster: cluster(1, 1), hosts: hosts},
			want: []string{"1 of 1 control planes ready", "etcd needs 1"},
		},
		"a control plane, allowed, while another one is cycled": {
			in: disruptionInput{host: ptr.To(allowed(runsNodeHost("h-2", "cp-2"))), self: onCP1, nodes: nodes, cluster: cluster(3, 3), hosts: hosts,
				ops: []metalv1alpha1.HostOperation{running("h-3", "bios-h-3")}},
			want: []string{"pk-metal/bios-h-3 is taking down control plane h-3", "one at a time"},
		},
		"a control plane, allowed, its cluster unknown": {
			in:   disruptionInput{host: ptr.To(allowed(runsNodeHost("h-2", "cp-2"))), self: onCP1, nodes: nodes, hosts: hosts},
			want: []string{"TalosCluster mgmt is not found"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := disruptionVerdict(tc.in)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			for _, w := range tc.want {
				assert.Contains(t, got, w)
			}
		})
	}
}

// guardedObjects is the operator's cluster: three control planes, the
// operator on cp-1 (Kubernetes node talos-1 at 10.0.0.1), and host-a running
// cp-2.
func guardedObjects() []client.Object {
	cp1 := selfNode("cp-1", osv1alpha1.RoleControlPlane, 1)
	cp2 := selfNode("cp-2", osv1alpha1.RoleControlPlane, 2)
	cp3 := selfNode("cp-3", osv1alpha1.RoleControlPlane, 3)
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "mgmt"}}
	cluster.Status.ControlPlanes, cluster.Status.ReadyControlPlanes = 3, 3
	return []client.Object{&cp1, &cp2, &cp3, cluster, k8sNodeAt("talos-1", "10.0.0.1")}
}

func TestSelfGuardCheckReadsTheCluster(t *testing.T) {
	objs := guardedObjects()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	g := &SelfGuard{Reader: cl, NodeName: "talos-1"}
	host := runsNodeHost("host-a", "cp-2")

	msg, err := g.Check(t.Context(), &host)
	require.NoError(t, err)
	assert.Contains(t, msg, "a control plane of cluster mgmt")

	var nilGuard *SelfGuard
	msg, err = nilGuard.Check(t.Context(), &host)
	require.NoError(t, err)
	assert.Empty(t, msg, "no guard wired: allowed")

	unknown := &SelfGuard{Reader: cl, NodeName: "kind-control-plane"}
	msg, err = unknown.Check(t.Context(), &host)
	require.NoError(t, err)
	assert.Empty(t, msg, "a node no TalosNode describes: allowed")
}

func TestSelfGuardHoldsAPowerActionNotStarted(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	r, _, rec := executorFor(t, "huawei-2288h-v5", append(guardedObjects(), op)...)
	r.Self = &SelfGuard{Reader: r.Client, NodeName: "talos-1"}
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Spec.OS = &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "cp-2"}}
	require.NoError(t, r.Update(t.Context(), host))
	key := client.ObjectKeyFromObject(op)

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "what it waits for wakes nothing, so it looks again")
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	assert.Empty(t, rec.powers, "nothing runs on a control plane of the operator's own cluster")
	assert.Nil(t, got.Status.Baseline)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metalv1alpha1.ReasonSelfDisruption, ex.Reason)
	assert.Contains(t, ex.Message, metalv1alpha1.AnnotationAllowSelfDisruption)
	assert.Equal(t, metalv1alpha1.OperationProposed, got.Status.Phase)
	assert.Equal(t, metalv1alpha1.ReasonSelfDisruption, got.Status.BlockedOn)

	// A person allows it: three of three are ready, so it runs.
	host.Annotations = map[string]string{metalv1alpha1.AnnotationAllowSelfDisruption: "true"}
	require.NoError(t, r.Update(t.Context(), host))
	got = runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
}

func TestSelfGuardStopsReinstallAndPowerOff(t *testing.T) {
	claim := testClaim("cp-2")
	claim.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "cp-2", Operation: "pk-metal/boot", At: metav1.Now()}
	r := claimReconcilerFor(t, append(guardedObjects(), boundHost(claim))...)
	r.Self = &SelfGuard{Reader: r.Client, NodeName: "talos-1"}
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Spec.OS.NodeRef.Name = "cp-2"
	require.NoError(t, r.Update(t.Context(), host))

	done := releasePass(t, r, claim)
	assert.False(t, done, "the machine stays bound")
	assert.Empty(t, releaseOps(t, r, claim), "no power-off is proposed")
	c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonSelfDisruption, c.Reason)
	assert.Contains(t, c.Message, "kubectl annotate host host-a")
	assert.True(t, hostBound(t, r))

	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Annotations[metalv1alpha1.AnnotationAllowSelfDisruption] = ""
	require.NoError(t, r.Update(t.Context(), host))
	releasePass(t, r, claim)
	assert.Len(t, releaseOps(t, r, claim), 1, "allowed, with a majority left: the power-off is proposed")
}
