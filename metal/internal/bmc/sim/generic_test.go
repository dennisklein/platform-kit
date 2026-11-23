package sim_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// The generic personality at the wire: the parts of the specification the
// vendor fixtures do not exercise and no bmc.Client call observes directly —
// task states, fixed account slots, the action parameters of InsertMedia.

// wire is a Redfish endpoint as curl sees it.
type wire struct {
	t          *testing.T
	base       string
	user, pass string
}

func genericWire(t *testing.T, tune func(*sim.Fixture)) (*wire, *sim.State) {
	t.Helper()
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	if tune != nil {
		tune(f)
	}
	st := sim.NewState(f)
	srv := httptest.NewServer(sim.NewServer(st))
	t.Cleanup(srv.Close)
	return &wire{t: t, base: srv.URL, user: f.Accounts[0].Username, pass: f.Accounts[0].Password}, st
}

func (w *wire) send(req *http.Request) (*http.Response, map[string]any) {
	w.t.Helper()
	if _, _, ok := req.BasicAuth(); !ok {
		req.SetBasicAuth(w.user, w.pass)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(w.t, err)
	var doc map[string]any
	if len(data) > 0 {
		require.NoError(w.t, json.Unmarshal(data, &doc), "%s %s", req.Method, req.URL.Path)
	}
	return resp, doc
}

func (w *wire) get(uri string) (*http.Response, map[string]any) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.base+uri, nil)
	require.NoError(w.t, err)
	return w.send(req)
}

func (w *wire) post(uri string, body any) (*http.Response, map[string]any) {
	w.t.Helper()
	data, err := json.Marshal(body)
	require.NoError(w.t, err)
	req, err := http.NewRequest(http.MethodPost, w.base+uri, bytes.NewReader(data))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	return w.send(req)
}

// patch sends the resource's current ETag as If-Match, as the fixture demands.
func (w *wire) patch(uri string, body any) (*http.Response, map[string]any) {
	w.t.Helper()
	current, _ := w.get(uri)
	data, err := json.Marshal(body)
	require.NoError(w.t, err)
	req, err := http.NewRequest(http.MethodPatch, w.base+uri, bytes.NewReader(data))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", current.Header.Get("ETag"))
	return w.send(req)
}

// upload is the multipart push as curl -F frames it: both parts as plain form
// fields, UpdateParameters first.
func (w *wire) upload(uri string, params any, image []byte) (*http.Response, map[string]any) {
	w.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if params != nil {
		data, err := json.Marshal(params)
		require.NoError(w.t, err)
		require.NoError(w.t, mw.WriteField("UpdateParameters", string(data)))
	}
	part, err := mw.CreateFormFile("UpdateFile", "firmware.bin")
	require.NoError(w.t, err)
	_, err = part.Write(image)
	require.NoError(w.t, err)
	require.NoError(w.t, mw.Close())
	req, err := http.NewRequest(http.MethodPost, w.base+uri, &buf)
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return w.send(req)
}

func messageID(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	info, _ := e["@Message.ExtendedInfo"].([]any)
	if len(info) == 0 {
		return ""
	}
	first, _ := info[0].(map[string]any)
	id, _ := first["MessageId"].(string)
	return id
}

