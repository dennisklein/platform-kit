//go:build e2e

// The VM tier — everything the container tier cannot reach: a real UEFI boot,
// the config travelling over the BMC's virtual media instead of an insecure
// apply, a real Talos install to a blank disk, and the reboot that proves it.
// The kind cluster stores the CRs and the managers run in-process exactly as
// in the container tier; the machine behind the simulated BMC is a QEMU VM
// whose installer CD is the factory Talos ISO (downloaded once into .cache/)
// and whose second CD is the config ISO the sim pulled from the boot verb's
// asset server — the two media of the metal story.
//
// The VM lives behind user-mode networking with 1:1 port forwards on a
// dedicated loopback address, which doubles as the node's address in
// certificates and endpoints — reachable identically from host and guest.
//
// Needs qemu-system-x86_64, /dev/kvm, OVMF, and the dev cluster; run via
// `mise run e2e:qemu`.
package e2e_test

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	qemumachine "github.com/dennisklein/platform-kit/metal/internal/machine/qemu"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

const (
	qemuNS        = "pk-e2e-qemu"
	qemuAssetName = "pk-e2e-qemu-smc"
	qemuNodeName  = "e2e-qemu-cp1"
	qemuCluster   = "pk-e2e-qemu"
	// qemuNodeIP is a dedicated loopback address: the guest's forwarded ports
	// surface here on the host, and inside the guest the same address reaches
	// the guest's own services (all of 127/8 routes to lo) — one address for
	// endpoints and certificates on both sides, colliding with nothing.
	qemuNodeIP     = "127.42.16.10"
	qemuDiskSerial = "pk-e2e-disk"

	// qemuWaitFor bounds phases after the bootstrap verb already waited for
	// readiness — check-in lag, not machine work.
	qemuWaitFor = 5 * time.Minute
	// qemuRebootWait bounds a full power-on from the installed disk.
	qemuRebootWait = 5 * time.Minute
)

// qemuBootKernelArgs are what the boot medium cannot know about itself. The
// factory ISO ships `console=tty0 talos.platform=metal` and nothing else, so
// without these two the machine boots deaf and mute: it never looks at the
// config medium beside it, and everything the kernel says goes to a VGA console
// no one is capturing.
var qemuBootKernelArgs = []string{
	// Read the config off the second medium — the metal-iso volume the boot
	// verb had the BMC mount. Talos ignores it unless told to look.
	"talos.config=metal-iso",
	// Speak on the serial port the harness records (qemu -serial file:).
	"console=ttyS0",
}

// qemuInstallKernelArgs carry the console across the install. The boot medium's
// args die with the installer, and machine.install.extraKernelArgs is not the
// way back: Talos takes the installed cmdline from the image's baked UKI
// (grubUseUKICmdline) and refuses the config field while it does. So the
// installed system's console is a property of the installer image — a schematic,
// like the medium's. It omits talos.config: the config lives in STATE after the
// install, not on a medium that has been ejected.
var qemuInstallKernelArgs = []string{"console=ttyS0"}

