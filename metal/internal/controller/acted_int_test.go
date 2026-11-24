package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// A firmware step whose image went to the BMC has acted: an abort or the
// cluster guard waits for it to finish instead of leaving the manager
// halfway through a write.
func TestActedAfterFirmwareUpload(t *testing.T) {
	op := &metalv1alpha1.HostOperation{Spec: metalv1alpha1.HostOperationSpec{Actions: []metalv1alpha1.HostOperationAction{
		{EjectMedia: &metalv1alpha1.EjectMediaAction{}},
		{FlashFirmware: &metalv1alpha1.FlashFirmwareAction{}},
		{StageFirmware: &metalv1alpha1.StageFirmwareAction{}},
	}}}
	x := &exec{op: op}
	assert.False(t, x.acted(1), "nothing sent yet")
	assert.False(t, x.acted(2), "nothing sent yet")

	op.Status.UploadAccepted = &metalv1alpha1.FirmwareUploadProgress{Step: 1}
	assert.True(t, x.acted(1), "the flash image went to the BMC")
	assert.False(t, x.acted(2), "the record names another step")

	op.Status.UploadAccepted = &metalv1alpha1.FirmwareUploadProgress{Step: 2}
	assert.True(t, x.acted(2), "the staged image went to the BMC")
	assert.False(t, x.acted(1))
	assert.False(t, x.acted(0))
}
