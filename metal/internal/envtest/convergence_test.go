// The convergence suite: profiles select hosts over both label axes, the
// layered merge materializes BiosSettings/BiosSchema, drift proposes a gated
// HostOperation, approval stages and reboots through the sim, and the world
// reads back converged. The planHash guard is the load-bearing assertion: a
// held plan never spawns a second operation — not after failure, not after
// the operation record is deleted — until the desired state changes. Firmware
// converges before BIOS, through a real OCI pull off a fake registry.
package envtest_test

import (
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/api/meta"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
)

const (
	smcHost    = "simx12b000001"
	huaweiHost = "simhw2288v5000391"
)

func jv(t *testing.T, v any) apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return apiextensionsv1.JSON{Raw: raw}
}

// fakeRegistry serves one single-layer OCI artifact the way the internal Zot
// would — the firmware image the flash lane pulls.
func fakeRegistry(t *testing.T, blob []byte) (*httptest.Server, string) {
	t.Helper()
	blobDigest := digest.FromBytes(blob).String()
	man := fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"mediaType":"application/octet-stream","digest":%q,"size":%d}]}`,
		blobDigest, len(blob))
	manDigest := digest.FromString(man).String()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/firmware/x12-bmc/manifests/"+manDigest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(man))
	})
	mux.HandleFunc("GET /v2/firmware/x12-bmc/blobs/"+blobDigest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "https://") + "/firmware/x12-bmc@" + manDigest
}

func TestConvergence(t *testing.T) {
	st := startEnv(t)

	huawei, _ := simServer(t, "huawei-2288h-v5")
	smc, smcState := simServer(t, "supermicro-x12")
	registry, imageRef := fakeRegistry(t, []byte("SIMFW 02.00.00 fake x12 bmc image"))

	led := writeLedger(t, `