func TestE2EQemu(t *testing.T) {
	ctx := t.Context()
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// Safety rail: this test installs CRDs and runs managers — kind only.
	raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(raw.CurrentContext, "kind-"),
		"current kube-context %q is not a kind cluster — `mise run lab:up` and switch to kind-pk-metal", raw.CurrentContext)
	cfg, err := ctrl.GetConfig()
	require.NoError(t, err)

	for _, tool := range []string{"qemu-system-x86_64", "qemu-img"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not on PATH — the VM tier needs it", tool)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf("no usable /dev/kvm — the VM tier needs hardware acceleration: %v", err)
	}
	talosVersion := e2eTalosVersion
	installerISO := ensureTalosISO(ctx, t, talosVersion, qemuBootKernelArgs)
	// The installer image the rendered config names is pulled by the guest, mid
	// install, where the only witness is a serial port. Register it here so an
	// unknown schematic fails now, in a sentence, instead of there.
	_, installID := factoryISO(t, talosVersion, qemuInstallKernelArgs)
	registerSchematic(ctx, t, qemuInstallKernelArgs, installID)

	// ── the machine: a blank-disk UEFI VM with the installer CD attached ─────
	machine := qemumachine.New(qemumachine.Options{
		Name:         "pk-e2e-qemu-cp1",
		Dir:          filepath.Join("..", "..", ".cache", "vm-e2e-qemu-cp1"),
		InstallerISO: installerISO,
		DiskSerial:   qemuDiskSerial,
		HostIP:       qemuNodeIP,
		ForwardPorts: []int{50000, 6443},
	})
	require.NoError(t, machine.Remove(ctx), "start from a blank disk")
	require.NoError(t, machine.Ensure(ctx))
	t.Cleanup(func() {
		// Keep the VM's world (disk, console.log, qemu.log) on failure — the
		// serial console is the only witness to a boot that never reached the
		// machine API. The next run's Remove clears it before starting fresh.
		if t.Failed() {
			_ = machine.Stop(context.Background())
			t.Logf("VM preserved for inspection at %s (console: %s)", machine.Dir(), machine.ConsoleLog())
			return
		}
		_ = machine.Remove(context.Background())
	})

	// ── the simulated BMC, with the VM attached ──────────────────────────────
	fixture, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	state := sim.NewState(fixture)
	systemID := fixture.Systems[0].ID
	state.AttachMachine(systemID, machine)
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

	_, err = envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("..", "..", "config", "crd")},
	})
	require.NoError(t, err, "install the CRDs onto the dev cluster")

	cleanSlate(ctx, t, cl, qemuTier)
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: qemuNS}}))
	t.Cleanup(func() { cleanSlate(context.Background(), t, cl, qemuTier) })

	bundleBytes := newBundle(t, talosVersion)
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-ca", Namespace: qemuNS},
			Data: map[string][]byte{"ca.crt": pem.EncodeToMemory(
				&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})}},
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-factory", Namespace: qemuNS},
			Data: map[string][]byte{"username": []byte("admin"), "password": []byte("dev")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "talos-secrets", Namespace: qemuNS},
			Data: map[string][]byte{render.BundleKey: bundleBytes}},
	} {
		require.NoError(t, cl.Create(ctx, sec))
	}

	// The managers this tier exercises — the BMC spine, the operation executor
	// (whose setup also registers the host→operation field index the Host
	// reconciler joins over), and the OS check-in. Approval, convergence, and
	// asset artifacts are the container tier's and envtest's territory.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: qemuNS,
		OperationsNamespace:  qemuNS,
		Proposer:             "pk-metal-e2e-qemu",
		EventSource:          "pk-metal-e2e-qemu",
		Ledger:               led,
		TLS:                  &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: "bmc-ca"}},
		PollInterval:         5 * time.Second,
		Factory:              qemuFactory,
		TalosDial:            machineapi.Dial,
		CheckInterval:        2 * time.Second,
	}, controller.LaneAsset, controller.LaneEndpoint, controller.LaneHost, controller.LaneHostClaim,
		controller.LaneHostOperation, controller.LaneTalosNode))
	mgrCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()

	waitHost := func(pred func(metalv1alpha1.Host) bool, timeout time.Duration, msg string) metalv1alpha1.Host {
		var last metalv1alpha1.Host
		require.Eventually(t, func() bool {
			h := metalv1alpha1.Host{}
			if err := cl.Get(ctx, client.ObjectKey{Name: hostName}, &h); err != nil {
				return false
			}
			last = h
			return pred(h)
		}, timeout, tick, "%s (last: phase=%s blocked=%s)", msg, last.Status.Phase, last.Status.BlockedOn)
		return last
	}

	// The OS payload, metal-shaped: a real install disk selected by the VM
	// disk's serial, and the loopback address in both certificates so the
	// secure dials verify on it. No links: the guest takes its user-mode
	// address over the default DHCP, which any link document would disable.
	talosCluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: qemuCluster},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion:      talosVersion,
			KubernetesVersion: e2eKubernetesVersion,
			Endpoint:          "https://" + qemuNodeIP + ":6443",
			CertSANs:          []string{qemuNodeIP},
			SecretsRef:        osv1alpha1.SecretReference{Namespace: qemuNS, Name: "talos-secrets"},
		},
	}
	talosNode := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: qemuNodeName},
		Spec: osv1alpha1.TalosNodeSpec{
			ClusterRef:          osv1alpha1.Reference{Name: qemuCluster},
			Role:                osv1alpha1.RoleControlPlane,
			Address:             qemuNodeIP,
			InstallDiskSelector: osv1alpha1.DiskSelector{Serial: qemuDiskSerial},
			Schematic:           kernelArgsSchematic(qemuInstallKernelArgs),
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
			ObjectMeta: metav1.ObjectMeta{Name: qemuAssetName},
			Spec: metalv1alpha1.AssetSpec{
				CID:            "CI-" + qemuAssetName,
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
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-qemu", Namespace: qemuNS},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: qemuAssetName},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: qemuNodeName}},
			},
		}))
		waitHost(func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostProvisioning
		}, waitFor, "claim with OS intent never reached Provisioning")
	})

	rung("boot verb delivers the config over the BMC's media", func(t *testing.T) {
		// The config, rendered in-process (the renderer has its own golden
		// tests); the boot verb is what is under test: the day-0 media ladder
		// against a BMC whose machine really boots what it mounts.
		bundle, err := render.LoadBundle(bundleBytes, talosVersion)
		require.NoError(t, err)
		installer, err := qemuFactory.InstallerImage(talosNode.Spec.Schematic, talosVersion)
		require.NoError(t, err)
		cfgBytes, err := render.Node(render.Input{
			Cluster: talosCluster, Node: talosNode, Bundle: bundle, InstallerImage: installer,
		})
		require.NoError(t, err)
		cfgPath := filepath.Join(t.TempDir(), "machineconfig.yaml")
		require.NoError(t, os.WriteFile(cfgPath, cfgBytes, 0o600))

		cmd := exec.CommandContext(ctx, "go", "run", "../../cmd/pk-metal",
			"boot", "--config", cfgPath, "--node", qemuNodeName,
			"--listen", "127.0.0.1:0", "--ttl", "5m",
			"--user", "admin", "--insecure", srv.URL)
		cmd.Env = append(os.Environ(), "PK_METAL_BMC_PASSWORD=dev")
		out, err := cmd.CombinedOutput()
		t.Logf("boot output:\n%s", out)
		require.NoError(t, err, "the boot verb failed")

		running, err := machine.Running(ctx)
		require.NoError(t, err)
		assert.True(t, running, "the media boot left the VM running")
	})

	rung("bootstrap verb waits out install and first boot", func(t *testing.T) {
		// No in-band config: the config travelled on the medium, so the
		// machine comes up configured and only answers securely — the metal
		// flow, end to end. The wait covers install, reboot, and etcd.
		crDir := t.TempDir()
		require.NoError(t, render.WriteFiles(crDir, talosCluster, []osv1alpha1.TalosNode{*talosNode}))
		secretPath := filepath.Join(t.TempDir(), render.BundleKey)
		require.NoError(t, os.WriteFile(secretPath, bundleBytes, 0o600))

		cmd := exec.CommandContext(ctx, "go", "run", "../../cmd/pk-metal",
			"bootstrap", "--dir", crDir, "--secrets", secretPath, "--node", qemuNodeName,
			"--timeout", "20m")
		out, err := cmd.CombinedOutput()
		t.Logf("bootstrap output:\n%s", out)
		if err != nil {
			t.Logf("guest console tail:\n%s", consoleTail(machine.ConsoleLog()))
		}
		require.NoError(t, err, "the bootstrap verb failed")

		h := waitHost(func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostProvisioned
		}, qemuWaitFor, "the check-in never advanced the host to Provisioned")
		assert.Equal(t, osv1alpha1.ReasonNodeReady,
			meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady).Reason)

		n := &osv1alpha1.TalosNode{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: qemuNodeName}, n))
		assert.Equal(t, "running", n.Status.Stage)
		assert.Equal(t, talosVersion, n.Status.Version)
	})

	rung("the installed disk boots alone", func(t *testing.T) {
		// Power off, withdraw the installer CD (the config medium is already
		// ejected — the boot verb withdrew it), power on: the only bootable
		// thing left is what the install wrote to disk.
		require.NoError(t, state.SetPower(ctx, systemID, bmc.ActionForceOff))
		waitHost(func(h metalv1alpha1.Host) bool {
			return !meta.IsStatusConditionTrue(h.Status.Conditions, metalv1alpha1.ConditionBooted)
		}, qemuWaitFor, "the check-in never noticed the machine going dark")

		machine.DetachInstaller()
		require.NoError(t, state.SetPower(ctx, systemID, bmc.ActionOn))
		h := waitHost(func(h metalv1alpha1.Host) bool {
			return meta.IsStatusConditionTrue(h.Status.Conditions, metalv1alpha1.ConditionBooted) &&
				h.Status.Phase == metalv1alpha1.HostProvisioned
		}, qemuRebootWait, "the installed disk never came back Ready on its own")
		assert.Equal(t, osv1alpha1.ReasonNodeReady,
			meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady).Reason,
			"a machine running from disk answers ready without any medium")
	})
}

