package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// A machine behind a one-slot BMC boots one installer that carries its
// config. The step list alone would read like a generic install; the plan
// says that this medium is a secret built for this node.
func TestRenderEmbeddedInstallerPlan(t *testing.T) {
	op, objs := bootPlan()
	op.Spec.Actions = []metalv1alpha1.HostOperationAction{
		{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "CD"}},
		{InsertMedia: &metalv1alpha1.InsertMediaAction{MediaID: "CD",
			BootAssetRef: &metalv1alpha1.Reference{Name: "node-1-embedded"},
			Digest:       "sha256:fedcba9876543210fedcba9876543210"}},
		{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd", Once: ptr.To(true)}},
		{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{NodeRef: metalv1alpha1.Reference{Name: "node-1"}, TimeoutSeconds: 1800}},
		{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "CD"}},
	}
	embedded := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-embedded"},
		Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyEmbeddedInstaller, TalosVersion: "v1.14.6",
			NodeRef: &osv1alpha1.Reference{Name: "node-1"}},
	}
	pc, err := loadPlanContext(context.Background(), fakeClient(append([]client.Object{embedded}, objs[1:]...)...), op)
	require.NoError(t, err)

	want := `operation pk-metal/provision-node1-1a2b3c4d
  why:         Provisioning
  proposed by: system:serviceaccount:pk-metal:pk-metal-manager
host pk0001 (asset node-1, serial PK0001)
  BMC: https://10.0.0.1:8441 (endpoint node-1-bmc)
steps, in this order:
   1. eject whatever is mounted on virtual media slot CD
   2. mount the Talos v1.14.6 installer with the machine config of TalosNode node-1 built in (boot asset node-1-embedded) on virtual media slot CD (content sha256:fedcba987654…)
   3. boot from Cd at the next start only
   4. power cycle: if the machine is on, force it off (no clean shutdown) and on again; if it is off, power it on
   5. wait up to 30m for TalosNode node-1 to report in at 10.0.0.11
   6. eject whatever is mounted on virtual media slot CD
note: the BMC fetches each medium from the operator's media service over HTTPS, certificate not verified by the BMC; a grant, bound to the first address that fetches it, and the digest of each medium protect it
note: the installer on slot CD carries the machine config of TalosNode node-1, with its keys and tokens. It is built for this node only, the BMC fetches it under a grant made for this boot, and it is ejected when the node reports in
boots TalosNode node-1: address 10.0.0.11, role controlplane
  installs Talos onto the disk that matches serial=disk-0001; everything on that disk is overwritten
`
	assert.Equal(t, want, renderPlan(op, pc))
}
