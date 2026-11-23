package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Server exposes a State as a Redfish HTTP tree — the same world the in-process client
// mutates, viewed the way gofish and curl see it. Dialect asymmetry is served, not
// abstracted: Supermicro-shaped fixtures expose #VirtualMedia.InsertMedia; Huawei-shaped
// ones expose only the OEM #VirtualMedia.VmmControl and answer it with 202 + a task URI
// (the iBMC behaviour vmm-boot.sh documents). Auth, TLS, ranged GETs and convergence
// modelling arrive with their consumers.
type Server struct {
	st  *State
	mux *http.ServeMux
}

func NewServer(st *State) *Server {
	s := &Server{st: st, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Basic-auth enforcement, on only when the fixture seeds accounts. The
	// service root stays public (Redfish spec: unauthenticated), which is
	// also what lets a basic-auth gofish client connect before its first
	// authenticated call reveals a bad credential.
	if s.st.authEnforced() && r.URL.Path != "/redfish/v1" && r.URL.Path != "/redfish/v1/" {
		user, pass, ok := r.BasicAuth()
		if !ok || !s.st.Authenticate(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="sim"`)
			redfishError(w, http.StatusUnauthorized, "while attempting to establish a connection to the service, the service denied access")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /redfish/v1", s.serviceRoot)
	s.mux.HandleFunc("GET /redfish/v1/", s.serviceRoot)
	s.mux.HandleFunc("GET /redfish/v1/Managers", s.managers)
	s.mux.HandleFunc("GET /redfish/v1/Managers/1", s.manager)
	s.mux.HandleFunc("GET /redfish/v1/Managers/1/VirtualMedia", s.mediaCollection)
	s.mux.HandleFunc("GET /redfish/v1/Managers/1/VirtualMedia/{id}", s.mediaSlot)
	s.mux.HandleFunc("POST /redfish/v1/Managers/1/VirtualMedia/{id}/Actions/VirtualMedia.InsertMedia", s.insertStandard)
	s.mux.HandleFunc("POST /redfish/v1/Managers/1/VirtualMedia/{id}/Actions/VirtualMedia.EjectMedia", s.eject)
	s.mux.HandleFunc("POST /redfish/v1/Managers/1/VirtualMedia/{id}/Actions/Oem/VirtualMedia.VmmControl", s.vmmControl)
	s.mux.HandleFunc("GET /redfish/v1/Managers/1/EthernetInterfaces", s.managerNICs)
	s.mux.HandleFunc("GET /redfish/v1/Managers/1/EthernetInterfaces/1", s.managerNIC)
	s.mux.HandleFunc("GET /redfish/v1/Chassis", s.chassisCollection)
	s.mux.HandleFunc("GET /redfish/v1/Chassis/1", s.chassis)
	s.mux.HandleFunc("GET /redfish/v1/Chassis/1/Drives/{n}", s.drive)
	s.mux.HandleFunc("GET /redfish/v1/Chassis/1/NetworkAdapters", s.adapters)
	s.mux.HandleFunc("GET /redfish/v1/Chassis/1/NetworkAdapters/{n}", s.adapter)
	s.mux.HandleFunc("GET /redfish/v1/Systems", s.systems)
	s.mux.HandleFunc("GET /redfish/v1/Systems/{id}", s.system)
	s.mux.HandleFunc("GET /redfish/v1/Systems/{id}/EthernetInterfaces", s.systemNICs)
	s.mux.HandleFunc("GET /redfish/v1/Systems/{id}/EthernetInterfaces/{n}", s.systemNIC)
	s.mux.HandleFunc("POST /redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset", s.reset)
	s.mux.HandleFunc("PATCH /redfish/v1/Systems/{id}", s.patchSystem)
	s.mux.HandleFunc("GET /redfish/v1/TaskService/Tasks/{id}", s.task)
	s.mux.HandleFunc("POST /redfish/v1/Managers/1/Actions/Manager.Reset", s.managerReset)
	s.mux.HandleFunc("GET /redfish/v1/UpdateService", s.updateService)
	s.mux.HandleFunc("GET /redfish/v1/UpdateService/FirmwareInventory", s.firmwareInventory)
	s.mux.HandleFunc("GET /redfish/v1/UpdateService/FirmwareInventory/{id}", s.firmwareEntry)
	s.mux.HandleFunc("POST "+multipartPushURI, s.firmwareUpload)
	s.mux.HandleFunc("POST /redfish/v1/UpdateService/Actions/UpdateService.StartUpdate", s.startUpdate)
	s.mux.HandleFunc("POST /redfish/v1/UpdateService/Actions/Oem/SmcUpdateService.Install", s.oemInstall)
	s.mux.HandleFunc("GET /redfish/v1/Systems/{id}/Bios", s.bios)
	s.mux.HandleFunc("GET /redfish/v1/Systems/{id}/Bios/{setting}", s.biosSettings)
	s.mux.HandleFunc("PATCH /redfish/v1/Systems/{id}/Bios/{setting}", s.patchBiosSettings)
	s.mux.HandleFunc("GET /redfish/v1/Registries", s.registries)
	s.mux.HandleFunc("GET /redfish/v1/Registries/{name}", s.registryFile)
	s.mux.HandleFunc("GET /redfish/v1/Managers/1/NetworkProtocol", s.networkProtocol)
	if s.st.f.NTP.Standard {
		s.mux.HandleFunc("PATCH /redfish/v1/Managers/1/NetworkProtocol", s.patchNetworkProtocol)
	}
	if s.st.f.NTP.OemService {
		// The iBMC shape: an OEM HwNtpService, linked under Manager Oem.Huawei.
		s.mux.HandleFunc("GET /redfish/v1/Managers/1/NtpService", s.ntpService)
		s.mux.HandleFunc("PATCH /redfish/v1/Managers/1/NtpService", s.patchNtpService)
	}
	if s.st.f.Identity.CSRNative {
		// The iBMC chain: Manager Oem → SecurityService → Links.HttpsCert.
		s.mux.HandleFunc("GET /redfish/v1/Managers/1/SecurityService", s.securityService)
		s.mux.HandleFunc("GET /redfish/v1/Managers/1/SecurityService/HttpsCert", s.httpsCert)
		s.mux.HandleFunc("POST /redfish/v1/Managers/1/SecurityService/HttpsCert/Actions/HttpsCert.GenerateCSR", s.generateCSR)
		s.mux.HandleFunc("POST /redfish/v1/Managers/1/SecurityService/HttpsCert/Actions/HttpsCert.ImportServerCertificate", s.importServerCertificate)
	}
	if s.st.f.Identity.KeyUpload {
		// The X12 OEM route at its fixed captured path.
		s.mux.HandleFunc("GET /redfish/v1/UpdateService/Oem/Supermicro/SSLCert", s.smcSSLCert)
		s.mux.HandleFunc("POST /redfish/v1/UpdateService/Oem/Supermicro/SSLCert/Actions/SmcSSLCert.Upload", s.smcSSLCertUpload)
	}
	if len(s.st.f.Accounts) > 0 {
		// An open fixture serves no AccountService at all, so its absence
		// reads as absence on every transport.
		s.mux.HandleFunc("GET /redfish/v1/AccountService", s.accountService)
		s.mux.HandleFunc("GET /redfish/v1/AccountService/Accounts", s.accounts)
		s.mux.HandleFunc("POST /redfish/v1/AccountService/Accounts", s.createAccount)
		s.mux.HandleFunc("GET /redfish/v1/AccountService/Accounts/{id}", s.account)
		s.mux.HandleFunc("PATCH /redfish/v1/AccountService/Accounts/{id}", s.patchAccount)
	}
	if len(s.st.f.Bios.Schema) > 0 {
		// The registry FILE lives wherever Location.Uri points — the X12 serves
		// it outside the Redfish tree entirely (/registries/…), the iBMC under
		// its RegistryStore. The route is registered at the captured path.
		s.mux.HandleFunc("GET "+s.st.f.registryURI(), s.registryEntries)
	}
}

// bySystem finds the fixture/live view of one system, or nil.
func (s *Server) bySystem(r *http.Request) *bmc.System {
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		return nil
	}
	id := r.PathValue("id")
	for i := range systems {
		if systems[i].ID == id {
			return &systems[i]
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func redfishError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg}})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if err == errUnreachable {
		// An unreachable BMC doesn't answer at all; the closest HTTP approximation
		// without killing the listener is a 503 the client treats as down.
		redfishError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	redfishError(w, http.StatusBadRequest, err.Error())
}

func (s *Server) serviceRoot(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"@odata.id":      "/redfish/v1",
		"Id":             "RootService",
		"RedfishVersion": "1.15.0",
		"Systems":        map[string]string{"@odata.id": "/redfish/v1/Systems"},
		"Managers":       map[string]string{"@odata.id": "/redfish/v1/Managers"},
		"Chassis":        map[string]string{"@odata.id": "/redfish/v1/Chassis"},
		"Tasks":          map[string]string{"@odata.id": "/redfish/v1/TaskService"},
		"UpdateService":  map[string]string{"@odata.id": "/redfish/v1/UpdateService"},
		"Registries":     map[string]string{"@odata.id": "/redfish/v1/Registries"},
	}
	if len(s.st.f.Accounts) > 0 {
		doc["AccountService"] = map[string]string{"@odata.id": "/redfish/v1/AccountService"}
	}
	writeJSON(w, 200, doc)
}

func collection(path string, ids []string) map[string]any {
	members := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		members = append(members, map[string]string{"@odata.id": path + "/" + id})
	}
	return map[string]any{
		"@odata.id":           path,
		"Members":             members,
		"Members@odata.count": len(members),
	}
}

func (s *Server) managers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, collection("/redfish/v1/Managers", []string{"1"}))
}

func (s *Server) manager(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Manager(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	oem := map[string]any{}
	if s.st.f.NTP.OemService {
		// The captured iBMC link position: Oem.Huawei.NtpService.
		oem["NtpService"] = map[string]string{"@odata.id": "/redfish/v1/Managers/1/NtpService"}
	}
	if s.st.f.Identity.CSRNative {
		oem["SecurityService"] = map[string]string{"@odata.id": "/redfish/v1/Managers/1/SecurityService"}
	}
	doc := map[string]any{
		"@odata.id":          "/redfish/v1/Managers/1",
		"Id":                 "1",
		"ManagerType":        "BMC",
		"FirmwareVersion":    m.FirmwareVersion,
		"HostName":           m.Hostname,
		"Oem":                map[string]any{m.Vendor: oem},
		"NetworkProtocol":    map[string]string{"@odata.id": "/redfish/v1/Managers/1/NetworkProtocol"},
		"VirtualMedia":       map[string]string{"@odata.id": "/redfish/v1/Managers/1/VirtualMedia"},
		"EthernetInterfaces": map[string]string{"@odata.id": "/redfish/v1/Managers/1/EthernetInterfaces"},
		"Actions": map[string]any{
			"#Manager.Reset": map[string]any{
				"target":                            "/redfish/v1/Managers/1/Actions/Manager.Reset",
				"ResetType@Redfish.AllowableValues": []string{"GracefulRestart", "ForceRestart"},
			},
		},
	}
	// The vendor split the captures pin: the X12 serves the manager's own
	// LastResetTime, the iBMC omits the property.
	if !m.LastResetTime.IsZero() {
		doc["LastResetTime"] = m.LastResetTime.Format(time.RFC3339Nano)
	}
	writeJSON(w, 200, doc)
}

func (s *Server) managerReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType string `json:"ResetType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		redfishError(w, 400, "bad request body")
		return
	}
	if err := s.st.ResetManager(r.Context(), bmc.PowerAction(body.ResetType)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) managerNICs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, collection("/redfish/v1/Managers/1/EthernetInterfaces", []string{"1"}))
}

func (s *Server) managerNIC(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Manager(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"@odata.id":  "/redfish/v1/Managers/1/EthernetInterfaces/1",
		"Id":         "1",
		"MACAddress": m.MAC,
	})
}

