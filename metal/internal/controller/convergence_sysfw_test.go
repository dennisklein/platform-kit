package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func sysProfile(name string, layer int32, targets ...metalv1alpha1.SystemFirmwareTarget) metalv1alpha1.FirmwareProfile {
	return metalv1alpha1.FirmwareProfile{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: metalv1alpha1.FirmwareProfileSpec{Layer: layer, System: targets}}
}

func TestMergeSystemFirmware(t *testing.T) {
	bios1 := metalv1alpha1.SystemFirmwareTarget{Component: "BIOS", Version: "1", Image: "r/b@sha256:01"}
	bios2 := metalv1alpha1.SystemFirmwareTarget{Component: "BIOS", Version: "2", Image: "r/b@sha256:02"}
	nic := metalv1alpha1.SystemFirmwareTarget{Component: `NIC\..*`, Version: "9", Image: "r/n@sha256:09"}

	got, conflicts := mergeSystemFirmware([]metalv1alpha1.FirmwareProfile{
		sysProfile("site", 0, bios1, nic), sysProfile("rack", 10, bios2),
	})
	assert.Empty(t, conflicts)
	assert.Equal(t, []metalv1alpha1.SystemFirmwareTarget{bios2, nic}, got, "a later layer overrides per component, sorted")

	_, conflicts = mergeSystemFirmware([]metalv1alpha1.FirmwareProfile{sysProfile("a", 5, bios1), sysProfile("b", 5, bios2)})
	require.Len(t, conflicts, 1)
	assert.Contains(t, conflicts[0], "a and b")

	assert.Empty(t, managerProfiles([]metalv1alpha1.FirmwareProfile{sysProfile("a", 0, bios1)}),
		"a system-only profile is not a manager profile")
}

func TestSystemDrift(t *testing.T) {
	inv := []metalv1alpha1.FirmwareInventoryItem{
		{ID: "BMC", Version: "2.0", Updateable: true},
		{ID: "BIOS", Version: "1", Updateable: true},
		{ID: "NIC.Slot.1", Version: "8", Updateable: true},
		{ID: "NIC.Slot.2", Version: "9", Updateable: true},
		{ID: "CPLD", Version: "1", Updateable: false},
	}
	targets := []metalv1alpha1.SystemFirmwareTarget{
		{Component: "BIOS", Version: "1", Image: "i@sha256:01"},
		{Component: `NIC\.Slot\.[0-9]+`, Version: "9", Image: "i@sha256:09"},
	}
	stages, drifted, hold := systemDrift(targets, inv)
	require.Empty(t, hold)
	assert.Equal(t, []systemStage{{Component: `NIC\.Slot\.[0-9]+`, Version: "9", Image: "i@sha256:09", Entries: []string{"NIC.Slot.1"}}}, stages,
		"only the entries off their version are staged")
	assert.Equal(t, []string{"NIC.Slot.1 serves 8, want 9"}, drifted)

	_, _, hold = systemDrift([]metalv1alpha1.SystemFirmwareTarget{{Component: "NIC", Version: "9"}}, inv)
	assert.Contains(t, hold, "matches no inventory entry", "the pattern is anchored: NIC alone matches no NIC.Slot.n")

	_, _, hold = systemDrift([]metalv1alpha1.SystemFirmwareTarget{{Component: "CPLD", Version: "2"}}, inv)
	assert.Contains(t, hold, "not updateable")

	_, _, hold = systemDrift([]metalv1alpha1.SystemFirmwareTarget{{Component: "(", Version: "2"}}, inv)
	assert.Contains(t, hold, "not a valid pattern")
}

func TestDisruptivePlanShapes(t *testing.T) {
	stage := []metalv1alpha1.HostOperationAction{{ApplyBios: &metalv1alpha1.ApplyBiosAction{}}}
	keys := func(as []metalv1alpha1.HostOperationAction) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.Key())
		}
		return out
	}
	assert.Equal(t, []string{"applyBios", "powerCycle"}, keys(disruptivePlan(nil, stage)))
	worker := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "w1"}, Spec: osv1alpha1.TalosNodeSpec{Role: osv1alpha1.RoleWorker}}
	assert.Equal(t, []string{"applyBios", "cordon", "drain", "talosReboot", "waitNodeReady", "uncordon"},
		keys(disruptivePlan(worker, stage)), "a worker has no etcd member to wait for")
	cp := worker.DeepCopy()
	cp.Spec.Role = osv1alpha1.RoleControlPlane
	assert.Equal(t, []string{"applyBios", "waitEtcdHealthy", "cordon", "drain", "talosReboot", "waitNodeReady", "waitEtcdHealthy", "uncordon"},
		keys(disruptivePlan(cp, stage)))

	desired := map[string]any{"QuietBoot": false}
	assert.Equal(t, planHash(desired), disruptiveHash(desired, nil), "without a node the hash is the one plans had before")
	assert.NotEqual(t, disruptiveHash(desired, worker), disruptiveHash(desired, cp), "the role is part of the plan")
	assert.NotEqual(t, disruptiveHash(desired, nil), disruptiveHash(desired, worker))
}