func TestGenericFirmwareTask(t *testing.T) {
	w, _ := genericWire(t, func(f *sim.Fixture) { f.Protocol.TaskDuration = 400 * time.Millisecond })

	_, us := w.get("/redfish/v1/UpdateService")
	push, _ := us["MultipartHttpPushUri"].(string)
	require.NotEmpty(t, push, "the update service advertises the push URI")

	_, mgr := w.get("/redfish/v1/Managers/1")
	links, _ := mgr["Links"].(map[string]any)
	active, _ := links["ActiveSoftwareImage"].(map[string]any)
	target, _ := active["@odata.id"].(string)
	require.NotEmpty(t, target, "the manager names its firmware image")
	before := mgr["FirmwareVersion"]

	resp, body := w.upload(push, map[string]any{"Targets": []string{"/redfish/v1/Chassis/1"}},
		[]byte("image SIMFW 3.0.0 end"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a target that is no firmware image")
	assert.Contains(t, messageID(body), "PropertyValueNotInList")

	resp, body = w.upload(push, map[string]any{"Targets": []string{target}, "@Redfish.OperationApplyTime": "Immediate"},
		[]byte("image SIMFW 3.0.0 end"))
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "upload: %v", body)
	monitor := resp.Header.Get("Location")
	require.Regexp(t, `^/redfish/v1/TaskService/Tasks/\d+$`, monitor)
	assert.Equal(t, "New", body["TaskState"], "the task starts out New")

	// The flash is the task's work: the version moves when the task
	// completes, not when the upload returns.
	_, mgr = w.get("/redfish/v1/Managers/1")
	assert.Equal(t, before, mgr["FirmwareVersion"], "nothing flashed while the task runs")

	states := map[string]bool{}
	require.Eventually(t, func() bool {
		_, task := w.get(monitor)
		state, _ := task["TaskState"].(string)
		states[state] = true
		return state == "Completed"
	}, 5*time.Second, 20*time.Millisecond)
	assert.True(t, states["Running"], "the task was seen Running on its way: %v", states)

	_, mgr = w.get("/redfish/v1/Managers/1")
	assert.Equal(t, "3.0.0", mgr["FirmwareVersion"], "the version reads back once the task completed")
	_, tasks := w.get("/redfish/v1/TaskService/Tasks")
	assert.EqualValues(t, 1, tasks["Members@odata.count"], "the task stays listed")
}

func TestGenericStagedUpdate(t *testing.T) {
	w, _ := genericWire(t, func(f *sim.Fixture) { f.Protocol.TaskDuration = 0 })
	const start = "/redfish/v1/UpdateService/Actions/UpdateService.StartUpdate"

	resp, _ := w.post(start, map[string]any{})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "nothing staged, nothing to start")

	// OnStartUpdateRequest stages; a non-manager component just reads back.
	resp, body := w.upload("/redfish/v1/UpdateService/update-multipart", map[string]any{
		"Targets":                     []string{"/redfish/v1/UpdateService/FirmwareInventory/BIOS"},
		"@Redfish.OperationApplyTime": "OnStartUpdateRequest",
	}, []byte("image SIMFW 2.0.0 end"))
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "upload: %v", body)
	_, bios := w.get("/redfish/v1/UpdateService/FirmwareInventory/BIOS")
	assert.Equal(t, "1.8.2", bios["Version"], "staged is not installed")

	resp, body = w.post(start, map[string]any{})
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "StartUpdate: %v", body)
	_, bios = w.get("/redfish/v1/UpdateService/FirmwareInventory/BIOS")
	assert.Equal(t, "2.0.0", bios["Version"], "StartUpdate installs the staged image")
	_, mgr := w.get("/redfish/v1/Managers/1")
	assert.Equal(t, "2.14.0", mgr["FirmwareVersion"], "the manager firmware was not the target")
}

