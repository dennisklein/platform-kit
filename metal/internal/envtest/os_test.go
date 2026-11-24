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
	tconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
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

// pinImage stands in for the registry: it pins a tag to a digest derived
// from the reference, so a plan is stable and visibly pinned.
func pinImage(_ context.Context, ref string) (string, error) {
	return ref + "@" + digest.FromString(ref).String(), nil
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
	st := startEnv(t)

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

	cl := st.cl
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

	mgr := st.newManager(t)
	factory, posted := stubFactory(t)
	store, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		Factory:              factory,
		MediaStore:           store,
		Imager: &controller.Imager{Repository: controller.DefaultImagerRepository,
			Digests: controller.DefaultImagerDigests(), StoreClaim: "pk-metal-media-store",
			HelperImage: "pk-metal:envtest"},
		TalosDial:       fleet.Dialer(),
		ProbeKubernetes: probe,
		CheckInterval:   100 * time.Millisecond,
		ReadyInterval:   200 * time.Millisecond,
		DriftInterval:   200 * time.Millisecond,
		Proposer:        "pk-metal-operator",
		ResolveImage:    pinImage,
	}, controller.LaneTalosNode, controller.LaneTalosCluster, controller.LaneBootAsset, controller.LaneTalosRollout))
	runManager(t, ctx, mgr)

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
	st.run(t, "node renders into its config Secret", func(t *testing.T) {
		require.Eventually(t, func() bool {
			_, c := nodeCondition("node-1", osv1alpha1.ConditionRendered)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "Rendered never went True")

		n, _ := nodeCondition("node-1", osv1alpha1.ConditionRendered)
		// Content-addressed: the name carries the digest, so an approved
		// apply finds exactly the bytes it was approved for.
		require.NotNil(t, n.Status.ConfigSecretRef)
		assert.Equal(t, "default", n.Status.ConfigSecretRef.Namespace)
		assert.Regexp(t, `^machineconfig-node-1-[0-9a-f]{12}$`, n.Status.ConfigSecretRef.Name)
		assert.Equal(t, labTalos, n.Status.RenderedFor)
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
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: n.Status.ConfigSecretRef.Name}, &sec))
		assert.True(t, strings.HasPrefix(n.Status.ConfigDigest, "sha256:"+strings.TrimPrefix(sec.Name, "machineconfig-node-1-")),
			"the name carries the digest of what it holds")
		require.NotNil(t, sec.Immutable)
		assert.True(t, *sec.Immutable, "a config Secret is never rewritten")
		require.Len(t, sec.OwnerReferences, 1, "config Secret must die with its node")
		assert.Equal(t, "TalosNode", sec.OwnerReferences[0].Kind)
		id := strings.TrimSuffix(strings.Split(n.Status.InstallerImage, "/")[2], ":"+labTalos)
		assert.Contains(t, posted(), id, "the schematic must be registered before the config names it")
	})

	st.run(t, "cluster publishes credentials derived from the bundle", func(t *testing.T) {
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
		// No machine runs any of the nodes yet: they are written ahead, not
		// members, and do not hold the cluster down.
		assert.EqualValues(t, 0, c.Status.ControlPlanes)
		assert.EqualValues(t, 0, c.Status.Workers)
		assert.EqualValues(t, 3, c.Status.UnassignedNodes)

		// Re-reconciles keep the credential instead of minting a new one.
		before := secretData(c.Status.KubeconfigSecretRef, controller.KubeconfigKey)
		st.settle(t, ctx, &c, 3)
		assert.Equal(t, before, secretData(c.Status.KubeconfigSecretRef, controller.KubeconfigKey))
	})

	st.run(t, "configISO BootAsset packs the medium", func(t *testing.T) {
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

	st.run(t, "embeddedInstaller BootAsset is built by an imager Job", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1-installer"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyEmbeddedInstaller,
				NodeRef:      &osv1alpha1.Reference{Name: "node-1"},
				TalosVersion: labTalos,
				Schematic: &osv1alpha1.Schematic{Customization: osv1alpha1.SchematicCustomization{
					ExtraKernelArgs: []string{"console=ttyS0"},
				}},
			},
		}))
		job := waitImagerJob(t, ctx, cl, "default")
		// The Job is created before the status write that names it, so the
		// condition can lag the Job by a reconcile.
		require.Eventually(t, func() bool {
			_, c := assetCondition("node-1-installer")
			return c != nil && c.Reason == osv1alpha1.ReasonBuilding
		}, waitFor, tick, "Ready never said Building")

		// The config reaches the imager as the node's config Secret, mounted;
		// nothing in the Job object carries it.
		n, _ := nodeCondition("node-1", osv1alpha1.ConditionRendered)
		config := secretData(n.Status.ConfigSecretRef, asset.ConfigFilename)
		raw, err := json.Marshal(job)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), string(config[:64]))
		mounted := false
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == n.Status.ConfigSecretRef.Name {
				mounted = true
			}
		}
		assert.True(t, mounted, "the config Secret is a volume of the build")
		require.Len(t, job.OwnerReferences, 1)
		assert.Equal(t, "node-1-installer", job.OwnerReferences[0].Name)

		playImager(t, ctx, cl, store, job, []byte("an installer for node-1"))
		require.Eventually(t, func() bool {
			_, c := assetCondition("node-1-installer")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "Ready never went True")
		ba, _ := assetCondition("node-1-installer")
		assert.Equal(t, digest.FromString("an installer for node-1").String(), ba.Status.Digest)
		assert.Equal(t, n.Status.ConfigDigest, ba.Status.ConfigDigest)
		assert.True(t, store.Has(ba.Name, ba.Status.BuildKey))

		// Deleting the asset takes its installer off the disk.
		require.NoError(t, cl.Delete(ctx, &ba))
		require.Eventually(t, func() bool { return !store.Has(ba.Name, ba.Status.BuildKey) }, waitFor, tick)
	})

	st.run(t, "factory BootAsset computes the content address", func(t *testing.T) {
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

	st.run(t, "build waits for the render and follows it", func(t *testing.T) {
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

	st.run(t, "broken inputs are alert-and-hold, and recover through the watches", func(t *testing.T) {
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
		// The cluster's own reconcile may land after the node's.
		require.Eventually(t, func() bool {
			_, b := clusterCondition("other", osv1alpha1.ConditionBootstrapped)
			return b != nil && b.Reason == osv1alpha1.ReasonSecretsUnavailable
		}, waitFor, tick)

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

	st.run(t, "check-in ladder against the scripted machine", func(t *testing.T) {
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

	st.run(t, "cluster bootstraps once and becomes Available", func(t *testing.T) {
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
		cluster, requested := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
		// Ten passes, and the member and Secret events of the hand-off, go
		// by while etcd is not running: each one waits on the request.
		st.settle(t, ctx, &cluster, 10)
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
		st.settle(t, ctx, &c, 5)
		assert.Equal(t, 1, machines["node-1"].BootstrapCalls(), "bootstrap must be called exactly once")
		for _, name := range []string{"node-2", "node-3"} {
			assert.Zero(t, machines[name].BootstrapCalls(), name)
		}
	})

	rolloutOf := func(kind osv1alpha1.RolloutKind) *osv1alpha1.TalosRollout {
		list := osv1alpha1.TalosRolloutList{}
		require.NoError(t, cl.List(ctx, &list))
		for i := range list.Items {
			if ro := &list.Items[i]; ro.Spec.Kind == kind && ro.Spec.ClusterRef.Name == "lab" &&
				!meta.IsStatusConditionTrue(ro.Status.Conditions, osv1alpha1.ConditionComplete) {
				return ro
			}
		}
		return nil
	}

	st.run(t, "a node edit shows as no-reboot drift, and a Config rollout is proposed", func(t *testing.T) {
		inSync := func(name string) bool {
			n, c := nodeCondition(name, osv1alpha1.ConditionConfigInSync)
			return c != nil && c.Status == metav1.ConditionTrue && n.Status.AppliedConfigDigest == n.Status.ConfigDigest
		}
		require.Eventually(t, func() bool { return inSync("node-1") && inSync("node-2") && inSync("node-3") },
			waitFor, tick, "a machine that runs its render must read as in sync")

		n, _ := nodeCondition("node-3", osv1alpha1.ConditionConfigInSync)
		running := n.Status.ConfigSecretRef.Name
		n.Spec.Labels = map[string]string{"example.com/rack": "r2"}
		require.NoError(t, cl.Update(ctx, &n))

		require.Eventually(t, func() bool {
			n, c := nodeCondition("node-3", osv1alpha1.ConditionConfigInSync)
			return c != nil && c.Reason == osv1alpha1.ReasonConfigDrift && n.Status.ConfigSecretRef.Name != running
		}, waitFor, tick, "the edit must render anew and read as drift")
		n, c := nodeCondition("node-3", osv1alpha1.ConditionConfigInSync)
		require.NotNil(t, n.Status.Drift)
		assert.True(t, n.Status.Drift.Changed)
		assert.False(t, n.Status.Drift.RebootRequired, "a label applies without a reboot")
		assert.Contains(t, c.Message, "without a reboot")
		assert.NotContains(t, n.Status.Drift.Summary, "r2", "the summary names keys, never values")
		current := n.Status.ConfigSecretRef.Name
		var old corev1.Secret
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: running}, &old),
			"the config the machine runs is kept")

		var ro *osv1alpha1.TalosRollout
		require.Eventually(t, func() bool {
			ro = rolloutOf(osv1alpha1.RolloutConfig)
			return ro != nil && meta.IsStatusConditionTrue(ro.Status.Conditions, osv1alpha1.ConditionPlanned)
		}, waitFor, tick, "drift must propose a Config rollout")
		assert.Equal(t, "pk-metal-operator", ro.Spec.ProposedBy)
		assert.Empty(t, ro.Spec.Approval.ApprovedBy, "a proposal is never approved")
		require.Len(t, ro.Status.Plan, 1)
		assert.Equal(t, "node-3", ro.Status.Plan[0].Node)
		assert.Equal(t, current, ro.Status.Plan[0].ConfigSecret)
		assert.False(t, ro.Status.Plan[0].RebootRequired)
		assert.Equal(t, []string{"talosApplyConfig", "waitNodeReady"}, ro.Status.Plan[0].Actions,
			"a no-reboot change is neither drained nor uncordoned")
		ops := metalv1alpha1.HostOperationList{}
		require.NoError(t, cl.List(ctx, &ops))
		assert.Empty(t, ops.Items, "a proposal starts nothing")

		// The machine takes the new config, as the approved apply would.
		machines["node-3"].Configure(secretData(n.Status.ConfigSecretRef, asset.ConfigFilename))
		require.Eventually(t, func() bool { return inSync("node-3") }, waitFor, tick)
		require.Eventually(t, func() bool {
			err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: running}, &old)
			return apierrors.IsNotFound(err)
		}, waitFor, tick, "the config the machine no longer runs is collected")
		require.Eventually(t, func() bool {
			var got osv1alpha1.TalosRollout
			return cl.Get(ctx, types.NamespacedName{Name: ro.Name}, &got) == nil &&
				meta.IsStatusConditionTrue(got.Status.Conditions, osv1alpha1.ConditionComplete)
		}, waitFor, tick, "a proposal whose change happened has nothing left to do")
	})

	st.run(t, "a machine on an older Talos keeps its contract and a TalosUpgrade is proposed", func(t *testing.T) {
		setVersion := func(v string) {
			require.NoError(t, talos.With(ctx, fleet.Dialer(), talos.Target{Endpoint: "10.77.0.13", Talosconfig: []byte("tc")},
				func(c talos.Client) error {
					_, err := c.Upgrade(ctx, talos.UpgradeOptions{Image: "factory.test/installer:" + v})
					return err
				}))
		}
		setVersion("v1.14.5")
		require.Eventually(t, func() bool {
			n, c := nodeCondition("node-3", osv1alpha1.ConditionVersionCurrent)
			return c != nil && c.Reason == osv1alpha1.ReasonVersionSkew && n.Status.RenderedFor == "v1.14.5"
		}, waitFor, tick, "the skew must show, and the render must follow the version the machine runs")
		n, _ := nodeCondition("node-3", osv1alpha1.ConditionVersionCurrent)
		assert.True(t, strings.HasSuffix(n.Status.InstallerImage, ":v1.14.5"))
		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionVersionsCurrent)
			return c != nil && c.Status == metav1.ConditionFalse
		}, waitFor, tick)

		var ro *osv1alpha1.TalosRollout
		require.Eventually(t, func() bool {
			ro = rolloutOf(osv1alpha1.RolloutTalosUpgrade)
			return ro != nil && meta.IsStatusConditionTrue(ro.Status.Conditions, osv1alpha1.ConditionPlanned)
		}, waitFor, tick, "skew must propose a TalosUpgrade")
		require.Len(t, ro.Status.Plan, 1)
		step := ro.Status.Plan[0]
		assert.Equal(t, "node-3", step.Node)
		assert.Equal(t, "v1.14.5", step.From)
		assert.Equal(t, labTalos, step.To)
		assert.Regexp(t, `:v1\.14\.6@sha256:[0-9a-f]{64}$`, step.Image, "the installer is pinned by digest at planning")
		assert.Equal(t, []string{"drain", "talosUpgrade", "waitNodeReady", "uncordon"}, step.Actions)

		setVersion(labTalos)
		require.Eventually(t, func() bool {
			n, c := nodeCondition("node-3", osv1alpha1.ConditionVersionCurrent)
			return c != nil && c.Status == metav1.ConditionTrue && n.Status.RenderedFor == labTalos
		}, waitFor, tick)
	})

	st.run(t, "an edited secrets bundle is held while the machines run the old one", func(t *testing.T) {
		cluster, _ := clusterCondition("lab", osv1alpha1.ConditionAvailable)
		kubeconfig := secretData(cluster.Status.KubeconfigSecretRef, controller.KubeconfigKey)
		node1, _ := nodeCondition("node-1", osv1alpha1.ConditionRendered)

		other, err := secrets.NewBundle(secrets.NewClock(), tconfig.TalosVersion1_14)
		require.NoError(t, err)
		otherYAML, err := yaml.Marshal(other)
		require.NoError(t, err)
		var sec corev1.Secret
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "lab-talos-secrets"}, &sec))
		sec.Data[render.BundleKey] = otherYAML
		require.NoError(t, cl.Update(ctx, &sec))

		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionSecretsBundleHeld)
			return c != nil && c.Status == metav1.ConditionTrue && c.Reason == osv1alpha1.ReasonBundleChanged
		}, waitFor, tick, "the edit must be held, not acted on")
		_, held := clusterCondition("lab", osv1alpha1.ConditionSecretsBundleHeld)
		assert.Contains(t, held.Message, "Talos API CA")
		assert.Contains(t, held.Message, "lab-bundle-applied")
		st.settle(t, ctx, &cluster, 3)
		assert.Equal(t, kubeconfig, secretData(cluster.Status.KubeconfigSecretRef, controller.KubeconfigKey),
			"the published kubeconfig still works against the running cluster")
		n1, _ := nodeCondition("node-1", osv1alpha1.ConditionRendered)
		assert.Equal(t, node1.Status.ConfigDigest, n1.Status.ConfigDigest, "configs still render from the bundle the machines run")

		require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "lab-talos-secrets"}, &sec))
		sec.Data[render.BundleKey] = bundle
		require.NoError(t, cl.Update(ctx, &sec))
		require.Eventually(t, func() bool {
			_, c := clusterCondition("lab", osv1alpha1.ConditionSecretsBundleHeld)
			return c != nil && c.Status == metav1.ConditionFalse
		}, waitFor, tick, "putting the bundle back ends the hold")
	})

	st.run(t, "credentials are short-lived and a reader talosconfig is published on request", func(t *testing.T) {
		c, _ := clusterCondition("lab", osv1alpha1.ConditionAvailable)
		require.NotNil(t, c.Status.CredentialsExpireAt)
		assert.WithinDuration(t, time.Now().Add(24*time.Hour), c.Status.CredentialsExpireAt.Time, time.Hour,
			"the default lifetime is a day")

		c.Spec.Credentials = &osv1alpha1.CredentialsSpec{TTL: &metav1.Duration{Duration: 2 * time.Hour}, ReaderTalosconfig: true}
		require.NoError(t, cl.Update(ctx, &c))
		require.Eventually(t, func() bool {
			c, _ = clusterCondition("lab", osv1alpha1.ConditionAvailable)
			return c.Status.ReaderTalosconfigSecretRef != nil && c.Status.CredentialsExpireAt != nil &&
				c.Status.CredentialsExpireAt.Before(&metav1.Time{Time: time.Now().Add(3 * time.Hour)})
		}, waitFor, tick, "a shorter TTL re-mints at once, and the reader appears")
		reader := secretData(c.Status.ReaderTalosconfigSecretRef, controller.TalosconfigKey)
		roles, err := render.TalosconfigRoles(reader)
		require.NoError(t, err)
		assert.Equal(t, []string{"os:reader"}, roles)
		admin := secretData(c.Status.TalosconfigSecretRef, controller.TalosconfigKey)
		roles, err = render.TalosconfigRoles(admin)
		require.NoError(t, err)
		assert.Equal(t, []string{"os:admin"}, roles)

		c.Spec.Credentials = nil
		require.NoError(t, cl.Update(ctx, &c))
		require.Eventually(t, func() bool {
			c, _ = clusterCondition("lab", osv1alpha1.ConditionAvailable)
			return c.Status.ReaderTalosconfigSecretRef == nil
		}, waitFor, tick, "the reader goes when it is no longer asked for")
	})

	st.run(t, "CEL refuses malformed specs", func(t *testing.T) {
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

	st.run(t, "a cluster whose etcd data is lost is never bootstrapped again", func(t *testing.T) {
		c, _ := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
		require.NotNil(t, c.Status.BootstrappedAt, "the first observed bootstrap is remembered")
		calls := machines["node-1"].BootstrapCalls()

		// Every control plane (the lab has one) loses its etcd data, the way
		// a forced reset of EPHEMERAL or a reinstall loses it, and Kubernetes
		// stops answering. The machine still answers its authenticated API:
		// to the observer this is a cluster that was never bootstrapped.
		require.NoError(t, talos.With(ctx, fleet.Dialer(), talos.Target{Endpoint: "10.77.0.11", Talosconfig: []byte("tc")},
			func(c talos.Client) error {
				return c.Reset(ctx, talos.ResetOptions{Reboot: true, SystemPartitions: []string{"EPHEMERAL"}})
			}))
		kubeUp.Store(false)
		require.False(t, machines["node-1"].Bootstrapped())

		require.Eventually(t, func() bool {
			_, b := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
			return b != nil && b.Status == metav1.ConditionFalse && b.Reason == osv1alpha1.ReasonEtcdDataLost
		}, waitFor, tick, "the loss must be reported, not repaired with a new etcd")
		c, lost := clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
		assert.Contains(t, lost.Message, "docs/metal/disaster-recovery.md", "the condition points at the restore procedure")
		_, avail := clusterCondition("lab", osv1alpha1.ConditionAvailable)
		require.NotNil(t, avail)
		assert.Equal(t, osv1alpha1.ReasonEtcdDataLost, avail.Reason)

		st.settle(t, ctx, &c, 5)
		assert.Equal(t, calls, machines["node-1"].BootstrapCalls(), "no pass may bootstrap a cluster that was bootstrapped before")

		// The remembered fact cannot be dropped by a status write.
		c, _ = clusterCondition("lab", osv1alpha1.ConditionBootstrapped)
		c.Status.BootstrappedAt = nil
		err := cl.Status().Update(ctx, &c)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bootstrappedAt is write-once")
	})
}