func (s *Server) chassisCollection(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, collection("/redfish/v1/Chassis", []string{"1"}))
}

func (s *Server) chassis(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	systems, _ := s.st.Systems(r.Context())
	var driveLinks []map[string]string
	if len(systems) > 0 {
		for i := range systems[0].Drives {
			driveLinks = append(driveLinks, map[string]string{
				"@odata.id": fmt.Sprintf("/redfish/v1/Chassis/1/Drives/%d", i+1),
			})
		}
	}
	doc := map[string]any{
		"@odata.id":       "/redfish/v1/Chassis/1",
		"Id":              "1",
		"Model":           c.Model,
		"NetworkAdapters": map[string]string{"@odata.id": "/redfish/v1/Chassis/1/NetworkAdapters"},
		"Links":           map[string]any{"Drives": driveLinks},
	}
	// The observed vendor split: iBMC puts the serial in SerialNumber; the X12 leaves it
	// null and carries it in Oem.Supermicro.BoardSerialNumber (bmc-inventory.sh's sweep).
	if s.oemBoardSerial() {
		doc["SerialNumber"] = ""
		doc["Oem"] = map[string]any{"Supermicro": map[string]any{"BoardSerialNumber": c.BoardSerial}}
	} else {
		doc["SerialNumber"] = c.BoardSerial
	}
	writeJSON(w, 200, doc)
}

