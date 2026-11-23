package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func TestVerifiedCondition(t *testing.T) {
	snap := &metalv1alpha1.AssetSnapshot{Serial: "SIMX12B000001", BMCMac: "02:00:5a:3a:cc:18"}
	tests := []struct {
		name    string
		snap    *metalv1alpha1.AssetSnapshot
		mac     string
		anchors []string
		status  metav1.ConditionStatus
		reason  string
	}{
		{name: "board serial anchors (Supermicro: system serial empty)",
			snap: snap, mac: "02:00:5a:3a:cc:18", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "system serial anchors (Huawei)",
			snap:    &metalv1alpha1.AssetSnapshot{Serial: "SIMHW2288V5000391"},
			anchors: []string{"SIMHW2288V5000391", "SIMHW2288V5000391"},
			status:  metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "serial mismatch",
			snap: snap, mac: "02:00:5a:3a:cc:18", anchors: []string{"OTHER"},
			status: metav1.ConditionFalse, reason: metalv1alpha1.ReasonSerialMismatch},
		{name: "mac mismatch outranks serial (wrong box behind the address)",
			snap: snap, mac: "02:00:48:0b:74:83", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionFalse, reason: metalv1alpha1.ReasonMACMismatch},
		{name: "mac compare is case-insensitive",
			snap: snap, mac: "02:00:5A:3A:CC:18", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "an asserted mac in dashed upper case matches",
			snap: &metalv1alpha1.AssetSnapshot{Serial: "SIMX12B000001", BMCMac: "02-00-5A-3A-CC-18"},
			mac:  "02:00:5a:3a:cc:18", anchors: []string{"SIMX12B000001"},
			status: metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "no asserted mac skips the mac check",
			snap: &metalv1alpha1.AssetSnapshot{Serial: "SIMX12B000001"}, mac: "02:00:48:0b:74:83",
			anchors: []string{"SIMX12B000001"},
			status:  metav1.ConditionTrue, reason: "SerialMatch"},
		{name: "no anchors cannot verify",
			snap: snap, mac: "02:00:5a:3a:cc:18",
			status: metav1.ConditionUnknown, reason: "NoSerialAnchor"},
		{name: "no snapshot cannot verify",
			status: metav1.ConditionUnknown, reason: "NoAssertion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, _ := verifiedCondition(tt.snap, tt.mac, tt.anchors)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.reason, reason)
		})
	}
}

func TestSerialAnchors(t *testing.T) {
	anchors := serialAnchors(
		bmc.Chassis{BoardSerial: "SIMX12B000001"},
		[]bmc.System{{ID: "1", SerialNumber: ""}, {ID: "2", SerialNumber: "S2"}},
	)
	assert.Equal(t, []string{"SIMX12B000001", "S2"}, anchors, "empty system serials are dropped")
}

func TestEndpointCapabilities(t *testing.T) {
	x12 := endpointCapabilities(bmc.Capabilities{MediaDialect: map[string]bmc.MediaDialect{
		"VirtualMedia1": bmc.DialectStandard,
		"VirtualMedia2": bmc.DialectStandard,
	}})
	assert.True(t, x12.DualMedia)
	assert.Equal(t, "insert-media", x12.MediaDialects["VirtualMedia1"])

	ibmc := endpointCapabilities(bmc.Capabilities{MediaDialect: map[string]bmc.MediaDialect{
		"CD":       bmc.DialectVmmControl,
		"USBStick": bmc.DialectNone,
	}})
	assert.False(t, ibmc.DualMedia, "a slot with no insert mechanism does not count")
	assert.Equal(t, "vmm-control", ibmc.MediaDialects["CD"])
}