// qemuTier is what the VM tier creates.
var qemuTier = tier{namespace: qemuNS, asset: qemuAssetName, node: qemuNodeName, cluster: qemuCluster}

// kernelArgsSchematic is an image customized only by its kernel command line.
func kernelArgsSchematic(args []string) *osv1alpha1.Schematic {
	return &osv1alpha1.Schematic{
		Customization: osv1alpha1.SchematicCustomization{ExtraKernelArgs: args},
	}
}

// qemuFactory is the public image factory, addressed the way the operator
// addresses it — the harness fetches media the operator would not, but it
// computes and registers through the same code the reconcilers use.
var qemuFactory = render.NewFactory("")

// factoryISO is the URL of the boot ISO for a set of kernel args, and the
// schematic id it is addressed by.
func factoryISO(t *testing.T, version string, kernelArgs []string) (string, string) {
	t.Helper()
	url, err := qemuFactory.ISOURL(kernelArgsSchematic(kernelArgs), version)
	require.NoError(t, err)
	// .../image/<schematic id>/<version>/metal-amd64.iso
	seg := strings.Split(url, "/")
	require.Greater(t, len(seg), 3, "factory URL %q carries no schematic id", url)
	id := seg[len(seg)-3]
	require.Len(t, id, 64, "factory URL %q carries no schematic id", url)
	return url, id
}

