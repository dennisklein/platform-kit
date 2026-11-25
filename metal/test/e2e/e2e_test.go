//go:build e2e

// The kind tier — the plan's "fake the BMC, boot a real machine" trick, end to
// end: the kind cluster stores the CRs, the managers run in-process against it,
// the simulated BMC serves on the host with a REAL Talos container attached as
// its machine, and the provisioning ladder runs exactly as it would on metal —
// claim → power-on operation → blank machine in maintenance mode → bootstrap
// verb → check-in → Provisioned. The stated fidelity boundary: a container has
// no media channel, so the test itself applies the config over the
// unauthenticated maintenance API (neither the operator nor the CLI has that
// path); the config rides the BMC's virtual media only from the QEMU tier up.
//
// Needs docker and a kind cluster named pk-metal; it only stores objects there,
// so kind's default node is enough (CI: `kind create cluster --name pk-metal`).
// Run via `mise run e2e:kind`.
package e2e_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	dockermachine "github.com/dennisklein/platform-kit/metal/internal/machine/docker"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

const (
	ns          = "pk-e2e"
	network     = "pk-metal-e2e"
	subnet      = "172.28.100.0/24"
	machineIP   = "172.28.100.10"
	machineName = "pk-e2e-cp1"
	nodeName    = "e2e-cp1"
	assetName   = "pk-e2e-smc"
	hostName    = "simx12b000001" // the supermicro-x12 fixture's board serial, normalized
	clusterName = "pk-e2e"

	waitFor = 3 * time.Minute
	tick    = 500 * time.Millisecond
	// bootWait bounds the container's blank boot to maintenance mode (the spike
	// measured ~20s) plus the check-in noticing.
	bootWait = 2 * time.Minute
)

// kindFactory is the public image factory. This tier's node carries no
// schematic, so what gets registered is the vanilla one — but it registers it,
// because the tier's job is to run the reconcilers as they run in production.
var kindFactory = render.NewFactory("")