"02:00:48:0b:74:60": `+huawei.URL+`
"02:00:5a:3a:cc:18": `+smc.URL+`
`)

	cl := st.cl
	ctx := t.Context()

	seedBMCSecrets(t, ctx, cl, huawei.Certificate().Raw, smc.Certificate().Raw)

	// The committed approval policy runs here too: proposals must pass its
	// no-pre-approved-CREATE rule, approvals are identity acts.
	operator := whoAmI(t, ctx, cl)
	installApprovalPolicy(t, ctx, cl, operator)

	aliceCl := st.userClient(t, "alice")

	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		OperationsNamespace:  "default",
		Proposer:             operator,
		Ledger:               led,
		TLS:                  &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: "bmc-ca"}},
		PollInterval:         pollInterval,
		RegistryClient:       registry.Client(),
	}, controller.LaneAsset, controller.LaneEndpoint, controller.LaneHost, controller.LaneHostClaim,
		controller.LaneHostOperation, controller.LaneConvergence))
	runManager(t, ctx, mgr)

	for _, a := range []*metalv1alpha1.Asset{
		{ObjectMeta: metav1.ObjectMeta{Name: "ci-huawei"},
			Spec: metalv1alpha1.AssetSpec{CID: "CI-1", Serial: "SIMHW2288V5000391",
				BMCMac: "02:00:48:0b:74:60", CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ci-smc"},
			Spec: metalv1alpha1.AssetSpec{CID: "CI-2", Serial: "SIMX12B000001",
				BMCMac: "02:00:5a:3a:cc:18", CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"}}},
	} {
		require.NoError(t, cl.Create(ctx, a))
	}

	host := func(name string) metalv1alpha1.Host {
		h := metalv1alpha1.Host{}
		_ = cl.Get(ctx, client.ObjectKey{Name: name}, &h)
		return h
	}
	waitHost := func(name string, ok func(metalv1alpha1.Host) bool) metalv1alpha1.Host {
		var last metalv1alpha1.Host
		if !assert.Eventually(t, func() bool {
			last = host(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("host %s: condition never met, last %+v / %+v", name, last.Labels, last.Status)
		}
		return last
	}
	settingsOf := func(name string) metalv1alpha1.BiosSettings {
		bs := metalv1alpha1.BiosSettings{}
		_ = cl.Get(ctx, client.ObjectKey{Name: name}, &bs)
		return bs
	}
	converged := func(bs metalv1alpha1.BiosSettings) *metav1.Condition {
		return meta.FindStatusCondition(bs.Status.Conditions, metalv1alpha1.ConditionConverged)
	}
	waitSettings := func(name string, ok func(metalv1alpha1.BiosSettings) bool) metalv1alpha1.BiosSettings {
		var last metalv1alpha1.BiosSettings
		if !assert.Eventually(t, func() bool {
			last = settingsOf(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("biossettings %s: condition never met, last %+v", name, last.Status)
		}
		return last
	}
	settingsReason := func(reason string) func(metalv1alpha1.BiosSettings) bool {
		return func(bs metalv1alpha1.BiosSettings) bool {
			c := converged(bs)
			return c != nil && c.Reason == reason
		}
	}
	biosOps := func(hostName string) []metalv1alpha1.HostOperation {
		ops := &metalv1alpha1.HostOperationList{}
		require.NoError(t, cl.List(ctx, ops, client.InNamespace("default")))
		var out []metalv1alpha1.HostOperation
		for _, op := range ops.Items {
			if op.Spec.HostRef.Name == hostName && strings.Contains(op.Name, "-bios-") {
				out = append(out, op)
			}
		}
		return out
	}
	approve := func(nsName string) {
		ns, name, ok := strings.Cut(nsName, "/")
		require.True(t, ok, "operation key %q", nsName)
		op := metalv1alpha1.HostOperation{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &op))
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))
		require.NoError(t, aliceCl.Patch(ctx, &op, patch))
	}
	waitOpSucceeded := func(nsName string) {
		ns, name, _ := strings.Cut(nsName, "/")
		require.Eventually(t, func() bool {
			op := metalv1alpha1.HostOperation{}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &op); err != nil {
				return false
			}
			c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
			return c != nil && c.Status == metav1.ConditionTrue
		}, waitFor, tick, "operation %s never succeeded", nsName)
	}

	// The VAP must be enforcing before the approval lane is deterministic.
	require.Eventually(t, func() bool {
		probe := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "vap-probe", Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:    metalv1alpha1.Reference{Name: smcHost},
				ProposedBy: operator,
				Actions:    []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
			},
		}
		probe.Spec.Approval.ApprovedBy = "someone"
		err := cl.Create(ctx, probe)
		if err == nil {
			require.NoError(t, cl.Delete(ctx, probe))
			return false
		}
		return strings.Contains(err.Error(), "pre-approved")
	}, waitFor, tick, "the ValidatingAdmissionPolicy never became enforcing")

	st.run(t, "discovered facts label both hosts", func(t *testing.T) {
		h := waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			return h.Labels["metal.pk.io/model"] != ""
		})
		assert.Equal(t, "Supermicro", h.Labels["metal.pk.io/vendor"])
		assert.Equal(t, "X12DPi-N6", h.Labels["metal.pk.io/model"])

		h = waitHost(huaweiHost, func(h metalv1alpha1.Host) bool {
			return h.Labels["metal.pk.io/model"] != ""
		})
		assert.Equal(t, "Huawei", h.Labels["metal.pk.io/vendor"])
		assert.Equal(t, "2288H-V5", h.Labels["metal.pk.io/model"], "the space sanitized away")
	})

	st.run(t, "claim intent labels the host, prefixed keys refused", func(t *testing.T) {
		bad := &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-labels", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "ci-smc"},
				Labels:   map[string]metalv1alpha1.LabelValue{"metal.pk.io/vendor": "spoof"},
			},
		}
		err := cl.Create(ctx, bad)
		require.Error(t, err, "prefixed intent keys must be rejected at admission")
		assert.Contains(t, err.Error(), "unprefixed")

		claim := &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "fleet-conv", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "ci-smc"},
				Labels:   map[string]metalv1alpha1.LabelValue{"role": "worker"},
			},
		}
		require.NoError(t, cl.Create(ctx, claim))
		waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			return h.Labels["role"] == "worker"
		})
	})

	st.run(t, "profiles materialize settings and schema, drift proposes", func(t *testing.T) {
		for _, p := range []*metalv1alpha1.BiosProfile{
			{ObjectMeta: metav1.ObjectMeta{Name: "x12-baseline"},
				Spec: metalv1alpha1.BiosProfileSpec{
					Match: metav1.LabelSelector{MatchLabels: map[string]string{"metal.pk.io/model": "X12DPi-N6"}},
					Attributes: map[string]apiextensionsv1.JSON{
						"CSMSupport":                jv(t, "Enabled"), // fixture serves Disabled — drift
						"CorrectableErrorThreshold": jv(t, 512),       // fixture serves 512 — in sync
					}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "role-worker"},
				Spec: metalv1alpha1.BiosProfileSpec{
					Match: metav1.LabelSelector{MatchLabels: map[string]string{"role": "worker"}},
					Layer: 10,
					Attributes: map[string]apiextensionsv1.JSON{
						"QuietBoot": jv(t, false), // fixture serves true — drift
					}}},
		} {
			require.NoError(t, cl.Create(ctx, p))
		}

		// Two profiles, but the first one's drift is enough to reach AwaitingOperation — so
		// waiting on the reason alone can return inside the window where both layers are
		// already merged into the spec and the second one's provenance is still a pass
		// behind. Wait for the state this subtest goes on to assert, not for a reason that
		// precedes it.
		bs := waitSettings(smcHost, func(bs metalv1alpha1.BiosSettings) bool {
			return settingsReason(metalv1alpha1.ReasonAwaitingOperation)(bs) &&
				len(bs.Status.Provenance) == 3
		})
		assert.Len(t, bs.Spec.Attributes, 3, "both layers merged")
		assert.Equal(t, map[string]string{
			"CSMSupport":                "x12-baseline",
			"CorrectableErrorThreshold": "x12-baseline",
			"QuietBoot":                 "role-worker",
		}, bs.Status.Provenance)
		require.NotNil(t, bs.Status.Proposed)
		require.Len(t, bs.OwnerReferences, 1)
		assert.Equal(t, "Host", bs.OwnerReferences[0].Kind)

		schema := metalv1alpha1.BiosSchema{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: smcHost}, &schema))
		assert.Equal(t, "BiosAttributeRegistry.v1_0_0", schema.Spec.Registry, "advertised identity, verbatim")
		require.Len(t, schema.Spec.Attributes, 5)
		assert.Equal(t, "Above4GDecoding", schema.Spec.Attributes[0].Name, "sorted by name")

		// blockedOn (from the pending op) and the mirrored condition are written
		// by two reconcilers — wait for both to land.
		h := waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			return h.Status.BlockedOn == "Approval(BiosConvergence)" &&
				meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBiosConverged) != nil
		})
		c := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBiosConverged)
		assert.Equal(t, metalv1alpha1.ReasonAwaitingOperation, c.Reason, "mirrored onto the host")
	})

	st.run(t, "same-layer conflict holds whole", func(t *testing.T) {
		before := settingsOf(smcHost).Spec.Attributes
		rogue := &metalv1alpha1.BiosProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "rogue"},
			Spec: metalv1alpha1.BiosProfileSpec{
				Match:      metav1.LabelSelector{MatchLabels: map[string]string{"metal.pk.io/model": "X12DPi-N6"}},
				Attributes: map[string]apiextensionsv1.JSON{"CSMSupport": jv(t, "Disabled")},
			}}
		require.NoError(t, cl.Create(ctx, rogue))
		bs := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonProfileConflict))
		assert.Equal(t, before, bs.Spec.Attributes, "a conflicted merge never lands, the previous rendering holds")
		require.NoError(t, cl.Delete(ctx, rogue))
		waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
	})

	st.run(t, "schema violation holds, nothing new proposed", func(t *testing.T) {
		bad := &metalv1alpha1.BiosProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-value"},
			Spec: metalv1alpha1.BiosProfileSpec{
				Match:      metav1.LabelSelector{MatchLabels: map[string]string{"metal.pk.io/model": "X12DPi-N6"}},
				Layer:      20,
				Attributes: map[string]apiextensionsv1.JSON{"BootModeSelect": jv(t, "Insecure")},
			}}
		require.NoError(t, cl.Create(ctx, bad))
		bs := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonSchemaViolation))
		assert.Contains(t, converged(bs).Message, "BootModeSelect")
		assert.Len(t, biosOps(smcHost), 1, "no operation proposed for an invalid desired state")
		require.NoError(t, cl.Delete(ctx, bad))
		waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
	})

	st.run(t, "approval applies, reboots, converges", func(t *testing.T) {
		bs := settingsOf(smcHost)
		require.NotNil(t, bs.Status.Proposed)
		approve(bs.Status.Proposed.Operation)
		waitOpSucceeded(bs.Status.Proposed.Operation)

		bs = waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonInSync))
		assert.Equal(t, metav1.ConditionTrue, converged(bs).Status)
		assert.Equal(t, `"Enabled"`, string(bs.Status.Observed["CSMSupport"].Raw))
		assert.Equal(t, `false`, string(bs.Status.Observed["QuietBoot"].Raw))
		h := waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			c := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBiosConverged)
			return c != nil && c.Status == metav1.ConditionTrue && h.Status.BlockedOn == ""
		})
		assert.Equal(t, metalv1alpha1.HostAssigned, h.Status.Phase)
	})

	st.run(t, "drift after convergence is proposed again", func(t *testing.T) {
		bs := settingsOf(smcHost)
		require.Equal(t, metalv1alpha1.ReasonInSync, converged(bs).Reason)
		assert.Nil(t, bs.Status.Proposed, "a converged plan is spent and forgotten")
		first := biosOps(smcHost)
		require.Len(t, first, 1)

		// Someone flips a managed attribute by hand and the machine reboots
		// into it: the same desired state is drifted again.
		require.NoError(t, smcState.SetBiosAttributes(ctx, "1", map[string]any{"QuietBoot": true}))
		require.NoError(t, smcState.SetPower(ctx, "1", bmc.ActionForceRestart))
		bs = waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
		require.NotNil(t, bs.Status.Proposed)
		assert.NotEqual(t, first[0].Namespace+"/"+first[0].Name, bs.Status.Proposed.Operation,
			"the earlier round's finished record is not mistaken for this round's proposal")
		assert.Len(t, biosOps(smcHost), 2)

		again := bs.Status.Proposed.Operation
		approve(again)
		waitOpSucceeded(again)
		waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonInSync))
		// Keep the record count the later subtests reason about.
		ns, name, _ := strings.Cut(again, "/")
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}))
		require.Eventually(t, func() bool { return len(biosOps(smcHost)) == 1 }, waitFor, tick)
	})

	st.run(t, "unchanged planHash spawns nothing", func(t *testing.T) {
		smcState.SetFault(sim.FaultBiosStuck, true)

		// New desired state → exactly one new proposal.
		baseline := &metalv1alpha1.BiosProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "x12-baseline"}, baseline))
		baseline.Spec.Attributes["CorrectableErrorThreshold"] = jv(t, 1000)
		require.NoError(t, cl.Update(ctx, baseline))
		bs := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
		require.Len(t, biosOps(smcHost), 2)
		stuckOp := bs.Status.Proposed.Operation

		// The operation runs clean, but the fault pins the pending set — the
		// world never converges: the lying-oracle shape, alert-and-hold.
		approve(stuckOp)
		waitOpSucceeded(stuckOp)
		failed := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonConvergenceFailed))
		st.settle(t, ctx, &failed, 3)
		assert.Len(t, biosOps(smcHost), 2, "a failed convergence never re-proposes its own hash")

		// Deleting the record is a veto, not an invitation.
		ns, name, _ := strings.Cut(stuckOp, "/")
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}))
		gone := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonOperationGone))
		st.settle(t, ctx, &gone, 3)
		assert.Len(t, biosOps(smcHost), 1, "the hash stays spent after the record is gone")

		// Only a changed desired state re-proposes — and the fresh staging
		// overwrites the stale contradictory pending value left by the fault.
		smcState.SetFault(sim.FaultBiosStuck, false)
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "x12-baseline"}, baseline))
		baseline.Spec.Attributes["CorrectableErrorThreshold"] = jv(t, 2000)
		require.NoError(t, cl.Update(ctx, baseline))
		bs = waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
		approve(bs.Status.Proposed.Operation)
		waitOpSucceeded(bs.Status.Proposed.Operation)
		bs = waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonInSync))
		assert.Equal(t, "2000", string(bs.Status.Observed["CorrectableErrorThreshold"].Raw))
	})

	st.run(t, "retry of a failed proposal proposes the plan again", func(t *testing.T) {
		baseline := &metalv1alpha1.BiosProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "x12-baseline"}, baseline))
		baseline.Spec.Attributes["CorrectableErrorThreshold"] = jv(t, 3000)
		require.NoError(t, cl.Update(ctx, baseline))
		bs := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
		plan := *bs.Status.Proposed
		ns, name, _ := strings.Cut(plan.Operation, "/")
		key := client.ObjectKey{Namespace: ns, Name: name}

		// The operation fails, as the executor records a refused step. The
		// sim has no fault that refuses a BIOS PATCH the schema admits, so
		// the verdict is written here; only the convergence lane is under
		// test.
		var failed metalv1alpha1.HostOperation
		require.Eventually(t, func() bool {
			if err := cl.Get(ctx, key, &failed); err != nil {
				return false
			}
			meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{
				Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
				Reason: metalv1alpha1.ReasonStepFailed, Message: "step 1/2 applyBios: refused",
			})
			return cl.Status().Update(ctx, &failed) == nil
		}, waitFor, tick)
		held := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonOperationFailed))
		assert.Contains(t, converged(held).Message, "pk-metal retry "+plan.Operation, "the condition says how to go on")
		st.settle(t, ctx, &held, 3)
		assert.Len(t, biosOps(smcHost), 3, "a failure alone proposes nothing")

		// pk-metal retry: an approver marks the failed operation. That is a
		// patch the approval policy admits; it approves nothing.
		require.NoError(t, cl.Get(ctx, key, &failed))
		mark := client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"`+metalv1alpha1.AnnotationRetry+`":"now"}}}`))
		require.NoError(t, aliceCl.Patch(ctx, &failed, mark))

		var next metalv1alpha1.HostOperation
		require.Eventually(t, func() bool {
			cur := settingsOf(smcHost)
			if !settingsReason(metalv1alpha1.ReasonAwaitingOperation)(cur) || cur.Status.Proposed == nil {
				return false
			}
			ns, name, _ := strings.Cut(cur.Status.Proposed.Operation, "/")
			return cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &next) == nil && next.UID != failed.UID
		}, waitFor, tick, "the operator never proposed the plan again")
		assert.Equal(t, plan.PlanHash, next.Spec.PlanHash, "the same plan")
		assert.Empty(t, next.Spec.Approval.ApprovedBy, "a new proposal waits for its own approval")
		assert.NotContains(t, next.Annotations, metalv1alpha1.AnnotationRetry)
		// The replacement may reuse the name; the failed operation itself is gone.
		if got := (metalv1alpha1.HostOperation{}); cl.Get(ctx, key, &got) == nil {
			assert.NotEqual(t, failed.UID, got.UID, "the failed operation is replaced, not kept beside")
		}
		assert.Len(t, biosOps(smcHost), 3)

		nextRef := next.Namespace + "/" + next.Name
		approve(nextRef)
		waitOpSucceeded(nextRef)
		waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonInSync))
		// Keep the record count the later subtests reason about.
		require.NoError(t, cl.Delete(ctx, &next))
		require.Eventually(t, func() bool { return len(biosOps(smcHost)) == 2 }, waitFor, tick)
	})

	st.run(t, "firmware converges before bios", func(t *testing.T) {
		fw := &metalv1alpha1.FirmwareProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "x12-fw"},
			Spec: metalv1alpha1.FirmwareProfileSpec{
				Match:   metav1.LabelSelector{MatchLabels: map[string]string{"metal.pk.io/model": "X12DPi-N6"}},
				Manager: metalv1alpha1.FirmwareTarget{Version: "02.00.00", Image: imageRef},
			}}
		require.NoError(t, cl.Create(ctx, fw))
		h := waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			c := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionFirmwareConverged)
			return c != nil && c.Reason == metalv1alpha1.ReasonAwaitingOperation
		})
		require.NotNil(t, h.Status.FirmwareProposed)

		// BIOS drift arriving now must hold behind the firmware.
		role := &metalv1alpha1.BiosProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "role-worker"}, role))
		role.Spec.Attributes["QuietBoot"] = jv(t, true)
		require.NoError(t, cl.Update(ctx, role))
		waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingFirmware))
		assert.Len(t, biosOps(smcHost), 2, "no bios proposal while firmware is unconverged")

		// Approve the flash: OCI pull, multipart push, BMC reboot, version
		// readback — then the endpoint poll observes it and the bios lane wakes.
		approve(h.Status.FirmwareProposed.Operation)
		waitOpSucceeded(h.Status.FirmwareProposed.Operation)
		waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			c := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionFirmwareConverged)
			return c != nil && c.Status == metav1.ConditionTrue
		})
		ep := metalv1alpha1.ManagementEndpoint{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ci-smc"}, &ep))
		assert.Equal(t, "02.00.00", ep.Status.FirmwareVersion)
		var bmcEntry *metalv1alpha1.FirmwareInventoryItem
		for i := range ep.Status.Firmware {
			if ep.Status.Firmware[i].ID == "BMC" {
				bmcEntry = &ep.Status.Firmware[i]
			}
		}
		require.NotNil(t, bmcEntry, "the inventory rides endpoint status now")
		assert.Equal(t, "02.00.00", bmcEntry.Version)

		bs := waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonAwaitingOperation))
		approve(bs.Status.Proposed.Operation)
		waitOpSucceeded(bs.Status.Proposed.Operation)
		waitSettings(smcHost, settingsReason(metalv1alpha1.ReasonInSync))
		waitHost(smcHost, func(h metalv1alpha1.Host) bool {
			b := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionBiosConverged)
			f := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionFirmwareConverged)
			return b != nil && b.Status == metav1.ConditionTrue && f != nil && f.Status == metav1.ConditionTrue
		})
	})

	st.run(t, "no proven transport holds the flash", func(t *testing.T) {
		fw := &metalv1alpha1.FirmwareProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "ibmc-fw"},
			Spec: metalv1alpha1.FirmwareProfileSpec{
				Match:   metav1.LabelSelector{MatchLabels: map[string]string{"metal.pk.io/vendor": "Huawei"}},
				Manager: metalv1alpha1.FirmwareTarget{Version: "9.99", Image: imageRef},
			}}
		require.NoError(t, cl.Create(ctx, fw))
		h := waitHost(huaweiHost, func(h metalv1alpha1.Host) bool {
			c := meta.FindStatusCondition(h.Status.Conditions, metalv1alpha1.ConditionFirmwareConverged)
			return c != nil && c.Reason == metalv1alpha1.ReasonNoUpdateTransport
		})
		assert.Nil(t, h.Status.FirmwareProposed, "drift is visible but nothing is proposed over an unproven transport")
		ops := &metalv1alpha1.HostOperationList{}
		require.NoError(t, cl.List(ctx, ops, client.InNamespace("default")))
		for _, op := range ops.Items {
			assert.NotEqual(t, huaweiHost, op.Spec.HostRef.Name, "no operation for the held host")
		}
	})

	st.run(t, "finished operations expire by TTL", func(t *testing.T) {
		ttl := int32(1)
		op := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "ttl-cycle", Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:                 metalv1alpha1.Reference{Name: huaweiHost},
				ProposedBy:              operator,
				Actions:                 []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
				TTLSecondsAfterFinished: &ttl,
			},
		}
		require.NoError(t, cl.Create(ctx, op))
		approve("default/ttl-cycle")
		require.Eventually(t, func() bool {
			got := metalv1alpha1.HostOperation{}
			err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ttl-cycle"}, &got)
			return err != nil
		}, waitFor, tick, "the finished operation never expired")
	})
}
