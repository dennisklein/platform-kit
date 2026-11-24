package boot_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/relay"
)

var config = []byte("version: v1alpha1\nmachine:\n  type: worker\n")

func simState(t *testing.T, fixture string) *sim.State {
	t.Helper()
	f, err := sim.LoadFixture(fixture)
	require.NoError(t, err)
	return sim.NewState(f)
}

// TestLadderDirect climbs the whole ladder against the X12-shaped sim with the
// asset server on its own listener: mount with credentials, override, power,
// full pull, then eject + revoke.
func TestLadderDirect(t *testing.T) {
	st := simState(t, "supermicro-x12")
	res, err := boot.Run(context.Background(), boot.Options{
		Client:   st,
		NodeName: "demo-w1",
		Config:   config,
		TTL:      time.Minute,
		Listen:   "127.0.0.1:0",
		Logf:     t.Logf,
		// The fixture's machine is on: these tests boot it anyway.
		AllowRestart: true,
	})
	require.NoError(t, err)
	assert.True(t, res.ReadComplete, "the boot pull must complete")
	assert.GreaterOrEqual(t, res.ServedBytes, int64(res.Size))

	target, once := st.BootOverride("1")
	assert.Equal(t, "UsbCd", target, "the X12 boots UsbCd, never Cd")
	assert.True(t, once)
	state, err := st.Power(context.Background(), "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOn, state)

	inserted, err := st.Media(context.Background())
	require.NoError(t, err)
	for _, m := range inserted {
		assert.False(t, m.Inserted, "slot %s must be ejected after the ladder", m.ID)
	}
}

// TestLadderOverRelay is the media-fetch-traverses-relay gate: the asset server
// has NO listener of its own — it serves purely over the tunnel, so the sim's
// pull succeeding proves the traversal.
func TestLadderOverRelay(t *testing.T) {
	relaySecret := []byte("0123456789abcdef0123456789abcdef")
	tunnel, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = relay.Serve(ctx, tunnel, front, "", relaySecret) }()

	rc, err := relay.Dial(ctx, tunnel.Addr().String(), relaySecret)
	require.NoError(t, err)

	st := simState(t, "supermicro-x12")
	res, err := boot.Run(ctx, boot.Options{
		Client:   st,
		NodeName: "demo-w2",
		Config:   config,
		TTL:      time.Minute,
		Relay:    rc,
		Logf:     t.Logf,
		// The fixture's machine is on: this test boots it anyway.
		AllowRestart: true,
	})
	require.NoError(t, err)
	assert.True(t, res.ReadComplete, "the pull must complete through the relay")

	u, err := url.Parse(res.ImageURL)
	require.NoError(t, err)
	assert.Equal(t, front.Addr().String(), u.Host, "the grant URL names the relay front")
}

// TestLadderVmmDialect runs the Huawei shape: single CD slot, credentials as
// URL userinfo, no UsbCd target.
func TestLadderVmmDialect(t *testing.T) {
	st := simState(t, "huawei-2288h-v5")
	res, err := boot.Run(context.Background(), boot.Options{
		Client:   st,
		NodeName: "demo-cp1",
		Config:   config,
		TTL:      time.Minute,
		Listen:   "127.0.0.1:0",
		Logf:     t.Logf,
		// The fixture's machine is on: these tests boot it anyway.
		AllowRestart: true,
	})
	require.NoError(t, err)
	assert.True(t, res.ReadComplete)

	target, _ := st.BootOverride("1")
	assert.Equal(t, "Cd", target, "no UsbCd on the iBMC — Cd is the fallback")
}

// TestLadderHold keeps the mount and grant alive until cancellation, so the
// booted image stays inspectable: it must be byte-identical to the packed ISO.
func TestLadderHold(t *testing.T) {
	st := simState(t, "supermicro-x12")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := boot.Run(ctx, boot.Options{
			Client:   st,
			NodeName: "demo-w3",
			Config:   config,
			TTL:      time.Minute,
			Listen:   "127.0.0.1:0",
			Hold:     true,
			Logf:     t.Logf,
			// The fixture's machine is on: hold boots it anyway.
			AllowRestart: true,
		})
		assert.NoError(t, err)
	}()

	want, err := asset.BuildConfigISO(config)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(st.BootImage("VirtualMedia1")) > 0
	}, 10*time.Second, 50*time.Millisecond, "the boot pull lands while holding")
	assert.Equal(t, want, st.BootImage("VirtualMedia1"), "the sim booted the exact packed ISO")

	media, err := st.Media(context.Background())
	require.NoError(t, err)
	var mounted bool
	for _, m := range media {
		mounted = mounted || m.Inserted
	}
	assert.True(t, mounted, "hold keeps the medium mounted")

	cancel()
	<-done
	media, err = st.Media(context.Background())
	require.NoError(t, err)
	for _, m := range media {
		assert.False(t, m.Inserted, "cancellation ejects the medium")
	}
}

