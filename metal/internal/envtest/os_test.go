// The OS-lane suite against a real apiserver: TalosNodes render the virtual
// lab's sample cluster into config Secrets, BootAssets pack and address boot
// media, the check-in observes a scripted machine, and the TalosCluster
// reconciler bootstraps etcd exactly once and reaches Available. The render
// itself is locked down by the render package's tests; here the subject is
// the reconcile loops and their watches.
package envtest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/siderolabs/image-factory/pkg/schematic"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

const labTalos = "v1.14.6"

// stubFactory stands in for the image factory: it answers a schematic POST the
// way the real one does (201 with the id the posted content hashes to) and
// records what it was asked to register. Registration is a network call on the
// render path, so the suite runs its own factory instead of the public one, and
// gets to watch what the reconcilers tell it.
func stubFactory(t *testing.T) (*render.Factory, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/schematics" {
			http.Error(w, "not the schematic endpoint", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var sch schematic.Schematic
		if err := yaml.Unmarshal(body, &sch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := sch.ID()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()
		normalized, _ := sch.Marshal()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "schematic": string(normalized)})
	}))
	t.Cleanup(srv.Close)
	return &render.Factory{
			RegistryURL: strings.TrimPrefix(srv.URL, "https://"),
			HTTPClient:  srv.Client(),
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), ids...)
		}
}

// labSample reads config/samples/talos.yaml: the cluster and nodes under test
// are the ones a person applies to the virtual lab.
func labSample(t *testing.T) *render.Manifests {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "talos.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "talos.yaml"), data, 0o600))
	m, err := render.LoadDir(dir)
	require.NoError(t, err)
	return m
}

