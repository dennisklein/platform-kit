package sim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// upload POSTs a multipart firmware push the way curl -F does, with the given
// UpdateParameters JSON. Returns the response (body closed).
func upload(t *testing.T, srv *httptest.Server, params string, image []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ph := textproto.MIMEHeader{}
	ph.Set("Content-Disposition", `form-data; name="UpdateParameters"`)
	ph.Set("Content-Type", "application/json")
	pw, err := mw.CreatePart(ph)
	require.NoError(t, err)
	_, err = pw.Write([]byte(params))
	require.NoError(t, err)
	fw, err := mw.CreateFormFile("UpdateFile", "firmware.bin")
	require.NoError(t, err)
	_, err = fw.Write(image)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	resp, err := srv.Client().Post(srv.URL+"/redfish/v1/UpdateService/upload", mw.FormDataContentType(), &buf)
	require.NoError(t, err)
	resp.Body.Close()
	return resp
}

func getJSON(t *testing.T, srv *httptest.Server, path string) map[string]any {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
	var v map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body string) (*http.Response, string) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	var sb strings.Builder
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) == nil {
		sb.WriteString(doc.Error.Message)
	}
	return resp, sb.String()
}

const (
	preserveParams = `{"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"Immediate","Oem":{"Supermicro":{"BMC":{"PreserveCfg":true,"PreserveSdr":true,"PreserveSsl":true,"BackupBMC":true}}}}`
	noPreserve     = `{"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"Immediate"}`
	stagedParams   = `{"Targets":["/redfish/v1/Managers/1"],"@Redfish.OperationApplyTime":"OnStartUpdateRequest","Oem":{"Supermicro":{"BMC":{"PreserveCfg":true}}}}`
)

// TestWedgeLadder walks the recorded X12 failure ladder: a staged image wedges
// the BMC in update mode; only a forced manager reset cures it. Both roads into
// the wedge are covered — apply-time OnStartUpdateRequest and a missing
// preserve block.
func TestWedgeLadder(t *testing.T) {
	for name, params := range map[string]string{"staged-apply-time": stagedParams, "missing-preserve": noPreserve} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture("supermicro-x12")
			require.NoError(t, err)
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			image := []byte("SIMFW 88.00.00")
			baseReset := getJSON(t, srv, "/redfish/v1/Managers/1")["LastResetTime"]

			resp := upload(t, srv, params, image)
			require.Equal(t, http.StatusAccepted, resp.StatusCode, "the wrong invocation is still ACCEPTED — that is the trap")

			m := getJSON(t, srv, "/redfish/v1/Managers/1")
			assert.Equal(t, "01.07.09", m["FirmwareVersion"], "nothing flashed")
			assert.Equal(t, baseReset, m["LastResetTime"], "no BMC reboot")
			staging := getJSON(t, srv, "/redfish/v1/UpdateService/FirmwareInventory/Staging_BMC")
			assert.Equal(t, "88.00.00", staging["Version"], "the image landed in the staging slot")

			// Wedged: a correct upload is now refused.
			resp = upload(t, srv, preserveParams, image)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

			// StartUpdate cannot commit it — the recorded X12 truth.
			resp, msg := postJSON(t, srv, "/redfish/v1/UpdateService/Actions/UpdateService.StartUpdate", "{}")
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, msg, "malformed JSON")

			// The OEM install answers success — and changes nothing.
			resp, _ = postJSON(t, srv, "/redfish/v1/UpdateService/Actions/Oem/SmcUpdateService.Install",
				`{"Targets":["/redfish/v1/Managers/1"],"InstallOptions":[]}`)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "01.07.09", getJSON(t, srv, "/redfish/v1/Managers/1")["FirmwareVersion"],
				"Install answered success without observable effect")

			// GracefulRestart runs the attestation path and is refused mid-update.
			resp, msg = postJSON(t, srv, "/redfish/v1/Managers/1/Actions/Manager.Reset", `{"ResetType":"GracefulRestart"}`)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, msg, "FW update in progress")

			// ForceRestart discards the staging slot — the cure.
			resp, _ = postJSON(t, srv, "/redfish/v1/Managers/1/Actions/Manager.Reset", `{"ResetType":"ForceRestart"}`)
			require.Equal(t, http.StatusNoContent, resp.StatusCode)
			staging = getJSON(t, srv, "/redfish/v1/UpdateService/FirmwareInventory/Staging_BMC")
			assert.Equal(t, "01.07.09", staging["Version"], "staging discarded, mirrors current again")

			// Unwedged: the correct recipe now flashes.
			resp = upload(t, srv, preserveParams, image)
			require.Equal(t, http.StatusAccepted, resp.StatusCode)
			assert.Equal(t, "88.00.00", getJSON(t, srv, "/redfish/v1/Managers/1")["FirmwareVersion"])
		})
	}
}

