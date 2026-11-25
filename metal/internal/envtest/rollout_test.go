// The rollout suite: a TalosUpgrade proposed for a four-node cluster runs
// under one approval, in order (control planes one at a time with the etcd
// leader last, then the workers), pauses after its first node, waits on the
// live health gate, and ends; a second rollout is aborted midway, a third is
// approved for a plan it does not have; and the executor refuses an
// operation the rollout it names does not cover. The test plays the
// executor for the rollout's own operations: what an operation does to a
// machine is the executor suite's subject, the order and the gates are this
// one's.
package envtest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/k8supgrade"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// stubWorkload stands in for the workload cluster's Kubernetes API.
type stubWorkload struct {
	mu       sync.Mutex
	notReady map[string]bool
	proxy    []string
}

func (w *stubWorkload) setReady(node string, ready bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notReady[node] = !ready
}

func (w *stubWorkload) NodesReady(context.Context) (map[string]bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]bool{}
	for _, n := range []string{"cp-1", "cp-2", "w-1", "w-2"} {
		out[n] = !w.notReady[n]
	}
	return out, nil
}

func (w *stubWorkload) SyncKubeProxy(_ context.Context, image string) (bool, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.proxy = append(w.proxy, image)
	return true, "kube-proxy: done", nil
}

// installRolloutPolicy loads the committed rollout approval policy; its
// params are the ones installApprovalPolicy created.
func installRolloutPolicy(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	docs := strings.Split(readFile(t, filepath.Join("..", "..", "config", "policy", "talosrollout-approval.yaml")), "\n---")
	require.Len(t, docs, 2)
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), vap))
	require.NoError(t, cl.Create(ctx, vap))
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), binding))
	require.NoError(t, cl.Create(ctx, binding))
}

