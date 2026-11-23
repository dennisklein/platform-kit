// The provisioning suite: a claim naming a rendered TalosNode gets one boot
// operation proposed; a person approves it; the executor mounts the installer
// and the granted config medium on a sim BMC, sets a Once UEFI override and
// cycles power; a scripted check-in ends the hand-off (media ejected, grants
// revoked) and the Host reaches Provisioned; deleting the claim powers the
// machine off and hands it back. Around it: the single-medium hold and its
// maintenance-mode path with automatic approval, the approval policy's
// identity rules, and the claim protection that keeps a claimed machine
// through the deletion of its Asset.
package envtest_test

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// fakeFactory answers schematic registrations with the locally computed id
// and serves a stand-in install ISO under any .iso path.
func fakeFactory(t *testing.T) *httptest.Server {
	t.Helper()
	installer := []byte("stand-in installer image")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /schematics", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s, err := schematic.Unmarshal(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, _ := s.ID()
		normalized, _ := s.Marshal()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "schematic": string(normalized)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".iso") {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "installer.iso", time.Time{}, strings.NewReader(string(installer)))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// freeAddr reserves a loopback port for a listener started later.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestProvisioning(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset — run via `mise run envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	// Two BMCs: one that holds several media at once, one that holds one.
	dualFix, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	dual, dualState := simServer(t, "supermicro-x12")
	singleFix, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	single, singleState := simServer(t, "huawei-2288h-v5")
	factory := fakeFactory(t)

	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.yaml")
	require.NoError(t, os.WriteFile(ledgerPath, []byte(fmt.Sprintf("%q: %s\n%q: %s\n",
		dualFix.Manager.MAC, dual.URL, singleFix.Manager.MAC, single.URL)), 0o600))
	led, err := ledger.Load(ledgerPath)
	require.NoError(t, err)

	s, err := pkscheme.New()
	require.NoError(t, err)
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	ctx := t.Context()

	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: "default"},
			Data: map[string][]byte{"username": []byte("admin"), "password": []byte("dev")}},
	} {
		require.NoError(t, cl.Create(ctx, sec))
	}
	operator := whoAmI(t, ctx, cl)
	installApprovalPolicy(t, ctx, cl, operator)
	installClaimPolicy(t, ctx, cl)

	userClient := func(name string, groups ...string) client.Client {
		u, err := env.AddUser(envtest.User{Name: name, Groups: append(groups, "system:masters")}, nil)
		require.NoError(t, err)
		c, err := client.New(u.Config(), client.Options{Scheme: s})
		require.NoError(t, err)
		return c
	}
	aliceCl := userClient("alice")
	bobCl := userClient("bob")
	carolCl := userClient("carol", "pk-metal:admins")

	cert, err := asset.EphemeralCert()
	require.NoError(t, err)
	mediaAddr := freeAddr(t)
	media, err := asset.NewService(mediaAddr, "https://"+mediaAddr, cert)
	require.NoError(t, err)

	imageFactory := render.NewFactory(strings.TrimPrefix(factory.URL, "https://"))
	imageFactory.HTTPClient = factory.Client()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     s,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Add(media))
	require.NoError(t, (&controller.AssetReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Ledger: led,
		TLS:      &metalv1alpha1.EndpointTLS{InsecureSkipVerify: true},
		Recorder: mgr.GetEventRecorder("pk-metal"),
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.ManagementEndpointReconciler{
		Client: mgr.GetClient(), CredentialsNamespace: "default", PollInterval: pollInterval,
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.HostReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	require.NoError(t, (&controller.HostClaimReconciler{
		Client: mgr.GetClient(), OperationsNamespace: "default", Proposer: operator,
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.ProtectionReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	require.NoError(t, (&controller.HostOperationReconciler{
		Client: mgr.GetClient(), CredentialsNamespace: "default", Media: media,
		Recorder: mgr.GetEventRecorder("pk-metal"), WaitInterval: 200 * time.Millisecond,
	}).SetupWithManager(mgr))
	require.NoError(t, (&controller.BootAssetReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Namespace: "default", Factory: imageFactory,
	}).SetupWithManager(mgr))
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()

	// --- helpers -----------------------------------------------------------

	createAsset := func(name, serial, mac string) {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: metalv1alpha1.AssetSpec{CID: "CI-" + name, Serial: serial, BMCMac: mac,
				CredentialsRef: corev1.LocalObjectReference{Name: "bmc-creds"}},
		}))
	}
	// hostBehind finds the Host an endpoint fronts — host names derive from
	// fixture serials, which this suite never spells out.
	hostBehind := func(endpoint string) metalv1alpha1.Host {
		var found metalv1alpha1.Host
		require.Eventually(t, func() bool {
			hosts := &metalv1alpha1.HostList{}
			if err := cl.List(ctx, hosts); err != nil {
				return false
			}
			for _, h := range hosts.Items {
				for _, p := range h.Status.ManagedBy {
					if p.EndpointRef.Name == endpoint && h.Status.Phase == metalv1alpha1.HostAvailable {
						found = h
						return true
					}
				}
			}
			return false
		}, waitFor, tick, "no Available host behind endpoint %s", endpoint)
		return found
	}
	getHost := func(name string) (metalv1alpha1.Host, error) {
		h := metalv1alpha1.Host{}
		return h, cl.Get(ctx, client.ObjectKey{Name: name}, &h)
	}
	waitHost := func(name string, ok func(metalv1alpha1.Host) bool) metalv1alpha1.Host {
		var last metalv1alpha1.Host
		if !assert.Eventually(t, func() bool {
			last, _ = getHost(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("host %s: condition never met, last %+v", name, last.Status)
		}
		return last
	}
	claimOf := func(name string) (metalv1alpha1.HostClaim, error) {
		c := metalv1alpha1.HostClaim{}
		return c, cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &c)
	}
	provisioned := func(c metalv1alpha1.HostClaim) *metav1.Condition {
		return findCondition(c.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	}
	waitClaim := func(name string, ok func(metalv1alpha1.HostClaim) bool) metalv1alpha1.HostClaim {
		var last metalv1alpha1.HostClaim
		if !assert.Eventually(t, func() bool {
			last, _ = claimOf(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("claim %s: condition never met, last %+v", name, last.Status)
		}
		return last
	}
	provisionedReason := func(reason string) func(metalv1alpha1.HostClaim) bool {
		return func(c metalv1alpha1.HostClaim) bool {
			p := provisioned(c)
			return p != nil && p.Reason == reason
		}
	}
	opsFor := func(hostName, purpose string) []metalv1alpha1.HostOperation {
		ops := &metalv1alpha1.HostOperationList{}
		require.NoError(t, cl.List(ctx, ops, client.InNamespace("default"),
			client.MatchingLabels{"metal.pk.io/purpose": purpose}))
		var out []metalv1alpha1.HostOperation
		for _, op := range ops.Items {
			if op.Spec.HostRef.Name == hostName {
				out = append(out, op)
			}
		}
		return out
	}
	waitOp := func(key types.NamespacedName, ok func(metalv1alpha1.HostOperation) bool) metalv1alpha1.HostOperation {
		var last metalv1alpha1.HostOperation
		if !assert.Eventually(t, func() bool {
			last = metalv1alpha1.HostOperation{}
			_ = cl.Get(ctx, key, &last)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("operation %s: condition never met, last %+v", key, last.Status)
		}
		return last
	}
	executing := func(step string) func(metalv1alpha1.HostOperation) bool {
		return func(op metalv1alpha1.HostOperation) bool {
			ex := findCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
			return ex != nil && ex.Reason == metalv1alpha1.ReasonExecuting && strings.Contains(ex.Message, step)
		}
	}
	succeeded := func(op metalv1alpha1.HostOperation) bool {
		ex := findCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		return ex != nil && ex.Status == metav1.ConditionTrue
	}
	// renderedNode creates a TalosNode whose render is scripted: the config
	// Secret holds a stand-in config and the status says Rendered.
	renderedNode := func(name, cluster string) []byte {
		cfgBytes := []byte("version: v1alpha1\nmachine:\n  type: worker # stand-in for " + name + "\n")
		require.NoError(t, cl.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-render", Namespace: "default"},
			Data:       map[string][]byte{asset.ConfigFilename: cfgBytes},
		}))
		node := &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: cluster}, Role: osv1alpha1.RoleWorker,
				Address: "192.0.2.11", InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "sim-disk"}},
		}
		require.NoError(t, cl.Create(ctx, node))
		node.Status.ConfigSecretRef = &osv1alpha1.SecretReference{Namespace: "default", Name: name + "-render"}
		node.Status.ConfigDigest = digest.FromBytes(cfgBytes).String()
		node.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionRendered, Status: metav1.ConditionTrue,
			Reason: osv1alpha1.ReasonConfigRendered, LastTransitionTime: metav1.Now()}}
		require.NoError(t, cl.Status().Update(ctx, node))
		return cfgBytes
	}
	checkIn := func(name string, conds ...metav1.Condition) {
		require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node := &osv1alpha1.TalosNode{}
			if err := cl.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
				return err
			}
			for _, c := range conds {
				c.LastTransitionTime = metav1.Now()
				found := false
				for i := range node.Status.Conditions {
					if node.Status.Conditions[i].Type == c.Type {
						node.Status.Conditions[i], found = c, true
					}
				}
				if !found {
					node.Status.Conditions = append(node.Status.Conditions, c)
				}
			}
			return cl.Status().Update(ctx, node)
		}))
	}
	machineAPI := []metav1.Condition{
		{Type: osv1alpha1.ConditionMachineReachable, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonMachineAPI},
		{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady},
	}
	mediaOf := func(st *sim.State) map[string]string {
		out := map[string]string{}
		ms, err := st.Media(ctx)
		require.NoError(t, err)
		for _, m := range ms {
			if m.Inserted {
				out[m.ID] = m.ImageURL
			}
		}
		return out
	}
	grantStatus := func(url string) int {
		// The media service's ephemeral certificate is self-signed; the
		// status code is what is under test, not the chain.
		insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec
		resp, err := insecure.Get(url)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	stripUserinfo := func(raw string) string {
		if i := strings.Index(raw, "@"); i >= 0 {
			return "https://" + raw[i+1:]
		}
		return raw
	}

	// The VAP must be enforcing before anything relies on it.
	require.Eventually(t, func() bool {
		probe := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "vap-probe", Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef: metalv1alpha1.Reference{Name: "no-such-host"}, ProposedBy: operator,
				Actions:  []metalv1alpha1.HostOperationAction{{PowerOff: &metalv1alpha1.PowerOffAction{}}},
				Approval: metalv1alpha1.OperationApproval{ApprovedBy: "someone"},
			},
		}
		err := cl.Create(ctx, probe)
		if err == nil {
			require.NoError(t, cl.Delete(ctx, probe))
			return false
		}
		return strings.Contains(err.Error(), "pre-approved")
	}, waitFor, tick, "the ValidatingAdmissionPolicy never became enforcing")

	require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.0", KubernetesVersion: "v1.36.2",
			Endpoint:   "https://192.0.2.10:6443",
			SecretsRef: osv1alpha1.SecretReference{Namespace: "default", Name: "lab-secrets"}},
	}))

	t.Run("the approval policy holds identities", func(t *testing.T) {
		op := func(name, proposer string, required bool) *metalv1alpha1.HostOperation {
			return &metalv1alpha1.HostOperation{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: metalv1alpha1.HostOperationSpec{
					HostRef: metalv1alpha1.Reference{Name: "no-such-host"}, ProposedBy: proposer,
					Actions:  []metalv1alpha1.HostOperationAction{{PowerOff: &metalv1alpha1.PowerOffAction{}}},
					Approval: metalv1alpha1.OperationApproval{Required: ptr.To(required)},
				},
			}
		}
		err := bobCl.Create(ctx, op("forged", "alice", true))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must name you", "the proposer record cannot name someone else")

		err = bobCl.Create(ctx, op("skip-gate", "bob", false))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only the operator or an admin", "a person cannot skip the gate")

		admin := op("admin-cleared", "carol", false)
		require.NoError(t, carolCl.Create(ctx, admin), "an admin may create a pre-cleared operation")
		require.NoError(t, cl.Delete(ctx, admin))

		own := op("own", "bob", true)
		require.NoError(t, bobCl.Create(ctx, own))
		approveAs := func(c client.Client, who string) error {
			return c.Patch(ctx, own, client.RawPatch(types.MergePatchType,
				[]byte(`{"spec":{"approval":{"approvedBy":"`+who+`"}}}`)))
		}
		err = approveAs(bobCl, "bob")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "your own proposal")

		err = bobCl.Patch(ctx, own, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"required":false}}}`)))
		require.Error(t, err, "flipping the gate off is an approval without a name")
		assert.Contains(t, err.Error(), "frozen")

		require.NoError(t, approveAs(aliceCl, "alice"), "another person approves")
		require.NoError(t, cl.Delete(ctx, own))
	})

	t.Run("only an admin points a claim at a node or waives its approval", func(t *testing.T) {
		claim := func(name string, node string, approval metalv1alpha1.ProvisioningApproval) *metalv1alpha1.HostClaim {
			c := &metalv1alpha1.HostClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "no-such-asset"}},
			}
			if node != "" {
				c.Spec.OS = &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: node}}
			}
			if approval != "" {
				c.Spec.Provisioning = &metalv1alpha1.ClaimProvisioning{Approval: approval}
			}
			return c
		}
		// The claim policy came up after the operation policy was probed.
		require.Eventually(t, func() bool {
			err := bobCl.Create(ctx, claim("bob-cp", "control-plane-1", ""))
			if err == nil {
				require.NoError(t, cl.Delete(ctx, claim("bob-cp", "", "")))
				return false
			}
			return strings.Contains(err.Error(), "only an admin")
		}, waitFor, tick, "a claim author names a node: denied")

		err := bobCl.Create(ctx, claim("bob-auto", "", metalv1alpha1.ProvisioningApprovalAutomatic))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "waive provisioning approval")

		plain := claim("bob-plain", "", "")
		require.NoError(t, bobCl.Create(ctx, plain), "reserving a machine needs no admin")
		err = bobCl.Patch(ctx, plain, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"os":{"nodeRef":{"name":"control-plane-1"}}}}`)))
		require.Error(t, err, "nor does adding the intent later get around it")
		assert.Contains(t, err.Error(), "only an admin")
		err = bobCl.Patch(ctx, plain, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"assetRef":{"name":"another-asset"}}}`)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "assetRef is immutable")
		require.NoError(t, carolCl.Patch(ctx, plain, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"os":{"nodeRef":{"name":"control-plane-1"}},"provisioning":{"approval":"Automatic"}}}`))),
			"an admin may")
		require.NoError(t, bobCl.Patch(ctx, plain, client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"labels":{"team":"bob"}}}`))), "other edits stay open to the author")
		err = carolCl.Patch(ctx, plain, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"os":{"nodeRef":{"name":"control-plane-2"}}}}`)))
		require.Error(t, err, "not even an admin re-targets a standing intent")
		assert.Contains(t, err.Error(), "os.nodeRef is immutable")
		require.NoError(t, cl.Delete(ctx, plain))
	})

	var dualHost string
	var configISO []byte

	t.Run("a claim with OS intent proposes one boot operation", func(t *testing.T) {
		createAsset("lab-dual", dualFix.Chassis.BoardSerial, dualFix.Manager.MAC)
		dualHost = hostBehind("lab-dual").Name
		cfgBytes := renderedNode("node-dual", "lab")
		var err error
		configISO, err = asset.BuildConfigISO(cfgBytes)
		require.NoError(t, err)

		require.NoError(t, carolCl.Create(ctx, &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "dual", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "lab-dual"},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "node-dual"}},
			},
		}))
		c := waitClaim("dual", func(c metalv1alpha1.HostClaim) bool {
			p := provisioned(c)
			return p != nil && strings.Contains(p.Message, "awaits approval")
		})
		assert.Equal(t, metalv1alpha1.ClaimBound, c.Status.Phase)

		ops := opsFor(dualHost, "provision")
		require.Len(t, ops, 1, "exactly one boot operation")
		op := ops[0]
		assert.Equal(t, operator, op.Spec.ProposedBy)
		assert.Equal(t, "Provisioning", op.Spec.Reason)
		require.NotNil(t, op.Spec.Approval.Required)
		assert.True(t, *op.Spec.Approval.Required, "approval is required by default")

		var names []string
		for _, a := range op.Spec.Actions {
			switch {
			case a.InsertMedia != nil && a.InsertMedia.BootAssetRef != nil:
				names = append(names, "insert:"+a.InsertMedia.MediaID+":"+a.InsertMedia.BootAssetRef.Name)
			case a.SetBootOverride != nil:
				names = append(names, "override:"+a.SetBootOverride.Target+":"+a.SetBootOverride.Mode)
			case a.EjectMedia != nil:
				names = append(names, "eject:"+a.EjectMedia.MediaID)
			case a.PowerCycle != nil:
				names = append(names, "powerCycle")
			case a.AwaitCheckIn != nil:
				names = append(names, "await:"+a.AwaitCheckIn.NodeRef.Name)
			}
		}
		installer := ""
		for _, n := range names {
			if strings.HasPrefix(n, "insert:VirtualMedia1:") {
				installer = strings.TrimPrefix(n, "insert:VirtualMedia1:")
			}
		}
		require.NotEmpty(t, installer, "installer in the first slot: %v", names)
		assert.Equal(t, []string{
			"eject:VirtualMedia1", "eject:VirtualMedia2",
			"insert:VirtualMedia1:" + installer, "insert:VirtualMedia2:node-dual-config",
			"override:UsbCd:UEFI", "powerCycle", "await:node-dual",
			"eject:VirtualMedia2", "eject:VirtualMedia1",
		}, names)

		waitHost(dualHost, func(h metalv1alpha1.Host) bool {
			return h.Status.BlockedOn == "Approval(Provisioning)"
		})
		time.Sleep(5 * pollInterval)
		assert.Len(t, opsFor(dualHost, "provision"), 1, "nothing is proposed twice")
	})

	t.Run("a hold withdraws the proposal until it is lifted", func(t *testing.T) {
		annotate := func(value string) {
			c, err := claimOf("dual")
			require.NoError(t, err)
			require.NoError(t, cl.Patch(ctx, &c, client.RawPatch(types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"metal.pk.io/hold":`+value+`}}}`))))
		}
		annotate(`"change freeze"`)
		c := waitClaim("dual", func(c metalv1alpha1.HostClaim) bool { return c.Status.BlockedOn == metalv1alpha1.ReasonHeldByUser })
		assert.Contains(t, provisioned(c).Message, "change freeze")
		require.Eventually(t, func() bool { return len(opsFor(dualHost, "provision")) == 0 }, waitFor, tick,
			"the proposal waiting for approval is withdrawn")
		time.Sleep(3 * pollInterval)
		assert.Empty(t, opsFor(dualHost, "provision"), "and none comes back while held")

		annotate(`null`)
		waitClaim("dual", func(c metalv1alpha1.HostClaim) bool {
			p := provisioned(c)
			return p != nil && strings.Contains(p.Message, "awaits approval") && c.Status.BlockedOn == ""
		})
		require.Len(t, opsFor(dualHost, "provision"), 1, "lifted, the same plan is proposed again")
	})

	t.Run("approval mounts both media with credentials, overrides, powers on", func(t *testing.T) {
		op := opsFor(dualHost, "provision")[0]
		key := client.ObjectKeyFromObject(&op)
		resetsBefore := dualState.Resets("1")
		require.NoError(t, aliceCl.Patch(ctx, &op, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))))

		waitOp(key, executing("awaitCheckIn"))
		mounted := mediaOf(dualState)
		assert.True(t, strings.HasPrefix(mounted["VirtualMedia1"], "https://"+strings.TrimPrefix(factory.URL, "https://")),
			"installer from the factory: %v", mounted)
		configURL := mounted["VirtualMedia2"]
		require.True(t, strings.HasPrefix(configURL, media.BaseURL+"/grants/"), "config under a grant: %v", mounted)
		assert.Equal(t, http.StatusUnauthorized, grantStatus(configURL),
			"the grant answers only with its credentials — the sim's mount presented them")
		assert.Equal(t, configISO, dualState.BootImage("VirtualMedia2"), "the machine booted the approved config medium")

		systems, err := dualState.Systems(ctx)
		require.NoError(t, err)
		assert.Equal(t, bmc.BootOverride{Target: "UsbCd", Enabled: bmc.OverrideDisabled, Mode: "UEFI"},
			systems[0].BootOverride, "a Once UEFI override, used up by the boot")
		assert.Equal(t, bmc.PowerOn, systems[0].PowerState)
		assert.Equal(t, resetsBefore+1, dualState.Resets("1"), "one boot")

		h := waitHost(dualHost, func(h metalv1alpha1.Host) bool { return h.Status.Phase == metalv1alpha1.HostProvisioning })
		assert.Empty(t, h.Status.BlockedOn)

		// The machine installs and comes up with its config.
		checkIn("node-dual", machineAPI...)
		op = waitOp(key, succeeded)
		assert.Empty(t, mediaOf(dualState), "media ejected after the check-in")
		assert.Equal(t, http.StatusForbidden, grantStatus(configURL), "the grant is revoked")
		require.Len(t, op.Status.Steps, len(op.Spec.Actions))

		waitHost(dualHost, func(h metalv1alpha1.Host) bool { return h.Status.Phase == metalv1alpha1.HostProvisioned })
		c := waitClaim("dual", provisionedReason(metalv1alpha1.ReasonHandedOff))
		require.NotNil(t, c.Status.HandOff, "the claim records the hand-off")
		assert.Equal(t, dualHost, c.Status.HandOff.Host)
		assert.Equal(t, "node-dual", c.Status.HandOff.Node)
		assert.True(t, strings.HasPrefix(c.Status.HandOff.ConfigDigest, "sha256:"))
	})

	t.Run("the hand-off outlives its operations", func(t *testing.T) {
		// A person tidies up the finished operations, and later the node goes
		// down. Neither is a reason to install the machine again.
		for _, op := range opsFor(dualHost, "provision") {
			require.NoError(t, cl.Delete(ctx, &op))
		}
		checkIn("node-dual",
			metav1.Condition{Type: osv1alpha1.ConditionMachineReachable, Status: metav1.ConditionFalse, Reason: "Unreachable"},
			metav1.Condition{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "NotReady"})
		waitClaim("dual", func(c metalv1alpha1.HostClaim) bool {
			p := provisioned(c)
			return p != nil && p.Reason == metalv1alpha1.ReasonHandedOff && strings.Contains(p.Message, "does not start a new install")
		})
		time.Sleep(5 * pollInterval)
		assert.Empty(t, opsFor(dualHost, "provision"), "no reinstall is proposed")

		checkIn("node-dual", machineAPI...)
		waitHost(dualHost, func(h metalv1alpha1.Host) bool { return h.Status.Phase == metalv1alpha1.HostProvisioned })
	})

	t.Run("deleting the claim of a running node waits for approval, then shuts it down", func(t *testing.T) {
		c, err := claimOf("dual")
		require.NoError(t, err)
		require.NoError(t, cl.Delete(ctx, &c))

		var rel []metalv1alpha1.HostOperation
		require.Eventually(t, func() bool { rel = opsFor(dualHost, "release"); return len(rel) == 1 }, waitFor, tick)
		assert.True(t, *rel[0].Spec.Approval.Required, "turning off a running node is a decision for a person")
		assert.Equal(t, operator, rel[0].Spec.ProposedBy, "admission takes the operator's gated proposal")
		require.NotNil(t, rel[0].Spec.Actions[0].PowerOff)
		assert.Positive(t, rel[0].Spec.Actions[0].PowerOff.GracefulTimeoutSeconds, "the OS is asked to shut down first")
		waitClaim("dual", func(c metalv1alpha1.HostClaim) bool {
			p := provisioned(c)
			return p != nil && strings.Contains(p.Message, "pk-metal approve default/"+rel[0].Name)
		})
		waitHost(dualHost, func(h metalv1alpha1.Host) bool { return h.Status.BlockedOn == "Approval(Release)" })
		time.Sleep(3 * pollInterval)
		power, err := dualState.Power(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, bmc.PowerOn, power, "nothing happens before the approval")

		require.NoError(t, aliceCl.Patch(ctx, &rel[0], client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))))
		op := waitOp(client.ObjectKeyFromObject(&rel[0]), succeeded)
		assert.NotNil(t, op.Status.ShutdownRequested, "a graceful shutdown was asked for")

		h := waitHost(dualHost, func(h metalv1alpha1.Host) bool { return h.Status.Phase == metalv1alpha1.HostAvailable })
		assert.Nil(t, h.Spec.ClaimRef)
		assert.Nil(t, h.Spec.OS)
		power, err = dualState.Power(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, bmc.PowerOff, power, "released powered off")
		require.Eventually(t, func() bool {
			_, err := claimOf("dual")
			return apierrors.IsNotFound(err)
		}, waitFor, tick, "the claim goes once the machine is back")
		assert.Empty(t, opsFor(dualHost, "provision"), "the boot record leaves with its claim")
	})

	t.Run("a one-slot BMC holds a blank machine", func(t *testing.T) {
		createAsset("lab-single", singleFix.Systems[0].Serial, singleFix.Manager.MAC)
		singleHost := hostBehind("lab-single").Name
		renderedNode("node-single", "lab")
		require.NoError(t, carolCl.Create(ctx, &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "single", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef:     metalv1alpha1.Reference{Name: "lab-single"},
				OS:           &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "node-single"}},
				Provisioning: &metalv1alpha1.ClaimProvisioning{Approval: metalv1alpha1.ProvisioningApprovalAutomatic},
			},
		}))
		c := waitClaim("single", provisionedReason(metalv1alpha1.ReasonSingleMediumUnsupported))
		assert.Equal(t, metalv1alpha1.ReasonSingleMediumUnsupported, c.Status.BlockedOn)
		assert.Empty(t, opsFor(singleHost, "provision"))

		// The machine runs Talos from its disk, blank: the config medium alone
		// configures it — and the claim waives the gate for its cattle.
		checkIn("node-single", metav1.Condition{Type: osv1alpha1.ConditionMachineReachable,
			Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonMaintenanceMode})
		var ops []metalv1alpha1.HostOperation
		require.Eventually(t, func() bool { ops = opsFor(singleHost, "provision"); return len(ops) == 1 }, waitFor, tick)
		assert.False(t, *ops[0].Spec.Approval.Required, "Automatic: the operator's proposal is pre-cleared")
		for _, a := range ops[0].Spec.Actions {
			assert.Nil(t, a.SetBootOverride, "boots from disk, no override")
		}
		key := client.ObjectKeyFromObject(&ops[0])
		waitOp(key, executing("awaitCheckIn"))
		mounted := mediaOf(singleState)
		require.Contains(t, mounted, "CD")
		assert.True(t, strings.HasPrefix(stripUserinfo(mounted["CD"]), media.BaseURL+"/grants/"),
			"the iBMC carries the grant credentials in the URL: %v", mounted)

		checkIn("node-single", machineAPI...)
		waitOp(key, succeeded)
		assert.Empty(t, mediaOf(singleState))
		waitClaim("single", provisionedReason(metalv1alpha1.ReasonHandedOff))

		c, err := claimOf("single")
		require.NoError(t, err)
		require.NoError(t, cl.Delete(ctx, &c))
		var rel []metalv1alpha1.HostOperation
		require.Eventually(t, func() bool { rel = opsFor(singleHost, "release"); return len(rel) == 1 }, waitFor, tick)
		assert.False(t, *rel[0].Spec.Approval.Required, "Automatic: the claim waived the gate for its release too")
		waitHost(singleHost, func(h metalv1alpha1.Host) bool { return h.Status.Phase == metalv1alpha1.HostAvailable })
	})

	t.Run("a claimed host outlives the deletion of its asset", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: "default"},
			Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "lab-dual"}},
		}))
		waitHost(dualHost, func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostAssigned && slicesContain(h.Finalizers, "metal.pk.io/claim-protection")
		})
		require.Eventually(t, func() bool {
			ep := &metalv1alpha1.ManagementEndpoint{}
			return cl.Get(ctx, client.ObjectKey{Name: "lab-dual"}, ep) == nil &&
				slicesContain(ep.Finalizers, "metal.pk.io/claim-protection")
		}, waitFor, tick, "the fronting endpoint is protected too")

		// The asset goes; envtest runs no garbage collector, so its cascade is
		// played by hand — exactly the deletes the collector would issue.
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.Asset{ObjectMeta: metav1.ObjectMeta{Name: "lab-dual"}}))
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "lab-dual"}}))
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: dualHost}}))

		h := waitHost(dualHost, func(h metalv1alpha1.Host) bool {
			d := findCondition(h.Status.Conditions, metalv1alpha1.ConditionDeletionHeld)
			return d != nil && d.Status == metav1.ConditionTrue
		})
		assert.NotNil(t, h.DeletionTimestamp, "deletion requested, the host stays")
		require.NotNil(t, h.Spec.ClaimRef, "and stays claimed")
		require.Eventually(t, func() bool {
			ep := &metalv1alpha1.ManagementEndpoint{}
			if cl.Get(ctx, client.ObjectKey{Name: "lab-dual"}, ep) != nil {
				return false
			}
			d := findCondition(ep.Status.Conditions, metalv1alpha1.ConditionDeletionHeld)
			return d != nil && d.Reason == metalv1alpha1.ReasonHostClaimed
		}, waitFor, tick, "the endpoint says why it is held")

		// Releasing the claim lets the deletion finish.
		c, err := claimOf("keep")
		require.NoError(t, err)
		require.NoError(t, cl.Delete(ctx, &c))
		require.Eventually(t, func() bool {
			_, err := getHost(dualHost)
			ep := &metalv1alpha1.ManagementEndpoint{}
			epErr := cl.Get(ctx, client.ObjectKey{Name: "lab-dual"}, ep)
			return apierrors.IsNotFound(err) && apierrors.IsNotFound(epErr)
		}, waitFor, tick, "released, the held objects go")
	})
}

func slicesContain(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
