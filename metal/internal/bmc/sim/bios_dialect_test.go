package sim_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// TestBiosApplyTimeDialect pins the iDRAC 9 staging shape the sim plays with
// apply_time_required: a PATCH without @Redfish.SettingsApplyTime stages but
// is never applied, one asking for OnReset is applied at the next reset, and
// an apply time the service does not list is refused.
func TestBiosApplyTimeDialect(t *testing.T) {
	ctx := t.Context()
	setup := func(t *testing.T) (*sim.State, *httptest.Server) {
		f, err := sim.LoadFixture("supermicro-x12") // open, no If-Match: raw PATCHes are enough
		require.NoError(t, err)
		f.Bios.ApplyTimeRequired = true
		f.Bios.SupportedApplyTimes = []string{"OnReset", "AtMaintenanceWindowStart", "InMaintenanceWindowOnReset"}
		st := sim.NewState(f)
		srv := httptest.NewServer(sim.NewServer(st))
		t.Cleanup(srv.Close)
		return st, srv
	}
	patch := func(t *testing.T, srv *httptest.Server, body map[string]any) int {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, srv.URL+"/redfish/v1/Systems/1/Bios/SD", bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	attrs := map[string]any{"QuietBoot": false}

	t.Run("without an apply time nothing applies", func(t *testing.T) {
		st, srv := setup(t)
		require.Less(t, patch(t, srv, map[string]any{"Attributes": attrs}), 300)
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		state, err := st.Bios(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, true, state.Attributes["QuietBoot"], "no job, no change")
		assert.Equal(t, false, state.Pending["QuietBoot"], "still pending after the reset")

		// A later PATCH that asks for OnReset schedules the whole pending set.
		require.Less(t, patch(t, srv, map[string]any{"Attributes": map[string]any{"CSMSupport": "Disabled"},
			"@Redfish.SettingsApplyTime": map[string]any{"ApplyTime": "OnReset"}}), 300)
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		state, err = st.Bios(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, false, state.Attributes["QuietBoot"])
		assert.Empty(t, state.Pending)
	})

	t.Run("OnReset applies at the next reset", func(t *testing.T) {
		st, srv := setup(t)
		require.Less(t, patch(t, srv, map[string]any{"Attributes": attrs,
			"@Redfish.SettingsApplyTime": map[string]any{"ApplyTime": "OnReset"}}), 300)
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		state, err := st.Bios(ctx, "1")
		require.NoError(t, err)
		assert.Equal(t, false, state.Attributes["QuietBoot"])
	})

	t.Run("an apply time not offered is refused", func(t *testing.T) {
		_, srv := setup(t)
		assert.Equal(t, http.StatusBadRequest, patch(t, srv, map[string]any{"Attributes": attrs,
			"@Redfish.SettingsApplyTime": map[string]any{"ApplyTime": "Immediate"}}))
	})
}