func (s *Server) oemBoardSerial() bool { return s.st.f.Chassis.OemBoardSerial }

func (s *Server) drive(w http.ResponseWriter, r *http.Request) {
	systems, err := s.st.Systems(r.Context())
	if err != nil || len(systems) == 0 {
		redfishError(w, 404, "no systems")
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	drives := systems[0].Drives
	if n < 1 || n > len(drives) {
		redfishError(w, 404, "no such drive")
		return
	}
	d := drives[n-1]
	doc := map[string]any{
		"@odata.id":     fmt.Sprintf("/redfish/v1/Chassis/1/Drives/%d", n),
		"Id":            fmt.Sprintf("%d", n),
		"Model":         d.Model,
		"SerialNumber":  d.Serial,
		"CapacityBytes": int64(d.CapacityGiB) << 30,
	}
	// Served only when the fixture carries them — a vendor that omits the field
	// must stay omitted, so the fact-file layer's "?" convention gets exercised.
	if d.MediaType != "" {
		doc["MediaType"] = d.MediaType
	}
	if d.Protocol != "" {
		doc["Protocol"] = d.Protocol
	}
	writeJSON(w, 200, doc)
}

func (s *Server) adapters(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(c.Adapters))
	for i := range c.Adapters {
		ids = append(ids, fmt.Sprintf("%d", i+1))
	}
	writeJSON(w, 200, collection("/redfish/v1/Chassis/1/NetworkAdapters", ids))
}

