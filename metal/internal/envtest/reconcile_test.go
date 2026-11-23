// The reconciler suite against a real apiserver and TLS-serving sims. Endpoints:
// an Asset materializes into a ManagementEndpoint that reaches Ready; a corrupted
// assertion holds at Mismatch (alert-and-hold) and recovers; status writes dedupe.
// Hosts: identity-keyed projection from the endpoints, two managers converge on
// one Host. Claims: bind PVC-style — a race has one winner, release hands over,
// delete frees the machine. Operations: a proposed operation blocks its host on
// Approval(...), the VAP denies a foreign approval stamp, a self-approval executes
// against the sim, and an out-of-band reset raises Drifted.
package envtest_test

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
)

const (
	pollInterval = 200 * time.Millisecond
	waitFor      = 20 * time.Second
	tick         = 100 * time.Millisecond
)

func simServer(t *testing.T, fixture string) (*httptest.Server, *sim.State) {
	t.Helper()
	f, err := sim.LoadFixture(fixture)
	require.NoError(t, err)
	st := sim.NewState(f)
	srv := httptest.NewTLSServer(sim.NewServer(st))
	t.Cleanup(srv.Close)
	return srv, st
}

func TestReconcile(t *testing.T) {
	st := startEnv(t)

	huawei, _ := simServer(t, "huawei-2288h-v5")
	smc, smcState := simServer(t, "supermicro-x12")

	// A second manager fronting the SAME Huawei system (the two-manager topology
	// behind the identity invariant): identical machine facts, its own BMC MAC.
	hf, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	hf2 := *hf
	hf2.Manager.MAC = "02:00:48:0b:74:99"
	huawei2 := httptest.NewTLSServer(sim.NewServer(sim.NewState(&hf2)))
	t.Cleanup(huawei2.Close)

	// The ledger joins the fixtures' manager MACs to the sims. aa:…:01 resolves to
	// the Huawei sim although the fixture reports another MAC (the wrong-box case);
	// the dead entry points at a closed port. The Supermicro line is written the
	// way a BMC label prints it (upper case, dashes) and still joins the
	// asset's lower-case colon form; the line that is no MAC is skipped, not
	// fatal.
	led := writeLedger(t, `
"02:00:48:0b:74:60": `+huawei.URL+`
"02:00:48:0b:74:99": `+huawei2.URL+`
"02-00-5A-3A-CC-18": `+smc.URL+`
"aa:bb:cc:dd:ee:01": `+huawei.URL+`
"00:11:22:33:44:55": https://127.0.0.1:1
"02:00:5a:3a:cc": https://127.0.0.1:2
`)
	require.Len(t, led.Problems(), 1, "the five-byte line is skipped")

	cl := st.cl
	ctx := t.Context()

	// The endpoints pin the httptest CA — the same trust posture as live endpoints
	// pinning internal-ca (the PoC identity verdict).
	seedBMCSecrets(t, ctx, cl,
		huawei.Certificate().Raw, huawei2.Certificate().Raw, smc.Certificate().Raw)

	// The committed policy IS what's tested: the approval gate loads from
	// config/policy, not from a test-local copy.
	operator := whoAmI(t, ctx, cl)
	installApprovalPolicy(t, ctx, cl, operator)

	// Two humans, distinct usernames: exactly what the VAP's
	// request.userInfo rules see.
	aliceCl := st.userClient(t, "alice")
	malloryCl := st.userClient(t, "mallory")

	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		OperationsNamespace:  "default",
		Proposer:             operator,
		Ledger:               led,
		TLS:                  &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: "bmc-ca"}},
		PollInterval:         pollInterval,
	}, controller.LaneAsset, controller.LaneEndpoint, controller.LaneHost, controller.LaneHostClaim,
		controller.LaneHostOperation))
	runManager(t, ctx, mgr)

	asset := func(name, serial, mac string) *metalv1alpha1.Asset {
		return &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: metalv1alpha1.AssetSpec{
				CID:            "CI-" + name,
				Serial:         serial,
				BMCMac:         mac,
				CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"},
			},
		}
	}
	endpointPhase := func(name string) (metalv1alpha1.ManagementEndpoint, metalv1alpha1.EndpointPhase) {
		ep := metalv1alpha1.ManagementEndpoint{}
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, &ep); err != nil {
			return ep, ""
		}
		return ep, ep.Status.Phase
	}
	waitPhase := func(name string, want metalv1alpha1.EndpointPhase) metalv1alpha1.ManagementEndpoint {
		var last metalv1alpha1.ManagementEndpoint
		if !assert.Eventually(t, func() bool {
			ep, p := endpointPhase(name)
			last = ep
			return p == want
		}, waitFor, tick) {
			t.Fatalf("endpoint %s: want phase %s, last status %+v", name, want, last.Status)
		}
		return last
	}
	hostByName := func(name string) (metalv1alpha1.Host, metalv1alpha1.HostPhase) {
		h := metalv1alpha1.Host{}
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, &h); err != nil {
			return h, ""
		}
		return h, h.Status.Phase
	}
	waitHost := func(name string, ok func(metalv1alpha1.Host) bool) metalv1alpha1.Host {
		var last metalv1alpha1.Host
		if !assert.Eventually(t, func() bool {
			h, _ := hostByName(name)
			last = h
			return ok(h)
		}, waitFor, tick) {
			t.Fatalf("host %s: condition never met, last status %+v", name, last.Status)
		}
		return last
	}
	hostPhaseIs := func(want metalv1alpha1.HostPhase) func(metalv1alpha1.Host) bool {
		return func(h metalv1alpha1.Host) bool { return h.Status.Phase == want }
	}
	claimByName := func(name string) metalv1alpha1.HostClaim {
		c := metalv1alpha1.HostClaim{}
		_ = cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &c)
		return c
	}
	// editClaim re-reads before each attempt. The claim reconciler writes the
	// same object while the test edits its spec, so a plain read-modify-write
	// loses that race often enough to matter once the machine is busy.
	editClaim := func(name string, edit func(*metalv1alpha1.HostClaim)) {
		require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			c := claimByName(name)
			edit(&c)
			return cl.Update(ctx, &c)
		}))
	}
	waitClaim := func(name string, ok func(metalv1alpha1.HostClaim) bool) metalv1alpha1.HostClaim {
		var last metalv1alpha1.HostClaim
		if !assert.Eventually(t, func() bool {
			last = claimByName(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("claim %s: condition never met, last status %+v", name, last.Status)
		}
		return last
	}
	claimPhaseIs := func(want metalv1alpha1.ClaimPhase) func(metalv1alpha1.HostClaim) bool {
		return func(c metalv1alpha1.HostClaim) bool { return c.Status.Phase == want }
	}
	opByName := func(name string) metalv1alpha1.HostOperation {
		op := metalv1alpha1.HostOperation{}
		_ = cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &op)
		return op
	}
	waitOp := func(name string, ok func(metalv1alpha1.HostOperation) bool) metalv1alpha1.HostOperation {
		var last metalv1alpha1.HostOperation
		if !assert.Eventually(t, func() bool {
			last = opByName(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("operation %s: condition never met, last status %+v", name, last.Status)
		}
		return last
	}
	opPhaseIs := func(want metalv1alpha1.OperationPhase) func(metalv1alpha1.HostOperation) bool {
		return func(op metalv1alpha1.HostOperation) bool { return op.Status.Phase == want }
	}
	powerCycleOp := func(name string) *metalv1alpha1.HostOperation {
		return &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:    metalv1alpha1.Reference{Name: "simx12b000001"},
				ProposedBy: operator,
				Actions:    []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
			},
		}
	}

	st.run(t, "asset materializes and reaches Ready", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, asset("ci-huawei", "SIMHW2288V5000391", "02:00:48:0b:74:60")))
		ep := waitPhase("ci-huawei", metalv1alpha1.EndpointReady)

		assert.Equal(t, huawei.URL, ep.Spec.Address, "address joined from the ledger")
		assert.Equal(t, "ibmc-factory", ep.Spec.CredentialsRef.Name, "credentials defaulted from the asset")
		assert.Equal(t, "Huawei", ep.Status.Vendor)
		require.NotNil(t, ep.Status.Asset)
		assert.Equal(t, "CI-ci-huawei", ep.Status.Asset.CID, "assertion snapshot cached")
		require.NotNil(t, ep.Status.Capabilities)
		assert.False(t, ep.Status.Capabilities.DualMedia, "iBMC's second slot has no actions")
		assert.Equal(t, "vmm-control", ep.Status.Capabilities.MediaDialects["CD"])
		assert.Equal(t, "SIMHW2288V5000391", ep.Status.BoardSerial)
		require.Len(t, ep.Status.Systems, 1)
		assert.Equal(t, "Huawei", ep.Status.Systems[0].Manufacturer)
	})

	st.run(t, "host projects from the endpoint and reaches Available", func(t *testing.T) {
		h := waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return h.Status.Phase == metalv1alpha1.HostAvailable && h.Labels["metal.pk.io/asset"] != ""
		})
		assert.Equal(t, "ci-huawei", h.Labels["metal.pk.io/asset"], "the host names the asset it was found by")

		require.NotNil(t, h.Status.Identity)
		assert.Equal(t, "SIMHW2288V5000391", h.Status.Identity.Serial, "the Huawei anchor: system serial")
		assert.Equal(t, "Huawei", h.Status.Identity.Vendor)
		assert.Equal(t, "5A1B0001-0000-4000-8000-000000000391", h.Status.Identity.UUID)
		require.Len(t, h.Status.ManagedBy, 1)
		assert.Equal(t, "ci-huawei", h.Status.ManagedBy[0].EndpointRef.Name)
		assert.Equal(t, "1", h.Status.ManagedBy[0].SystemID)
		assert.True(t, h.Status.ManagedBy[0].Reachable)
		require.Len(t, h.OwnerReferences, 1, "owned by the fronting endpoint")
		assert.Equal(t, "ci-huawei", h.OwnerReferences[0].Name)
	})

	st.run(t, "board serial anchors the Supermicro verification", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, asset("ci-smc", "SIMX12B000001", "02:00:5a:3a:cc:18")))
		ep := waitPhase("ci-smc", metalv1alpha1.EndpointReady)

		assert.Equal(t, "Supermicro", ep.Status.Vendor)
		require.NotNil(t, ep.Status.Capabilities)
		assert.True(t, ep.Status.Capabilities.DualMedia, "X12: three insertable slots")

		h := waitHost("simx12b000001", hostPhaseIs(metalv1alpha1.HostAvailable))
		require.NotNil(t, h.Status.Identity)
		assert.Equal(t, "SIMX12B000001", h.Status.Identity.Serial, "the Supermicro anchor: board serial")
		assert.Equal(t, "Supermicro", h.Status.Identity.Vendor)
	})

	st.run(t, "second manager converges on the same host", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, asset("ci-huawei2", "SIMHW2288V5000391", "02:00:48:0b:74:99")))
		waitPhase("ci-huawei2", metalv1alpha1.EndpointReady)

		h := waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return len(h.Status.ManagedBy) == 2
		})
		assert.Equal(t, metalv1alpha1.HostAvailable, h.Status.Phase, "still one Available host, now dual-path")
		assert.Equal(t, "ci-huawei", h.Status.ManagedBy[0].EndpointRef.Name)
		assert.Equal(t, "ci-huawei2", h.Status.ManagedBy[1].EndpointRef.Name)
		assert.True(t, h.Status.ManagedBy[0].Reachable)
		assert.True(t, h.Status.ManagedBy[1].Reachable)
		require.NotNil(t, h.Status.Identity)
		assert.Equal(t, "SIMHW2288V5000391", h.Status.Identity.Serial, "identity untouched by the extra path")
		assert.Len(t, h.OwnerReferences, 2, "owned by both fronting endpoints")
	})

	st.run(t, "two claims race for one host", func(t *testing.T) {
		// Both claims select the same machine — through DIFFERENT assets (the two
		// managers fronting it). Created back-to-back with no wait between: exactly
		// one may win the bind pointer, whichever reconciles first.
		claim := func(name, asset string) *metalv1alpha1.HostClaim {
			return &metalv1alpha1.HostClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: asset}},
			}
		}
		require.NoError(t, cl.Create(ctx, claim("fleet-a", "ci-huawei")))
		require.NoError(t, cl.Create(ctx, claim("fleet-b", "ci-huawei2")))

		h := waitHost("simhw2288v5000391", hostPhaseIs(metalv1alpha1.HostAssigned))
		require.NotNil(t, h.Spec.ClaimRef)
		assert.Equal(t, "default", h.Spec.ClaimRef.Namespace)

		winner := h.Spec.ClaimRef.Name
		loser := "fleet-b"
		if winner == loser {
			loser = "fleet-a"
		}
		w := waitClaim(winner, claimPhaseIs(metalv1alpha1.ClaimBound))
		require.NotNil(t, w.Status.HostRef)
		assert.Equal(t, "simhw2288v5000391", w.Status.HostRef.Name)
		l := waitClaim(loser, func(c metalv1alpha1.HostClaim) bool {
			return c.Status.BlockedOn == "HostAlreadyClaimed"
		})
		assert.Equal(t, metalv1alpha1.ClaimPending, l.Status.Phase)
		assert.Nil(t, l.Status.HostRef)

		// Releasing the winner hands the host to the waiting loser — level-triggered
		// retry off the host release, no polling loop in the claim reconciler.
		require.NoError(t, cl.Delete(ctx, &w))
		waitClaim(loser, claimPhaseIs(metalv1alpha1.ClaimBound))
		h = waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return h.Spec.ClaimRef != nil && h.Spec.ClaimRef.Name == loser
		})
		assert.Equal(t, metalv1alpha1.HostAssigned, h.Status.Phase, "still assigned, new holder")

		// Releasing the survivor returns the host to the pool.
		l = claimByName(loser)
		require.NoError(t, cl.Delete(ctx, &l))
		h = waitHost("simhw2288v5000391", hostPhaseIs(metalv1alpha1.HostAvailable))
		assert.Nil(t, h.Spec.ClaimRef, "released: the bind pointer is gone")
	})

	st.run(t, "claim OS intent stamps the host and follows edits", func(t *testing.T) {
		c := &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "fleet-os", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "ci-huawei"},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "ref-2288h"}},
			},
		}
		require.NoError(t, cl.Create(ctx, c))
		h := waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return h.Spec.OS != nil && h.Status.Phase == metalv1alpha1.HostProvisioning
		})
		assert.Equal(t, metalv1alpha1.Reference{Name: "ref-2288h"}, h.Spec.OS.NodeRef)
		// The intent engages the lane, but its TalosNode doesn't exist — the
		// authoring error is an observation, not a phase.
		osReady := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady)
		require.NotNil(t, osReady)
		assert.Equal(t, metav1.ConditionUnknown, osReady.Status)
		assert.Equal(t, metalv1alpha1.ReasonNodeNotFound, osReady.Reason)

		// The node the intent names is fixed while it stands, and so is the
		// machine the claim binds.
		c2 := claimByName("fleet-os")
		c2.Spec.OS.NodeRef.Name = "ref-2288h-b"
		err := cl.Update(ctx, &c2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "os.nodeRef is immutable")
		c2 = claimByName("fleet-os")
		c2.Spec.AssetRef.Name = "some-other-asset"
		err = cl.Update(ctx, &c2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "assetRef is immutable")

		// Dropping the intent clears the hand-off but keeps the bind.
		editClaim("fleet-os", func(c *metalv1alpha1.HostClaim) { c.Spec.OS = nil })
		h = waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return h.Spec.OS == nil
		})
		require.NotNil(t, h.Spec.ClaimRef, "the bind survives the dropped intent")

		// Release clears bind and hand-off together.
		c2 = claimByName("fleet-os")
		require.NoError(t, cl.Delete(ctx, &c2))
		waitHost("simhw2288v5000391", hostPhaseIs(metalv1alpha1.HostAvailable))
	})

	st.run(t, "check-in observations advance the host to Provisioned", func(t *testing.T) {
		// The TalosNode exists but no OS reconciler runs in this suite — its
		// check-in conditions are scripted below, exactly the seam the
		// HostReconciler joins on.
		node := &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: "ref-x12-b"},
			Spec: osv1alpha1.TalosNodeSpec{
				ClusterRef:          osv1alpha1.Reference{Name: "fleet"},
				Role:                osv1alpha1.RoleWorker,
				Address:             "127.0.0.1",
				InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "pklab-disk-9"},
			},
		}
		require.NoError(t, cl.Create(ctx, node))
		c := &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "fleet-prov", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "ci-huawei"},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "ref-x12-b"}},
			},
		}
		require.NoError(t, cl.Create(ctx, c))

		// No observation yet: the lane is engaged and waiting.
		h := waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			cond := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady)
			return cond != nil && cond.Reason == metalv1alpha1.ReasonAwaitingContact
		})
		assert.Equal(t, metalv1alpha1.HostProvisioning, h.Status.Phase)

		// The machine comes up blank — the check-in sees the maintenance service.
		nodeCond := func(t_, reason string, s metav1.ConditionStatus) metav1.Condition {
			return metav1.Condition{Type: t_, Status: s, Reason: reason,
				LastTransitionTime: metav1.Now()}
		}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ref-x12-b"}, node))
		node.Status.Conditions = []metav1.Condition{
			nodeCond(osv1alpha1.ConditionMachineReachable, osv1alpha1.ReasonMaintenanceMode, metav1.ConditionTrue),
			nodeCond(osv1alpha1.ConditionReady, osv1alpha1.ReasonMaintenanceMode, metav1.ConditionUnknown),
		}
		node.Status.Stage = "maintenance"
		require.NoError(t, cl.Status().Update(ctx, node))

		h = waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			cond := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBooted)
			return cond != nil && cond.Status == metav1.ConditionTrue
		})
		assert.Equal(t, osv1alpha1.ReasonMaintenanceMode,
			meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBooted).Reason)
		assert.Equal(t, metalv1alpha1.HostProvisioning, h.Status.Phase, "blank machine is still provisioning")

		// The node converges — Provisioned.
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ref-x12-b"}, node))
		node.Status.Conditions = []metav1.Condition{
			nodeCond(osv1alpha1.ConditionMachineReachable, osv1alpha1.ReasonMachineAPI, metav1.ConditionTrue),
			nodeCond(osv1alpha1.ConditionReady, osv1alpha1.ReasonNodeReady, metav1.ConditionTrue),
		}
		node.Status.Stage = "running"
		require.NoError(t, cl.Status().Update(ctx, node))
		h = waitHost("simhw2288v5000391", hostPhaseIs(metalv1alpha1.HostProvisioned))
		assert.Equal(t, osv1alpha1.ReasonNodeReady,
			meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady).Reason)

		// Release: the OS observations leave with the claim.
		c2 := claimByName("fleet-prov")
		require.NoError(t, cl.Delete(ctx, &c2))
		h = waitHost("simhw2288v5000391", hostPhaseIs(metalv1alpha1.HostAvailable))
		assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionOSReady))
		assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBooted))
		require.NoError(t, cl.Delete(ctx, node))
	})

	st.run(t, "corrupted assertion is alert-and-hold, and recovers", func(t *testing.T) {
		a := &metalv1alpha1.Asset{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ci-huawei"}, a))
		good := a.Spec.Serial
		a.Spec.Serial = "2102311XBHP0LB00WRONG"
		require.NoError(t, cl.Update(ctx, a))

		ep := waitPhase("ci-huawei", metalv1alpha1.EndpointMismatch)
		assert.Equal(t, "SerialMismatch", ep.Status.BlockedOn)
		cond := meta.FindStatusCondition(ep.Status.Conditions, metalv1alpha1.ConditionVerified)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)

		// One alarming path condemns the host even though the second path (the
		// ci-huawei2 manager) still verifies — any-False-wins aggregation.
		h := waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return h.Status.BlockedOn == "SerialMismatch"
		})
		assert.NotEqual(t, metalv1alpha1.HostAvailable, h.Status.Phase)

		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ci-huawei"}, a))
		a.Spec.Serial = good
		require.NoError(t, cl.Update(ctx, a))
		ep = waitPhase("ci-huawei", metalv1alpha1.EndpointReady)
		assert.Empty(t, ep.Status.BlockedOn)
		waitHost("simhw2288v5000391", hostPhaseIs(metalv1alpha1.HostAvailable))
	})

	st.run(t, "wrong box behind the address", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, asset("ci-macwrong", "SIMHW2288V5000391", "aa:bb:cc:dd:ee:01")))
		ep := waitPhase("ci-macwrong", metalv1alpha1.EndpointMismatch)
		assert.Equal(t, "MACMismatch", ep.Status.BlockedOn)

		// That endpoint discovered the huawei system, so it becomes a third path —
		// and its alarm condemns the host the two healthy paths agree on.
		waitHost("simhw2288v5000391", func(h metalv1alpha1.Host) bool {
			return h.Status.BlockedOn == "MACMismatch" && len(h.Status.ManagedBy) == 3
		})
	})

	st.run(t, "dead address is Unreachable", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, asset("ci-dead", "X", "00:11:22:33:44:55")))
		waitPhase("ci-dead", metalv1alpha1.EndpointUnreachable)

		hosts := &metalv1alpha1.HostList{}
		require.NoError(t, cl.List(ctx, hosts))
		assert.Len(t, hosts.Items, 2, "an endpoint that never discovered fronts no host")
	})

	st.run(t, "no ledger entry materializes nothing", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, asset("ci-unknown", "X", "FF-FF-FF-FF-FF-FF")))
		var cond *metav1.Condition
		require.Eventually(t, func() bool {
			a := &metalv1alpha1.Asset{}
			if cl.Get(ctx, client.ObjectKey{Name: "ci-unknown"}, a) != nil {
				return false
			}
			cond = meta.FindStatusCondition(a.Status.Conditions, metalv1alpha1.ConditionMaterialized)
			return cond != nil
		}, waitFor, tick, "the asset says whether it materialized")
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, metalv1alpha1.ReasonNoLedgerEntry, cond.Reason)
		assert.Contains(t, cond.Message, "FF-FF-FF-FF-FF-FF", "the message names the MAC")
		assert.Contains(t, cond.Message, "not in the ledger")
		_, p := endpointPhase("ci-unknown")
		assert.Empty(t, p, "no endpoint should exist")

		// A claim on it says the same, as a block rather than a wait.
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "unknown", Namespace: "default"},
			Spec:       metalv1alpha1.HostClaimSpec{AssetRef: metalv1alpha1.Reference{Name: "ci-unknown"}},
		}))
		c := waitClaim("unknown", func(c metalv1alpha1.HostClaim) bool {
			return c.Status.BlockedOn == metalv1alpha1.ReasonAssetNotMaterialized
		})
		b := meta.FindStatusCondition(c.Status.Conditions, metalv1alpha1.ConditionBound)
		require.NotNil(t, b)
		assert.Contains(t, b.Message, "FF-FF-FF-FF-FF-FF")
		require.NoError(t, cl.Delete(ctx, &c))

		// A known asset's condition names its endpoint.
		a := &metalv1alpha1.Asset{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ci-smc"}, a))
		m := meta.FindStatusCondition(a.Status.Conditions, metalv1alpha1.ConditionMaterialized)
		require.NotNil(t, m)
		assert.Equal(t, metav1.ConditionTrue, m.Status)
	})

	st.run(t, "steady state writes no status", func(t *testing.T) {
		ep := waitPhase("ci-smc", metalv1alpha1.EndpointReady)
		h := waitHost("simx12b000001", hostPhaseIs(metalv1alpha1.HostAvailable))
		epRV, hostRV := ep.ResourceVersion, h.ResourceVersion
		time.Sleep(5 * pollInterval)
		got, _ := endpointPhase("ci-smc")
		assert.Equal(t, epRV, got.ResourceVersion, "unchanged observations must not write")
		gotHost, _ := hostByName("simx12b000001")
		assert.Equal(t, hostRV, gotHost.ResourceVersion, "unchanged projections must not write")
	})

	st.run(t, "proposed operation awaits its human gate", func(t *testing.T) {
		// First, wait for the VAP to be compiled and enforcing: a pre-approved
		// create must bounce. Only then is the rest of this lane deterministic.
		require.Eventually(t, func() bool {
			probe := powerCycleOp("vap-probe")
			probe.Spec.Approval.ApprovedBy = "someone"
			err := cl.Create(ctx, probe)
			if err == nil {
				require.NoError(t, cl.Delete(ctx, probe))
				return false
			}
			return strings.Contains(err.Error(), "pre-approved")
		}, waitFor, tick, "the ValidatingAdmissionPolicy never became enforcing")

		// The kubectl path: no approval key at all (the typed client always
		// serializes the struct, which is why only YAML applies caught this) —
		// the schema default must close the gate.
		yamlOp := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "metal.pk.io/v1alpha1",
			"kind":       "HostOperation",
			"metadata":   map[string]any{"name": "yaml-shaped", "namespace": "default"},
			"spec": map[string]any{
				"hostRef":    map[string]any{"name": "simx12b000001"},
				"proposedBy": operator,
				"actions":    []any{map[string]any{"powerCycle": map[string]any{}}},
			},
		}}
		require.NoError(t, cl.Create(ctx, yamlOp))
		req := opByName("yaml-shaped").Spec.Approval.Required
		require.NotNil(t, req, "the schema default must persist required")
		assert.True(t, *req, "omitted approval must default to a closed gate")
		require.NoError(t, cl.Delete(ctx, yamlOp))

		require.NoError(t, cl.Create(ctx, powerCycleOp("cycle-smc")))
		op := waitOp("cycle-smc", opPhaseIs(metalv1alpha1.OperationAwaitingApproval))
		assert.Equal(t, "AwaitingApproval", op.Status.BlockedOn)

		h := waitHost("simx12b000001", func(h metalv1alpha1.Host) bool {
			return h.Status.BlockedOn == "Approval(PowerCycle)"
		})
		assert.Equal(t, metalv1alpha1.HostAvailable, h.Status.Phase,
			"awaiting approval is a reason, not a phase")
	})

	st.run(t, "the gate is CEL-guarded", func(t *testing.T) {
		// Stamping someone else's name is denied — approval is an identity act.
		op := opByName("cycle-smc")
		foreign := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))
		err := malloryCl.Patch(ctx, &op, foreign)
		require.Error(t, err, "mallory must not approve as alice")
		assert.Contains(t, err.Error(), "approve as yourself")

		// The action set is frozen after creation (schema CEL) — "patch" IS "approve".
		op = opByName("cycle-smc")
		op.Spec.Actions = []metalv1alpha1.HostOperationAction{
			{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "CD"}}}
		err = cl.Update(ctx, &op)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "frozen")
	})

	st.run(t, "approval executes and the sim power-cycles", func(t *testing.T) {
		before := smcState.Resets("1")
		op := opByName("cycle-smc")
		self := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))
		require.NoError(t, aliceCl.Patch(ctx, &op, self), "self-approval passes the VAP")

		op = waitOp("cycle-smc", opPhaseIs(metalv1alpha1.OperationSucceeded))
		assert.Equal(t, before+1, smcState.Resets("1"), "the sim power-cycled exactly once")
		require.NotNil(t, op.Status.Baseline, "baseline persisted before the mutating call")
		require.NotNil(t, op.Status.Baseline.LastResetTime)
		require.NotNil(t, op.Status.ObservedResetTime, "the closing observation proves the reset")
		assert.True(t, op.Status.ObservedResetTime.After(op.Status.Baseline.LastResetTime.Time),
			"baseline-and-advance: the world moved past the baseline")
		require.Len(t, op.Status.Steps, 1)
		assert.Equal(t, "powerCycle", op.Status.Steps[0].Action)
		assert.Equal(t, "Succeeded", op.Status.Steps[0].Phase)

		waitHost("simx12b000001", func(h metalv1alpha1.Host) bool {
			return h.Status.BlockedOn == ""
		})
	})

	st.run(t, "out-of-band reset raises Drifted", func(t *testing.T) {
		// Someone resets the machine behind the operator's back — no operation in
		// flight. The next poll observes the advance and flags it: information,
		// not error (the host stays in its phase).
		require.NoError(t, smcState.SetPower(ctx, "1", bmc.ActionForceRestart))
		h := waitHost("simx12b000001", func(h metalv1alpha1.Host) bool {
			d := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionDrifted)
			return d != nil && d.Status == metav1.ConditionTrue
		})
		d := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionDrifted)
		assert.Equal(t, metalv1alpha1.ReasonUnexpectedReset, d.Reason)
		assert.Equal(t, metalv1alpha1.HostAvailable, h.Status.Phase, "drift is not a failure")
	})

	st.run(t, "a host in maintenance starts nothing until it is given back", func(t *testing.T) {
		annotate := func(value string) {
			h, _ := hostByName("simx12b000001")
			require.NoError(t, cl.Patch(ctx, &h, client.RawPatch(types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"metal.pk.io/maintenance":`+value+`}}}`))))
		}
		annotate(`"replacing DIMM B2"`)
		h := waitHost("simx12b000001", hostPhaseIs(metalv1alpha1.HostMaintenance))
		assert.Equal(t, metalv1alpha1.ReasonHostInMaintenance, h.Status.BlockedOn)
		m := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionMaintenance)
		require.NotNil(t, m)
		assert.Contains(t, m.Message, "replacing DIMM B2", "the person's reason is shown")

		// Even a pre-cleared operation waits.
		before := smcState.Resets("1")
		op := powerCycleOp("cycle-in-maintenance")
		op.Spec.Approval.Required = ptr.To(false)
		require.NoError(t, cl.Create(ctx, op))
		waitOp("cycle-in-maintenance", func(op metalv1alpha1.HostOperation) bool {
			return op.Status.BlockedOn == metalv1alpha1.ReasonHostInMaintenance
		})
		time.Sleep(3 * pollInterval)
		assert.Equal(t, before, smcState.Resets("1"), "nothing ran on the machine")

		annotate(`null`)
		waitOp("cycle-in-maintenance", opPhaseIs(metalv1alpha1.OperationSucceeded))
		assert.Equal(t, before+1, smcState.Resets("1"))
		h = waitHost("simx12b000001", hostPhaseIs(metalv1alpha1.HostAvailable))
		assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionMaintenance))
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}
