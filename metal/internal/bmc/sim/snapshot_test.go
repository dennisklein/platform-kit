package sim_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0xCD}, 2048)
	media := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "node.iso", time.Time{}, bytes.NewReader(payload))
	}))
	defer media.Close()
	u, err := url.Parse(media.URL)
	require.NoError(t, err)

	f, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	st := sim.NewState(f)
	require.NoError(t, st.InsertMedia(ctx, "CD", "https://media:grant@"+u.Host+"/node.iso", nil))
	require.NoError(t, st.SetBootOverride(ctx, "1", bmc.BootOverride{Target: "Cd", Enabled: bmc.OverrideOnce}))
	require.NoError(t, st.SetPower(ctx, "1", bmc.ActionOn))
	// An unreachable BMC still has a world; the snapshot is not a Redfish read.
	st.SetFault(sim.FaultUnreachable, true)

	snap := st.Snapshot(ctx)
	require.Len(t, snap.Systems, 1)
	assert.Equal(t, "1", snap.Systems[0].ID)
	assert.Equal(t, string(bmc.PowerOn), snap.Systems[0].Power)
	assert.Equal(t, "Cd", snap.Systems[0].BootOverride.Target)
	assert.Equal(t, bmc.OverrideDisabled, snap.Systems[0].BootOverride.Enabled, "the boot used the one-time override up")

	require.Len(t, snap.Media, 2)
	assert.Equal(t, sim.MediaSnapshot{ID: "CD", Inserted: true, ImageURL: "https://" + u.Host + "/node.iso"}, snap.Media[0],
		"the grant in the URL is not shown")
	assert.Equal(t, sim.MediaSnapshot{ID: "USBStick"}, snap.Media[1])
}

func TestWithoutUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:secret@10.0.0.1:8443/a.iso?x=1": "https://10.0.0.1:8443/a.iso?x=1",
		"https://10.0.0.1/a.iso":                      "https://10.0.0.1/a.iso",
		"https://user@host/a.iso":                     "https://host/a.iso",
		"https://user:se cret@host/%zz":               "(not a URL)",
	} {
		assert.Equal(t, want, sim.WithoutUserinfo(in), in)
	}
}
