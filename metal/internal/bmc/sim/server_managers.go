package sim

import (
	"net/http"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The Manager — the BMC itself — with its NIC and network protocol settings.

func (s *Server) managerRoutes() {
	s.mux.HandleFunc("GET "+rootURI+"/Managers", s.managers)
	s.mux.HandleFunc("GET "+managerURI, s.manager)
	s.mux.HandleFunc("POST "+managerURI+"/Actions/Manager.Reset", s.managerReset)
	s.mux.HandleFunc("GET "+managerURI+"/EthernetInterfaces", s.managerNICs)
	s.mux.HandleFunc("GET "+managerURI+"/EthernetInterfaces/1", s.managerNIC)
	s.mux.HandleFunc("GET "+managerURI+"/NetworkProtocol", s.networkProtocol)
	if s.f.NTP.Standard {
		s.mux.HandleFunc("PATCH "+managerURI+"/NetworkProtocol", s.patchNetworkProtocol)
	}
	if s.f.NTP.OemService {
		// The iBMC shape: an OEM HwNtpService, linked under the Manager's Oem block.
		s.mux.HandleFunc("GET "+managerURI+"/NtpService", s.ntpService)
		s.mux.HandleFunc("PATCH "+managerURI+"/NtpService", s.patchNtpService)
	}
}

func (s *Server) managers(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("Manager", rootURI+"/Managers", []string{"1"}))
}

func (s *Server) manager(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Manager(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#Manager.v1_20_0.Manager", managerURI, "1", "Manager")
	doc["ManagerType"] = "BMC"
	doc["FirmwareVersion"] = m.FirmwareVersion
	doc["Status"] = map[string]string{"State": "Enabled", "Health": "OK"}
	doc["NetworkProtocol"] = link(managerURI + "/NetworkProtocol")
	doc["EthernetInterfaces"] = link(managerURI + "/EthernetInterfaces")
	if s.f.Manager.Model != "" {
		doc["Model"] = s.f.Manager.Model
	}
	if s.f.Layout.VirtualMedia != layoutSystem {
		doc["VirtualMedia"] = link(s.f.mediaBase())
	}
	if s.f.Protocol.OemVendorKey {
		// The vendor's OEM block: the iBMC serves its HostName here, and
		// links the OEM services it has instead of the standard ones.
		oem := map[string]any{}
		if s.f.NTP.OemService {
			oem["NtpService"] = link(managerURI + "/NtpService")
		}
		if s.f.Identity.CSRNative {
			oem["SecurityService"] = link(managerURI + "/SecurityService")
		}
		doc["Oem"] = map[string]any{m.Vendor: oem}
		doc["HostName"] = m.Hostname
	}
	mlinks := map[string]any{
		"ManagerForServers": links(systemsURI + "/" + s.f.Systems[0].ID),
		"ManagerForChassis": links(chassisURI),
	}
	for _, e := range s.f.Update.Inventory {
		if e.BMC && s.f.Manager.ActiveSoftwareImage {
			mlinks["ActiveSoftwareImage"] = link(firmwareInventoryURI + "/" + e.ID)
		}
	}
	doc["Links"] = mlinks
	doc["Actions"] = map[string]any{
		"#Manager.Reset": map[string]any{
			"target":                            managerURI + "/Actions/Manager.Reset",
			"ResetType@Redfish.AllowableValues": []string{"GracefulRestart", "ForceRestart"},
		},
	}
	// The iBMC omits the manager's own LastResetTime; the fixture decides.
	if !m.LastResetTime.IsZero() {
		doc["LastResetTime"] = m.LastResetTime.Format(time.RFC3339Nano)
	}
	s.reply(w, r, doc)
}

func (s *Server) managerReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType string `json:"ResetType"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.st.ResetManager(r.Context(), bmc.PowerAction(body.ResetType)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) managerNICs(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("EthernetInterface", managerURI+"/EthernetInterfaces", []string{"1"}))
}

func (s *Server) managerNIC(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Manager(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#EthernetInterface.v1_12_3.EthernetInterface", managerURI+"/EthernetInterfaces/1", "1", "Manager Ethernet Interface")
	doc["MACAddress"] = m.MAC
	doc["PermanentMACAddress"] = m.MAC
	doc["InterfaceEnabled"] = true
	if !s.f.Protocol.OemVendorKey {
		doc["HostName"] = m.Hostname
	}
	s.reply(w, r, doc)
}

// networkProtocol serves the standard shape. The NTP block appears only on
// fixtures that model it there — the iBMC serves the resource without any NTP
// key, its time sources live in the OEM NtpService.
func (s *Server) networkProtocol(w http.ResponseWriter, r *http.Request) {
	m, err := s.st.Manager(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#ManagerNetworkProtocol.v1_10_1.ManagerNetworkProtocol", managerURI+"/NetworkProtocol", "NetworkProtocol", "Manager Network Protocol")
	https := map[string]any{"ProtocolEnabled": true, "Port": 443}
	if s.f.Identity.CertificateService {
		https["Certificates"] = link(httpsCertsURI)
	}
	doc["HTTPS"] = https
	if !s.f.Protocol.OemVendorKey {
		doc["HostName"] = m.Hostname
	}
	if s.f.NTP.Standard {
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
	}
	s.reply(w, r, doc)
}

func (s *Server) patchNetworkProtocol(w http.ResponseWriter, r *http.Request) {
	cur, err := s.st.NTP(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var body struct {
		NTP *struct {
			ProtocolEnabled *bool     `json:"ProtocolEnabled"`
			NTPServers      *[]string `json:"NTPServers"`
		} `json:"NTP"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.NTP == nil {
		s.fail(w, refuse("the request changes no property this resource can update (NTP)"))
		return
	}
	// Partial PATCH semantics: an absent property keeps its value.
	if body.NTP.ProtocolEnabled != nil {
		cur.Enabled = *body.NTP.ProtocolEnabled
	}
	if body.NTP.NTPServers != nil {
		cur.Servers = *body.NTP.NTPServers
	}
	if err := s.st.SetNTP(r.Context(), cur); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ntpService serves the iBMC's HwNtpService shape: three discrete server
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
	doc := resource("#HwNtpService.v1_0_0.HwNtpService", managerURI+"/NtpService", "NtpService", "Ntp Service")
	doc["ServiceEnabled"] = cfg.Enabled
	doc["PreferredNtpServer"] = slot(0)
	doc["AlternateNtpServer"] = slot(1)
	doc["ExtraNtpServer"] = slot(2)
	doc["NtpServerCount"] = 3
	doc["NtpAddressOrigin"] = "Static"
	s.reply(w, r, doc)
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
	if !s.decode(w, r, &body) {
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
