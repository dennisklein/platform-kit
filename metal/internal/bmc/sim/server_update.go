package sim

import (
	"encoding/json"
	"io"
	"net/http"
)

// The UpdateService: the firmware inventory and the multipart push. What an
// upload does once accepted is firmware.go's business.

func (s *Server) updateRoutes() {
	s.mux.HandleFunc("GET "+updateServiceURI, s.updateService)
	s.mux.HandleFunc("GET "+firmwareInventoryURI, s.firmwareInventory)
	s.mux.HandleFunc("GET "+firmwareInventoryURI+"/{id}", s.firmwareEntry)
	if s.f.Update.MultipartPush {
		s.mux.HandleFunc("POST "+s.f.pushURI(), s.firmwareUpload)
		s.mux.HandleFunc("POST "+updateServiceURI+"/Actions/UpdateService.StartUpdate", s.startUpdate)
	}
	if s.f.Update.Wedge {
		s.mux.HandleFunc("POST "+updateServiceURI+"/Actions/Oem/SmcUpdateService.Install", s.oemInstall)
	}
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	doc := resource("#UpdateService.v1_14_1.UpdateService", updateServiceURI, "UpdateService", "Update Service")
	doc["ServiceEnabled"] = true
	doc["FirmwareInventory"] = link(firmwareInventoryURI)
	actions := map[string]any{}
	if s.f.Update.SimpleUpdate {
		// Advertised, as on both captured vendors; no route answers it — the
		// transport is unproven and the client refuses to use it.
		actions["#UpdateService.SimpleUpdate"] = map[string]string{
			"target": updateServiceURI + "/Actions/UpdateService.SimpleUpdate",
		}
	}
	if s.f.Update.MultipartPush {
		doc["MultipartHttpPushUri"] = s.f.pushURI()
		doc["MultipartHttpPushUri@Redfish.OperationApplyTimeSupport"] = map[string]any{
			"@odata.type":     "#Settings.v1_4_0.OperationApplyTimeSupport",
			"SupportedValues": s.f.applyTimes(),
		}
		actions["#UpdateService.StartUpdate"] = map[string]string{
			"target": updateServiceURI + "/Actions/UpdateService.StartUpdate",
		}
	}
	if s.f.Update.Wedge {
		actions["Oem"] = map[string]any{
			"#SmcUpdateService.Install": map[string]string{
				"target": updateServiceURI + "/Actions/Oem/SmcUpdateService.Install",
			},
		}
	}
	if s.f.Identity.KeyUpload {
		doc["Oem"] = map[string]any{s.f.Vendor: map[string]any{"SSLCert": link(smcSSLCertURI)}}
	}
	doc["Actions"] = actions
	s.reply(w, r, doc)
}

func (s *Server) firmwareInventory(w http.ResponseWriter, r *http.Request) {
	inv, err := s.st.FirmwareInventory(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(inv))
	for _, e := range inv {
		ids = append(ids, e.ID)
	}
	s.reply(w, r, collection("SoftwareInventory", firmwareInventoryURI, ids))
}

func (s *Server) firmwareEntry(w http.ResponseWriter, r *http.Request) {
	inv, err := s.st.FirmwareInventory(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	for _, e := range inv {
		if e.ID != id {
			continue
		}
		doc := resource("#SoftwareInventory.v1_10_2.SoftwareInventory", firmwareInventoryURI+"/"+id, id, id)
		doc["Version"] = e.Version
		doc["Updateable"] = e.Updateable
		doc["Status"] = map[string]string{"State": "Enabled", "Health": "OK"}
		s.reply(w, r, doc)
		return
	}
	s.fail(w, notFound("SoftwareInventory", id))
}

// firmwareUpload is the multipart push endpoint: an UpdateParameters part
// (JSON) and an UpdateFile part (the image). The answer is a task monitor.
func (s *Server) firmwareUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		s.fail(w, refuse("malformed multipart form"))
		return
	}
	var params struct {
		Targets   []string `json:"Targets"`
		ApplyTime string   `json:"@Redfish.OperationApplyTime"`
		Oem       struct {
			Supermicro struct {
				BMC map[string]any `json:"BMC"`
			} `json:"Supermicro"`
		} `json:"Oem"`
	}
	// The parameters part is optional in the specification (every parameter
	// has a default); when present it has to be JSON.
	if raw := formPart(r, "UpdateParameters"); raw != nil && json.Unmarshal(raw, &params) != nil {
		s.fail(w, refuse("the UpdateParameters part is not valid JSON"))
		return
	}
	image := formPart(r, "UpdateFile")
	if image == nil {
		s.fail(w, refuse("the UpdateFile part is required"))
		return
	}
	t, err := s.st.pushFirmware(fwUpload{
		image:     image,
		applyTime: params.ApplyTime,
		preserve:  params.Oem.Supermicro.BMC != nil,
		targets:   params.Targets,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.accepted(w, t)
}

// applyTimes are the push URI's supported apply times.
func (f *Fixture) applyTimes() []string {
	times := []string{"Immediate", "OnStartUpdateRequest"}
	if f.Update.OnReset {
		times = append(times, "OnReset")
	}
	return times
}

// formPart reads one multipart part regardless of whether the sender framed it
// as a field or a file (curl -F and Go's multipart writer differ here).
func formPart(r *http.Request, name string) []byte {
	if v, ok := r.MultipartForm.Value[name]; ok && len(v) > 0 {
		return []byte(v[0])
	}
	if fhs, ok := r.MultipartForm.File[name]; ok && len(fhs) > 0 {
		f, err := fhs[0].Open()
		if err != nil {
			return nil
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			return nil
		}
		return data
	}
	return nil
}

// startUpdate installs a staged image. On a wedging fixture it models the
// recorded X12 truth instead: the action rejects every body form (411 without
// a body, "malformed JSON" with one) — the staged path has no working commit.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	if s.f.Update.Wedge {
		if r.ContentLength == 0 {
			s.fail(w, &Error{Status: http.StatusLengthRequired, MessageID: "GeneralError", Message: "length required"})
			return
		}
		s.fail(w, &Error{
			Status: http.StatusBadRequest, MessageID: "MalformedJSON",
			Message: "the request body submitted was malformed JSON and could not be parsed",
		})
		return
	}
	t, err := s.st.startUpdate()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.accepted(w, t)
}

// oemInstall answers success without observable effect — the recorded X12
// pattern (its true post-wedge semantics are uncaptured; nothing depends on it).
func (s *Server) oemInstall(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"Success": map[string]any{"Message": "Successfully Completed Request"}})
}
