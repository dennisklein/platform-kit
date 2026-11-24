package fleet_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

func TestStateEndpoint(t *testing.T) {
	ctx := context.Background()
	media := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(media, "boot.iso"), make([]byte, 4096), 0o600))
	cfg, err := fleet.Parse([]byte(`
media: { listen: "127.0.0.1:0", dir: ` + media + ` }
machines:
  - name: node-1
    listen: 127.0.0.1:0
    personality: huawei-2288h-v5
    credentials: { username: lab, password: lab-password-1 }
    identity: { serial: PKLAB0001, bmcMAC: "02:77:00:00:01:01" }
  - name: node-2
    listen: 127.0.0.1:0
`))
	require.NoError(t, err)
	f, err := fleet.Start(ctx, cfg, fleet.Options{StateListen: "127.0.0.1:0", RequestHistory: 3})
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close(context.Background()) })
	require.NotEmpty(t, f.StateURL())
	assert.True(t, strings.HasPrefix(f.StateURL(), "http://127.0.0.1:"), f.StateURL())

	ep := f.Endpoints()[0]
	c, err := gofishclient.Connect(ctx, gofishclient.Config{
		Endpoint: ep.URL, Username: ep.Username, Password: ep.Password, Insecure: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	// The iBMC takes media credentials as URL userinfo; the state must not
	// show them.
	image := strings.Replace(f.MediaURL(), "https://", "https://media:grant@", 1) + "/boot.iso"
	require.NoError(t, c.InsertMedia(ctx, "CD", image, nil))
	require.NoError(t, c.SetBootOverride(ctx, "1", bmc.BootOverride{Target: "Cd", Enabled: bmc.OverrideContinuous}))
	require.NoError(t, c.SetPower(ctx, "1", bmc.ActionOn))

	resp, err := http.Get(f.StateURL())
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.NotContains(t, string(raw), "grant", "no media credentials")
	assert.NotContains(t, string(raw), "lab-password-1", "no BMC credentials")

	var st fleet.State
	require.NoError(t, json.Unmarshal(raw, &st))
	assert.Equal(t, 1, st.Version)
	assert.Equal(t, f.MediaURL(), st.MediaURL)
	require.Len(t, st.Machines, 2)

	m := st.Machines[0]
	assert.Equal(t, "node-1", m.Name)
	assert.Equal(t, "huawei-2288h-v5", m.Personality)
	assert.Equal(t, ep.URL, m.BMC)
	assert.Equal(t, "PKLAB0001", m.Serial)
	assert.Equal(t, "02:77:00:00:01:01", m.BMCMAC)
	assert.Equal(t, "On", m.Power)
	assert.Equal(t, "Cd", m.BootOverride.Target)
	assert.Equal(t, bmc.OverrideContinuous, m.BootOverride.Enabled)
	require.NotEmpty(t, m.Media)
	assert.True(t, m.Media[0].Inserted)
	assert.Equal(t, f.MediaURL()+"/boot.iso", m.Media[0].ImageURL)
	assert.Equal(t, fleet.BackendInfo{Kind: fleet.BackendNone, Running: true}, m.Backend,
		"a machine without a backend runs when the sim says it is on")
	assert.Empty(t, m.ConsoleLog)

	// The history keeps the last requests only, oldest first: the power
	// action came last.
	require.Len(t, m.Requests, 3)
	last := m.Requests[2]
	assert.Equal(t, http.MethodPost, last.Method)
	assert.Equal(t, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", last.Path)
	assert.Less(t, last.Status, 300)
	assert.False(t, last.Time.IsZero())

	other := st.Machines[1]
	assert.Equal(t, "Off", other.Power)
	assert.Empty(t, other.Requests, "a request to one BMC is not in another's history")
	assert.Equal(t, "generic", other.Personality)

	// Read-only.
	resp, err = http.Post(f.StateURL(), "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}