func TestE2EKind(t *testing.T) {
	ctx := t.Context()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// Safety rail: this test installs CRDs and runs managers — kind only.
	raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(raw.CurrentContext, "kind-"),
		"current kube-context %q is not a kind cluster — `mise run lab:up` and switch to kind-pk-metal", raw.CurrentContext)
	cfg, err := ctrl.GetConfig()
	require.NoError(t, err)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker not on PATH — the machine tier needs it")
	}
	talosVersion := e2eTalosVersion

	// ── the machine: a blank Talos container behind a static address ─────────
	docker := func(args ...string) error {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	_ = docker("network", "rm", network)
	require.NoError(t, docker("network", "create", "--subnet", subnet, network),
		"create the e2e docker network")
	machine := dockermachine.New(machineName, "ghcr.io/siderolabs/talos:"+talosVersion, network, machineIP)
	_ = machine.Remove(ctx)
	require.NoError(t, machine.Ensure(ctx), "create the blank machine")
	t.Cleanup(func() {
		_ = machine.Remove(context.Background())
		_, _ = exec.Command("docker", "network", "rm", network).CombinedOutput()
	})

	// ── the simulated BMC, with the machine attached ─────────────────────────
	fixture, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	state := sim.NewState(fixture)
	state.AttachMachine(fixture.Systems[0].ID, machine)
	srv := httptest.NewTLSServer(sim.NewServer(state))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.yaml")
	require.NoError(t, os.WriteFile(ledgerPath,
		[]byte(fmt.Sprintf("%q: %s\n", fixture.Manager.MAC, srv.URL)), 0o600))
	led, err := ledger.Load(ledgerPath)
	require.NoError(t, err)

	// ── the cluster side: CRDs, namespace, secrets, managers ─────────────────
	s, err := pkscheme.New()
	require.NoError(t, err)
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	// Who the test acts as. This tier installs no admission policies, but it
	// stamps approvals and guard lifts as pk-metal does, with the username
	// the API server reports for the kube-context, so that what it writes is
	// what the policies (config/policy) accept from that user. The objects
	// that make the operator propose work name it as their requester
	// (metal.pk.io/requested-by), as pk-metal does; under selfApproval:
	// Forbidden another person would approve what they cause.
	ssr := &authenticationv1.SelfSubjectReview{}
	require.NoError(t, cl.Create(ctx, ssr), "ask the cluster who the test is")
	user := ssr.Status.UserInfo.Username
	require.NotEmpty(t, user, "the cluster reports no username for the kube-context")
	approvedBy, err := json.Marshal(map[string]any{"spec": map[string]any{"approval": map[string]any{"approvedBy": user}}})
	require.NoError(t, err)

	_, err = envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("..", "..", "config", "crd")},
	})
	require.NoError(t, err, "install the CRDs onto the dev cluster")

	cleanSlate(ctx, t, cl, kindTier)
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	t.Cleanup(func() { cleanSlate(context.Background(), t, cl, kindTier) })

	bundleBytes := newBundle(t, talosVersion)
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-ca", Namespace: ns},
			Data: map[string][]byte{"ca.crt": pem.EncodeToMemory(
				&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})}},
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-factory", Namespace: ns},
			Data: map[string][]byte{"username": []byte("admin"), "password": []byte("dev")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "talos-secrets", Namespace: ns},
			Data: map[string][]byte{render.BundleKey: bundleBytes}},
	} {
		require.NoError(t, cl.Create(ctx, sec))
	}

	// The reconcilers, built by controller.SetupAll as cmd/manager builds them,
	// minus the lanes this suite does not exercise. In-process is the dev-run
	// composition; in-cluster deployment fidelity arrives with the
	// image lane. The approval VAP is envtest-proven and not re-tested here.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: ns,
		OperationsNamespace:  ns,
		Proposer:             "pk-metal-e2e",
		EventSource:          "pk-metal-e2e",
		// The audit trail goes to the test's output beside the log.
		Audit:         audit.New(os.Stderr, "pk-metal-e2e"),
		Ledger:        led,
		TLS:           &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: "bmc-ca"}},
		PollInterval:  5 * time.Second,
		Factory:       kindFactory,
		TalosDial:     machineapi.Dial,
		CheckInterval: 2 * time.Second,
	}, controller.LaneAsset, controller.LaneEndpoint, controller.LaneHost, controller.LaneHostClaim,
		controller.LaneHostOperation, controller.LaneTalosNode, controller.LaneTalosCluster,
		controller.LaneBootAsset))
	mgrCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()

	// assert.Eventually plus an explicit fatal, not require.Eventually: the
	// message arguments of the latter are evaluated before polling begins, so a
	// "last seen" value interpolated there reports the zero value every time —
	// a diagnostic that cannot diagnose, and one that sends the next reader
	// after the wrong thing.
	waitHost := func(pred func(metalv1alpha1.Host) bool, timeout time.Duration, msg string) metalv1alpha1.Host {
		var last metalv1alpha1.Host
		if !assert.Eventually(t, func() bool {
			h := metalv1alpha1.Host{}
			if err := cl.Get(ctx, client.ObjectKey{Name: hostName}, &h); err != nil {
				return false
			}
			last = h
			return pred(h)
		}, timeout, tick) {
			// The operations say what the host waits on: log each one's status.
			ops := &metalv1alpha1.HostOperationList{}
			if err := cl.List(context.Background(), ops, client.InNamespace(ns)); err == nil {
				for i := range ops.Items {
					st, _ := json.Marshal(ops.Items[i].Status)
					t.Logf("operation %s: %s", ops.Items[i].Name, st)
				}
			}
			t.Fatalf("%s (last: phase=%q blockedOn=%q conditions=%+v)",
				msg, last.Status.Phase, last.Status.BlockedOn, last.Status.Conditions)
		}
		return last
	}

	// The OS payload, shaped for the container platform: no VIP and no links,
	// because docker owns the container's network. The API requires an install
	// disk selector; the container ignores the install document. Built once:
	// subtests create copies on the cluster, and the bootstrap verb reads the
	// same shape from a CR dir, which is the git-native story in miniature.
	requested := map[string]string{metalv1alpha1.AnnotationRequestedBy: user}
	talosCluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Annotations: requested},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion:      talosVersion,
			KubernetesVersion: e2eKubernetesVersion,
			Endpoint:          "https://" + machineIP + ":6443",
			SecretsRef:        osv1alpha1.SecretReference{Namespace: ns, Name: "talos-secrets"},
		},
	}
	talosNode := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: requested},
		Spec: osv1alpha1.TalosNodeSpec{
			ClusterRef:          osv1alpha1.Reference{Name: clusterName},
			Role:                osv1alpha1.RoleControlPlane,
			Address:             machineIP,
			InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "e2e-no-disk"},
		},
	}

	// ── the ladder ───────────────────────────────────────────────────────────
	// Rungs build on one another: once one fails there is nothing left to
	// observe, only timeouts to wait out — so the ladder stops.
	rung := func(name string, fn func(t *testing.T)) {
		if t.Failed() {
			t.Logf("skipping %q — an earlier rung failed", name)
			return
		}
		t.Run(name, fn)
	}

	rung("host materializes Available", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: assetName},
			Spec: metalv1alpha1.AssetSpec{
				CID:            "CI-" + assetName,
				Serial:         "SIMX12B000001",
				BMCMac:         fixture.Manager.MAC,
				CredentialsRef: corev1.LocalObjectReference{Name: "bmc-factory"},
			},
		}))
		waitHost(func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostAvailable
		}, waitFor, "host never reached Available")
	})

	rung("claim with OS intent engages the lane", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, talosCluster.DeepCopy()))
		require.NoError(t, cl.Create(ctx, talosNode.DeepCopy()))
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: ns, Annotations: requested},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: assetName},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: nodeName}},
			},
		}))
		h := waitHost(func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostProvisioning
		}, waitFor, "claim with OS intent never reached Provisioning")
		ready := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady)
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionUnknown, ready.Status, "the machine is off — nothing observed yet")
	})

	rung("approved power-on boots the machine to maintenance", func(t *testing.T) {
		op := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-power-on", Namespace: ns},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:    metalv1alpha1.Reference{Name: hostName},
				Reason:     "Provision",
				ProposedBy: "e2e-proposer",
				Actions:    []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
			},
		}
		require.NoError(t, cl.Create(ctx, op))
		// The human gate, stamped the way the approve verb stamps it: a merge
		// patch — conflict-free against the reconciler's status writes. Its
		// userInfo rules are VAP territory, envtest-proven.
		require.NoError(t, cl.Patch(ctx, op, client.RawPatch(types.MergePatchType, approvedBy)))

		require.Eventually(t, func() bool {
			running, err := machine.Running(ctx)
			return err == nil && running
		}, waitFor, tick, "the approved operation never started the container")

		h := waitHost(func(h metalv1alpha1.Host) bool {
			return meta.IsStatusConditionTrue(h.Status.Conditions, metalv1alpha1.ConditionBooted)
		}, bootWait, "the check-in never observed the blank machine")
		booted := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBooted)
		assert.Equal(t, osv1alpha1.ReasonMaintenanceMode, booted.Reason,
			"a blank machine answers only on the maintenance service")
		assert.Equal(t, metalv1alpha1.HostProvisioning, h.Status.Phase)
	})

	rung("bootstrap verb provisions the node", func(t *testing.T) {
		// A container has no media channel, and the shipped CLI has no
		// in-band config path. The harness stands in for the BMC's virtual
		// media: it hands the container the config the operator rendered,
		// on the private docker network of this test only.
		rendered := &osv1alpha1.TalosNode{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: nodeName}, rendered))
		require.NotNil(t, rendered.Status.ConfigSecretRef, "the operator never rendered the node's config")
		cfgSecret := &corev1.Secret{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{
			Namespace: rendered.Status.ConfigSecretRef.Namespace, Name: rendered.Status.ConfigSecretRef.Name,
		}, cfgSecret))
		machineConfig := cfgSecret.Data["config.yaml"]
		require.NotEmpty(t, machineConfig, "the config Secret holds no config.yaml")
		require.Eventually(t, func() bool {
			return talos.With(ctx, machineapi.Dial, talos.Target{Endpoint: machineIP, Insecure: true}, func(c talos.Client) error {
				_, err := c.ApplyConfiguration(ctx, machineConfig, talos.ApplyOptions{})
				return err
			}) == nil
		}, waitFor, tick, "the maintenance service never took the config")

		// The REAL CLI, on the same inputs a day-0 operator would hold: the CR
		// dir and the secrets bundle.
		crDir := t.TempDir()
		require.NoError(t, render.WriteFiles(crDir, talosCluster, []osv1alpha1.TalosNode{*talosNode}))
		// The secret lives NEXT TO the CR dir, never in it — LoadDir walks the
		// whole directory and rightly refuses foreign documents.
		secretPath := filepath.Join(t.TempDir(), render.BundleKey)
		require.NoError(t, os.WriteFile(secretPath, bundleBytes, 0o600))

		// The TalosCluster reconciler watches the same machine and may call
		// Bootstrap before the verb does; the loser sees AlreadyExists, which
		// both treat as done, so the race is part of what this rung proves.
		cmd := exec.CommandContext(ctx, "go", "run", "../../cmd/pk-metal",
			"bootstrap", "--dir", crDir, "--secrets", secretPath, "--node", nodeName,
			"--timeout", "8m")
		out, err := cmd.CombinedOutput()
		t.Logf("bootstrap output:\n%s", out)
		require.NoError(t, err, "the bootstrap verb failed")

		h := waitHost(func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostProvisioned
		}, waitFor, "the check-in never advanced the host to Provisioned")
		assert.Equal(t, osv1alpha1.ReasonNodeReady,
			meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady).Reason)

		n := &osv1alpha1.TalosNode{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: nodeName}, n))
		assert.Equal(t, "running", n.Status.Stage)
		assert.Equal(t, talosVersion, n.Status.Version)
	})

	rung("cluster reports Bootstrapped and Available", func(t *testing.T) {
		var last osv1alpha1.TalosCluster
		if !assert.Eventually(t, func() bool {
			c := osv1alpha1.TalosCluster{}
			if err := cl.Get(ctx, client.ObjectKey{Name: clusterName}, &c); err != nil {
				return false
			}
			last = c
			return meta.IsStatusConditionTrue(c.Status.Conditions, osv1alpha1.ConditionAvailable)
		}, waitFor, tick) {
			t.Fatalf("cluster never became Available (last conditions=%+v)", last.Status.Conditions)
		}
		assert.True(t, meta.IsStatusConditionTrue(last.Status.Conditions, osv1alpha1.ConditionBootstrapped))
		assert.Equal(t, int32(1), last.Status.ReadyControlPlanes)
		require.NotNil(t, last.Status.KubeconfigSecretRef)
		kc := &corev1.Secret{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{
			Namespace: last.Status.KubeconfigSecretRef.Namespace, Name: last.Status.KubeconfigSecretRef.Name}, kc))
		_, err := controller.ProbeKubernetes(ctx, kc.Data[controller.KubeconfigKey])
		require.NoError(t, err, "the published kubeconfig reaches the API server")
	})

	rung("release returns the machine", func(t *testing.T) {
		claim := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: ns}}
		// The node is the only control plane of a cluster that stays: the
		// etcd guard keeps it until a person acknowledges the risk. The
		// release names who asks for it before the delete.
		ack, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{
			metalv1alpha1.AnnotationAckEtcdRisk: metalv1alpha1.GuardLiftValue(user, "the test cluster ends with this rung"),
			metalv1alpha1.AnnotationRequestedBy: user,
		}}})
		require.NoError(t, err)
		require.NoError(t, cl.Patch(ctx, claim, client.RawPatch(types.MergePatchType, ack)))
		require.NoError(t, cl.Delete(ctx, claim))
		// The machine runs a node, so the release that powers it off is a
		// proposal: approve it the way the approve verb does.
		var release *metalv1alpha1.HostOperation
		require.Eventually(t, func() bool {
			ops := &metalv1alpha1.HostOperationList{}
			if err := cl.List(ctx, ops, client.InNamespace(ns)); err != nil {
				return false
			}
			for i := range ops.Items {
				if strings.HasPrefix(ops.Items[i].Name, "release-") {
					release = &ops.Items[i]
					return true
				}
			}
			return false
		}, waitFor, tick, "the release of a running node was never proposed")
		// The cluster guard keeps the only control plane of a live cluster
		// up until a person lifts it on the operation, naming themselves
		// and a reason, and approves it.
		lift, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{
				metalv1alpha1.AnnotationOverrideClusterGuard: metalv1alpha1.GuardLiftValue(user, "the test cluster ends with this rung"),
			}},
			"spec": map[string]any{"approval": map[string]any{"approvedBy": user}},
		})
		require.NoError(t, err)
		require.NoError(t, cl.Patch(ctx, release, client.RawPatch(types.MergePatchType, lift)))
		h := waitHost(func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostAvailable
		}, waitFor, "release never returned the host to Available")
		assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady))
		assert.Nil(t, h.Spec.OS)
	})
}

