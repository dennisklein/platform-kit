package sim

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The three certificate-replacement surfaces (identity.go has the model): the
// standard CertificateService, the iBMC's OEM HttpsCert, the X12's OEM
// SmcSSLCert.

// smcSSLCertURI is the X12's OEM certificate resource — a fixed OEM path.
const smcSSLCertURI = updateServiceURI + "/Oem/Supermicro/SSLCert"

func (s *Server) certRoutes() {
	if s.f.Identity.CertificateService {
		s.mux.HandleFunc("GET "+certServiceURI, s.certificateService)
		s.mux.HandleFunc("GET "+certServiceURI+"/CertificateLocations", s.certificateLocations)
		s.mux.HandleFunc("POST "+certServiceURI+"/Actions/CertificateService.GenerateCSR", s.generateCSR)
		s.mux.HandleFunc("POST "+certServiceURI+"/Actions/CertificateService.ReplaceCertificate", s.replaceCertificate)
		s.mux.HandleFunc("GET "+httpsCertsURI, s.httpsCertificates)
		s.mux.HandleFunc("GET "+httpsCertsURI+"/1", s.httpsCertificate)
	}
	if s.f.Identity.CSRNative {
		// The iBMC chain: Manager Oem → SecurityService → Links.HttpsCert.
		s.mux.HandleFunc("GET "+managerURI+"/SecurityService", s.securityService)
		s.mux.HandleFunc("GET "+managerURI+"/SecurityService/HttpsCert", s.httpsCert)
		s.mux.HandleFunc("POST "+managerURI+"/SecurityService/HttpsCert/Actions/HttpsCert.GenerateCSR", s.oemGenerateCSR)
		s.mux.HandleFunc("POST "+managerURI+"/SecurityService/HttpsCert/Actions/HttpsCert.ImportServerCertificate", s.importServerCertificate)
	}
	if s.f.Identity.KeyUpload {
		s.mux.HandleFunc("GET "+smcSSLCertURI, s.smcSSLCert)
		s.mux.HandleFunc("POST "+smcSSLCertURI+"/Actions/SmcSSLCert.Upload", s.smcSSLCertUpload)
	}
}

func (s *Server) certificateService(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	doc := resource("#CertificateService.v1_0_6.CertificateService", certServiceURI, "CertificateService", "Certificate Service")
	doc["CertificateLocations"] = link(certServiceURI + "/CertificateLocations")
	doc["Actions"] = map[string]any{
		"#CertificateService.GenerateCSR": map[string]string{
			"target": certServiceURI + "/Actions/CertificateService.GenerateCSR",
		},
		"#CertificateService.ReplaceCertificate": map[string]string{
			"target": certServiceURI + "/Actions/CertificateService.ReplaceCertificate",
		},
	}
	s.reply(w, r, doc)
}

func (s *Server) certificateLocations(w http.ResponseWriter, r *http.Request) {
	doc := resource("#CertificateLocations.v1_0_4.CertificateLocations", certServiceURI+"/CertificateLocations", "CertificateLocations", "Certificate Locations")
	doc["Links"] = map[string]any{"Certificates": links(httpsCertsURI + "/1")}
	s.reply(w, r, doc)
}

func (s *Server) httpsCertificates(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("Certificate", httpsCertsURI, []string{"1"}))
}

// httpsCertificate is the certificate the HTTPS listener presents. A sim served
// over plain HTTP has none; the resource is still there, without a
// certificate string.
func (s *Server) httpsCertificate(w http.ResponseWriter, r *http.Request) {
	doc := resource("#Certificate.v1_9_0.Certificate", httpsCertsURI+"/1", "1", "HTTPS Certificate")
	doc["CertificateType"] = "PEM"
	if leaf := s.st.ServedLeaf(); leaf != nil {
		doc["CertificateString"] = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
		doc["Subject"] = map[string]string{"CommonName": leaf.Subject.CommonName}
		doc["Issuer"] = map[string]string{"CommonName": leaf.Issuer.CommonName}
		doc["ValidNotBefore"] = leaf.NotBefore.UTC().Format(time.RFC3339)
		doc["ValidNotAfter"] = leaf.NotAfter.UTC().Format(time.RFC3339)
		doc["SerialNumber"] = leaf.SerialNumber.Text(16)
	}
	s.reply(w, r, doc)
}

