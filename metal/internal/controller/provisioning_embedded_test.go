package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestDecideShape(t *testing.T) {
	twoSlots := endpointView{insertable: []string{"CD1", "USB1"}, bootSlots: []string{"CD1"}}
	oneCD := endpointView{insertable: []string{"CD"}, bootSlots: []string{"CD"}}
	oneStick := endpointView{insertable: []string{"USBStick"}}
	for _, tc := range []struct {
		name      string
		view      endpointView
		reachable bool
		version   string
		want      bootShape
		hold      string
		msg       string
	}{
		{name: "two slots: installer and config side by side", view: twoSlots, version: "v1.14.6", want: shapeTwoMedia},
		{name: "two slots, machine answers: still two media", view: twoSlots, reachable: true, version: "v1.14.6", want: shapeTwoMedia},
		{name: "one slot, Talos on disk answers: the config alone", view: oneCD, reachable: true, version: "v1.14.6", want: shapeConfigOnly},
		{name: "one slot, blank machine: installer with the config built in", view: oneCD, version: "v1.14.6", want: shapeEmbedded},
		{name: "one slot, blank, a release that cannot read an embedded config", view: oneCD, version: "v1.11.3",
			hold: metalv1alpha1.ReasonSingleMediumUnsupported, msg: "Talos 1.12 or later"},
		{name: "one slot that boots no ISO, blank", view: oneStick, version: "v1.14.6",
			hold: metalv1alpha1.ReasonSingleMediumUnsupported, msg: "none of them boots an ISO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, hold, msg := decideShape("host-a", tc.view, tc.reachable, tc.version)
			assert.Equal(t, tc.hold, hold)
			if tc.hold != "" {
				assert.Contains(t, msg, tc.msg)
				assert.Contains(t, msg, "host-a")
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// Every boot the lane proposes has the shape the protocol model abstracts:
// mount, (override,) power cycle, wait for the check-in, eject. The model's
// step 0 ("the media are mounted") covers one medium as well as two.
func TestEveryPlanShapeMountsPowersWaitsEjects(t *testing.T) {
	r := &HostClaimReconciler{OperationsNamespace: "pk-metal"}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	config := &osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "node-config"}}
	installer := &osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "installer"}}
	embedded := &osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "node-embedded"}}
	for name, tc := range map[string]struct {
		plan              provisionPlan
		config, installer *osv1alpha1.BootAsset
		mounts            []string // slot=asset, in order
	}{
		"two media": {
			plan:   provisionPlan{ArtifactDigest: "sha256:c", Slots: []string{"VM1", "VM2"}, Target: "UsbCd"},
			config: config, installer: installer, mounts: []string{"VM1=installer", "VM2=node-config"},
		},
		"config only": {
			plan:   provisionPlan{ArtifactDigest: "sha256:c", Slots: []string{"CD"}},
			config: config, mounts: []string{"CD=node-config"},
		},
		"embedded": {
			plan:      provisionPlan{ArtifactDigest: "sha256:e", Slots: []string{"CD"}, Target: "Cd", Embedded: true},
			installer: embedded, mounts: []string{"CD=node-embedded"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			op := r.provisionOp(testClaim("node"), "host-a", "op", tc.plan, node, tc.config, tc.installer)
			var mounts []string
			phase := 0 // 0 ejects, 1 mounts, 2 override, 3 power, 4 await, 5 ejects
			advance := func(to int) {
				require.GreaterOrEqual(t, to, phase, "step order in %+v", op.Spec.Actions)
				phase = to
			}
			for _, a := range op.Spec.Actions {
				switch {
				case a.EjectMedia != nil && phase < 1:
				case a.InsertMedia != nil:
					advance(1)
					mounts = append(mounts, a.InsertMedia.MediaID+"="+a.InsertMedia.BootAssetRef.Name)
					if a.InsertMedia.BootAssetRef.Name != "installer" {
						assert.Equal(t, tc.plan.ArtifactDigest, a.InsertMedia.Digest,
							"the medium carrying the config is pinned to what was approved")
					}
				case a.SetBootOverride != nil:
					advance(2)
					assert.Equal(t, tc.plan.Target, a.SetBootOverride.Target)
				case a.PowerCycle != nil:
					advance(3)
				case a.AwaitCheckIn != nil:
					advance(4)
				case a.EjectMedia != nil:
					advance(5)
				default:
					t.Fatalf("unexpected action %+v", a)
				}
			}
			assert.Equal(t, 5, phase, "ends with the ejects")
			assert.Equal(t, tc.mounts, mounts)
		})
	}
}