// tier names the objects one e2e tier creates. The two tiers use different
// names, so they can run against the same cluster.
type tier struct {
	namespace, asset, node, cluster string
}

// kindTier is what the kind tier creates.
var kindTier = tier{namespace: ns, asset: assetName, node: nodeName, cluster: clusterName}

// cleanSlate removes everything a previous run of the tier may have left: the
// namespace (claims, operations, secrets) and the cluster-scoped CRs. The Host
// name is shared by both tiers. CRDs stay — the dev cluster keeps them,
// dev-nuke resets everything.
func cleanSlate(ctx context.Context, t *testing.T, cl client.Client, tr tier) {
	t.Helper()
	for _, obj := range []client.Object{
		&metalv1alpha1.Asset{ObjectMeta: metav1.ObjectMeta{Name: tr.asset}},
		&metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: hostName}},
		&osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: tr.node}},
		&osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: tr.cluster}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tr.namespace}},
	} {
		if err := cl.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("clean slate: delete %T %s: %v", obj, obj.GetName(), err)
		}
	}
	require.Eventually(t, func() bool {
		n := &corev1.Namespace{}
		return apierrors.IsNotFound(cl.Get(ctx, types.NamespacedName{Name: tr.namespace}, n))
	}, waitFor, tick, "the %s namespace never finished deleting", tr.namespace)
}

// The versions both tiers run. They match the lab sample in config/samples,
// which pins the same pair, so a bump changes them together.
const (
	e2eTalosVersion      = "v1.14.6"
	e2eKubernetesVersion = "v1.36.2"
)

// newBundle generates a fresh secrets bundle in the file format `talosctl gen
// secrets` writes. Each run gets its own, so no key material is checked in
// for the tiers that boot real machines.
func newBundle(t *testing.T, talosVersion string) []byte {
	t.Helper()
	contract, err := render.Contract(talosVersion)
	require.NoError(t, err)
	bundle, err := secrets.NewBundle(secrets.NewClock(), contract)
	require.NoError(t, err)
	data, err := yaml.Marshal(bundle)
	require.NoError(t, err)
	_, err = render.LoadBundle(data, talosVersion)
	require.NoError(t, err, "the generated bundle round-trips through the operator's decoder")
	return data
}
