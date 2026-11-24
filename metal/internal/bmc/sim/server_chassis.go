package sim

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The Chassis: the board serial, the network adapters as hardware (the host
// NICs seen from the chassis side), and — on fixtures that keep them there —
// the drives.

func (s *Server) chassisRoutes() {
	s.mux.HandleFunc("GET "+rootURI+"/Chassis", s.chassisCollection)
	s.mux.HandleFunc("GET "+chassisURI, s.chassis)
	s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters", s.adapters)
	s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}", s.adapter)
	switch s.f.Layout.AdapterMACs {
	case layoutDeviceFunctions:
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkDeviceFunctions", s.deviceFunctions)
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkDeviceFunctions/{p}", s.deviceFunction)
	case layoutNetworkPorts:
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkPorts", s.networkPorts)
		s.mux.HandleFunc("GET "+chassisURI+"/NetworkAdapters/{n}/NetworkPorts/{p}", s.networkPort)
	}
	if s.f.Layout.Drives != layoutStorage {
		s.mux.HandleFunc("GET "+chassisURI+"/Drives/{n}", s.chassisDrive)
	}
}

func (s *Server) chassisCollection(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("Chassis", rootURI+"/Chassis", []string{"1"}))
}

func (s *Server) chassis(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	doc := resource("#Chassis.v1_26_0.Chassis", chassisURI, "1", "Chassis")
	doc["ChassisType"] = "RackMount"
	doc["Manufacturer"] = s.f.Vendor
	doc["Model"] = c.Model
	doc["PowerState"] = string(systems[0].PowerState)
	doc["Status"] = map[string]string{"State": "Enabled", "Health": "OK"}
	doc["NetworkAdapters"] = link(chassisURI + "/NetworkAdapters")
	clinks := map[string]any{
		"ComputerSystems": links(systemsURI + "/" + systems[0].ID),
		"ManagedBy":       links(managerURI),
	}
	if s.f.Layout.Drives != layoutStorage {
		drives := make([]string, 0, len(systems[0].Drives))
		for i := range systems[0].Drives {
			drives = append(drives, fmt.Sprintf("%s/Drives/%d", chassisURI, i+1))
		}
		clinks["Drives"] = links(drives...)
	}
	doc["Links"] = clinks
	// The X12 leaves SerialNumber empty and carries the board serial in its
	// OEM block; everyone else serves it where the schema puts it.
	if s.f.Chassis.OemBoardSerial {
		doc["SerialNumber"] = ""
		doc["Oem"] = map[string]any{s.f.Vendor: map[string]any{"BoardSerialNumber": c.BoardSerial}}
	} else {
		doc["SerialNumber"] = c.BoardSerial
	}
	s.reply(w, r, doc)
}

func (s *Server) chassisDrive(w http.ResponseWriter, r *http.Request) {
	systems, err := s.st.Systems(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.serveDrive(w, r, &systems[0], chassisURI+"/Drives")
}

func (s *Server) adapters(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]string, 0, len(c.Adapters))
	for i := range c.Adapters {
		ids = append(ids, strconv.Itoa(i+1))
	}
	s.reply(w, r, collection("NetworkAdapter", chassisURI+"/NetworkAdapters", ids))
}

// byAdapter resolves the {n} path value to an adapter; it answers the request
// itself when there is none.
func (s *Server) byAdapter(w http.ResponseWriter, r *http.Request) (*bmc.Adapter, string) {
	c, err := s.st.Chassis(r.Context())
	if err != nil {
		s.fail(w, err)
		return nil, ""
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	if n < 1 || n > len(c.Adapters) {
		s.fail(w, notFound("NetworkAdapter", r.PathValue("n")))
		return nil, ""
	}
	return &c.Adapters[n-1], fmt.Sprintf("%s/NetworkAdapters/%d", chassisURI, n)
}

func (s *Server) adapter(w http.ResponseWriter, r *http.Request) {
	a, self := s.byAdapter(w, r)
	if a == nil {
		return
	}
	doc := resource("#NetworkAdapter.v1_11_0.NetworkAdapter", self, r.PathValue("n"), "Network Adapter")
	doc["Model"] = a.Model
	switch s.f.Layout.AdapterMACs {
	case layoutDeviceFunctions:
		doc["NetworkDeviceFunctions"] = link(self + "/NetworkDeviceFunctions")
	case layoutNetworkPorts:
		doc["NetworkPorts"] = link(self + "/NetworkPorts")
	}
	s.reply(w, r, doc)
}

// portIDs names an adapter's ports 1..N.
func portIDs(a *bmc.Adapter) []string {
	ids := make([]string, 0, len(a.PortMACs))
	for i := range a.PortMACs {
		ids = append(ids, strconv.Itoa(i+1))
	}
	return ids
}

// byPort resolves the {p} path value to a port MAC of the adapter.
func (s *Server) byPort(w http.ResponseWriter, r *http.Request, a *bmc.Adapter, kind string) (string, bool) {
	p, _ := strconv.Atoi(r.PathValue("p"))
	if p < 1 || p > len(a.PortMACs) {
		s.fail(w, notFound(kind, r.PathValue("p")))
		return "", false
	}
	return a.PortMACs[p-1], true
}

func (s *Server) deviceFunctions(w http.ResponseWriter, r *http.Request) {
	a, self := s.byAdapter(w, r)
	if a == nil {
		return
	}
	s.reply(w, r, collection("NetworkDeviceFunction", self+"/NetworkDeviceFunctions", portIDs(a)))
}

// deviceFunction is where the specification puts a port's MAC today: the
// Ethernet block of the function the host sees.
func (s *Server) deviceFunction(w http.ResponseWriter, r *http.Request) {
	a, self := s.byAdapter(w, r)
	if a == nil {
		return
	}
	mac, ok := s.byPort(w, r, a, "NetworkDeviceFunction")
	if !ok {
		return
	}
	p := r.PathValue("p")
	doc := resource("#NetworkDeviceFunction.v1_9_2.NetworkDeviceFunction", self+"/NetworkDeviceFunctions/"+p, p, "Network Device Function")
	doc["NetDevFuncType"] = "Ethernet"
	doc["DeviceEnabled"] = true
	doc["Ethernet"] = map[string]any{"MACAddress": mac, "PermanentMACAddress": mac}
	s.reply(w, r, doc)
}

func (s *Server) networkPorts(w http.ResponseWriter, r *http.Request) {
	a, self := s.byAdapter(w, r)
	if a == nil {
		return
	}
	s.reply(w, r, collection("NetworkPort", self+"/NetworkPorts", portIDs(a)))
}

// networkPort is the deprecated NetworkPort resource the iBMC still serves;
// the MAC sits in AssociatedNetworkAddresses.
func (s *Server) networkPort(w http.ResponseWriter, r *http.Request) {
	a, self := s.byAdapter(w, r)
	if a == nil {
		return
	}
	mac, ok := s.byPort(w, r, a, "NetworkPort")
	if !ok {
		return
	}
	p := r.PathValue("p")
	doc := resource("#NetworkPort.v1_4_3.NetworkPort", self+"/NetworkPorts/"+p, p, p)
	doc["PhysicalPortNumber"] = p
	doc["AssociatedNetworkAddresses"] = []string{mac}
	s.reply(w, r, doc)
}