func TestRollout(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()
	operator := whoAmI(t, ctx, cl)
	installApprovalPolicy(t, ctx, cl, operator)
	installRolloutPolicy(t, ctx, cl)
	aliceCl := st.userClient(t, "alice")
	bobCl := st.userClient(t, "bob")

	// Four machines on Talos v1.14.5; the two control planes share one etcd,
	// cp-1 leads it.
	fleet := fake.NewFleet()
	etcd := fake.NewEtcdCluster()
	machinesByName := map[string]*fake.Machine{}
	addrs := map[string]string{"cp-1": "10.88.0.11", "cp-2": "10.88.0.12", "w-1": "10.88.0.21", "w-2": "10.88.0.22"}
	for name, addr := range addrs {
		m := fake.NewMachine("v1.14.5")
		m.Configure([]byte("running config"))
		m.SetStage("running", true)
		fleet.Add(addr, name, m)
		machinesByName[name] = m
	}
	machinesByName["cp-1"].JoinEtcd(etcd, false)
	machinesByName["cp-2"].JoinEtcd(etcd, false)
	workload := &stubWorkload{notReady: map[string]bool{}}

	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	require.NoError(t, cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-secrets", Namespace: "default"},
		Data:       map[string][]byte{render.BundleKey: bundle},
	}))
	require.NoError(t, cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-kubeconfig", Namespace: "default"},
		Data:       map[string][]byte{controller.KubeconfigKey: []byte("stub")},
	}))
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: labTalos, KubernetesVersion: "v1.36.2", Endpoint: "https://10.88.0.10:6443",
			SecretsRef: osv1alpha1.SecretReference{Namespace: "default", Name: "fleet-secrets"},
		},
	}
	require.NoError(t, cl.Create(ctx, cluster))
	cluster.Status.BootstrappedAt = ptr.To(metav1.Now())
	cluster.Status.KubeconfigSecretRef = &osv1alpha1.SecretReference{Namespace: "default", Name: "fleet-kubeconfig"}
	require.NoError(t, cl.Status().Update(ctx, cluster))
	for _, name := range []string{"cp-1", "cp-2", "w-1", "w-2"} {
		role := osv1alpha1.RoleWorker
		if strings.HasPrefix(name, "cp-") {
			role = osv1alpha1.RoleControlPlane
		}
		n := &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{
				ClusterRef: osv1alpha1.Reference{Name: "fleet"}, Role: role, Address: addrs[name],
				InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "disk-" + name},
			},
		}
		require.NoError(t, cl.Create(ctx, n))
		n.Status.Version = "v1.14.5"
		require.NoError(t, cl.Status().Update(ctx, n))
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "box-" + name},
			Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: name}}},
		}))
	}

	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		OperationsNamespace:  "default",
		Proposer:             operator,
		Factory:              render.NewFactory("factory.test"),
		ResolveImage:         pinImage,
		TalosDial:            fleet.Dialer(),
		Workload:             func([]byte) (k8supgrade.Workload, error) { return workload, nil },
		GateInterval:         100 * time.Millisecond,
	}, controller.LaneTalosRollout))
	runManager(t, ctx, mgr)

	getRollout := func(name string) *osv1alpha1.TalosRollout {
		ro := &osv1alpha1.TalosRollout{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: name}, ro))
		return ro
	}
	opsOf := func(rollout string) []metalv1alpha1.HostOperation {
		list := metalv1alpha1.HostOperationList{}
		require.NoError(t, cl.List(ctx, &list, client.MatchingLabels{osv1alpha1.LabelRollout: rollout}))
		return list.Items
	}
	opFor := func(rollout, node string) *metalv1alpha1.HostOperation {
		for _, op := range opsOf(rollout) {
			if op.Spec.HostRef.Name == "box-"+node {
				return &op
			}
		}
		return nil
	}
	// finish plays the executor: the operation of node ends as ok says.
	finish := func(rollout, node string, ok bool) {
		t.Helper()
		op := opFor(rollout, node)
		require.NotNil(t, op, "no operation for %s", node)
		status := metav1.ConditionTrue
		if !ok {
			status = metav1.ConditionFalse
		}
		meta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
			Type: metalv1alpha1.ConditionExecuted, Status: status, Reason: "Test", Message: "played by the test",
		})
		require.NoError(t, cl.Status().Update(ctx, op))
	}
	condition := func(name, t string) *metav1.Condition {
		return meta.FindStatusCondition(getRollout(name).Status.Conditions, t)
	}
	nodesWithOps := func(rollout string) []string {
		var out []string
		for _, op := range opsOf(rollout) {
			out = append(out, strings.TrimPrefix(op.Spec.HostRef.Name, "box-"))
		}
		return out
	}

	var upgrade string
	st.run(t, "skew proposes a TalosUpgrade with a frozen-to-be, ordered plan", func(t *testing.T) {
		require.Eventually(t, func() bool {
			list := osv1alpha1.TalosRolloutList{}
			require.NoError(t, cl.List(ctx, &list))
			for _, ro := range list.Items {
				if ro.Spec.Kind == osv1alpha1.RolloutTalosUpgrade &&
					meta.IsStatusConditionTrue(ro.Status.Conditions, osv1alpha1.ConditionPlanned) {
					upgrade = ro.Name
					return true
				}
			}
			return false
		}, waitFor, tick, "the skew must propose an upgrade")
		ro := getRollout(upgrade)
		var order []string
		for _, s := range ro.Status.Plan {
			order = append(order, s.Node)
			assert.Regexp(t, `factory\.test/metal-installer/[0-9a-f]{64}:v1\.14\.6@sha256:[0-9a-f]{64}$`, s.Image)
		}
		assert.Equal(t, []string{"cp-2", "cp-1", "w-1", "w-2"}, order,
			"control planes first with the etcd leader last, then the workers")
		assert.Equal(t, []string{"drain", "talosUpgrade", "waitNodeReady", "waitEtcdHealthy", "uncordon"}, ro.Status.Plan[0].Actions)
		assert.Equal(t, []string{"drain", "talosUpgrade", "waitNodeReady", "uncordon"}, ro.Status.Plan[2].Actions)
		assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, ro.Status.PlanHash)
		assert.Empty(t, opsOf(upgrade), "nothing runs before an approval")
	})

	st.run(t, "the operator approves nothing, and a person only as themselves", func(t *testing.T) {
		ro := getRollout(upgrade)
		approve := func(c client.Client, who, hash string) error {
			patch := client.MergeFrom(ro.DeepCopy())
			cp := ro.DeepCopy()
			cp.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: who, PlanHash: hash}
			return c.Patch(ctx, cp, patch)
		}
		assert.ErrorContains(t, approve(cl, operator, ro.Status.PlanHash), "the operator approves nothing")
		assert.ErrorContains(t, approve(aliceCl, "bob", ro.Status.PlanHash), "only approve as yourself")
		assert.Error(t, approve(bobCl, "bob", ""), "an approval names the plan it approves")
		require.NoError(t, approve(bobCl, "bob", ro.Status.PlanHash))
		ro = getRollout(upgrade)
		assert.ErrorContains(t, approve(aliceCl, "alice", ro.Status.PlanHash), "write-once")
	})

	st.run(t, "the first control plane goes alone, and the rollout pauses after it", func(t *testing.T) {
		require.Eventually(t, func() bool { return len(opsOf(upgrade)) == 1 }, waitFor, tick)
		op := opFor(upgrade, "cp-2")
		require.NotNil(t, op, "the first step is cp-2")
		ro := getRollout(upgrade)
		assert.Equal(t, &metalv1alpha1.RolloutReference{Name: upgrade, PlanHash: ro.Status.PlanHash}, op.Spec.RolloutRef)
		require.NotNil(t, op.Spec.Approval.Required)
		assert.False(t, *op.Spec.Approval.Required, "the rollout's approval covers it")
		assert.Equal(t, operator, op.Spec.ProposedBy)
		require.NotNil(t, op.Spec.Actions[1].TalosUpgrade)
		assert.Equal(t, ro.Status.Plan[0].Image, op.Spec.Actions[1].TalosUpgrade.Image, "the approved image, pinned")

		finish(upgrade, "cp-2", true)
		require.Eventually(t, func() bool {
			c := condition(upgrade, osv1alpha1.ConditionPaused)
			return c != nil && c.Status == metav1.ConditionTrue && c.Reason == osv1alpha1.ReasonCanaryPause
		}, waitFor, tick, "the rollout must pause after its first node")
		cur := getRollout(upgrade)
		st.settle(t, ctx, cur, 5)
		assert.Equal(t, []string{"cp-2"}, nodesWithOps(upgrade), "nothing else starts while paused")
	})

	st.run(t, "a resume waits on the live health gate, then the next control plane goes", func(t *testing.T) {
		workload.setReady("w-2", false)
		ro := getRollout(upgrade)
		ro.Annotations = map[string]string{osv1alpha1.AnnotationRolloutResume: ro.Status.PlanHash}
		require.NoError(t, bobCl.Update(ctx, ro))
		require.Eventually(t, func() bool {
			c := condition(upgrade, osv1alpha1.ConditionProgressing)
			return c != nil && c.Reason == osv1alpha1.ReasonWaitingForHealth && strings.Contains(c.Message, "w-2")
		}, waitFor, tick, "a node that is not Ready, read now, holds the next step")
		assert.Equal(t, []string{"cp-2"}, nodesWithOps(upgrade))

		workload.setReady("w-2", true)
		require.Eventually(t, func() bool { return opFor(upgrade, "cp-1") != nil }, waitFor, tick)
		cur := getRollout(upgrade)
		st.settle(t, ctx, cur, 3)
		assert.Len(t, opsOf(upgrade), 2, "control planes go one at a time")
	})

	st.run(t, "an unhealthy etcd holds the next step too", func(t *testing.T) {
		machinesByName["cp-2"].SetEtcdErrors("NOSPACE")
		finish(upgrade, "cp-1", true)
		require.Eventually(t, func() bool {
			c := condition(upgrade, osv1alpha1.ConditionProgressing)
			return c != nil && c.Reason == osv1alpha1.ReasonWaitingForHealth && strings.Contains(c.Message, "etcd")
		}, waitFor, tick)
		assert.Len(t, opsOf(upgrade), 2)
		machinesByName["cp-2"].SetEtcdErrors()
	})

	st.run(t, "workers go at most maxUnavailableWorkers at a time, and the rollout completes", func(t *testing.T) {
		require.Eventually(t, func() bool { return opFor(upgrade, "w-1") != nil }, waitFor, tick)
		cur := getRollout(upgrade)
		st.settle(t, ctx, cur, 3)
		assert.Nil(t, opFor(upgrade, "w-2"), "one worker at a time by default")
		finish(upgrade, "w-1", true)
		require.Eventually(t, func() bool { return opFor(upgrade, "w-2") != nil }, waitFor, tick)
		finish(upgrade, "w-2", true)
		require.Eventually(t, func() bool {
			c := condition(upgrade, osv1alpha1.ConditionComplete)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick)
	})

	newRollout := func(name string, nodes ...string) *osv1alpha1.TalosRollout {
		ro := &osv1alpha1.TalosRollout{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosRolloutSpec{
				ClusterRef: osv1alpha1.Reference{Name: "fleet"}, Kind: osv1alpha1.RolloutReboot,
				Nodes: nodes, ProposedBy: "alice",
			},
		}
		require.NoError(t, aliceCl.Create(ctx, ro))
		require.Eventually(t, func() bool {
			c := condition(name, osv1alpha1.ConditionPlanned)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick)
		return getRollout(name)
	}
	approveAs := func(c client.Client, who, name, hash string) {
		ro := getRollout(name)
		patch := client.MergeFrom(ro.DeepCopy())
		ro.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: who, PlanHash: hash}
		require.NoError(t, c.Patch(ctx, ro, patch))
	}

	st.run(t, "an abort starts nothing more and lets the running node finish", func(t *testing.T) {
		ro := newRollout("reboot-workers", "w-1", "w-2")
		approveAs(bobCl, "bob", ro.Name, ro.Status.PlanHash)
		require.Eventually(t, func() bool { return len(opsOf(ro.Name)) == 1 }, waitFor, tick)

		ro = getRollout(ro.Name)
		patch := client.MergeFrom(ro.DeepCopy())
		ro.Spec.Abort = true
		require.NoError(t, aliceCl.Patch(ctx, ro, patch))
		require.Eventually(t, func() bool {
			c := condition(ro.Name, osv1alpha1.ConditionProgressing)
			return c != nil && c.Reason == osv1alpha1.ReasonRolloutAborted
		}, waitFor, tick)
		cur := getRollout(ro.Name)
		assert.Equal(t, osv1alpha1.RolloutNodeSkipped, cur.Status.Steps[1].State)

		ro = getRollout(ro.Name)
		patch = client.MergeFrom(ro.DeepCopy())
		ro.Spec.Abort = false
		assert.ErrorContains(t, aliceCl.Patch(ctx, ro, patch), "cannot be taken back")

		finish(ro.Name, "w-1", true)
		require.Eventually(t, func() bool {
			c := condition(ro.Name, osv1alpha1.ConditionComplete)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonRolloutAborted
		}, waitFor, tick)
		assert.Len(t, opsOf(ro.Name), 1)
	})

	st.run(t, "an approval for another plan runs nothing", func(t *testing.T) {
		ro := newRollout("reboot-mismatch", "w-1")
		approveAs(bobCl, "bob", ro.Name, "sha256:"+strings.Repeat("0", 64))
		require.Eventually(t, func() bool {
			c := condition(ro.Name, osv1alpha1.ConditionComplete)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonPlanChanged
		}, waitFor, tick)
		cur := getRollout(ro.Name)
		st.settle(t, ctx, cur, 3)
		assert.Empty(t, opsOf(ro.Name))
	})

	st.run(t, "the executor refuses an operation the rollout does not cover", func(t *testing.T) {
		exec := st.newManager(t)
		require.NoError(t, controller.SetupAll(exec, controller.Options{
			CredentialsNamespace: "default", OperationsNamespace: "default", Proposer: operator,
			WaitInterval: 100 * time.Millisecond,
		}, controller.LaneHostOperation))
		runManager(t, ctx, exec)

		real := opFor(upgrade, "cp-2")
		require.NotNil(t, real)
		forge := func(name string, mutate func(*metalv1alpha1.HostOperationSpec)) {
			spec := *real.Spec.DeepCopy()
			spec.ProposedBy = operator
			mutate(&spec)
			require.NoError(t, cl.Create(ctx, &metalv1alpha1.HostOperation{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec,
			}))
		}
		executed := func(name string) *metav1.Condition {
			op := &metalv1alpha1.HostOperation{}
			if cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, op) != nil {
				return nil
			}
			return meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		}
		refused := func(name, why string) {
			t.Helper()
			require.Eventually(t, func() bool {
				c := executed(name)
				return c != nil && c.Status == metav1.ConditionFalse && c.Reason == metalv1alpha1.ReasonRolloutRefused
			}, waitFor, tick, name)
			assert.Contains(t, executed(name).Message, why)
		}

		forge("forged-hash", func(s *metalv1alpha1.HostOperationSpec) {
			s.RolloutRef = &metalv1alpha1.RolloutReference{Name: upgrade, PlanHash: "sha256:" + strings.Repeat("1", 64)}
		})
		refused("forged-hash", "plan")
		forge("forged-host", func(s *metalv1alpha1.HostOperationSpec) { s.HostRef.Name = "box-w-1" })
		refused("forged-host", "targets host box-w-1")
		forge("forged-image", func(s *metalv1alpha1.HostOperationSpec) {
			s.Actions[1].TalosUpgrade.Image = "evil.test/installer:v1.14.6@sha256:" + strings.Repeat("2", 64)
		})
		refused("forged-image", "differ from the ones the approved plan")
		forge("forged-gone", func(s *metalv1alpha1.HostOperationSpec) {
			s.RolloutRef = &metalv1alpha1.RolloutReference{Name: "no-such-rollout", PlanHash: s.RolloutRef.PlanHash}
		})
		refused("forged-gone", "does not exist")
		forge("unapproved", func(s *metalv1alpha1.HostOperationSpec) {
			s.RolloutRef = &metalv1alpha1.RolloutReference{Name: "reboot-mismatch", PlanHash: getRollout("reboot-mismatch").Status.PlanHash}
		})
		refused("unapproved", "does not cover")

		// An exact copy is covered: the rollout gate lets it through to
		// the rest of the executor.
		forge("covered", func(*metalv1alpha1.HostOperationSpec) {})
		require.Eventually(t, func() bool {
			op := &metalv1alpha1.HostOperation{}
			if cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "covered"}, op) != nil {
				return false
			}
			c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionApproved)
			return c != nil && strings.Contains(c.Message, "covered by the approval of rollout "+upgrade)
		}, waitFor, tick)
		if c := executed("covered"); c != nil {
			assert.NotEqual(t, metalv1alpha1.ReasonRolloutRefused, c.Reason)
		}
	})
}