func TestOSLane(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run via `mise run envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	// The scripted lab: one machine per sample node, all off.
	fleet := fake.NewFleet()
	machines := map[string]*fake.Machine{}
	for i, name := range []string{"node-1", "node-2", "node-3", "node-4"} {
		m := fake.NewMachine(labTalos)
		m.SetUnreachable(true)
		fleet.Add(fmt.Sprintf("10.77.0.1%d", i+1), name, m)
		machines[name] = m
	}
	var kubeUp atomic.Bool
	probe := func(_ context.Context, kubeconfig []byte) (string, error) {
		if _, err := clientcmd.Load(kubeconfig); err != nil {
			return "", err
		}
		if !kubeUp.Load() {
			return "", errors.New("connection refused")
		}
		return "v1.36.2", nil
	}

	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, osv1alpha1.AddToScheme(s))
	require.NoError(t, metalv1alpha1.AddToScheme(s))
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	ctx := t.Context()

	// The cluster secrets bundle, as a person creates it from the output of
	// `talosctl gen secrets`: the render package's throwaway test bundle.
	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	createBundle := func(name string) {
		require.NoError(t, cl.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Data:       map[string][]byte{render.BundleKey: bundle},
		}))
	}
	createBundle("lab-talos-secrets")

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	factory, posted := stubFactory(t)
	require.NoError(t, (&controller.TalosNodeReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Namespace:     "default",
		Factory:       factory,
		Dial:          fleet.Dialer(),
		CheckInterval: 100 * time.Millisecond,
		ReadyInterval: 200 * time.Millisecond,
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.TalosClusterReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Namespace:       "default",
		Dial:            fleet.Dialer(),
		ProbeKubernetes: probe,
		CheckInterval:   100 * time.Millisecond,
		ReadyInterval:   200 * time.Millisecond,
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.BootAssetReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Namespace: "default",
		Factory:   factory,
	}).SetupWithManager(mgr))
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()

	lab := labSample(t)
	lab.Cluster.Spec.SecretsRef.Namespace = "default"
	require.NoError(t, cl.Create(ctx, lab.Cluster.DeepCopy()))
	for i := range lab.Nodes {
		require.NoError(t, cl.Create(ctx, lab.Nodes[i].DeepCopy()))
	}

	nodeCondition := func(name, condType string) (osv1alpha1.TalosNode, *metav1.Condition) {
		n := osv1alpha1.TalosNode{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return n, nil
		}
		return n, meta.FindStatusCondition(n.Status.Conditions, condType)
	}
	clusterCondition := func(name, condType string) (osv1alpha1.TalosCluster, *metav1.Condition) {
		c := osv1alpha1.TalosCluster{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, &c); err != nil {
			return c, nil
		}
		return c, meta.FindStatusCondition(c.Status.Conditions, condType)
	}
	assetCondition := func(name string) (osv1alpha1.BootAsset, *metav1.Condition) {
		ba := osv1alpha1.BootAsset{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, &ba); err != nil {
			return ba, nil
		}
		return ba, meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
	}
	secretData := func(ref *osv1alpha1.SecretReference, key string) []byte {
		require.NotNil(t, ref)
		sec := corev1.Secret{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &sec))
		return sec.Data[key]
	}
	handOff := func(host, node string) {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: host},
			Spec:       metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: node}}},
		}))
	}

	var configDigest string
	t.Run("node renders into its config Secret", func(t *testing.T) {
		require.Eventually(t, func() bool {
			_, c := nodeCondition("node-1", osv1alpha1.ConditionRendered)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "Rendered never went True")

		n, _ := nodeCondition("node-1", osv1alpha1.ConditionRendered)
		assert.Equal(t, &osv1alpha1.SecretReference{Namespace: "default", Name: "machineconfig-node-1"}, n.Status.ConfigSecretRef)
		assert.Equal(t, "10.77.0.11", n.Status.Address)
		assert.Regexp(t, `^`+regexp.QuoteMeta(factory.RegistryURL)+`/metal-installer/[0-9a-f]{64}:v1\.14\.6$`, n.Status.InstallerImage,
			"the installer must be named on the factory the operator was given")

		mc := secretData(n.Status.ConfigSecretRef, asset.ConfigFilename)
		require.NotEmpty(t, mc)
		for _, want := range []string{"type: controlplane", "kind: UnattendedInstallConfig", "hostname: node-1",
			"kind: Layer2VIPConfig", `disk.serial == "pklab-disk-1"`, `mac(link.permanent_addr) == "52:54:00:77:00:11"`} {
			assert.Contains(t, string(mc), want)
		}
		assert.Equal(t, digest.FromBytes(mc).String(), n.Status.ConfigDigest)
		configDigest = n.Status.ConfigDigest

		sec := corev1.Secret{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "machineconfig-node-1"}, &sec))
		require.Len(t, sec.OwnerReferences, 1, "config Secret must die with its node")
		assert.Equal(t, "TalosNode", sec.OwnerReferences[0].Kind)
		id := strings.TrimSuffix(strings.Split(n.Status.InstallerImage, "/")[2], ":"+labTalos)
		assert.Contains(t, posted(), id, "the schematic must be registered before the config names it")
	})

	t.Run("cluster publishes credentials derived from the bundle", func(t *testing.T) {
		var c osv1alpha1.TalosCluster
		require.Eventually(t, func() bool {
			c, _ = clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
			return c.Status.KubeconfigSecretRef != nil && c.Status.TalosconfigSecretRef != nil
		}, waitFor, tick)

		kc, err := clientcmd.Load(secretData(c.Status.KubeconfigSecretRef, controller.KubeconfigKey))
		require.NoError(t, err)
		assert.Equal(t, "https://10.77.0.10:6443", kc.Clusters[kc.Contexts[kc.CurrentContext].Cluster].Server)

		tc, err := clientconfig.FromBytes(secretData(c.Status.TalosconfigSecretRef, controller.TalosconfigKey))
		require.NoError(t, err)
		assert.Equal(t, []string{"10.77.0.11"}, tc.Contexts[tc.Context].Endpoints, "every control-plane address")
		assert.EqualValues(t, 1, c.Status.ControlPlanes)
		assert.EqualValues(t, 2, c.Status.Workers)

		// Re-reconciles keep the credential instead of minting a new one.
		before := secretData(c.Status.KubeconfigSecretRef, controller.KubeconfigKey)
		time.Sleep(500 * time.Millisecond)
		assert.Equal(t, before, secretData(c.Status.KubeconfigSecretRef, controller.KubeconfigKey))
	})

	t.Run("configISO BootAsset packs the medium", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1-config"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy: osv1alpha1.StrategyConfigISO,
				NodeRef:  &osv1alpha1.Reference{Name: "node-1"},
			},
		}))
		require.Eventually(t, func() bool {
			_, c := assetCondition("node-1-config")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "Ready never went True")

		ba, c := assetCondition("node-1-config")
		assert.Equal(t, osv1alpha1.ReasonArtifactBuilt, c.Reason)
		require.NotNil(t, ba.Status.ArtifactSecretRef)

		sec := corev1.Secret{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{
			Namespace: ba.Status.ArtifactSecretRef.Namespace, Name: ba.Status.ArtifactSecretRef.Name}, &sec))
		iso := sec.Data["asset.iso"]
		require.NotEmpty(t, iso)
		assert.Equal(t, int64(len(iso)), ba.Status.Size)
		assert.Equal(t, digest.FromBytes(iso).String(), ba.Status.Digest)
		assert.Equal(t, configDigest, sec.Annotations["os.pk.io/config-digest"],
			"rebuild guard keys on the config digest")

		// The medium is what Talos mounts: ISO9660 magic and the metal-iso label.
		require.Greater(t, len(iso), 16*2048+72)
		assert.Equal(t, "CD001", string(iso[16*2048+1:16*2048+6]))
		assert.Equal(t, "metal-iso", string(bytes.TrimRight(iso[16*2048+40:16*2048+72], " ")))

		require.Len(t, sec.OwnerReferences, 1, "artifact Secret must die with its BootAsset")
		assert.Equal(t, "BootAsset", sec.OwnerReferences[0].Kind)
	})

	t.Run("factory BootAsset computes the content address", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-install"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyFactory,
				TalosVersion: labTalos,
				Schematic:    lab.Cluster.Spec.Schematic,
			},
		}))
		require.Eventually(t, func() bool {
			_, c := assetCondition("lab-install")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick)

		ba, c := assetCondition("lab-install")
		assert.Equal(t, osv1alpha1.ReasonURLComputed, c.Reason)
		assert.Regexp(t,
			`^https://`+regexp.QuoteMeta(factory.RegistryURL)+`/image/[0-9a-f]{64}/v1\.14\.6/metal-amd64\.iso$`,
			ba.Status.URL, "the URL must name the factory the operator was given, not the public one")
		assert.Empty(t, ba.Status.Digest, "remote artifact is addressed, not verified")

		// The boot medium and the installer the config names are one build.
		n, _ := nodeCondition("node-1", osv1alpha1.ConditionRendered)
		id := strings.Split(ba.Status.URL, "/")[4]
		assert.Contains(t, n.Status.InstallerImage, id)
		assert.Contains(t, posted(), id, "the schematic must be registered before Ready")
	})

	t.Run("build waits for the render and follows it", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "early"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy: osv1alpha1.StrategyConfigISO,
				NodeRef:  &osv1alpha1.Reference{Name: "node-4"},
			},
		}))
		require.Eventually(t, func() bool {
			_, c := assetCondition("early")
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonNodeNotFound
		}, waitFor, tick, "missing node must hold the build")

		n4 := lab.Node("node-3").DeepCopy()
		n4.Name = "node-4"
		n4.Spec.InstallDiskSelector.Serial = "pklab-disk-4"
		n4.Spec.Network.Links[0].HardwareAddr = "52:54:00:77:00:14"
		n4.Spec.Network.Links[0].Addresses = []string{"10.77.0.14/24"}
		require.NoError(t, cl.Create(ctx, n4))
		require.Eventually(t, func() bool {
			_, c := assetCondition("early")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "the build must follow the render without a nudge")
	})

	t.Run("broken inputs are alert-and-hold, and recover through the watches", func(t *testing.T) {
		orphan := lab.Node("node-3").DeepCopy()
		orphan.Name = "orphan"
		orphan.Spec.ClusterRef.Name = "no-such-cluster"
		require.NoError(t, cl.Create(ctx, orphan))
		require.Eventually(t, func() bool {
			_, c := nodeCondition("orphan", osv1alpha1.ConditionRendered)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonClusterNotFound
		}, waitFor, tick)

		// A cluster whose bundle does not exist yet, with a patch from a
		// ConfigMap that does not exist yet either.
		other := lab.Cluster.DeepCopy()
		other.Name = "other"
		other.Spec.SecretsRef.Name = "other-talos-secrets"
		other.Spec.Patches = append(other.Spec.Patches, osv1alpha1.Patch{ConfigMapRef: &osv1alpha1.ConfigMapKeyReference{
			Namespace: "default", Name: "other-patches", Key: "sysctl.yaml",
		}})
		require.NoError(t, cl.Create(ctx, other))
		member := lab.Node("node-3").DeepCopy()
		member.Name = "other-1"
		member.Spec.ClusterRef.Name = "other"
		require.NoError(t, cl.Create(ctx, member))

		require.Eventually(t, func() bool {
			_, c := nodeCondition("other-1", osv1alpha1.ConditionRendered)
			return c != nil && c.Reason == osv1alpha1.ReasonSecretsUnavailable
		}, waitFor, tick)
		_, b := clusterCondition("other", osv1alpha1.ConditionBootstrapped)
		require.NotNil(t, b)
		assert.Equal(t, osv1alpha1.ReasonSecretsUnavailable, b.Reason)

		createBundle("other-talos-secrets")
		require.Eventually(t, func() bool {
			_, c := nodeCondition("other-1", osv1alpha1.ConditionRendered)
			return c != nil && c.Reason == osv1alpha1.ReasonPatchesUnresolvable
		}, waitFor, tick, "the bundle watch must re-render")

		require.NoError(t, cl.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "other-patches", Namespace: "default"},
			Data: map[string]string{"sysctl.yaml": "apiVersion: v1alpha1\nkind: SysctlConfig\n" +
				"params:\n  net.core.somaxconn: \"65535\"\n"},
		}))
		require.Eventually(t, func() bool {
			_, c := nodeCondition("other-1", osv1alpha1.ConditionRendered)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "the ConfigMap watch must re-render")
		n, _ := nodeCondition("other-1", osv1alpha1.ConditionRendered)
		assert.Contains(t, string(secretData(n.Status.ConfigSecretRef, asset.ConfigFilename)), "net.core.somaxconn")
	})

	t.Run("check-in ladder against the scripted machine", func(t *testing.T) {
		m := machines["node-2"]
		handOff("box-2", "node-2")
		require.Eventually(t, func() bool {
			_, c := nodeCondition("node-2", osv1alpha1.ConditionMachineReachable)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonDialFailed
		}, waitFor, tick, "an off machine must observe as DialFailed")
		_, ready := nodeCondition("node-2", osv1alpha1.ConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, osv1alpha1.ReasonAwaitingContact, ready.Reason)

		// The machine comes up blank: only the maintenance service answers.
		m.SetUnreachable(false)
		require.Eventually(t, func() bool {
			n, c := nodeCondition("node-2", osv1alpha1.ConditionMachineReachable)
			return c != nil && c.Status == metav1.ConditionTrue &&
				c.Reason == osv1alpha1.ReasonMaintenanceMode && n.Status.Stage == "maintenance"
		}, waitFor, tick, "a blank machine must observe as MaintenanceMode")
		n, _ := nodeCondition("node-2", osv1alpha1.ConditionMachineReachable)
		assert.Equal(t, labTalos, n.Status.Version)

		// The config arrives (on metal: off the media) and the machine
		// converges; the check-in flips to the authenticated API and Ready.
		m.Configure(secretData(n.Status.ConfigSecretRef, asset.ConfigFilename))
		m.SetStage("running", true)
		require.Eventually(t, func() bool {
			n, c := nodeCondition("node-2", osv1alpha1.ConditionReady)
			return c != nil && c.Status == metav1.ConditionTrue &&
				c.Reason == osv1alpha1.ReasonNodeReady && n.Status.Stage == "running"
		}, waitFor, tick, "a converged machine must observe as NodeReady")
		_, reach := nodeCondition("node-2", osv1alpha1.ConditionMachineReachable)
		assert.Equal(t, osv1alpha1.ReasonMachineAPI, reach.Reason)

		// Release: the observations leave with the hand-off.
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "box-2"}}))
		require.Eventually(t, func() bool {
			n, c := nodeCondition("node-2", osv1alpha1.ConditionMachineReachable)
			return c == nil && n.Status.Stage == ""
		}, waitFor, tick, "observations must leave with the hand-off")
	})

	t.Run("cluster bootstraps once and becomes Available", func(t *testing.T) {
		// node-4 is no longer part of the lab: the cluster waits for every
		// member it has.
		require.NoError(t, cl.Delete(ctx, &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-4"}}))

		// The control plane is off: nothing may be bootstrapped.
		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonAwaitingControlPlane
		}, waitFor, tick)
		assert.Zero(t, machines["node-1"].BootstrapCalls())

		// The machines boot their media and converge as far as they can
		// without etcd. node-1's etcd takes its time after accepting the
		// bootstrap, as a real one does while its image is pulled.
		machines["node-1"].SetEtcdStartsLate(true)
		for i, name := range []string{"node-1", "node-2", "node-3"} {
			n, _ := nodeCondition(name, osv1alpha1.ConditionRendered)
			m := machines[name]
			m.Configure(secretData(n.Status.ConfigSecretRef, asset.ConfigFilename))
			m.SetStage("booting", false, "etcd")
			m.SetUnreachable(false)
			handOff(fmt.Sprintf("lab-box-%d", i+1), name)
		}

		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
			return c != nil && c.Status == metav1.ConditionUnknown && c.Reason == osv1alpha1.ReasonBootstrapRequested
		}, waitFor, tick, "the cluster must request a bootstrap once its control plane answers")
		_, requested := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
		// Ten check intervals and the member and Secret events of the hand-off
		// pass while etcd is not running: each one waits on the request.
		time.Sleep(time.Second)
		assert.Equal(t, 1, machines["node-1"].BootstrapCalls(), "a starting etcd must not be bootstrapped again")
		_, still := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
		require.NotNil(t, still)
		assert.Equal(t, osv1alpha1.ReasonBootstrapRequested, still.Reason)
		assert.Equal(t, requested.LastTransitionTime, still.LastTransitionTime, "the request time is kept")

		machines["node-1"].SetEtcdRunning(true)
		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
			return c != nil && c.Status == metav1.ConditionTrue && c.Reason == osv1alpha1.ReasonEtcdRunning
		}, waitFor, tick, "the cluster must observe etcd once it runs")
		assert.True(t, machines["node-1"].Bootstrapped())
		_, avail := clusterCondition("lab", osv1alpha1.ConditionAvailable)
		require.NotNil(t, avail)
		assert.Equal(t, metav1.ConditionFalse, avail.Status)

		// Kubernetes comes up and the nodes converge.
		kubeUp.Store(true)
		for _, m := range machines {
			m.SetStage("running", true)
		}
		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionAvailable)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "a ready cluster whose API answers must be Available")
		c, _ := clusterCondition("lab", osv1alpha1.ConditionAvailable)
		assert.Equal(t, "v1.36.2", c.Status.KubernetesVersion)
		assert.EqualValues(t, 1, c.Status.ReadyControlPlanes)
		assert.EqualValues(t, 2, c.Status.ReadyWorkers)

		// Further passes observe; they never bootstrap again.
		time.Sleep(time.Second)
		assert.Equal(t, 1, machines["node-1"].BootstrapCalls(), "bootstrap must be called exactly once")
		for _, name := range []string{"node-2", "node-3"} {
			assert.Zero(t, machines[name].BootstrapCalls(), name)
		}
	})

	t.Run("CEL refuses malformed specs", func(t *testing.T) {
		assert.Error(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-configiso"},
			Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO},
		}), "configISO without nodeRef must be refused")
		assert.Error(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-factory"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyFactory,
				TalosVersion: labTalos,
				NodeRef:      &osv1alpha1.Reference{Name: "node-1"},
			},
		}), "factory with nodeRef must be refused")

		bad := func(name string, mutate func(*osv1alpha1.TalosNode)) *osv1alpha1.TalosNode {
			n := lab.Node("node-3").DeepCopy()
			n.Name = name
			mutate(n)
			return n
		}
		assert.Error(t, cl.Create(ctx, bad("no-disk", func(n *osv1alpha1.TalosNode) {
			n.Spec.InstallDiskSelector = osv1alpha1.DiskSelector{}
		})), "a node that does not say where to install must be refused")
		assert.Error(t, cl.Create(ctx, bad("no-address", func(n *osv1alpha1.TalosNode) {
			n.Spec.Network.Links[0].Addresses = nil
		})), "a link with neither DHCP nor an address must be refused")
		assert.Error(t, cl.Create(ctx, bad("both-patch", func(n *osv1alpha1.TalosNode) {
			n.Spec.Patches = []osv1alpha1.Patch{{Inline: "x: y", ConfigMapRef: &osv1alpha1.ConfigMapKeyReference{Namespace: "a", Name: "b", Key: "c"}}}
		})), "a patch is inline or from a ConfigMap, not both")
		assert.Error(t, cl.Create(ctx, bad("bad-mac", func(n *osv1alpha1.TalosNode) {
			n.Spec.Network.Links[0].HardwareAddr = "52-54-00-77-00-99"
		})))

		old := lab.Cluster.DeepCopy()
		old.Name = "old"
		old.Spec.TalosVersion = "v1.13.4"
		assert.Error(t, cl.Create(ctx, old), "pre-1.14 Talos must be refused")
	})
}