// TestFlashLyingOracle: the injected fault where a correct upload answers
// success and nothing ever happens — what the LastResetTime verify exists for.
func TestFlashLyingOracle(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	st := sim.NewState(f)
	ctx := context.Background()

	st.SetFault(sim.FaultFlashFails, true)
	require.NoError(t, st.UpdateFirmware(ctx, []byte("SIMFW 77.00.00")), "the oracle answers success")
	m, err := st.Manager(ctx)
	require.NoError(t, err)
	assert.Equal(t, "01.07.09", m.FirmwareVersion, "version never advanced")
	assert.Equal(t, f.Manager.LastResetTime.UTC(), m.LastResetTime.UTC(), "no BMC reboot")

	st.SetFault(sim.FaultFlashFails, false)
	require.NoError(t, st.UpdateFirmware(ctx, []byte("SIMFW 77.00.00")))
	m, err = st.Manager(ctx)
	require.NoError(t, err)
	assert.Equal(t, "77.00.00", m.FirmwareVersion, "without the fault the flash is real")
	assert.True(t, m.LastResetTime.After(f.Manager.LastResetTime), "the flash rebooted the BMC")
}

// TestBiosStuck: pending settings that survive every reboot — the alert-and-hold
// input for a BIOS that never converges.
func TestBiosStuck(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	st := sim.NewState(f)
	ctx := context.Background()

	require.NoError(t, st.SetBiosAttributes(ctx, "1", map[string]any{"CSMSupport": "Enabled"}))
	st.SetFault(sim.FaultBiosStuck, true)
	require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
	state, err := st.Bios(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, "Enabled", state.Pending["CSMSupport"], "pending survived the reboot")
	assert.Equal(t, "Disabled", state.Attributes["CSMSupport"], "nothing applied")

	st.SetFault(sim.FaultBiosStuck, false)
	require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
	state, err = st.Bios(ctx, "1")
	require.NoError(t, err)
	assert.Empty(t, state.Pending)
	assert.Equal(t, "Enabled", state.Attributes["CSMSupport"], "applied once the fault clears")
}

// TestBiosApplyDropsDisallowed: a staged value outside the registry's allowed
// set is dropped at apply time with a message in @Redfish.Settings — staging
// never proves convergence.
func TestBiosApplyDropsDisallowed(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()

	patch, err := http.NewRequest(http.MethodPatch, srv.URL+"/redfish/v1/Systems/1/Bios/SD",
		strings.NewReader(`{"Attributes":{"CSMSupport":"Bogus","QuietBoot":false}}`))
	require.NoError(t, err)
	patch.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(patch)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "a bad VALUE stages fine — validation waits for apply")

	resp, _ = postJSON(t, srv, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", `{"ResetType":"ForceRestart"}`)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	bios := getJSON(t, srv, "/redfish/v1/Systems/1/Bios")
	attrs := bios["Attributes"].(map[string]any)
	assert.Equal(t, "Disabled", attrs["CSMSupport"], "the disallowed value was dropped")
	assert.Equal(t, false, attrs["QuietBoot"], "the valid one applied")
	settings := bios["@Redfish.Settings"].(map[string]any)
	msgs, ok := settings["Messages"].([]any)
	require.True(t, ok, "@Redfish.Settings must carry the apply-time messages")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].(map[string]any)["Message"], "not in the list of acceptable values")

	sd := getJSON(t, srv, "/redfish/v1/Systems/1/Bios/SD")
	_, has := sd["Attributes"]
	assert.False(t, has, "pending cleared — the capture shape omits the key entirely")
}