// TestLadderRefusals covers the rungs that must fail fast.
func TestLadderRefusals(t *testing.T) {
	st := simState(t, "supermicro-x12")
	_, err := boot.Run(context.Background(), boot.Options{
		Client: st, Config: config, TTL: time.Minute, Listen: "127.0.0.1:0",
	})
	assert.ErrorContains(t, err, "node name")

	_, err = boot.Run(context.Background(), boot.Options{
		Client: simState(t, "huawei-2288h-v5"), NodeName: "w", Config: config,
		TTL: time.Minute, Listen: "127.0.0.1:0",
		MediaID: "USBStick", // exists, but advertises no insert action
	})
	assert.ErrorContains(t, err, "USBStick", "a slot without an insert mechanism is refused")

	_, err = boot.Run(context.Background(), boot.Options{
		Client: st, NodeName: "w", Config: nil, TTL: time.Minute, Listen: "127.0.0.1:0", AllowRestart: true,
	})
	assert.Error(t, err, "an empty config must not pack")
}

// TestLadderRefusesRunningMachine: a machine that is on is not reset without
// consent, and the refusal comes before anything on the BMC changes.
func TestLadderRefusesRunningMachine(t *testing.T) {
	st := simState(t, "supermicro-x12")
	targetBefore, _ := st.BootOverride("1")
	var lines []string
	_, err := boot.Run(context.Background(), boot.Options{
		Client: st, NodeName: "w", Config: config, TTL: time.Minute, Listen: "127.0.0.1:0",
		Logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	})
	require.ErrorIs(t, err, boot.ErrMachineOn)

	plan := strings.Join(lines, "\n")
	assert.Contains(t, plan, "HARD-RESET", "the plan says the machine would be reset")
	assert.Contains(t, plan, "boot from UsbCd")

	state, err := st.Power(context.Background(), "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOn, state, "power untouched")
	target, _ := st.BootOverride("1")
	assert.Equal(t, targetBefore, target, "boot override untouched")
	media, err := st.Media(context.Background())
	require.NoError(t, err)
	for _, m := range media {
		assert.False(t, m.Inserted, "nothing mounted on %s", m.ID)
	}
}

// TestLadderNoPower mounts and sets the override but leaves power to the
// person: the boot happens when they restart the machine themselves.
func TestLadderNoPower(t *testing.T) {
	st := simState(t, "supermicro-x12")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The person: restart the machine once the medium is in.
	go func() {
		for ctx.Err() == nil {
			media, err := st.Media(ctx)
			if err == nil && slices.ContainsFunc(media, func(m bmc.VirtualMedia) bool { return m.Inserted }) {
				target, _ := st.BootOverride("1")
				if target == "UsbCd" {
					_ = st.SetPower(ctx, "1", bmc.ActionForceRestart)
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	res, err := boot.Run(ctx, boot.Options{
		Client: st, NodeName: "w", Config: config, TTL: 20 * time.Second, Listen: "127.0.0.1:0",
		NoPower: true, Logf: t.Logf,
	})
	require.NoError(t, err, "a running machine needs no consent when pk-metal does not touch power")
	assert.True(t, res.ReadComplete, "the person's restart boots the medium")
}

// stuckMedia is a manager whose eject never takes: the slot reads inserted
// for ever, and the media read ignores ctx the way a Redfish GET does.
type stuckMedia struct{ bmc.Client }

func (stuckMedia) Media(context.Context) ([]bmc.VirtualMedia, error) {
	return []bmc.VirtualMedia{{ID: "CD", Inserted: true}}, nil
}

func (stuckMedia) EjectMedia(context.Context, string) error { return nil }

// TestEjectAndVerifyHonoursCancel: a cancelled reconcile must not sit out the
// ten-second eject window.
func TestEjectAndVerifyHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err := boot.EjectAndVerify(ctx, stuckMedia{}, "CD", nil)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 2*time.Second, "cancellation must end the eject wait")
}

// The log shows the start of the grant token only: the token in the image
// URL is half of what lets a reader fetch the machine's secrets.
func TestLogDoesNotShowTheGrantToken(t *testing.T) {
	st := simState(t, "supermicro-x12")
	var lines []string
	res, err := boot.Run(context.Background(), boot.Options{
		Client: st, NodeName: "w", Config: config, TTL: time.Minute, Listen: "127.0.0.1:0",
		AllowRestart: true,
		Logf:         func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	})
	require.NoError(t, err)
	u, err := url.Parse(res.ImageURL)
	require.NoError(t, err)
	token := strings.Split(strings.TrimPrefix(u.Path, "/grants/"), "/")[0]
	require.NotEmpty(t, token)
	log := strings.Join(lines, "\n")
	assert.NotContains(t, log, token)
	assert.Contains(t, log, "/grants/"+token[:6]+"…/w.iso")
}