func (s *Server) adapter(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	if n < 1 || n > len(c.Adapters) {
		redfishError(w, 404, "no such adapter")
		return
	}
	a := c.Adapters[n-1]
	writeJSON(w, 200, map[string]any{
		"@odata.id": fmt.Sprintf("/redfish/v1/Chassis/1/NetworkAdapters/%d", n),
		"Id":        fmt.Sprintf("%d", n),
		"Model":     a.Model,
		"Oem":       map[string]any{"PortMACs": a.PortMACs}, // sim-shaped; PoC captures refine
	})
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
	writeJSON(w, 200, collection("/redfish/v1/Managers/1/VirtualMedia", ids))
}

func (s *Server) mediaSlot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	media, err := s.st.Media(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, m := range media {
		if m.ID != id {
			continue
		}
		self := "/redfish/v1/Managers/1/VirtualMedia/" + id
		actions := map[string]any{}
		if m.InsertStandard {
			actions["#VirtualMedia.InsertMedia"] = map[string]string{"target": self + "/Actions/VirtualMedia.InsertMedia"}
			actions["#VirtualMedia.EjectMedia"] = map[string]string{"target": self + "/Actions/VirtualMedia.EjectMedia"}
		}
		for _, oem := range m.OemActions {
			if oem == "#VirtualMedia.VmmControl" {
				actions["Oem"] = map[string]any{
					"#VirtualMedia.VmmControl": map[string]string{"target": self + "/Actions/Oem/VirtualMedia.VmmControl"},
				}
			}
		}
		writeJSON(w, 200, map[string]any{
			"@odata.id":  self,
			"Id":         id,
			"MediaTypes": m.MediaTypes,
			"Inserted":   m.Inserted,
			"Image":      m.ImageURL,
			"Actions":    actions,
		})
		return
	}
	redfishError(w, 404, "no such virtual media")
}

func (s *Server) insertStandard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.dialectOf(id) != "insert-media" {
		// The Huawei reality: the standard action simply is not there.
		redfishError(w, 404, "action not supported")
		return
	}
	var body struct {
		Image    string  `json:"Image"`
		UserName *string `json:"UserName"`
		Password *string `json:"Password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Image == "" {
		redfishError(w, 400, "Image required")
		return
	}
	// The X12 rejects userinfo in the URL at the Image schema — credentials are
	// action parameters on this dialect.
	if u, err := url.Parse(body.Image); err != nil || u.User != nil {
		redfishError(w, 400, "invalid Image")
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
	if err := s.st.InsertMedia(r.Context(), id, body.Image, creds); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// vmmControl is the Huawei OEM path: {VmmControlType: Connect|Disconnect, Image: url},
// answered 202 + task URI (iBMC returns a task monitor; the executor polls it).
func (s *Server) vmmControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.dialectOf(id) != "vmm-control" {
		redfishError(w, 404, "action not supported")
		return
	}
	var body struct {
		VmmControlType string `json:"VmmControlType"`
		Image          string `json:"Image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		redfishError(w, 400, "bad request body")
		return
	}
	var err error
	switch body.VmmControlType {
	case "Connect":
		// Credentials ride as URL userinfo on this dialect; State extracts them.
		err = s.st.InsertMedia(r.Context(), id, body.Image, nil)
	case "Disconnect":
		err = s.st.EjectMedia(r.Context(), id)
	default:
		redfishError(w, 400, fmt.Sprintf("unknown VmmControlType %q", body.VmmControlType))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/redfish/v1/TaskService/Tasks/1")
	writeJSON(w, http.StatusAccepted, map[string]any{"@odata.id": "/redfish/v1/TaskService/Tasks/1", "TaskState": "Running"})
}

// task reports instantly-completed tasks for now; convergence-faithful modelling comes
// with the convergence sim.
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"@odata.id": "/redfish/v1/TaskService/Tasks/" + r.PathValue("id"),
		"TaskState": "Completed", "TaskStatus": "OK",
	})
}

func (s *Server) eject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.dialectOf(id) != "insert-media" {
		redfishError(w, 404, "action not supported")
		return
	}
	if err := s.st.EjectMedia(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) dialectOf(mediaID string) string {
	caps, err := s.st.Capabilities(nil) //nolint:staticcheck // State ignores the context
	if err != nil {
		return ""
	}
	return string(caps.MediaDialect[mediaID])
}