func TestGenericAccountSlots(t *testing.T) {
	w, _ := genericWire(t, nil)
	const accounts = "/redfish/v1/AccountService/Accounts"

	_, coll := w.get(accounts)
	assert.EqualValues(t, 8, coll["Members@odata.count"], "a fixed set of slots")
	_, vacant := w.get(accounts + "/2")
	assert.Equal(t, "", vacant["UserName"], "an unused slot has no user name")
	assert.Equal(t, false, vacant["Enabled"])

	resp, body := w.post(accounts, map[string]any{"UserName": "ops", "Password": "OpsPass0001", "RoleId": "Operator"})
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "a slot collection takes no POST: %v", body)
	assert.Contains(t, resp.Header.Get("Allow"), http.MethodGet)

	resp, body = w.patch(accounts+"/2", map[string]any{
		"UserName": "ops", "Password": "OpsPass0001", "RoleId": "Operator", "Enabled": true,
	})
	require.Less(t, resp.StatusCode, 300, "claiming a slot: %v", body)
	_, taken := w.get(accounts + "/2")
	assert.Equal(t, "ops", taken["UserName"])
	assert.Nil(t, taken["Password"], "a password is never served")

	ops := &wire{t: t, base: w.base, user: "ops", pass: "OpsPass0001"}
	resp, _ = ops.get("/redfish/v1/Systems")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the new account authenticates")

	resp, body = w.patch(accounts+"/3", map[string]any{"UserName": "ops", "Password": "Another0001", "RoleId": "Operator"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a user name is unique: %v", body)
	resp, body = w.patch(accounts+"/3", map[string]any{"UserName": "aud", "Password": "AudPass0001", "RoleId": "NoSuchRole"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a role has to exist: %v", body)

	_, role := w.get("/redfish/v1/AccountService/Roles/Operator")
	assert.Contains(t, role["AssignedPrivileges"], "Login")
}

func TestGenericVirtualMedia(t *testing.T) {
	w, st := genericWire(t, nil)
	asset := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.ServeContent(rw, r, "boot.iso", time.Time{}, bytes.NewReader(make([]byte, 4096)))
	}))
	defer asset.Close()
	img := asset.URL + "/boot.iso"
	const media = "/redfish/v1/Systems/1/VirtualMedia"

	resp, body := w.get("/redfish/v1/Managers/1/VirtualMedia")
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "the collection lives under the system only")
	assert.Contains(t, messageID(body), "ResourceMissingAtURI")
	_, mgr := w.get("/redfish/v1/Managers/1")
	assert.NotContains(t, mgr, "VirtualMedia")
	assert.NotContains(t, mgr, "Oem", "a standard tree carries no OEM block")

	_, coll := w.get(media)
	members, _ := coll["Members"].([]any)
	require.Len(t, members, 2)
	_, first := w.get(media + "/1")
	assert.Equal(t, []any{"USBStick"}, first["MediaTypes"], "the first slot is not the optical one")
	_, second := w.get(media + "/2")
	assert.Equal(t, []any{"CD", "DVD"}, second["MediaTypes"])

	insert := media + "/2/Actions/VirtualMedia.InsertMedia"
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"no image":          {map[string]any{"Inserted": true}, "ActionParameterMissing"},
		"wrong protocol":    {map[string]any{"Image": img, "TransferProtocolType": "NFS"}, "ActionParameterValueNotInList"},
		"upload method":     {map[string]any{"Image": img, "TransferMethod": "Upload"}, "ActionParameterValueNotInList"},
		"not inserted":      {map[string]any{"Image": img, "Inserted": false}, "ActionParameterValueNotInList"},
		"userinfo in image": {map[string]any{"Image": "https://u:p@" + img[len("https://"):]}, "ActionParameterValueFormatError"},
	} {
		resp, body := w.post(insert, tc.body)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
		assert.Contains(t, messageID(body), tc.want, name)
	}

	resp, body = w.post(insert, map[string]any{
		"Image": img, "Inserted": true, "WriteProtected": true,
		"TransferProtocolType": "HTTPS", "TransferMethod": "Stream",
	})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "InsertMedia: %v", body)
	_, second = w.get(media + "/2")
	assert.Equal(t, true, second["Inserted"])
	assert.Equal(t, img, second["Image"])
	assert.Equal(t, "boot.iso", second["ImageName"])
	assert.Equal(t, "HTTPS", second["TransferProtocolType"])

	// The whole walk a boot from virtual media takes: override, power on,
	// and the one-time override is spent by that boot.
	resp, body = w.patch("/redfish/v1/Systems/1", map[string]any{"Boot": map[string]any{
		"BootSourceOverrideTarget": "Cd", "BootSourceOverrideEnabled": "Once", "BootSourceOverrideMode": "UEFI",
	}})
	require.Less(t, resp.StatusCode, 300, "boot override: %v", body)
	_, sys := w.get("/redfish/v1/Systems/1")
	assert.Equal(t, "Off", sys["PowerState"])
	progress, _ := sys["BootProgress"].(map[string]any)
	assert.Equal(t, "None", progress["LastState"], "a system that is off has not booted")

	resp, body = w.post("/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", map[string]any{"ResetType": "On"})
	require.Less(t, resp.StatusCode, 300, "reset: %v", body)
	assert.NotNil(t, st.BootImage("2"), "the boot read the medium")
	_, sys = w.get("/redfish/v1/Systems/1")
	assert.Equal(t, "On", sys["PowerState"])
	progress, _ = sys["BootProgress"].(map[string]any)
	assert.Equal(t, "OSRunning", progress["LastState"])
	boot, _ := sys["Boot"].(map[string]any)
	assert.Equal(t, "Disabled", boot["BootSourceOverrideEnabled"], "the one-time override is spent")
	assert.Equal(t, "Cd", boot["BootSourceOverrideTarget"], "the target stays")
	assert.Equal(t, "UEFI", boot["BootSourceOverrideMode"], "the mode stays")

	resp, body = w.post("/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", map[string]any{"ResetType": "Nmi"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, messageID(body), "ActionParameterValueNotInList")

	resp, _ = w.post(media+"/2/Actions/VirtualMedia.EjectMedia", map[string]any{})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	_, second = w.get(media + "/2")
	assert.Equal(t, false, second["Inserted"])
	assert.Nil(t, second["Image"])
}