// A plan gains a field for the embedded shape; the plans of the other shapes
// must hash as before, or every pending proposal would be re-proposed by a
// manager upgrade.
func TestEmbeddedFieldLeavesOtherPlanHashesAlone(t *testing.T) {
	type before struct {
		ConfigDigest   string   `json:"configDigest"`
		ArtifactDigest string   `json:"artifactDigest"`
		Installer      string   `json:"installer,omitempty"`
		Slots          []string `json:"slots"`
		Target         string   `json:"target,omitempty"`
		Mode           string   `json:"mode,omitempty"`
	}
	p := provisionPlan{ConfigDigest: "sha256:a", ArtifactDigest: "sha256:b", Installer: "https://i", Slots: []string{"1", "2"}, Target: "Cd"}
	assert.Equal(t, planHash(before{p.ConfigDigest, p.ArtifactDigest, p.Installer, p.Slots, p.Target, p.Mode}), planHash(p))
	p.Embedded = true
	assert.NotEqual(t, planHash(before{p.ConfigDigest, p.ArtifactDigest, p.Installer, p.Slots, p.Target, p.Mode}), planHash(p))
}

// huaweiEndpoint is an inventoried endpoint with one slot that takes an
// image, as the iBMC shows itself: CD, untyped, through the OEM action.
func huaweiEndpoint() *metalv1alpha1.ManagementEndpoint {
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ibmc"}}
	ep.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionInventoried, Status: metav1.ConditionTrue,
		Reason: "InventoryGathered", LastTransitionTime: metav1.Now()}}
	ep.Status.Capabilities = &metalv1alpha1.EndpointCapabilities{MediaDialects: map[string]string{"CD": "vmm-control", "USBStick": "none"}}
	ep.Status.VirtualMedia = []metalv1alpha1.DiscoveredMedia{{ID: "CD"}, {ID: "USBStick", MediaTypes: []string{"USBStick"}}}
	ep.Status.Systems = []metalv1alpha1.DiscoveredSystem{{ID: "1", BootTargets: []string{"None", "Pxe", "Cd", "Hdd"}, BootOverrideMode: "UEFI"}}
	return ep
}

