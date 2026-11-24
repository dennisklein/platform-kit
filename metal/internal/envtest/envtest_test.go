// The API-scaffold gate: the generated CRDs install into a real apiserver, the types round-trip
// through it, and the schema actually rejects what the markers claim it rejects.
// Needs the envtest control-plane binaries — `mise run envtest` provides them and
// runs this; a bare `go test ./...` skips.
package envtest_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

func TestAPI(t *testing.T) {
	st := startEnv(t)

	cl := st.cl
	ctx := t.Context()

	st.run(t, "asset round-trip and validation", func(t *testing.T) {
		asset := &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: "asset-07"},
			Spec: metalv1alpha1.AssetSpec{
				CID:            "ASSET-07",
				Serial:         "SIMHW2288V5000397",
				BMCMac:         "02:00:48:0b:74:83",
				VLAN:           100,
				CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"},
			},
		}
		require.NoError(t, cl.Create(ctx, asset))
		got := &metalv1alpha1.Asset{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "asset-07"}, got))
		assert.Equal(t, asset.Spec, got.Spec)

		missingCID := &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: "no-cid"},
			Spec: metalv1alpha1.AssetSpec{
				Serial:         "X",
				CredentialsRef: corev1.LocalObjectReference{Name: "c"},
			},
		}
		assert.Error(t, cl.Create(ctx, missingCID), "cid MinLength must reject")

		// The forms people paste from labels and inventories are accepted.
		for name, mac := range map[string]string{"upper-mac": "48:DC:2D:0B:74:83", "dashed-mac": "48-dc-2d-0b-74-84"} {
			ok := asset.DeepCopy()
			ok.ObjectMeta = metav1.ObjectMeta{Name: name}
			ok.Spec.BMCMac = mac
			assert.NoError(t, cl.Create(ctx, ok), mac)
		}
		badMAC := asset.DeepCopy()
		badMAC.ObjectMeta = metav1.ObjectMeta{Name: "bad-mac"}
		badMAC.Spec.BMCMac = "48:DC:2D:0B:74"
		err := cl.Create(ctx, badMAC)
		require.Error(t, err, "five bytes are not a MAC")
		assert.Contains(t, err.Error(), "such as 0c:c4:7a:12:34:56", "the refusal shows the expected form")
	})

	st.run(t, "endpoint status subresource and phase enum", func(t *testing.T) {
		ep := &metalv1alpha1.ManagementEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "asset-07"},
			Spec: metalv1alpha1.ManagementEndpointSpec{
				AssetRef: metalv1alpha1.Reference{Name: "asset-07"},
				Address:  "https://[2001:db8:0:100::14]",
			},
		}
		require.NoError(t, cl.Create(ctx, ep))

		plainHTTP := &metalv1alpha1.ManagementEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "plain-http"},
			Spec: metalv1alpha1.ManagementEndpointSpec{
				AssetRef: metalv1alpha1.Reference{Name: "asset-07"},
				Address:  "http://[2001:db8:0:100::14]",
			},
		}
		assert.Error(t, cl.Create(ctx, plainHTTP), "address pattern must reject plain http")

		ep.Status.Conditions = []metav1.Condition{{
			Type: metalv1alpha1.ConditionReachable, Status: metav1.ConditionTrue,
			Reason: "SessionUp", LastTransitionTime: metav1.Now(),
		}}
		ep.Status.Vendor = "Huawei"
		phase.StampEndpoint(ep)
		require.NoError(t, cl.Status().Update(ctx, ep))

		got := &metalv1alpha1.ManagementEndpoint{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "asset-07"}, got))
		assert.Equal(t, metalv1alpha1.EndpointInventorying, got.Status.Phase)
		assert.Equal(t, "Huawei", got.Status.Vendor)

		got.Status.Phase = "Bogus"
		assert.Error(t, cl.Status().Update(ctx, got), "phase enum must reject unknown values")
	})

	st.run(t, "host status shape", func(t *testing.T) {
		h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "node-2288h"}}
		require.NoError(t, cl.Create(ctx, h))

		h.Status = metalv1alpha1.HostStatus{
			Identity: &metalv1alpha1.HostIdentity{
				UUID:   "2D0B7483-48DC-B772-EB11-5D279EA3DCD3",
				Serial: "SIMHW2288V5000397",
				Vendor: "Huawei",
			},
			ManagedBy: []metalv1alpha1.ManagementPath{{
				EndpointRef: metalv1alpha1.Reference{Name: "asset-07"},
				SystemID:    "1",
				Reachable:   true,
			}},
			Conditions: []metav1.Condition{{
				Type: metalv1alpha1.ConditionInspected, Status: metav1.ConditionTrue,
				Reason: "InventoryGathered", LastTransitionTime: metav1.Now(),
			}},
		}
		phase.StampHost(h, phase.HostFacts{})
		require.NoError(t, cl.Status().Update(ctx, h))

		got := &metalv1alpha1.Host{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "node-2288h"}, got))
		assert.Equal(t, metalv1alpha1.HostInspecting, got.Status.Phase)
		require.Len(t, got.Status.ManagedBy, 1)
		assert.Equal(t, "asset-07", got.Status.ManagedBy[0].EndpointRef.Name)
		require.NotNil(t, got.Status.Identity)
		assert.Equal(t, "SIMHW2288V5000397", got.Status.Identity.Serial)
	})

	st.run(t, "talos actions and rolloutRef", func(t *testing.T) {
		node := metalv1alpha1.Reference{Name: "node-1"}
		digest := "sha256:" + strings.Repeat("a", 64)
		op := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "talos-actions", Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:    metalv1alpha1.Reference{Name: "node-2288h"},
				ProposedBy: "alice",
				RolloutRef: &metalv1alpha1.RolloutReference{Name: "upgrade-lab", PlanHash: "abc123"},
				Actions: []metalv1alpha1.HostOperationAction{
					{Cordon: &metalv1alpha1.CordonAction{NodeRef: node}},
					{Drain: &metalv1alpha1.DrainAction{NodeRef: node}},
					{EtcdLeave: &metalv1alpha1.EtcdLeaveAction{NodeRef: node}},
					{EtcdRemoveMember: &metalv1alpha1.EtcdRemoveMemberAction{NodeRef: node, MemberID: "8e9e05c52164694d", Hostname: "node-1"}},
					{TalosApplyConfig: &metalv1alpha1.TalosApplyConfigAction{NodeRef: node,
						ConfigSecretRef: metalv1alpha1.Reference{Name: "machineconfig-node-1"}, Digest: digest}},
					{TalosUpgrade: &metalv1alpha1.TalosUpgradeAction{NodeRef: node,
						Image: "factory.talos.dev/installer/x:v1.14.7@" + digest, Version: "v1.14.7"}},
					{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: node}},
					{TalosShutdown: &metalv1alpha1.TalosShutdownAction{NodeRef: node}},
					{TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node,
						SystemPartitions: []metalv1alpha1.SystemPartition{"STATE", "EPHEMERAL"}}},
					{WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{NodeRef: node, Version: "v1.14.7"}},
					{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{NodeRef: node}},
					{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: node}},
				},
			},
		}
		require.NoError(t, cl.Create(ctx, op))
		got := &metalv1alpha1.HostOperation{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(op), got))
		a := got.Spec.Actions
		// The schema defaults are the safe readings: a drain bounded, a config
		// applied without a reboot, a reset that leaves etcd first.
		assert.Equal(t, int32(900), a[1].Drain.TimeoutSeconds)
		assert.Equal(t, "NoReboot", a[4].TalosApplyConfig.Mode)
		require.NotNil(t, a[5].TalosUpgrade.Preserve)
		assert.True(t, *a[5].TalosUpgrade.Preserve)
		assert.Equal(t, "Default", a[6].TalosReboot.Mode)
		require.NotNil(t, a[8].TalosReset.Graceful)
		assert.True(t, *a[8].TalosReset.Graceful)
		assert.Equal(t, "All", a[8].TalosReset.Mode)
		assert.Equal(t, int32(1800), a[9].WaitNodeReady.TimeoutSeconds)
		assert.Equal(t, int32(900), a[10].WaitEtcdHealthy.TimeoutSeconds)

		// The rollout binding is as frozen as the actions it vouches for.
		moved := got.DeepCopy()
		moved.Spec.RolloutRef.PlanHash = "other"
		err := cl.Update(ctx, moved)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rolloutRef is frozen")
		dropped := got.DeepCopy()
		dropped.Spec.RolloutRef = nil
		err = cl.Update(ctx, dropped)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rolloutRef is frozen")

		// What the markers refuse, the apiserver refuses.
		for name, bad := range map[string]metalv1alpha1.HostOperationAction{
			"undigested config":    {TalosApplyConfig: &metalv1alpha1.TalosApplyConfigAction{NodeRef: node, ConfigSecretRef: node, Digest: "latest"}},
			"tagged image":         {TalosUpgrade: &metalv1alpha1.TalosUpgradeAction{NodeRef: node, Image: "installer:v1.14.7", Version: "v1.14.7"}},
			"member id not hex":    {EtcdRemoveMember: &metalv1alpha1.EtcdRemoveMemberAction{NodeRef: node, MemberID: "node-1"}},
			"unknown partition":    {TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node, SystemPartitions: []metalv1alpha1.SystemPartition{"BOOT"}}},
			"partitions of users":  {TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node, Mode: "UserDisks", SystemPartitions: []metalv1alpha1.SystemPartition{"STATE"}}},
			"user disks unnamed":   {TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node, Mode: "UserDisks"}},
			"user disks of system": {TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node, Mode: "SystemDisk", UserDisks: []string{"/dev/sdb"}}},
			"user disk not a path": {TalosReset: &metalv1alpha1.TalosResetAction{NodeRef: node, UserDisks: []string{"sdb"}}},
			"unbounded drain":      {Drain: &metalv1alpha1.DrainAction{NodeRef: node, TimeoutSeconds: 5}},
		} {
			o := &metalv1alpha1.HostOperation{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-" + strings.ReplaceAll(name, " ", "-"), Namespace: "default"},
				Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: "node-2288h"}, ProposedBy: "alice",
					Actions: []metalv1alpha1.HostOperationAction{bad}},
			}
			assert.Error(t, cl.Create(ctx, o), name)
		}
	})
}