func TestGenericInventoryShape(t *testing.T) {
	w, _ := genericWire(t, nil)

	_, sys := w.get("/redfish/v1/Systems/1")
	for _, property := range []string{"SerialNumber", "UUID", "Manufacturer", "Model", "SKU", "PowerState", "LastResetTime"} {
		assert.NotEmpty(t, sys[property], "ComputerSystem.%s", property)
	}
	actions, _ := sys["Actions"].(map[string]any)
	reset, _ := actions["#ComputerSystem.Reset"].(map[string]any)
	assert.Equal(t, []any{"On", "ForceOff", "GracefulShutdown", "ForceRestart", "GracefulRestart", "PowerCycle"},
		reset["ResetType@Redfish.AllowableValues"])
	boot, _ := sys["Boot"].(map[string]any)
	assert.Equal(t, []any{"None", "Pxe", "Cd", "Usb", "Hdd", "UefiHttp", "BiosSetup"},
		boot["BootSourceOverrideTarget@Redfish.AllowableValues"])

	// MACs sit where the specification puts them: on the device function.
	_, fn := w.get("/redfish/v1/Chassis/1/NetworkAdapters/1/NetworkDeviceFunctions/1")
	eth, _ := fn["Ethernet"].(map[string]any)
	assert.Equal(t, "52:54:00:00:00:01", eth["MACAddress"])
	resp, _ := w.get("/redfish/v1/Chassis/1/NetworkAdapters/1/NetworkPorts")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no deprecated NetworkPorts")

	// Drives hang off a Storage subsystem of the system.
	_, storage := w.get("/redfish/v1/Systems/1/Storage")
	members, _ := storage["Members"].([]any)
	require.Len(t, members, 1)
	first, _ := members[0].(map[string]any)
	_, sub := w.get(first["@odata.id"].(string))
	drives, _ := sub["Drives"].([]any)
	require.Len(t, drives, 1)
	link, _ := drives[0].(map[string]any)
	_, drive := w.get(link["@odata.id"].(string))
	assert.Equal(t, "GENDISK0001", drive["SerialNumber"])

	// The BIOS registry is found through the Registries collection, its file
	// inside the tree.
	_, bios := w.get("/redfish/v1/Systems/1/Bios")
	registry, _ := bios["AttributeRegistry"].(string)
	require.NotEmpty(t, registry)
	settings, _ := bios["@Redfish.Settings"].(map[string]any)
	assert.NotEmpty(t, settings["SettingsObject"])
	_, file := w.get("/redfish/v1/Registries/" + registry)
	locations, _ := file["Location"].([]any)
	require.NotEmpty(t, locations)
	loc, _ := locations[0].(map[string]any)
	uri, _ := loc["Uri"].(string)
	assert.Contains(t, uri, "/redfish/v1/Registries/")
	_, entries := w.get(uri)
	assert.NotEmpty(t, entries["RegistryEntries"])

	_, np := w.get("/redfish/v1/Managers/1/NetworkProtocol")
	assert.Contains(t, np, "NTP")
}