func TestBlankMachineBehindOneSlotBootsAnEmbeddedInstaller(t *testing.T) {
	ctx := context.Background()
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab", UID: "lab-uid"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", Schematic: &osv1alpha1.Schematic{
			Customization: osv1alpha1.SchematicCustomization{ExtraKernelArgs: []string{"console=ttyS0"}},
		}},
	}
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2", UID: "node-2-uid"},
		Spec:       osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}},
		Status: osv1alpha1.TalosNodeStatus{ConfigDigest: "sha256:render", Conditions: []metav1.Condition{{
			Type: osv1alpha1.ConditionRendered, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonConfigRendered, LastTransitionTime: metav1.Now(),
		}}},
	}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "ibmc"}, SystemID: "1", Reachable: true}}
	host.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionVerified, Status: metav1.ConditionTrue,
		Reason: "Verified", LastTransitionTime: metav1.Now()}}
	claim := testClaim("node-2")
	r := claimReconcilerFor(t, cluster, node, host, huaweiEndpoint(), claim)

	provisioned := func() *metav1.Condition {
		return meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
	}
	require.NoError(t, r.provision(ctx, claim, "host-a"))
	c := provisioned()
	require.NotNil(t, c)
	assert.Equal(t, metalv1alpha1.ReasonAwaitingBootAsset, c.Reason, c.Message)
	assert.Contains(t, c.Message, "config built in")

	// The lane asked for the installer: this node, the cluster's release, the
	// node's schematic without the config-medium argument.
	ba := &osv1alpha1.BootAsset{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: "node-2-embedded"}, ba))
	assert.Equal(t, osv1alpha1.StrategyEmbeddedInstaller, ba.Spec.Strategy)
	assert.Equal(t, "node-2", ba.Spec.NodeRef.Name)
	assert.Equal(t, "v1.14.6", ba.Spec.TalosVersion)
	assert.Equal(t, []string{"console=ttyS0"}, ba.Spec.Schematic.Customization.ExtraKernelArgs)
	assert.True(t, ownedBy(ba, node), "collected with its node")
	assert.Empty(t, opsFor(t, r, "host-a"), "nothing to mount yet")

	// The build lands.
	ba.Status = osv1alpha1.BootAssetStatus{Digest: "sha256:iso", Size: 400 << 20, ConfigDigest: "sha256:render", BuildKey: "k1",
		Conditions: []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonArtifactBuilt,
			ObservedGeneration: ba.Generation, LastTransitionTime: metav1.Now()}}}
	require.NoError(t, r.Update(ctx, ba))

	require.NoError(t, r.provision(ctx, claim, "host-a"))
	ops := opsFor(t, r, "host-a")
	require.Len(t, ops, 1, provisioned().Message)
	var inserts []metalv1alpha1.InsertMediaAction
	var override *metalv1alpha1.SetBootOverrideAction
	for _, a := range ops[0].Spec.Actions {
		if a.InsertMedia != nil {
			inserts = append(inserts, *a.InsertMedia)
		}
		if a.SetBootOverride != nil {
			override = a.SetBootOverride
		}
	}
	require.Len(t, inserts, 1, "one slot, one medium")
	assert.Equal(t, "CD", inserts[0].MediaID)
	assert.Equal(t, "node-2-embedded", inserts[0].BootAssetRef.Name)
	assert.Equal(t, "sha256:iso", inserts[0].Digest, "the approval pins the installer's digest")
	require.NotNil(t, override, "a blank machine has nothing to boot from disk")
	assert.Equal(t, "Cd", override.Target)
	assert.Equal(t, "sha256:render", ops[0].Annotations[annotationConfigDigest])

	t.Run("a new render waits for a new build", func(t *testing.T) {
		node.Status.ConfigDigest = "sha256:render2"
		require.NoError(t, r.Update(ctx, node))
		require.NoError(t, r.provision(ctx, claim, "host-a"))
		assert.Equal(t, metalv1alpha1.ReasonAwaitingBootAsset, provisioned().Reason)
		assert.Contains(t, provisioned().Message, "carries render")
	})

	t.Run("a new Talos release updates the installer's inputs", func(t *testing.T) {
		cluster.Spec.TalosVersion = "v1.14.7"
		require.NoError(t, r.Update(ctx, cluster))
		require.NoError(t, r.provision(ctx, claim, "host-a"))
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ba), ba))
		assert.Equal(t, "v1.14.7", ba.Spec.TalosVersion)
		assert.Equal(t, metalv1alpha1.ReasonAwaitingBootAsset, provisioned().Reason)
	})
}

func opsFor(t *testing.T, r *HostClaimReconciler, host string) []metalv1alpha1.HostOperation {
	t.Helper()
	l := &metalv1alpha1.HostOperationList{}
	require.NoError(t, r.List(context.Background(), l, client.InNamespace("pk-metal")))
	var out []metalv1alpha1.HostOperation
	for _, op := range l.Items {
		if op.Spec.HostRef.Name == host {
			out = append(out, op)
		}
	}
	return out
}

