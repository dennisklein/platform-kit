package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// biosStub serves a fixed BIOS state and records what is staged.
type biosStub struct {
	bmc.Client
	state  bmc.BiosState
	staged []map[string]any
	err    error
}

func (b *biosStub) Bios(context.Context, string) (bmc.BiosState, error) { return b.state, nil }

func (b *biosStub) SetBiosAttributes(_ context.Context, _ string, attrs map[string]any) error {
	b.staged = append(b.staged, attrs)
	return b.err
}

func biosAction(attrs map[string]string) *metalv1alpha1.ApplyBiosAction {
	a := &metalv1alpha1.ApplyBiosAction{Attributes: map[string]apiextensionsv1.JSON{}}
	for k, v := range attrs {
		a.Attributes[k] = jsonOf(v)
	}
	return a
}

// TestApplyBiosStagesAgainWhatIsStaged: a value an earlier client staged
// without an apply time sits pending on iDRAC with no configuration job, and
// the reset applies nothing. It used to be skipped as "already staged
// right", so the job was never made. It is sent again, so the client's
// OnReset request creates the job.
func TestApplyBiosStagesAgainWhatIsStaged(t *testing.T) {
	cl := &biosStub{state: bmc.BiosState{
		Attributes: map[string]any{"SriovGlobalEnable": "Disabled", "ProcVirtualization": "Enabled"},
		Pending:    map[string]any{"SriovGlobalEnable": "Enabled"},
	}}
	require.NoError(t, applyBios(t.Context(), cl, "1", biosAction(map[string]string{
		"SriovGlobalEnable": "Enabled", "ProcVirtualization": "Enabled",
	})))
	require.Len(t, cl.staged, 1)
	assert.Equal(t, map[string]any{"SriovGlobalEnable": "Enabled"}, cl.staged[0],
		"the staged value goes again; the current one does not")
}

// TestApplyBiosTakesARefusedRestageAsStaged: where a configuration job
// already holds the values (a pass resumed after its own PATCH landed), the
// service may refuse to stage them again. Nothing new was asked for, so the
// step is done. A refusal of a new value is still a failure.
func TestApplyBiosTakesARefusedRestageAsStaged(t *testing.T) {
	refused := errors.New("bmc: bios attributes: 400 a configuration job is pending")
	cl := &biosStub{err: refused, state: bmc.BiosState{
		Attributes: map[string]any{"SriovGlobalEnable": "Disabled"},
		Pending:    map[string]any{"SriovGlobalEnable": "Enabled"},
	}}
	require.NoError(t, applyBios(t.Context(), cl, "1", biosAction(map[string]string{"SriovGlobalEnable": "Enabled"})))

	cl.staged = nil
	err := applyBios(t.Context(), cl, "1", biosAction(map[string]string{"SriovGlobalEnable": "Enabled", "BootMode": "Uefi"}))
	require.ErrorIs(t, err, refused, "a new value the BMC refused is not staged")
}

// TestApplyBiosLeavesCurrentValuesAlone: nothing to stage, nothing sent.
func TestApplyBiosLeavesCurrentValuesAlone(t *testing.T) {
	cl := &biosStub{state: bmc.BiosState{Attributes: map[string]any{"SriovGlobalEnable": "Enabled"}}}
	require.NoError(t, applyBios(t.Context(), cl, "1", biosAction(map[string]string{"SriovGlobalEnable": "Enabled"})))
	assert.Empty(t, cl.staged)
}