func (s *Server) systems(w http.ResponseWriter, r *http.Request) {
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(systems))
	for _, sys := range systems {
		ids = append(ids, sys.ID)
	}
	writeJSON(w, 200, collection("/redfish/v1/Systems", ids))
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, sys := range systems {
		if sys.ID != id {
			continue
		}
		doc := map[string]any{
			"@odata.id":    "/redfish/v1/Systems/" + id,
			"Id":           id,
			"UUID":         sys.UUID,
			"SerialNumber": sys.SerialNumber,
			"Manufacturer": sys.Manufacturer,
			"Model":        sys.Model,
			"PowerState":   string(sys.PowerState),
			"MemorySummary": map[string]any{
				"TotalSystemMemoryGiB": sys.MemoryGiB,
			},
			"ProcessorSummary": map[string]any{
				"Count": sys.CPUCount,
				"Model": sys.CPUModel,
			},
			"Boot": map[string]any{
				"BootSourceOverrideTarget@Redfish.AllowableValues": sys.BootTargets,
			},
			"EthernetInterfaces": map[string]string{
				"@odata.id": "/redfish/v1/Systems/" + id + "/EthernetInterfaces",
			},
			"Bios": map[string]string{
				"@odata.id": "/redfish/v1/Systems/" + id + "/Bios",
			},
			"Links": map[string]any{
				"ManagedBy": []map[string]string{{"@odata.id": "/redfish/v1/Managers/1"}},
			},
			"Actions": map[string]any{
				"#ComputerSystem.Reset": map[string]any{
					"target":                            "/redfish/v1/Systems/" + id + "/Actions/ComputerSystem.Reset",
					"ResetType@Redfish.AllowableValues": []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart"},
				},
			},
		}
		// The vendor split the PoC captures pin: the X12 serves LastResetTime, the
		// iBMC omits the property entirely.
		if !sys.LastResetTime.IsZero() {
			doc["LastResetTime"] = sys.LastResetTime.Format(time.RFC3339Nano)
		}
		writeJSON(w, 200, doc)
		return
	}
	redfishError(w, 404, "no such system")
}

func (s *Server) systemNICs(w http.ResponseWriter, r *http.Request) {
	sys := s.bySystem(r)
	if sys == nil {
		redfishError(w, 404, "no such system")
		return
	}
	base := "/redfish/v1/Systems/" + sys.ID + "/EthernetInterfaces"
	ids := make([]string, 0, len(sys.NICs))
	for i := range sys.NICs {
		ids = append(ids, fmt.Sprintf("%d", i+1))
	}
	writeJSON(w, 200, collection(base, ids))
}

