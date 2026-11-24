// The pivot suite: two API servers. The operator provisions a machine on the
// first (the bootstrap cluster), then moves to the second, which starts
// without the operator's CRDs. The move installs the operator there, copies
// its state with every claim UID rewritten, and the manager that starts there
// proposes nothing new: the handed-off machine is not installed again. On the
// new cluster the manager runs on the machine it provisioned, so deleting
// that machine's claim is held until a person allows it.
package envtest_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/pivot"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// startBareEnv starts a control plane without the operator's CRDs: a cluster
// the operator has not been installed on yet.
func startBareEnv(t *testing.T) *suite {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run via `mise run envtest`")
	}
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })
	s, err := pkscheme.New()
	require.NoError(t, err)
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	return &suite{env: env, cfg: cfg, scheme: s, cl: cl, reads: &readLog{seen: map[string]int{}}}
}

// testLog passes the move's step lines to the test log.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// startManager runs mgr until the returned stop is called, and stop waits
// until the manager has returned: no pass of it runs after.
func startManager(t *testing.T, mgr interface{ Start(context.Context) error }) (stop func()) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestPivot(t *testing.T) {
	src := startEnv(t)
	dst := startBareEnv(t)
	ctx := t.Context()
	const ns, claimNS = "pk-metal", "lab"

	fix, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	bmcSrv, bmcState := simServer(t, "supermicro-x12")
	factory := fakeFactory(t)
	led := writeLedger(t, fmt.Sprintf("%q: %s\n", fix.Manager.MAC, bmcSrv.URL))
	imageFactory := render.NewFactory(strings.TrimPrefix(factory.URL, "https://"))
	imageFactory.HTTPClient = factory.Client()

	// --- the bootstrap cluster, as the installer leaves it ----------------

	cl := src.cl
	managerCfg := src.installManagerRBAC(t, ctx, ns)
	managerCl, err := client.New(managerCfg, client.Options{Scheme: src.scheme})
	require.NoError(t, err)
	operator := whoAmI(t, ctx, managerCl)
	installApprovalPolicy(t, ctx, cl, operator)
	installClaimPolicy(t, ctx, cl)
	require.NoError(t, cl.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "pk-metal-manager", Namespace: ns}}))
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "pk-metal-manager", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "pk-metal"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "pk-metal"}},
				Spec: corev1.PodSpec{ServiceAccountName: "pk-metal-manager", Containers: []corev1.Container{{
					Name: "manager", Image: "registry.example/pk-metal:v1",
					Args: []string{"--leader-elect", "--media-url=https://192.0.2.1:30443"},
				}}},
			},
		},
	}
	require.NoError(t, cl.Create(ctx, deploy))
	ensureNamespace(t, ctx, cl, claimNS)
	require.NoError(t, cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("dev")},
	}))
	aliceCl := src.approverClient(t, ctx, "alice")
	carolCl := src.userClient(t, "carol", "pk-metal:admins")

	managerOptions := func(media *asset.Service, nodeName string) controller.Options {
		return controller.Options{
			CredentialsNamespace: ns,
			OperationsNamespace:  ns,
			Proposer:             operator,
			Ledger:               led,
			TLS:                  &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true},
			PollInterval:         pollInterval,
			Media:                media,
			WaitInterval:         200 * time.Millisecond,
			Factory:              imageFactory,
			NodeName:             nodeName,
		}
	}
	newMedia := func() *asset.Service {
		cert, err := asset.EphemeralCert()
		require.NoError(t, err)
		addr := freeAddr(t)
		media, err := asset.NewService(addr, "https://"+addr, cert)
		require.NoError(t, err)
		return media
	}
	lanes := []controller.Lane{controller.LaneAsset, controller.LaneEndpoint, controller.LaneHost, controller.LaneHostClaim,
		controller.LaneProtection, controller.LaneHostOperation, controller.LaneBootAsset}

	srcMgr := src.newManagerAs(t, managerCfg, ns)
	require.NoError(t, controller.SetupAll(srcMgr, managerOptions(newMedia(), ""), lanes...))
	stopSource := startManager(t, srcMgr)

	// --- provision one machine there --------------------------------------

	require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mgmt"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.0", KubernetesVersion: "v1.36.2",
			Endpoint:   "https://192.0.2.10:6443",
			SecretsRef: osv1alpha1.SecretReference{Namespace: ns, Name: "mgmt-secrets"}},
	}))
	cfgBytes := []byte("version: v1alpha1\nmachine:\n  type: worker # stand-in\n")
	require.NoError(t, cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1-render", Namespace: ns},
		Data:       map[string][]byte{asset.ConfigFilename: cfgBytes},
	}))
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "mgmt"}, Role: osv1alpha1.RoleWorker,
			Address: "192.0.2.11", InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "sim-disk"}},
	}
	require.NoError(t, cl.Create(ctx, node))
	node.Status.ConfigSecretRef = &osv1alpha1.SecretReference{Namespace: ns, Name: "worker-1-render"}
	node.Status.ConfigDigest = digest.FromBytes(cfgBytes).String()
	node.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionRendered, Status: metav1.ConditionTrue,
		Reason: osv1alpha1.ReasonConfigRendered, LastTransitionTime: metav1.Now()}}
	require.NoError(t, cl.Status().Update(ctx, node))

	require.NoError(t, cl.Create(ctx, &metalv1alpha1.Asset{
		ObjectMeta: metav1.ObjectMeta{Name: "rack1-u1"},
		Spec: metalv1alpha1.AssetSpec{CID: "CI-rack1-u1", Serial: fix.Chassis.BoardSerial, BMCMac: fix.Manager.MAC,
			CredentialsRef: corev1.LocalObjectReference{Name: "bmc-creds"}},
	}))
	var hostName string
	require.Eventually(t, func() bool {
		hosts := &metalv1alpha1.HostList{}
		if cl.List(ctx, hosts) != nil || len(hosts.Items) != 1 {
			return false
		}
		hostName = hosts.Items[0].Name
		return hosts.Items[0].Status.Phase == metalv1alpha1.HostAvailable
	}, waitFor, tick, "no Available host")

	// The VAP must be enforcing before the claim relies on it.
	require.Eventually(t, func() bool {
		err := cl.Create(ctx, &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "vap-probe", Namespace: ns},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef: metalv1alpha1.Reference{Name: "no-such-host"}, ProposedBy: operator,
				Actions:  []metalv1alpha1.HostOperationAction{{PowerOff: &metalv1alpha1.PowerOffAction{}}},
				Approval: metalv1alpha1.OperationApproval{ApprovedBy: "someone"},
			},
		})
		if err == nil {
			require.NoError(t, cl.Delete(ctx, &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Name: "vap-probe", Namespace: ns}}))
			return false
		}
		return strings.Contains(err.Error(), "pre-approved")
	}, waitFor, tick, "the ValidatingAdmissionPolicy never became enforcing")

	require.NoError(t, carolCl.Create(ctx, &metalv1alpha1.HostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: claimNS},
		Spec: metalv1alpha1.HostClaimSpec{
			AssetRef: metalv1alpha1.Reference{Name: "rack1-u1"},
			OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "worker-1"}},
		},
	}))
	opsOf := func(c client.Client) []metalv1alpha1.HostOperation {
		ops := &metalv1alpha1.HostOperationList{}
		require.NoError(t, c.List(ctx, ops))
		return ops.Items
	}
	require.Eventually(t, func() bool { return len(opsOf(cl)) == 1 }, waitFor, tick, "no boot operation proposed")
	boot := opsOf(cl)[0]
	require.NoError(t, aliceCl.Patch(ctx, &boot, client.RawPatch(types.MergePatchType,
		[]byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))))
	require.Eventually(t, func() bool {
		op := &metalv1alpha1.HostOperation{}
		if cl.Get(ctx, client.ObjectKeyFromObject(&boot), op) != nil {
			return false
		}
		ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		return ex != nil && ex.Reason == metalv1alpha1.ReasonExecuting && strings.Contains(ex.Message, "awaitCheckIn")
	}, waitFor, tick, "the boot operation never waited for the check-in")
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n := &osv1alpha1.TalosNode{}
		if err := cl.Get(ctx, client.ObjectKey{Name: "worker-1"}, n); err != nil {
			return err
		}
		for _, c := range []metav1.Condition{
			{Type: osv1alpha1.ConditionMachineReachable, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonMachineAPI},
			{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady},
		} {
			meta.SetStatusCondition(&n.Status.Conditions, c)
		}
		return cl.Status().Update(ctx, n)
	}))
	handedOff := func(c client.Client) func() bool {
		return func() bool {
			claim := &metalv1alpha1.HostClaim{}
			if c.Get(ctx, client.ObjectKey{Namespace: claimNS, Name: "worker-1"}, claim) != nil {
				return false
			}
			p := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
			return p != nil && p.Reason == metalv1alpha1.ReasonHandedOff && claim.Status.HandOff != nil
		}
	}
	require.Eventually(t, handedOff(cl), waitFor, tick, "the machine was never handed off")
	require.Eventually(t, func() bool {
		op := &metalv1alpha1.HostOperation{}
		if cl.Get(ctx, client.ObjectKeyFromObject(&boot), op) != nil {
			return false
		}
		return meta.IsStatusConditionTrue(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	}, waitFor, tick, "the boot operation never finished")
	resetsBefore := bmcState.Resets("1")

	// --- the move ----------------------------------------------------------

	// The target cluster runs on the machine just provisioned: its Node has
	// the TalosNode's address.
	require.NoError(t, dst.cl.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-worker-1"},
	}))
	k8sNode := &corev1.Node{}
	require.NoError(t, dst.cl.Get(ctx, client.ObjectKey{Name: "talos-worker-1"}, k8sNode))
	k8sNode.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.11"}}
	k8sNode.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	require.NoError(t, dst.cl.Status().Update(ctx, k8sNode))

	// The deployed manager runs in a pod and scaling its Deployment stops it;
	// envtest runs no pods, so the in-process manager stands in and stops
	// here, before the move pauses the Deployment.
	stopSource()

	source, err := pivot.NewCluster("bootstrap", src.cfg, src.scheme)
	require.NoError(t, err)
	target, err := pivot.NewCluster("talos", dst.cfg, dst.scheme)
	require.NoError(t, err)
	mover, err := pivot.New(ctx, source, target, pivot.Options{
		MediaURL: "https://192.0.2.11:30443",
		Timeout:  waitFor, Poll: tick, Settle: 3 * time.Second,
		Log: testLog{t},
	})
	require.NoError(t, err)

	plan, err := mover.Preflight(ctx)
	require.NoError(t, err)
	assert.False(t, plan.Resume)
	assert.False(t, plan.Prune)
	require.NotEmpty(t, plan.Reachable)
	assert.Contains(t, strings.Join(plan.Reachable, "\n"), "192.0.2.11:50000", "the Talos API of the installed machine")

	require.NoError(t, mover.Pause(ctx))
	require.NoError(t, mover.Install(ctx))
	rep, err := mover.Copy(ctx, plan)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Copied["HostClaim"])
	assert.Equal(t, 1, rep.Copied["HostOperation"])
	assert.Equal(t, []string{claimNS + "/worker-1"}, rep.HandedOff)

	srcClaim := &metalv1alpha1.HostClaim{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: claimNS, Name: "worker-1"}, srcClaim))
	dstClaim := &metalv1alpha1.HostClaim{}
	require.NoError(t, dst.cl.Get(ctx, client.ObjectKey{Namespace: claimNS, Name: "worker-1"}, dstClaim))
	srcHost := &metalv1alpha1.Host{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: hostName}, srcHost))
	dstHost := &metalv1alpha1.Host{}
	require.NoError(t, dst.cl.Get(ctx, client.ObjectKey{Name: hostName}, dstHost))

	t.Run("every claim UID reference names the target's claim", func(t *testing.T) {
		require.NotEqual(t, srcClaim.UID, dstClaim.UID)
		require.NotNil(t, dstHost.Spec.ClaimRef)
		assert.Equal(t, dstClaim.UID, dstHost.Spec.ClaimRef.UID)
		for _, op := range opsOf(dst.cl) {
			assert.Equal(t, string(dstClaim.UID), op.Labels["metal.pk.io/claim-uid"], "operation %s", op.Name)
			assert.Equal(t, "alice", op.Spec.Approval.ApprovedBy, "the approval is replayed by its approver")
			assert.Equal(t, operator, op.Spec.ProposedBy)
		}
		require.NotEmpty(t, dstHost.OwnerReferences)
		for _, ref := range dstHost.OwnerReferences {
			ep := &metalv1alpha1.ManagementEndpoint{}
			require.NoError(t, dst.cl.Get(ctx, client.ObjectKey{Name: ref.Name}, ep))
			assert.Equal(t, ep.UID, ref.UID, "the owner reference names the target's endpoint")
		}
		assert.Equal(t, srcClaim.Finalizers, dstClaim.Finalizers)
		assert.Equal(t, srcHost.Finalizers, dstHost.Finalizers)
	})

	t.Run("status is copied with the controllers' field owners", func(t *testing.T) {
		assert.Equal(t, srcHost.Status.Phase, dstHost.Status.Phase)
		require.NotNil(t, dstClaim.Status.HandOff)
		assert.Equal(t, srcClaim.Status.HandOff.ConfigDigest, dstClaim.Status.HandOff.ConfigDigest)
		var owners []string
		for _, mf := range dstHost.ManagedFields {
			if mf.Subresource == "status" {
				owners = append(owners, mf.Manager)
			}
		}
		assert.Contains(t, owners, "pk-metal-host")
		assert.NotContains(t, owners, pivot.FieldManager, "the move owns no status field")
	})

	t.Run("a second copy changes nothing", func(t *testing.T) {
		versions := func() map[string]string {
			out := map[string]string{}
			for _, list := range []client.ObjectList{&metalv1alpha1.HostList{}, &metalv1alpha1.HostClaimList{},
				&metalv1alpha1.HostOperationList{}, &metalv1alpha1.AssetList{}, &metalv1alpha1.ManagementEndpointList{},
				&osv1alpha1.TalosNodeList{}, &osv1alpha1.TalosClusterList{}} {
				require.NoError(t, dst.cl.List(ctx, list))
				require.NoError(t, meta.EachListItem(list, func(o runtime.Object) error {
					co := o.(client.Object)
					out[fmt.Sprintf("%T %s", co, client.ObjectKeyFromObject(co))] = co.GetResourceVersion()
					return nil
				}))
			}
			return out
		}
		before := versions()
		again, err := mover.Copy(ctx, plan)
		require.NoError(t, err)
		assert.Equal(t, before, versions())
		for src, uid := range rep.UIDs {
			assert.Equal(t, uid, again.UIDs[src], "the same target object")
		}
	})

	require.NoError(t, mover.Start(ctx))
	d := &appsv1.Deployment{}
	require.NoError(t, dst.cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pk-metal-manager"}, d))
	assert.Equal(t, int32(1), *d.Spec.Replicas)
	assert.Contains(t, d.Spec.Template.Spec.Containers[0].Args, "--media-url=https://192.0.2.11:30443")
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pk-metal-manager"}, d))
	assert.Equal(t, int32(0), *d.Spec.Replicas, "the source stays paused")

	// The new manager runs under the RBAC the move installed.
	u, err := dst.env.AddUser(envtest.User{
		Name:   managerUser,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNamespace},
	}, nil)
	require.NoError(t, err)
	dstMgr := dst.newManagerAs(t, u.Config(), ns)
	require.NoError(t, controller.SetupAll(dstMgr, managerOptions(newMedia(), "talos-worker-1"), lanes...))
	startManager(t, dstMgr)

	t.Run("the new manager settles without a new operation", func(t *testing.T) {
		require.NoError(t, mover.Settle(ctx, rep))
		claim := dstClaim.DeepCopy()
		dst.settle(t, ctx, claim, 3)
		require.Eventually(t, handedOff(dst.cl), waitFor, tick)
		assert.Len(t, opsOf(dst.cl), 1, "only the copied boot record")
		assert.Equal(t, resetsBefore, bmcState.Resets("1"), "the machine was not booted again")

		again, err := mover.Preflight(ctx)
		require.NoError(t, err)
		assert.True(t, again.Resume, "a rerun only verifies")
	})

	t.Run("the manager does not shut down the machine it runs on", func(t *testing.T) {
		require.NoError(t, dst.cl.Delete(ctx, dstClaim))
		require.Eventually(t, func() bool {
			claim := &metalv1alpha1.HostClaim{}
			if dst.cl.Get(ctx, client.ObjectKeyFromObject(dstClaim), claim) != nil {
				return false
			}
			p := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
			return p != nil && p.Reason == metalv1alpha1.ReasonSelfDisruption
		}, waitFor, tick, "the release is not held")
		claim := dstClaim.DeepCopy()
		dst.settle(t, ctx, claim, 3)
		ops := opsOf(dst.cl)
		assert.False(t, slices.ContainsFunc(ops, func(op metalv1alpha1.HostOperation) bool {
			return op.Labels["metal.pk.io/purpose"] == "release"
		}), "no release is proposed")

		require.NoError(t, dst.cl.Patch(ctx, &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: hostName}},
			client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+metalv1alpha1.AnnotationAllowSelfDisruption+`":"true"}}}`))))
		require.Eventually(t, func() bool {
			return slices.ContainsFunc(opsOf(dst.cl), func(op metalv1alpha1.HostOperation) bool {
				return op.Labels["metal.pk.io/purpose"] == "release"
			})
		}, waitFor, tick, "allowed, the release is proposed")
	})
}
