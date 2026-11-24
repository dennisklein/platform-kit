package sim

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Virtual media. The collection hangs off the manager or the system, as the
// fixture's layout says. A slot offers the standard InsertMedia/EjectMedia
// actions, or the iBMC's OEM VmmControl, or nothing — the dialect is slot data.

func (s *Server) mediaRoutes() {
	base := s.f.mediaBase()
	s.mux.HandleFunc("GET "+base, s.mediaCollection)
	s.mux.HandleFunc("GET "+base+"/{slot}", s.mediaSlot)
	s.mux.HandleFunc("POST "+base+"/{slot}/Actions/VirtualMedia.InsertMedia", s.insertMedia)
	s.mux.HandleFunc("POST "+base+"/{slot}/Actions/VirtualMedia.EjectMedia", s.ejectMedia)
	// The iBMC declares its OEM action under the resource's Oem block, and
	// the target lives there too — not under Actions/Oem.
	for i := range s.f.VirtualMedia {
		if s.f.dialect(i) == bmc.DialectVmmControl {
			s.mux.HandleFunc("POST "+base+"/{slot}/Oem/"+s.f.Vendor+"/Actions/VirtualMedia.VmmControl", s.vmmControl)
			break
		}
	}
}

func (s *Server) mediaCollection(w http.ResponseWriter, r *http.Request) {
	media, err := s.st.Media(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(media))
	for _, m := range media {
		ids = append(ids, m.ID)
	}
	s.reply(w, r, collection("VirtualMedia", s.f.mediaBase(), ids))
}

// bySlot resolves the {slot} path value; it answers the request itself when
// there is no such slot.
func (s *Server) bySlot(w http.ResponseWriter, r *http.Request) *bmc.VirtualMedia {
	media, err := s.st.Media(r.Context())
	if err != nil {
		s.fail(w, err)
		return nil
	}
	id := r.PathValue("slot")
	for i := range media {
		if media[i].ID == id {
			return &media[i]
		}
	}
	s.fail(w, notFound("VirtualMedia", id))
	return nil
}

func (s *Server) mediaSlot(w http.ResponseWriter, r *http.Request) {
	m := s.bySlot(w, r)
	if m == nil {
		return
	}
	self := s.f.mediaBase() + "/" + m.ID
	types := m.MediaTypes
	if types == nil {
		types = []string{}
	}
	doc := resource("#VirtualMedia.v1_6_4.VirtualMedia", self, m.ID, "Virtual Media")
	doc["MediaTypes"] = types
	doc["Inserted"] = m.Inserted
	doc["WriteProtected"] = true
	if m.Inserted {
		// The stored URL keeps whatever the dialect put into it (userinfo on
		// the VmmControl dialect); the file name is its last element.
		doc["Image"] = m.ImageURL
		doc["ImageName"] = path.Base(m.ImageURL)
		doc["ConnectedVia"] = "URI"
		doc["TransferProtocolType"] = "HTTPS"
		doc["TransferMethod"] = "Stream"
	} else {
		doc["Image"] = nil
		doc["ImageName"] = nil
		doc["ConnectedVia"] = "NotConnected"
	}
	actions := map[string]any{}
	if m.InsertStandard {
		actions["#VirtualMedia.InsertMedia"] = map[string]string{"target": self + "/Actions/VirtualMedia.InsertMedia"}
		actions["#VirtualMedia.EjectMedia"] = map[string]string{"target": self + "/Actions/VirtualMedia.EjectMedia"}
	}
	doc["Actions"] = actions
	if slices.Contains(m.OemActions, vmmControlAction) {
		doc["Oem"] = map[string]any{s.f.Vendor: map[string]any{
			"Actions": map[string]any{
				vmmControlAction: map[string]string{
					"target": self + "/Oem/" + s.f.Vendor + "/Actions/VirtualMedia.VmmControl",
				},
			},
		}}
	}
	s.reply(w, r, doc)
}

