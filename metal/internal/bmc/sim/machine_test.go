package sim

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// fakeMachine records power verbs and carries a scripted running state.
type fakeMachine struct {
	verbs   []string
	running bool
	fail    error
}

func (m *fakeMachine) Start(context.Context) error {
	m.verbs = append(m.verbs, "start")
	if m.fail == nil {
		m.running = true
	}
	return m.fail
}

func (m *fakeMachine) Stop(context.Context) error {
	m.verbs = append(m.verbs, "stop")
	if m.fail == nil {
		m.running = false
	}
	return m.fail
}

func (m *fakeMachine) Restart(context.Context) error {
	m.verbs = append(m.verbs, "restart")
	return m.fail
}

func (m *fakeMachine) Running(context.Context) (bool, error) { return m.running, nil }

func TestMachineDrivesAndReadsThrough(t *testing.T) {
	ctx := context.Background()
	f, err := LoadFixture("supermicro-x12")
	require.NoError(t, err)
	s := NewState(f)
	sysID := f.Systems[0].ID

	m := &fakeMachine{}
	s.AttachMachine(sysID, m)

	// The machine is off — the BMC's view reads through, whatever the fixture said.
	p, err := s.Power(ctx, sysID)
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, p)

	// Power on drives the machine; the view follows it.
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionOn))
	assert.Equal(t, []string{"start"}, m.verbs)
	p, err = s.Power(ctx, sysID)
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOn, p)

	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionForceRestart))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionGracefulShutdown))
	assert.Equal(t, []string{"start", "restart", "stop"}, m.verbs)
	p, err = s.Power(ctx, sysID)
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, p)

	// A failing machine op fails the action and leaves the view untouched.
	m.fail = errors.New("no such container")
	err = s.SetPower(ctx, sysID, bmc.ActionOn)
	require.Error(t, err)
	p, err = s.Power(ctx, sysID)
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, p, "the sim view must not claim a machine the driver failed to start")
}

// fakeMediaMachine additionally takes media — the qemu-backend shape. SetMedia
// records into verbs so the ordering against power verbs is provable.
type fakeMediaMachine struct {
	fakeMachine
	media []Media
}

func (m *fakeMediaMachine) SetMedia(_ context.Context, media []Media) error {
	m.verbs = append(m.verbs, "media")
	m.media = media
	return nil
}

func TestMediaMachineGetsImagesBeforePower(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0xCD}, 2048)
	asset := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "node.iso", time.Time{}, bytes.NewReader(payload))
	}))
	defer asset.Close()

	f, err := LoadFixture("supermicro-x12")
	require.NoError(t, err)
	s := NewState(f)
	sysID := f.Systems[0].ID
	m := &fakeMediaMachine{}
	s.AttachMachine(sysID, m)

	require.NoError(t, s.InsertMedia(ctx, "VirtualMedia1", asset.URL+"/node.iso", nil))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionOn))
	assert.Equal(t, []string{"media", "start"}, m.verbs,
		"the media device is attached before the host boots")
	require.Len(t, m.media, 1)
	assert.Equal(t, Media{SlotID: "VirtualMedia1", Data: payload}, m.media[0], "the machine gets the pulled image")

	// Eject withdraws: the next boot-shaped action presents an empty set.
	require.NoError(t, s.EjectMedia(ctx, "VirtualMedia1"))
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionForceRestart))
	assert.Equal(t, []string{"media", "start", "media", "restart"}, m.verbs)
	assert.Empty(t, m.media, "ejected media never reaches the next boot")

	// A plain power-off presents nothing — media is a boot-time concern.
	require.NoError(t, s.SetPower(ctx, sysID, bmc.ActionForceOff))
	assert.Equal(t, []string{"media", "start", "media", "restart", "stop"}, m.verbs)
}

func TestMachineReadThroughInSystems(t *testing.T) {
	ctx := context.Background()
	f, err := LoadFixture("supermicro-x12")
	require.NoError(t, err)
	s := NewState(f)
	sysID := f.Systems[0].ID
	s.AttachMachine(sysID, &fakeMachine{running: true})

	systems, err := s.Systems(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, systems)
	assert.Equal(t, bmc.PowerOn, systems[0].PowerState, "Systems reads the machine, not the fixture")
}
