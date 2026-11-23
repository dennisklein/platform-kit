// The OS-lane suite: a TalosNode renders into its config Secret against a real
// apiserver, a configISO BootAsset packs that Secret into the metal-iso medium,
// a factory BootAsset computes its content-addressed URL, and the CEL rules
// refuse mispaired specs. The render engine itself is locked byte-for-byte by
// the render package's parity tests — here the subject is the reconcile loop.
package envtest_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	thtalos "github.com/budimanjojo/talhelper/v3/pkg/talos"
	"github.com/siderolabs/image-factory/pkg/schematic"
	tconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// stubFactory stands in for the image factory: it answers a schematic POST the
// way the real one does — 201 with the id the posted content hashes to — and
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
		if err := yamlv3.Unmarshal(body, &sch); err != nil {
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
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
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

func TestOSLane(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset — run via `make -C metal envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	// The scripted machine behind the check-in: one fake node every dial reaches.
	machine := fake.NewMachine("v1.13.4")
	machine.SetUnreachable(true)

	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, osv1alpha1.AddToScheme(s))
	require.NoError(t, metalv1alpha1.AddToScheme(s))
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	ctx := t.Context()

	// The cluster secrets bundle, as the day-0 runbook creates it: a throwaway
	// gensecret-equivalent under key talsecret.yaml (never the real talsecret in
	// tests — the CI-safe harness).
	vc, err := tconfig.ParseContractFromVersion("v1.13.4")
	require.NoError(t, err)
	bundle, err := thtalos.NewSecretBundle(secrets.NewClock(), *vc)
	require.NoError(t, err)
	bundleBytes, err := yamlv3.Marshal(bundle)
	require.NoError(t, err)
	require.NoError(t, cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-secrets", Namespace: "default"},
		Data:       map[string][]byte{"talsecret.yaml": bundleBytes},
	}))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	factory, posted := stubFactory(t)
	require.NoError(t, (&controller.TalosNodeReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SecretsSecret: "talos-secrets",
		Namespace:     "default",
		Factory:       factory,
		Dial:          machine.Dialer(),
		CheckInterval: 100 * time.Millisecond,
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

	require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion:      "v1.13.4",
			KubernetesVersion: "v1.36.2",
			Endpoint:          "https://10.77.35.200:6443",
		},
	}))
	node := func(name string) *osv1alpha1.TalosNode {
		return &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{
				ClusterRef:   osv1alpha1.Reference{Name: "demo"},
				IPAddress:    "10.77.38.202",
				ControlPlane: true,
				// talhelper refuses nodes without an install disk.
				InstallDiskSelector: &osv1alpha1.InstallDiskSelector{Serial: "PHLN000000000P6AGN"},
			},
		}
	}
	require.NoError(t, cl.Create(ctx, node("demo-cp1")))

	nodeCondition := func(name, condType string) (osv1alpha1.TalosNode, *metav1.Condition) {
		n := osv1alpha1.TalosNode{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return n, nil
		}
		return n, meta.FindStatusCondition(n.Status.Conditions, condType)
	}
	assetCondition := func(name string) (osv1alpha1.BootAsset, *metav1.Condition) {
		ba := osv1alpha1.BootAsset{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, &ba); err != nil {
			return ba, nil
		}
		return ba, meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
	}

	var configDigest string
	t.Run("node renders into its config Secret", func(t *testing.T) {
		require.Eventually(t, func() bool {
			_, c := nodeCondition("demo-cp1", osv1alpha1.ConditionRendered)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "Rendered never went True")

		n, _ := nodeCondition("demo-cp1", osv1alpha1.ConditionRendered)
		require.NotNil(t, n.Status.ConfigSecretRef)
		assert.Equal(t, "default", n.Status.ConfigSecretRef.Namespace)
		assert.Equal(t, "machineconfig-demo-cp1", n.Status.ConfigSecretRef.Name)

		sec := corev1.Secret{}
		require.NoError(t, cl.Get(ctx,
			types.NamespacedName{Namespace: "default", Name: "machineconfig-demo-cp1"}, &sec))
		mc := sec.Data[asset.ConfigFilename]
		require.NotEmpty(t, mc)
		assert.Contains(t, string(mc), "type: controlplane")
		assert.Contains(t, string(mc), "hostname: demo-cp1")
		assert.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(mc)), n.Status.ConfigDigest)
		configDigest = n.Status.ConfigDigest

		require.Len(t, sec.OwnerReferences, 1, "config Secret must die with its node")
		assert.Equal(t, "TalosNode", sec.OwnerReferences[0].Kind)
	})

	t.Run("configISO BootAsset packs the medium", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-cp1-config"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy: osv1alpha1.StrategyConfigISO,
				NodeRef:  &osv1alpha1.Reference{Name: "demo-cp1"},
			},
		}))
		require.Eventually(t, func() bool {
			_, c := assetCondition("demo-cp1-config")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "Ready never went True")

		ba, c := assetCondition("demo-cp1-config")
		assert.Equal(t, osv1alpha1.ReasonArtifactBuilt, c.Reason)
		require.NotNil(t, ba.Status.ArtifactSecretRef)

		sec := corev1.Secret{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{
			Namespace: ba.Status.ArtifactSecretRef.Namespace, Name: ba.Status.ArtifactSecretRef.Name}, &sec))
		iso := sec.Data["asset.iso"]
		require.NotEmpty(t, iso)
		assert.Equal(t, int64(len(iso)), ba.Status.Size)
		assert.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(iso)), ba.Status.Digest)
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
			ObjectMeta: metav1.ObjectMeta{Name: "install-drbd"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyFactory,
				TalosVersion: "v1.13.4",
				Schematic: &osv1alpha1.Schematic{
					Customization: osv1alpha1.SchematicCustomization{
						SystemExtensions: osv1alpha1.SystemExtensions{
							OfficialExtensions: []string{"siderolabs/drbd"},
						},
					},
				},
			},
		}))
		require.Eventually(t, func() bool {
			_, c := assetCondition("install-drbd")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick)

		ba, c := assetCondition("install-drbd")
		assert.Equal(t, osv1alpha1.ReasonURLComputed, c.Reason)
		assert.Regexp(t,
			`^https://`+regexp.QuoteMeta(factory.RegistryURL)+`/image/[0-9a-f]{64}/v1\.13\.4/metal-amd64\.iso$`,
			ba.Status.URL, "the URL must name the factory the operator was given, not the public one")
		assert.Empty(t, ba.Status.Digest, "remote artifact is addressed, not verified")

		// The point of the strategy: Ready means the factory can serve this, not
		// merely that a URL was computable. The id in the URL is the id posted.
		id := strings.Split(ba.Status.URL, "/")[4]
		assert.Contains(t, posted(), id, "the schematic must be registered before Ready")
	})

	t.Run("build waits for the render and follows it", func(t *testing.T) {
		// BootAsset first, node later: holds visibly, then flips on the node watch.
		require.NoError(t, cl.Create(ctx, &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "early"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy: osv1alpha1.StrategyConfigISO,
				NodeRef:  &osv1alpha1.Reference{Name: "demo-w1"},
			},
		}))
		require.Eventually(t, func() bool {
			_, c := assetCondition("early")
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonNodeNotFound
		}, waitFor, tick, "missing node must hold the build")

		require.NoError(t, cl.Create(ctx, node("demo-w1")))
		require.Eventually(t, func() bool {
			_, c := assetCondition("early")
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "the build must follow the render without a nudge")
	})

	t.Run("broken clusterRef is alert-and-hold", func(t *testing.T) {
		orphan := node("orphan")
		orphan.Spec.ClusterRef.Name = "no-such-cluster"
		require.NoError(t, cl.Create(ctx, orphan))
		require.Eventually(t, func() bool {
			_, c := nodeCondition("orphan", osv1alpha1.ConditionRendered)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonClusterNotFound
		}, waitFor, tick)
	})

	t.Run("check-in ladder against the scripted machine", func(t *testing.T) {
		// The hand-off starts the poll: a Host names demo-cp2, the machine is off.
		require.NoError(t, cl.Create(ctx, node("demo-cp2")))
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "fakebox"},
			Spec: metalv1alpha1.HostSpec{
				OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "demo-cp2"}},
			},
		}))
		require.Eventually(t, func() bool {
			_, c := nodeCondition("demo-cp2", osv1alpha1.ConditionMachineReachable)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonDialFailed
		}, waitFor, tick, "an off machine must observe as DialFailed")
		_, ready := nodeCondition("demo-cp2", osv1alpha1.ConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, osv1alpha1.ReasonAwaitingContact, ready.Reason)

		// The machine comes up blank: only the maintenance service answers.
		machine.SetUnreachable(false)
		require.Eventually(t, func() bool {
			n, c := nodeCondition("demo-cp2", osv1alpha1.ConditionMachineReachable)
			return c != nil && c.Status == metav1.ConditionTrue &&
				c.Reason == osv1alpha1.ReasonMaintenanceMode && n.Status.Stage == "maintenance"
		}, waitFor, tick, "a blank machine must observe as MaintenanceMode")
		n, _ := nodeCondition("demo-cp2", osv1alpha1.ConditionMachineReachable)
		assert.Equal(t, "v1.13.4", n.Status.MachineVersion)

		// The CLI's role: config lands over the insecure dial, the machine
		// converges — the check-in flips to the authenticated API and Ready.
		conn, err := machine.Dialer()(ctx, talos.Target{Endpoint: "10.77.38.202", Insecure: true})
		require.NoError(t, err)
		require.NoError(t, conn.ApplyConfiguration(ctx, []byte("cfg")))
		require.NoError(t, conn.Close())
		machine.SetStage("running", true)
		require.Eventually(t, func() bool {
			n, c := nodeCondition("demo-cp2", osv1alpha1.ConditionReady)
			return c != nil && c.Status == metav1.ConditionTrue &&
				c.Reason == osv1alpha1.ReasonNodeReady && n.Status.Stage == "running"
		}, waitFor, tick, "a converged machine must observe as NodeReady")
		_, reach := nodeCondition("demo-cp2", osv1alpha1.ConditionMachineReachable)
		assert.Equal(t, osv1alpha1.ReasonMachineAPI, reach.Reason)

		// Release: the observations leave with the hand-off.
		host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "fakebox"}}
		require.NoError(t, cl.Delete(ctx, host))
		require.Eventually(t, func() bool {
			n, c := nodeCondition("demo-cp2", osv1alpha1.ConditionMachineReachable)
			return c == nil && n.Status.Stage == ""
		}, waitFor, tick, "observations must leave with the hand-off")
	})

	t.Run("CEL refuses mispaired specs", func(t *testing.T) {
		noNode := &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-configiso"},
			Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO},
		}
		assert.Error(t, cl.Create(ctx, noNode), "configISO without nodeRef must be refused")

		crossed := &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-factory"},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyFactory,
				TalosVersion: "v1.13.4",
				NodeRef:      &osv1alpha1.Reference{Name: "demo-cp1"},
			},
		}
		assert.Error(t, cl.Create(ctx, crossed), "factory with nodeRef must be refused")
	})
}