// TestConfigMediaGoAfterTheBoot: the media the lane made that carry the
// node's machine config are deleted once the install is handed off or the
// boot failed, and not while a boot still runs. A failed boot holds the
// claim without making them again; a medium a person made, and the factory
// installer, which carries no secret, stay.
func TestConfigMediaGoAfterTheBoot(t *testing.T) {
	ctx := context.Background()
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab", UID: "lab-uid"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6"}}
	rendered := func() *osv1alpha1.TalosNode {
		return &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node-b", UID: "node-b-uid"},
			Spec:       osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}},
			Status: osv1alpha1.TalosNodeStatus{ConfigDigest: "sha256:render", Conditions: []metav1.Condition{{
				Type: osv1alpha1.ConditionRendered, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonConfigRendered,
				LastTransitionTime: metav1.Now(),
			}}},
		}
	}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	host.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "ibmc"}, SystemID: "1", Reachable: true}}
	host.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionVerified, Status: metav1.ConditionTrue,
		Reason: "Verified", LastTransitionTime: metav1.Now()}}
	media := func() []client.Object {
		lane := map[string]string{labelProvisioning: "true"}
		return []client.Object{
			&osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "node-b-config", Labels: lane},
				Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node-b"}}},
			&osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "node-b-embedded", Labels: lane},
				Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyEmbeddedInstaller, NodeRef: &osv1alpha1.Reference{Name: "node-b"}}},
			&osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "lab-installer-0123abcd", Labels: lane},
				Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: "v1.14.6"}},
			&osv1alpha1.BootAsset{ObjectMeta: metav1.ObjectMeta{Name: "by-hand"},
				Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node-b"}}},
		}
	}
	left := func(t *testing.T, r *HostClaimReconciler) []string {
		t.Helper()
		l := &osv1alpha1.BootAssetList{}
		require.NoError(t, r.List(ctx, l))
		var names []string
		for _, a := range l.Items {
			names = append(names, a.Name)
		}
		return names
	}
	failed := func() *metalv1alpha1.HostOperation {
		op := bootOp("boot", "node-b", false, true)
		op.Annotations = map[string]string{annotationConfigDigest: "sha256:render"}
		op.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionFalse,
			Reason: metalv1alpha1.ReasonStepFailed, Message: "step 3/6 insertMedia: the BMC refused the image", LastTransitionTime: metav1.Now()}}
		return op
	}

	t.Run("handed off", func(t *testing.T) {
		r := claimReconcilerFor(t, append(media(), cluster, rendered(), host, huaweiEndpoint(), bootOp("boot", "node-b", true, true))...)
		claim := testClaim("node-b")
		require.NoError(t, r.provision(ctx, claim, "host-a"))
		require.Equal(t, metalv1alpha1.ReasonHandedOff, meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned).Reason)
		assert.ElementsMatch(t, []string{"lab-installer-0123abcd", "by-hand"}, left(t, r))
	})
	t.Run("a boot still runs", func(t *testing.T) {
		node := rendered()
		node.Status.Conditions = append(node.Status.Conditions, metav1.Condition{Type: osv1alpha1.ConditionReady,
			Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady, LastTransitionTime: metav1.Now()})
		r := claimReconcilerFor(t, append(media(), cluster, node, host, huaweiEndpoint(), bootOp("boot", "node-b", false, true))...)
		claim := testClaim("node-b")
		require.NoError(t, r.provision(ctx, claim, "host-a"))
		require.Equal(t, metalv1alpha1.ReasonHandedOff, meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned).Reason)
		assert.Len(t, left(t, r), 4, "the boot ejects its media first")
	})
	t.Run("failed", func(t *testing.T) {
		r := claimReconcilerFor(t, append(media(), cluster, rendered(), host, huaweiEndpoint(), failed())...)
		claim := testClaim("node-b")
		for range 2 {
			require.NoError(t, r.provision(ctx, claim, "host-a"))
			c := meta.FindStatusCondition(claim.Status.Conditions, metalv1alpha1.ConditionProvisioned)
			require.NotNil(t, c)
			assert.Equal(t, metalv1alpha1.ReasonOperationFailed, c.Reason, c.Message)
			assert.Contains(t, c.Message, "the BMC refused the image")
			assert.Contains(t, c.Message, "pk-metal retry pk-metal/boot")
			assert.ElementsMatch(t, []string{"lab-installer-0123abcd", "by-hand"}, left(t, r), "and none is made again")
			assert.Len(t, opsFor(t, r, "host-a"), 1, "nothing new is proposed")
		}
	})
	t.Run("a retry makes them anew", func(t *testing.T) {
		r := claimReconcilerFor(t, cluster, rendered(), host, huaweiEndpoint())
		claim := testClaim("node-b")
		require.NoError(t, r.provision(ctx, claim, "host-a"))
		assert.Contains(t, left(t, r), "node-b-embedded")
	})
}