// generateCSR is CertificateService.GenerateCSR: the key is created on the
// manager and the request comes back in the response.
func (s *Server) generateCSR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CertificateCollection *struct {
			ODataID string `json:"@odata.id"`
		} `json:"CertificateCollection"`
		CommonName         string   `json:"CommonName"`
		AlternativeNames   []string `json:"AlternativeNames"`
		Country            string   `json:"Country"`
		State              string   `json:"State"`
		City               string   `json:"City"`
		Organization       string   `json:"Organization"`
		OrganizationalUnit string   `json:"OrganizationalUnit"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.CertificateCollection == nil {
		s.fail(w, missing("GenerateCSR", "CertificateCollection"))
		return
	}
	if body.CertificateCollection.ODataID != httpsCertsURI {
		s.fail(w, notInList("ActionParameterValueNotInList", "CertificateCollection", body.CertificateCollection.ODataID))
		return
	}
	subject := bmc.CSRSubject{
		CommonName:         body.CommonName,
		Country:            body.Country,
		State:              body.State,
		Locality:           body.City,
		Organization:       body.Organization,
		OrganizationalUnit: body.OrganizationalUnit,
	}
	// AlternativeNames is one untyped list; an entry that parses as an IP
	// address becomes an IP SAN, anything else a DNS SAN.
	for _, name := range body.AlternativeNames {
		if net.ParseIP(name) != nil {
			subject.IPAddresses = append(subject.IPAddresses, name)
		} else {
			subject.DNSNames = append(subject.DNSNames, name)
		}
	}
	csr, err := s.st.GenerateCSR(r.Context(), subject)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"CertificateCollection": link(httpsCertsURI),
		"CSRString":             string(csr),
	})
}

// replaceCertificate is CertificateService.ReplaceCertificate: the signed
// certificate for the key GenerateCSR created.
func (s *Server) replaceCertificate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CertificateURI *struct {
			ODataID string `json:"@odata.id"`
		} `json:"CertificateUri"`
		CertificateString string `json:"CertificateString"`
		CertificateType   string `json:"CertificateType"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	switch {
	case body.CertificateURI == nil:
		s.fail(w, missing("ReplaceCertificate", "CertificateUri"))
	case body.CertificateString == "":
		s.fail(w, missing("ReplaceCertificate", "CertificateString"))
	case body.CertificateType == "":
		s.fail(w, missing("ReplaceCertificate", "CertificateType"))
	case body.CertificateURI.ODataID != httpsCertsURI+"/1":
		s.fail(w, notInList("ActionParameterValueNotInList", "CertificateUri", body.CertificateURI.ODataID))
	case body.CertificateType != "PEM" && body.CertificateType != "PEMchain":
		s.fail(w, notInList("ActionParameterValueNotInList", "CertificateType", body.CertificateType))
	default:
		if err := s.st.ImportServerCertificate(r.Context(), []byte(body.CertificateString)); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) securityService(w http.ResponseWriter, r *http.Request) {
	doc := resource("#HwSecurityService.v1_0_0.HwSecurityService", managerURI+"/SecurityService", "SecurityService", "Security Service")
	doc["Links"] = map[string]any{"HttpsCert": link(managerURI + "/SecurityService/HttpsCert")}
	s.reply(w, r, doc)
}

// httpsCert mirrors the iBMC's HttpsCert shape: the served certificate's
// identity, the pending CSR (null when none), and the action set.
func (s *Server) httpsCert(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	self := managerURI + "/SecurityService/HttpsCert"
	var serverCert any
	if leaf := s.st.ServedLeaf(); leaf != nil {
		serverCert = certSummary(leaf)
	}
	var csr any
	if pending := s.st.PendingCSR(); pending != nil {
		csr = string(pending)
	}
	doc := resource("#HwHttpsCert.v1_0_0.HwHttpsCert", self, "HttpsCert", "Https Cert")
	doc["X509CertificateInformation"] = map[string]any{"ServerCert": serverCert}
	doc["CertificateSigningRequest"] = csr
	doc["Actions"] = map[string]any{
		"#HttpsCert.GenerateCSR":             map[string]string{"target": self + "/Actions/HttpsCert.GenerateCSR"},
		"#HttpsCert.ImportServerCertificate": map[string]string{"target": self + "/Actions/HttpsCert.ImportServerCertificate"},
	}
	s.reply(w, r, doc)
}

func certSummary(leaf *x509.Certificate) map[string]any {
	return map[string]any{
		"Subject":       leaf.Subject.String(),
		"Issuer":        leaf.Issuer.String(),
		"ValidNotAfter": leaf.NotAfter.UTC().Format("Jan 2 2006 UTC"),
	}
}

func (s *Server) oemGenerateCSR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Country, State, City, OrgName, OrgUnit, CommonName, SubjectAltName string
	}
	if !s.decode(w, r, &body) {
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
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) importServerCertificate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type        string
		Certificate string
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Certificate == "" {
		s.fail(w, missing("ImportServerCertificate", "Certificate"))
		return
	}
	if body.Type != "" && body.Type != "text" {
		s.fail(w, notInList("ActionParameterValueNotInList", "Type", body.Type))
		return
	}
	if err := s.st.ImportServerCertificate(r.Context(), []byte(body.Certificate)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// smcSSLCert mirrors the X12's SSLCert shape — including the vendor's
// VaildFrom typo, verbatim.
func (s *Server) smcSSLCert(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Manager(r.Context()); err != nil { // reachability gate
		s.fail(w, err)
		return
	}
	doc := resource("#SmcSSLCert.v1_0_0.SmcSSLCert", smcSSLCertURI, "SSLCert", "SSL Certificate")
	doc["Actions"] = map[string]any{
		"#SmcSSLCert.Upload": map[string]any{
			"target":                                 smcSSLCertURI + "/Actions/SmcSSLCert.Upload",
			"UploadKeyWords@Redfish.AllowableValues": []string{"cert_file", "key_file"},
		},
	}
	if leaf := s.st.ServedLeaf(); leaf != nil {
		doc["VaildFrom"] = leaf.NotBefore.UTC().Format("Jan 2 15:04:05 2006 GMT")
		doc["GoodTHRU"] = leaf.NotAfter.UTC().Format("Jan 2 15:04:05 2006 GMT")
	}
	s.reply(w, r, doc)
}

func (s *Server) smcSSLCertUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		s.fail(w, refuse("malformed multipart form"))
		return
	}
	cert, key := formPart(r, "cert_file"), formPart(r, "key_file")
	if cert == nil || key == nil {
		s.fail(w, refuse("cert_file and key_file required"))
		return
	}
	if err := s.st.UploadServerIdentity(r.Context(), cert, key); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"Message": "The certificate and private key were successfully uploaded"})
}
