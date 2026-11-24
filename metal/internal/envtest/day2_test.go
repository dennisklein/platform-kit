// Day-2 operations against a real apiserver: a Talos reboot that drains its
// node through the Eviction API and waits out a PodDisruptionBudget, and the
// cluster guard serializing reboots of three control planes approved at
// once. The apiserver doubles as the workload cluster: the TalosCluster's
// published kubeconfig points back at it, so the drain runs the real
// eviction and budget checks of kube-apiserver. No BMC exists anywhere in
// this suite: every step here must run without one.
package envtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	talosfake "github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

func TestDay2Operations(t *testing.T) {
	st := startEnv(t)
	cl, ctx := st.cl, t.Context()
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pk-metal"}}))
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "apps"}}))

	// The workload cluster's admin kubeconfig, as the TalosCluster
	// reconciler would publish it: here, for this apiserver.
	admin, err := st.env.AddUser(envtest.User{Name: "workload-admin", Groups: []string{"system:masters"}}, nil)
	require.NoError(t, err)
	kubeconfig, err := admin.KubeConfig()
	require.NoError(t, err)
	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	for name, data := range map[string]map[string][]byte{
		"lab-secrets":    {render.BundleKey: bundle},
		"lab-kubeconfig": {controller.KubeconfigKey: kubeconfig},
	} {
		require.NoError(t, cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: name}, Data: data}))
	}
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: labTalos, Endpoint: "https://10.0.0.10:6443", KubernetesVersion: "v1.36.0",
			SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "lab-secrets"},
		},
	}
	require.NoError(t, cl.Create(ctx, cluster))
	cluster.Status.BootstrappedAt = ptr.To(metav1.Now())
	cluster.Status.KubeconfigSecretRef = &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "lab-kubeconfig"}
	require.NoError(t, cl.Status().Update(ctx, cluster))

	fleet := talosfake.NewFleet()
	etcd := talosfake.NewEtcdCluster()
	machines := map[string]*talosfake.Machine{}
	for _, n := range []struct {
		name, addr string
		role       osv1alpha1.NodeRole
	}{
		{"cp-1", "10.0.0.1", osv1alpha1.RoleControlPlane},
		{"cp-2", "10.0.0.2", osv1alpha1.RoleControlPlane},
		{"cp-3", "10.0.0.3", osv1alpha1.RoleControlPlane},
		{"w-1", "10.0.0.4", osv1alpha1.RoleWorker},
	} {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: n.name},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}, Role: n.role, Address: n.addr,
				InstallDiskSelector: osv1alpha1.DiskSelector{Match: "disk.size > 0"}},
		}))
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "host-" + n.name},
			Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: n.name}}},
		}))
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.name}}
		require.NoError(t, cl.Create(ctx, node))
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
			LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now(), Reason: "KubeletReady"}}
		require.NoError(t, cl.Status().Update(ctx, node))

		m := talosfake.NewMachine(labTalos)
		m.Configure([]byte("machine: {}"))
		m.SetStage("running", true)
		fleet.Add(n.addr, n.name, m)
		if n.role == osv1alpha1.RoleControlPlane {
			m.JoinEtcd(etcd, false)
		}
		machines[n.name] = m
	}

	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "pk-metal", OperationsNamespace: "pk-metal",
		TalosDial: fleet.Dialer(), WaitInterval: pollInterval, GuardInterval: pollInterval,
	}, controller.LaneHostOperation))
	runManager(t, ctx, mgr)

	op := func(name, node string, actions ...metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
		return &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pk-metal"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef: metalv1alpha1.Reference{Name: "host-" + node}, ProposedBy: "alice",
				Approval: metalv1alpha1.OperationApproval{Required: ptr.To(false)},
				Actions:  actions,
			},
		}
	}
	get := func(name string) *metalv1alpha1.HostOperation {
		o := &metalv1alpha1.HostOperation{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: "pk-metal", Name: name}, o))
		return o
	}
	executed := func(o *metalv1alpha1.HostOperation) metav1.Condition {
		if c := meta.FindStatusCondition(o.Status.Conditions, metalv1alpha1.ConditionExecuted); c != nil {
			return *c
		}
		return metav1.Condition{}
	}
	unschedulable := func(node string) bool {
		n := &corev1.Node{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: node}, n))
		return n.Spec.Unschedulable
	}
	ref := func(n string) metalv1alpha1.Reference { return metalv1alpha1.Reference{Name: n} }
	reboot := func(node string) []metalv1alpha1.HostOperationAction {
		return []metalv1alpha1.HostOperationAction{
			{Cordon: &metalv1alpha1.CordonAction{NodeRef: ref(node)}},
			// No kubelet runs here to stop a pod, so evicted pods go at once.
			{Drain: &metalv1alpha1.DrainAction{NodeRef: ref(node), TimeoutSeconds: 600, GracePeriodSeconds: ptr.To[int32](0)}},
			{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref(node)}},
			{WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{NodeRef: ref(node)}},
			{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: ref(node)}},
		}
	}

	t.Run("a drain waits out a disruption budget", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps", Labels: map[string]string{"app": "web"}},
			Spec: corev1.PodSpec{NodeName: "w-1", TerminationGracePeriodSeconds: ptr.To[int64](0),
				Containers: []corev1.Container{{Name: "web", Image: "registry.invalid/web:1"}}},
		}
		require.NoError(t, cl.Create(ctx, pod))
		// A running, ready pod: the only kind a budget protects.
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		require.NoError(t, cl.Status().Update(ctx, pod))
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"},
			Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: ptr.To(intstr.FromInt32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
		}
		require.NoError(t, cl.Create(ctx, pdb))
		// No disruption controller runs here; its verdict is written by hand.
		budget := func(allowed int32) {
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(pdb), pdb))
			pdb.Status = policyv1.PodDisruptionBudgetStatus{ObservedGeneration: pdb.Generation,
				DisruptionsAllowed: allowed, CurrentHealthy: 1, DesiredHealthy: 1 - allowed, ExpectedPods: 1}
			require.NoError(t, cl.Status().Update(ctx, pdb))
		}
		budget(0)

		require.NoError(t, cl.Create(ctx, op("reboot-w-1", "w-1", reboot("w-1")...)))
		require.Eventually(t, func() bool {
			return unschedulable("w-1") && strings.Contains(executed(get("reboot-w-1")).Message, "eviction refused for apps/web-1")
		}, waitFor, tick, "the drain waits on the budget: %+v", executed(get("reboot-w-1")))
		assert.Contains(t, executed(get("reboot-w-1")).Message, "disruption budget")
		assert.Empty(t, machines["w-1"].Reboots(), "no reboot while the budget holds")

		budget(1)
		require.Eventually(t, func() bool {
			return meta.IsStatusConditionTrue(get("reboot-w-1").Status.Conditions, metalv1alpha1.ConditionExecuted)
		}, waitFor, tick, "%+v", executed(get("reboot-w-1")))
		assert.Len(t, machines["w-1"].Reboots(), 1)
		assert.False(t, unschedulable("w-1"))
		assert.Equal(t, metalv1alpha1.OperationSucceeded, get("reboot-w-1").Status.Phase)
		err := cl.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
		assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "the pod was evicted")
	})

	t.Run("control planes are rebooted one at a time", func(t *testing.T) {
		// Each machine comes back from its reboot only when the test says
		// so, which holds an operation in the middle.
		for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
			machines[n].SetStage("booting", false)
		}
		for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
			require.NoError(t, cl.Create(ctx, op("reboot-"+n, n,
				metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref(n)}},
				metalv1alpha1.HostOperationAction{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{NodeRef: ref(n)}})))
		}
		started := func() (running []string, held []string) {
			for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
				o := get("reboot-" + n)
				switch {
				case o.Status.Baseline != nil:
					running = append(running, n)
				case executed(o).Reason == metalv1alpha1.ReasonClusterDisruption:
					held = append(held, n)
				}
			}
			return running, held
		}
		require.Eventually(t, func() bool {
			running, held := started()
			return len(running) == 1 && len(held) == 2
		}, waitFor, tick)
		running, held := started()
		o := get("reboot-" + held[0])
		assert.Contains(t, executed(o).Message, "operation pk-metal/reboot-"+running[0]+" is taking down control plane "+running[0])
		assert.Equal(t, metalv1alpha1.ReasonClusterDisruption, o.Status.BlockedOn)
		assert.Equal(t, metalv1alpha1.OperationProposed, o.Status.Phase)

		// Hold the line for a few passes: still one.
		st.settle(t, ctx, get("reboot-"+held[1]), 3)
		running, _ = started()
		require.Len(t, running, 1)

		for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
			machines[n].SetStage("running", true)
		}
		require.Eventually(t, func() bool {
			for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
				if !meta.IsStatusConditionTrue(get("reboot-"+n).Status.Conditions, metalv1alpha1.ConditionExecuted) {
					return false
				}
			}
			return true
		}, waitFor, tick)
		for _, n := range []string{"cp-1", "cp-2", "cp-3"} {
			assert.Len(t, machines[n].Reboots(), 1, n)
		}
	})

	t.Run("an abort hands the cordon back", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "apps", Labels: map[string]string{"app": "web"}},
			Spec: corev1.PodSpec{NodeName: "w-1", TerminationGracePeriodSeconds: ptr.To[int64](0),
				Containers: []corev1.Container{{Name: "web", Image: "registry.invalid/web:1"}}},
		}
		require.NoError(t, cl.Create(ctx, pod))
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		require.NoError(t, cl.Status().Update(ctx, pod))
		pdb := &policyv1.PodDisruptionBudget{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: "apps", Name: "web"}, pdb))
		pdb.Status.DisruptionsAllowed = 0
		pdb.Status.ObservedGeneration = pdb.Generation
		require.NoError(t, cl.Status().Update(ctx, pdb))

		require.NoError(t, cl.Create(ctx, op("abort-w-1", "w-1", reboot("w-1")...)))
		require.Eventually(t, func() bool { return get("abort-w-1").Status.CordonedNode == "w-1" }, waitFor, tick)

		// The executor writes the operation while it waits on the drain, so the
		// update retries on a conflict instead of racing it.
		require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			o := get("abort-w-1")
			o.Annotations = map[string]string{metalv1alpha1.AnnotationAbort: "alice: not today"}
			return cl.Update(ctx, o)
		}))
		require.Eventually(t, func() bool { return get("abort-w-1").Status.Phase == metalv1alpha1.OperationAborted }, waitFor, tick)
		ex := executed(get("abort-w-1"))
		assert.Contains(t, ex.Message, "alice: not today")
		assert.Contains(t, ex.Message, "made Node w-1 schedulable again")
		assert.False(t, unschedulable("w-1"))
		assert.Len(t, machines["w-1"].Reboots(), 1, "only the first operation's reboot")
	})
}