// registerSchematic tells the image factory about a schematic before the
// harness fetches an image addressed by it. The reconcilers do this for the
// artifacts they own; this covers the media the harness downloads directly.
func registerSchematic(ctx context.Context, t *testing.T, kernelArgs []string, wantID string) {
	t.Helper()
	id, err := qemuFactory.Register(ctx, kernelArgsSchematic(kernelArgs))
	require.NoError(t, err, "register the schematic with the image factory")
	require.Equal(t, wantID, id)
}

// ensureTalosISO downloads the factory boot ISO for the pinned Talos version and
// kernel args once, into the module's .cache/ — later runs boot from the cache.
// The factory content-addresses a schematic, so its id keys the cache: changing
// the kernel args fetches a new ISO instead of quietly booting the stale one.
func ensureTalosISO(ctx context.Context, t *testing.T, version string, kernelArgs []string) string {
	t.Helper()
	url, id := factoryISO(t, version, kernelArgs)
	cacheDir := filepath.Join("..", "..", ".cache")
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	path := filepath.Join(cacheDir, "talos-"+version+"-"+id[:8]+"-metal-amd64.iso")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	registerSchematic(ctx, t, kernelArgs, id)

	t.Logf("downloading %s (cached at %s for later runs)", url, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "factory refused the ISO")

	tmp, err := os.CreateTemp(cacheDir, "talos-*.iso.part")
	require.NoError(t, err)
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = io.Copy(tmp, resp.Body)
	require.NoError(t, tmp.Close())
	require.NoError(t, err, "ISO download")
	require.NoError(t, os.Rename(tmp.Name(), path))
	return path
}

// consoleTail is the last stretch of the guest's serial console — the first
// witness when a boot never reaches the machine API.
func consoleTail(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(no console log)"
	}
	const tail = 16384
	if len(raw) > tail {
		raw = raw[len(raw)-tail:]
	}
	return string(raw)
}