func (s *Server) systemNIC(w http.ResponseWriter, r *http.Request) {
	sys := s.bySystem(r)
	if sys == nil {
		redfishError(w, 404, "no such system")
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	if n < 1 || n > len(sys.NICs) {
		redfishError(w, 404, "no such interface")
		return
	}
	nic := sys.NICs[n-1]
	writeJSON(w, 200, map[string]any{
		"@odata.id":  fmt.Sprintf("/redfish/v1/Systems/%s/EthernetInterfaces/%d", sys.ID, n),
		"Id":         nic.ID,
		"MACAddress": nic.MAC,
	})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType string `json:"ResetType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		redfishError(w, 400, "bad request body")
		return
	}
	if err := s.st.SetPower(r.Context(), r.PathValue("id"), bmc.PowerAction(body.ResetType)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// multipartPushURI is the X12's MultipartHttpPushUri, verbatim from the capture.
const multipartPushURI = "/redfish/v1/UpdateService/upload"

// updateService serves the per-vendor capture shape: the X12 advertises the
// multipart push URI, StartUpdate, and the OEM install alongside SimpleUpdate;
// the iBMC advertises SimpleUpdate and nothing else.
func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	actions := map[string]any{}
	if s.st.f.Update.SimpleUpdate {
		actions["#UpdateService.SimpleUpdate"] = map[string]string{
			"target": "/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate",
		}
	}
	doc := map[string]any{
		"@odata.id":         "/redfish/v1/UpdateService",
		"Id":                "UpdateService",
		"ServiceEnabled":    true,
		"FirmwareInventory": map[string]string{"@odata.id": "/redfish/v1/UpdateService/FirmwareInventory"},
	}
	if s.st.f.Update.MultipartPush {
		doc["MultipartHttpPushUri"] = multipartPushURI
		actions["#UpdateService.StartUpdate"] = map[string]string{
			"target": "/redfish/v1/UpdateService/Actions/UpdateService.StartUpdate",
		}
		actions["Oem"] = map[string]any{
			"#SmcUpdateService.Install": map[string]string{
				"target": "/redfish/v1/UpdateService/Actions/Oem/SmcUpdateService.Install",
			},
		}
	}
	doc["Actions"] = actions
	writeJSON(w, 200, doc)
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
	writeJSON(w, 200, collection("/redfish/v1/UpdateService/FirmwareInventory", ids))
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
		writeJSON(w, 200, map[string]any{
			"@odata.id":  "/redfish/v1/UpdateService/FirmwareInventory/" + id,
			"Id":         id,
			"Name":       id,
			"Version":    e.Version,
			"Updateable": e.Updateable,
		})
		return
	}
	redfishError(w, 404, "no such software inventory")
}

// firmwareUpload is the multipart push endpoint — the one flash path proven on
// hardware. UpdateParameters decides staged-vs-flash: apply-time Immediate plus
// the Oem.Supermicro.BMC preserve block installs; anything else stages and
// wedges (convergence.go models the consequences).
func (s *Server) firmwareUpload(w http.ResponseWriter, r *http.Request) {
	if !s.st.f.Update.MultipartPush {
		redfishError(w, 404, "no multipart push")
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		redfishError(w, 400, "malformed multipart form")
		return
	}
	var params struct {
		ApplyTime string `json:"@Redfish.OperationApplyTime"`
		Oem       struct {
			Supermicro struct {
				BMC map[string]any `json:"BMC"`
			} `json:"Supermicro"`
		} `json:"Oem"`
	}
	raw := formPart(r, "UpdateParameters")
	if raw == nil || json.Unmarshal(raw, &params) != nil {
		redfishError(w, 400, "UpdateParameters required")
		return
	}
	image := formPart(r, "UpdateFile")
	if image == nil {
		redfishError(w, 400, "UpdateFile required")
		return
	}
	switch params.ApplyTime {
	case "Immediate", "OnStartUpdateRequest":
	default:
		redfishError(w, 400, fmt.Sprintf("unsupported apply time %q — refer to HttpPushUriApplyTime", params.ApplyTime))
		return
	}
	immediate := params.ApplyTime == "Immediate"
	preserve := params.Oem.Supermicro.BMC != nil
	if err := s.st.uploadFirmware(image, immediate, preserve); err != nil {
		s.fail(w, err)
		return
	}
	// The upload task — instantly Completed, like every sim task. The flash
	// itself is proven by observables, never by this response.
	w.Header().Set("Location", "/redfish/v1/TaskService/Tasks/1")
	writeJSON(w, http.StatusAccepted, map[string]any{"@odata.id": "/redfish/v1/TaskService/Tasks/1", "TaskState": "Running"})
}

// formPart reads one multipart part regardless of whether the sender framed it
// as a field or a file (curl -F and gofish differ here).
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

// startUpdate models the recorded X12 truth: the DMTF commit rejects every body
// form (411 without a body, "malformed JSON" with one) — the staged path has no
// working commit, which is why the recipe is apply-time Immediate.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength == 0 {
		redfishError(w, http.StatusLengthRequired, "length required")
		return
	}
	redfishError(w, 400, "the request body submitted was malformed JSON")
}

// oemInstall answers success without observable effect — the recorded X12
// pattern (its true post-wedge semantics are uncaptured; nothing depends on it).
func (s *Server) oemInstall(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"Success": map[string]any{"Message": "Successfully Completed Request"}})
}

func (s *Server) bios(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	current, _, messages, err := s.st.biosView(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	self := "/redfish/v1/Systems/" + id + "/Bios"
	settings := map[string]any{
		"SettingsObject": map[string]string{"@odata.id": self + "/" + s.st.f.Bios.SettingsID},
	}
	if len(messages) > 0 {
		msgs := make([]map[string]string, 0, len(messages))
		for _, m := range messages {
			msgs = append(msgs, map[string]string{"Message": m})
		}
		settings["Messages"] = msgs
	}
	writeJSON(w, 200, map[string]any{
		"@odata.id":         self,
		"Id":                "Bios",
		"AttributeRegistry": s.st.f.Bios.Registry,
		"@Redfish.Settings": settings,
		"Attributes":        current,
	})
}

func (s *Server) biosSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.PathValue("setting") != s.st.f.Bios.SettingsID {
		redfishError(w, 404, "no such resource")
		return
	}
	_, pending, _, err := s.st.biosView(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := map[string]any{
		"@odata.id":         "/redfish/v1/Systems/" + id + "/Bios/" + s.st.f.Bios.SettingsID,
		"Id":                s.st.f.Bios.SettingsID,
		"AttributeRegistry": s.st.f.Bios.Registry,
	}
	// The capture shape: the pending object carries no Attributes key at all
	// when nothing is staged.
	if len(pending) > 0 {
		doc["Attributes"] = pending
	}
	writeJSON(w, 200, doc)
}

func (s *Server) patchBiosSettings(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("setting") != s.st.f.Bios.SettingsID {
		redfishError(w, 404, "no such resource")
		return
	}
	var body struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Attributes) == 0 {
		redfishError(w, 400, "Attributes required")
		return
	}
	if err := s.st.SetBiosAttributes(r.Context(), r.PathValue("id"), body.Attributes); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The attribute registry — served only when the fixture carries a schema, so a
// vendor without one exercises the consumer's absence path. The identity chaos
// is deliberate and capture-true: the member id, the Registry field, and what
// the Bios resource advertises are three different spellings on both vendors.
func (s *Server) registries(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if len(s.st.f.Bios.Schema) > 0 {
		ids = append(ids, s.st.f.registryID())
	}
	writeJSON(w, 200, collection("/redfish/v1/Registries", ids))
}

func (s *Server) registryFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if len(s.st.f.Bios.Schema) == 0 || name != s.st.f.registryID() {
		redfishError(w, 404, "no such registry")
		return
	}
	writeJSON(w, 200, map[string]any{
		"@odata.id": "/redfish/v1/Registries/" + name,
		"Id":        name,
		"Registry":  s.st.f.registryName(),
		"Location": []map[string]string{{
			"Language": "en",
			"Uri":      s.st.f.registryURI(),
		}},
	})
}

func (s *Server) registryEntries(w http.ResponseWriter, r *http.Request) {
	attrs := make([]map[string]any, 0, len(s.st.f.Bios.Schema))
	for _, a := range s.st.f.Bios.Schema {
		entry := map[string]any{"AttributeName": a.Name, "Type": a.Type}
		if len(a.AllowedValues) > 0 {
			values := make([]map[string]string, 0, len(a.AllowedValues))
			for _, v := range a.AllowedValues {
				values = append(values, map[string]string{"ValueName": v})
			}
			entry["Value"] = values
		}
		attrs = append(attrs, entry)
	}
	writeJSON(w, 200, map[string]any{
		"Id":              s.st.f.registryName(),
		"RegistryEntries": map[string]any{"Attributes": attrs},
	})
}

// networkProtocol serves the standard shape; the NTP block appears only on
// fixtures that model it there (the X12) — the iBMC serves the resource
// without any NTP key, its time sources live in the OEM NtpService.
func (s *Server) networkProtocol(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"@odata.id": "/redfish/v1/Managers/1/NetworkProtocol",
		"Id":        "NetworkProtocol",
	}
	if s.st.f.NTP.Standard {
		cfg, err := s.st.NTP(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		servers := cfg.Servers
		if servers == nil {
			servers = []string{}
		}
		doc["NTP"] = map[string]any{"ProtocolEnabled": cfg.Enabled, "NTPServers": servers}
	} else if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, doc)
}

func (s *Server) patchNetworkProtocol(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NTP *struct {
			ProtocolEnabled bool     `json:"ProtocolEnabled"`
			NTPServers      []string `json:"NTPServers"`
		} `json:"NTP"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.NTP == nil {
		redfishError(w, 400, "NTP required")
		return
	}
	if err := s.st.SetNTP(r.Context(), bmc.NTPConfig{
		Enabled: body.NTP.ProtocolEnabled, Servers: body.NTP.NTPServers,
	}); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ntpService serves the captured HwNtpService shape: three discrete server
// slots, empty strings for the unused ones.
func (s *Server) ntpService(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.st.NTP(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	slot := func(i int) string {
		if i < len(cfg.Servers) {
			return cfg.Servers[i]
		}
		return ""
	}
	writeJSON(w, 200, map[string]any{
		"@odata.id":          "/redfish/v1/Managers/1/NtpService",
		"@odata.type":        "#HwNtpService.v1_0_0.HwNtpService",
		"ServiceEnabled":     cfg.Enabled,
		"PreferredNtpServer": slot(0),
		"AlternateNtpServer": slot(1),
		"ExtraNtpServer":     slot(2),
		"NtpServerCount":     3,
		"NtpAddressOrigin":   "Static",
	})
}

func (s *Server) patchNtpService(w http.ResponseWriter, r *http.Request) {
	// Partial PATCH semantics: absent fields keep their value, the way the
	// live service treats them.
	cur, err := s.st.NTP(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	slot := func(i int) *string {
		if i < len(cur.Servers) {
			return &cur.Servers[i]
		}
		empty := ""
		return &empty
	}
	body := struct {
		ServiceEnabled     *bool   `json:"ServiceEnabled"`
		PreferredNtpServer *string `json:"PreferredNtpServer"`
		AlternateNtpServer *string `json:"AlternateNtpServer"`
		ExtraNtpServer     *string `json:"ExtraNtpServer"`
	}{&cur.Enabled, slot(0), slot(1), slot(2)}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		redfishError(w, 400, "bad request body")
		return
	}
	if err := s.st.SetNTP(r.Context(), bmc.NTPConfig{
		Enabled: *body.ServiceEnabled,
		Servers: []string{*body.PreferredNtpServer, *body.AlternateNtpServer, *body.ExtraNtpServer},
	}); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) securityService(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"@odata.id": "/redfish/v1/Managers/1/SecurityService",
		"Id":        "SecurityService",
		"Links": map[string]any{
			"HttpsCert": map[string]string{"@odata.id": "/redfish/v1/Managers/1/SecurityService/HttpsCert"},
		},
	})
}

// httpsCert mirrors the captured HttpsCert shape: the served certificate's
// identity, the pending CSR (null when none), and the action set.
func (s *Server) httpsCert(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	self := "/redfish/v1/Managers/1/SecurityService/HttpsCert"
	var serverCert any
	if leaf := s.st.ServedLeaf(); leaf != nil {
		serverCert = map[string]any{
			"Subject":       leaf.Subject.String(),
			"Issuer":        leaf.Issuer.String(),
			"ValidNotAfter": leaf.NotAfter.UTC().Format("Jan 2 2006 UTC"),
		}
	}
	var csr any
	if pending := s.st.PendingCSR(); pending != nil {
		csr = string(pending)
	}
	writeJSON(w, 200, map[string]any{
		"@odata.id":                  self,
		"Id":                         "HttpsCert",
		"X509CertificateInformation": map[string]any{"ServerCert": serverCert},
		"CertificateSigningRequest":  csr,
		"Actions": map[string]any{
			"#HttpsCert.GenerateCSR":             map[string]string{"target": self + "/Actions/HttpsCert.GenerateCSR"},
			"#HttpsCert.ImportServerCertificate": map[string]string{"target": self + "/Actions/HttpsCert.ImportServerCertificate"},
		},
	})
}

func (s *Server) generateCSR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Country, State, City, OrgName, OrgUnit, CommonName, SubjectAltName string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		redfishError(w, 400, "bad request body")
		return
	}
	subject := bmc.CSRSubject{
		CommonName:         body.CommonName,
		Country:            body.Country,
		State:              body.State,
		Locality:           body.City,
		Organization:       body.OrgName,
		OrganizationalUnit: body.OrgUnit,
	}
	// SubjectAltName arrives in openssl syntax — the undocumented seventh
	// parameter the live ActionInfo shows.
	for _, part := range strings.Split(body.SubjectAltName, ",") {
		switch kind, val, _ := strings.Cut(strings.TrimSpace(part), ":"); strings.ToUpper(kind) {
		case "DNS":
			subject.DNSNames = append(subject.DNSNames, val)
		case "IP", "IP ADDRESS":
			subject.IPAddresses = append(subject.IPAddresses, val)
		}
	}
	if _, err := s.st.GenerateCSR(r.Context(), subject); err != nil {
		s.fail(w, err)
		return
	}
	// The CSR is not in the response — it appears in the resource's
	// CertificateSigningRequest field, which clients poll.
	writeJSON(w, 200, map[string]any{})
}