// dialectOf is the insert mechanism a slot advertises.
func (s *Server) dialectOf(slot string) bmc.MediaDialect {
	for i, vm := range s.f.VirtualMedia {
		if vm.ID == slot {
			return s.f.dialect(i)
		}
	}
	return bmc.DialectNone
}

// insertMedia is the standard action. Credentials are action parameters on
// this dialect; userinfo in the image URL is refused.
func (s *Server) insertMedia(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	if s.dialectOf(slot) != bmc.DialectStandard {
		// The iBMC reality: the standard action simply is not there.
		s.noResource(w, r)
		return
	}
	var body struct {
		Image                string  `json:"Image"`
		Inserted             *bool   `json:"Inserted"`
		WriteProtected       *bool   `json:"WriteProtected"`
		TransferProtocolType *string `json:"TransferProtocolType"`
		TransferMethod       *string `json:"TransferMethod"`
		UserName             *string `json:"UserName"`
		Password             *string `json:"Password"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Image == "" {
		s.fail(w, missing("VirtualMedia.InsertMedia", "Image"))
		return
	}
	u, err := url.Parse(body.Image)
	if err != nil || u.User != nil {
		s.fail(w, notInList("ActionParameterValueFormatError", "Image", body.Image))
		return
	}
	// Both flags default to true and nothing else is modelled: a medium that
	// is attached but hidden, or writable, has no meaning for a boot image.
	if body.Inserted != nil && !*body.Inserted {
		s.fail(w, notInList("ActionParameterValueNotInList", "Inserted", "false"))
		return
	}
	if body.WriteProtected != nil && !*body.WriteProtected {
		s.fail(w, notInList("ActionParameterValueNotInList", "WriteProtected", "false"))
		return
	}
	// The protocol may be named or left to the URL; when named it has to agree.
	if t := body.TransferProtocolType; t != nil && !strings.EqualFold(*t, u.Scheme) {
		s.fail(w, notInList("ActionParameterValueNotInList", "TransferProtocolType", *t))
		return
	}
	if t := body.TransferMethod; t != nil && *t != "Stream" {
		s.fail(w, notInList("ActionParameterValueNotInList", "TransferMethod", *t))
		return
	}
	var creds *bmc.MediaCredentials
	if body.UserName != nil || body.Password != nil {
		creds = &bmc.MediaCredentials{}
		if body.UserName != nil {
			creds.Username = *body.UserName
		}
		if body.Password != nil {
			creds.Password = *body.Password
		}
	}
	if err := s.st.InsertMedia(r.Context(), slot, body.Image, creds); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ejectMedia(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	if s.dialectOf(slot) != bmc.DialectStandard {
		s.noResource(w, r)
		return
	}
	if err := s.st.EjectMedia(r.Context(), slot); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// vmmControl is the iBMC's OEM action: {VmmControlType: Connect|Disconnect,
// Image: url}, answered 202 with a task monitor the caller polls. Credentials
// ride as URL userinfo on this dialect.
func (s *Server) vmmControl(w http.ResponseWriter, r *http.Request) {
	slot := r.PathValue("slot")
	if s.dialectOf(slot) != bmc.DialectVmmControl {
		s.noResource(w, r)
		return
	}
	var body struct {
		VmmControlType string `json:"VmmControlType"`
		Image          string `json:"Image"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	var err error
	switch body.VmmControlType {
	case "Connect":
		if s.f.Protocol.TaskDuration > 0 {
			// The mount runs as the task: a failed mount is the task's
			// Exception, which only a client that follows the task sees.
			s.accepted(w, s.st.runTask("VirtualMedia control", func() error {
				return s.st.InsertMedia(context.WithoutCancel(r.Context()), slot, body.Image, nil)
			}))
			return
		}
		err = s.st.InsertMedia(r.Context(), slot, body.Image, nil)
	case "Disconnect":
		err = s.st.EjectMedia(r.Context(), slot)
	default:
		err = notInList("ActionParameterValueNotInList", "VmmControlType", body.VmmControlType)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.accepted(w, s.st.noteTask("VirtualMedia control"))
}
