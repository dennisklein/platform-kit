package boot_test

import (
	"context"
	"net"
	"net/url"
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
	tunnel, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = relay.Serve(ctx, tunnel, front, "") }()

	rc, err := relay.Dial(ctx, tunnel.Addr().String())
	require.NoError(t, err)

	st := simState(t, "supermicro-x12")
	res, err := boot.Run(ctx, boot.Options{
		Client:   st,
		NodeName: "demo-w2",
		Config:   config,
		TTL:      time.Minute,
		Relay:    rc,
		Logf:     t.Logf,
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
		Client: st, NodeName: "w", Config: nil, TTL: time.Minute, Listen: "127.0.0.1:0",
	})
	assert.Error(t, err, "an empty config must not pack")
}