func (s *Server) importServerCertificate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type        string
		Certificate string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Certificate == "" {
		redfishError(w, 400, "Certificate required")
		return
	}
	if body.Type != "" && body.Type != "text" {
		redfishError(w, 400, "only Type text is modeled")
		return
	}
	if err := s.st.ImportServerCertificate(r.Context(), []byte(body.Certificate)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{})
}

// smcSSLCert mirrors the captured SSLCert shape — including the vendor's
// VaildFrom typo, verbatim.
func (s *Server) smcSSLCert(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	self := "/redfish/v1/UpdateService/Oem/Supermicro/SSLCert"
	doc := map[string]any{
		"@odata.id": self,
		"Id":        "SSLCert",
		"Actions": map[string]any{
			"#SmcSSLCert.Upload": map[string]any{
				"target":                                 self + "/Actions/SmcSSLCert.Upload",
				"UploadKeyWords@Redfish.AllowableValues": []string{"cert_file", "key_file"},
			},
		},
	}
	if leaf := s.st.ServedLeaf(); leaf != nil {
		doc["VaildFrom"] = leaf.NotBefore.UTC().Format("Jan 2 15:04:05 2006 GMT")
		doc["GoodTHRU"] = leaf.NotAfter.UTC().Format("Jan 2 15:04:05 2006 GMT")
	}
	writeJSON(w, 200, doc)
}

func (s *Server) smcSSLCertUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		redfishError(w, 400, "malformed multipart form")
		return
	}
	cert, key := formPart(r, "cert_file"), formPart(r, "key_file")
	if cert == nil || key == nil {
		redfishError(w, 400, "cert_file and key_file required")
		return
	}
	if err := s.st.UploadServerIdentity(r.Context(), cert, key); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"Message": "The certificate and private key were successfully uploaded"})
}

func (s *Server) accountService(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"@odata.id": "/redfish/v1/AccountService",
		"Id":        "AccountService",
		"Accounts":  map[string]string{"@odata.id": "/redfish/v1/AccountService/Accounts"},
	})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	view, err := s.st.accountsView()
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(view))
	for _, a := range view {
		ids = append(ids, a.id)
	}
	writeJSON(w, 200, collection("/redfish/v1/AccountService/Accounts", ids))
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	view, err := s.st.accountsView()
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	for _, a := range view {
		if a.id != id {
			continue
		}
		// Password is write-only on every real account service.
		writeJSON(w, 200, map[string]any{
			"@odata.id": "/redfish/v1/AccountService/Accounts/" + id,
			"Id":        id,
			"UserName":  a.username,
			"RoleId":    a.role,
			"Enabled":   a.enabled,
			"Locked":    false,
		})
		return
	}
	redfishError(w, 404, "no such account")
}

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password *string `json:"Password"`
		Enabled  *bool   `json:"Enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		redfishError(w, 400, "bad request body")
		return
	}
	if err := s.st.patchAccount(r.PathValue("id"), body.Password, body.Enabled); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
		RoleId   string `json:"RoleId"` //nolint:revive // the Redfish property name, verbatim
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserName == "" || body.Password == "" || body.RoleId == "" {
		redfishError(w, 400, "UserName, Password, and RoleId required")
		return
	}
	id, err := s.st.createAccount(body.UserName, body.Password, body.RoleId)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", "/redfish/v1/AccountService/Accounts/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{
		"@odata.id": "/redfish/v1/AccountService/Accounts/" + id,
		"Id":        id,
		"UserName":  body.UserName,
		"RoleId":    body.RoleId,
		"Enabled":   true,
	})
}

func (s *Server) patchSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boot struct {
			BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Boot.BootSourceOverrideTarget == "" {
		redfishError(w, 400, "Boot.BootSourceOverrideTarget required")
		return
	}
	once := !strings.EqualFold(body.Boot.BootSourceOverrideEnabled, "Continuous")
	if err := s.st.SetBootOverride(r.Context(), r.PathValue("id"), body.Boot.BootSourceOverrideTarget, once); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
