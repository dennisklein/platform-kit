package gofish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
)

// The manager's HTTPS identity has three replacement surfaces. The two OEM
// ones are asked first, because where they exist they are the route proven on
// that hardware (the X12 serves a standard CertificateService next to its OEM
// upload); the standard CertificateService serves everything else.

// smcSSLCertURI is the X12's OEM certificate resource — a fixed OEM path, not
// schema-discoverable.
const smcSSLCertURI = "/redfish/v1/UpdateService/Oem/Supermicro/SSLCert"

// identityDialect probes the certificate-replacement surface: the iBMC's
// SecurityService HttpsCert (linked under Manager Oem) is csr-native — the key
// never leaves the manager; the X12's OEM SmcSSLCert.Upload is key-upload; a
// standard CertificateService with GenerateCSR and ReplaceCertificate is
// csr-native again; none of them is none.
func (c *client) identityDialect() bmc.IdentityDialect {
	if uri, _ := c.httpsCertURI(); uri != "" {
		return bmc.DialectCSRNative
	}
	if c.hasSmcSSLCert() {
		return bmc.DialectKeyUpload
	}
	if cs, _ := c.certificateService(); cs != nil {
		return bmc.DialectCSRNative
	}
	return bmc.DialectNoIdentity
}

// httpsCertURI resolves Manager Oem.<vendor>.SecurityService → Links.HttpsCert
// (the iBMC chain).
func (c *client) httpsCertURI() (string, error) {
	var doc struct {
		Oem map[string]struct {
			SecurityService ref `json:"SecurityService"`
		} `json:"Oem"`
	}
	if err := c.managerDoc(&doc); err != nil {
		return "", err
	}
	for _, oem := range doc.Oem {
		if oem.SecurityService.ODataID == "" {
			continue
		}
		var sdoc struct {
			Links struct {
				HttpsCert ref `json:"HttpsCert"` //nolint:revive // the Redfish property name, verbatim
			} `json:"Links"`
		}
		if _, err := c.getJSON(oem.SecurityService.ODataID, &sdoc); err != nil {
			return "", nil // advertised but unreadable — treat as absent
		}
		return sdoc.Links.HttpsCert.ODataID, nil
	}
	return "", nil
}

func (c *client) hasSmcSSLCert() bool {
	var doc struct {
		Actions map[string]json.RawMessage `json:"Actions"`
	}
	if _, err := c.getJSON(smcSSLCertURI, &doc); err != nil {
		return false
	}
	_, ok := doc.Actions["#SmcSSLCert.Upload"]
	return ok
}

// certService is the standard CertificateService as far as replacing the
// manager's HTTPS certificate needs it.
type certService struct {
	generateCSR string // action target
	replace     string // action target
	collection  string // the HTTPS certificate collection the CSR is for
	certificate string // the certificate ReplaceCertificate replaces
}

// certificateService discovers the standard surface; nil when the endpoint
// does not offer the whole of it. The HTTPS certificate collection is the one
// the Manager's NetworkProtocol links.
func (c *client) certificateService() (*certService, error) {
	root, err := c.root()
	if err != nil {
		return nil, err
	}
	if root.CertificateService.ODataID == "" {
		return nil, nil
	}
	var doc struct {
		Actions struct {
			GenerateCSR struct {
				Target string `json:"target"`
			} `json:"#CertificateService.GenerateCSR"`
			ReplaceCertificate struct {
				Target string `json:"target"`
			} `json:"#CertificateService.ReplaceCertificate"`
		} `json:"Actions"`
	}
	if _, err := c.getJSON(root.CertificateService.ODataID, &doc); err != nil {
		return nil, fmt.Errorf("bmc: certificate service: %w", err)
	}
	if doc.Actions.GenerateCSR.Target == "" || doc.Actions.ReplaceCertificate.Target == "" {
		return nil, nil
	}
	var mdoc struct {
		NetworkProtocol ref `json:"NetworkProtocol"`
	}
	if err := c.managerDoc(&mdoc); err != nil {
		return nil, err
	}
	if mdoc.NetworkProtocol.ODataID == "" {
		return nil, nil
	}
	var np struct {
		HTTPS struct {
			Certificates ref `json:"Certificates"`
		} `json:"HTTPS"`
	}
	if _, err := c.getJSON(mdoc.NetworkProtocol.ODataID, &np); err != nil {
		return nil, fmt.Errorf("bmc: network protocol: %w", err)
	}
	if np.HTTPS.Certificates.ODataID == "" {
		return nil, nil
	}
	certs, err := c.members(np.HTTPS.Certificates.ODataID)
	if err != nil {
		return nil, fmt.Errorf("bmc: https certificates: %w", err)
	}
	cs := &certService{
		generateCSR: doc.Actions.GenerateCSR.Target,
		replace:     doc.Actions.ReplaceCertificate.Target,
		collection:  np.HTTPS.Certificates.ODataID,
	}
	if len(certs) > 0 {
		cs.certificate = certs[0]
	}
	return cs, nil
}

// GenerateCSR has the manager create a key and a signing request for it.
func (c *client) GenerateCSR(ctx context.Context, subject bmc.CSRSubject) ([]byte, error) {
	if c.readOnly {
		return nil, errReadOnly
	}
	uri, err := c.httpsCertURI()
	if err != nil {
		return nil, err
	}
	if uri != "" {
		return c.oemGenerateCSR(ctx, uri, subject)
	}
	if c.hasSmcSSLCert() {
		return nil, fmt.Errorf("bmc: endpoint exposes no on-manager CSR generation")
	}
	cs, err := c.certificateService()
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, fmt.Errorf("bmc: endpoint exposes no on-manager CSR generation")
	}
	return c.standardGenerateCSR(cs, subject)
}

// standardGenerateCSR is CertificateService.GenerateCSR: the request names the
// certificate collection the CSR is for and comes back in the response. The
// alternative names are one list of plain strings, DNS names and addresses
// alike.
func (c *client) standardGenerateCSR(cs *certService, subject bmc.CSRSubject) ([]byte, error) {
	payload := map[string]any{
		"CertificateCollection": ref{ODataID: cs.collection},
		"CommonName":            subject.CommonName,
	}
	if names := slices.Concat(subject.DNSNames, subject.IPAddresses); len(names) > 0 {
		payload["AlternativeNames"] = names
	}
	for k, v := range map[string]string{
		"Country":            subject.Country,
		"State":              subject.State,
		"City":               subject.Locality,
		"Organization":       subject.Organization,
		"OrganizationalUnit": subject.OrganizationalUnit,
	} {
		if v != "" {
			payload[k] = v
		}
	}
	resp, err := c.api.Post(cs.generateCSR, payload)
	if err != nil {
		return nil, fmt.Errorf("bmc: GenerateCSR: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		CSRString string `json:"CSRString"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("bmc: GenerateCSR: %w", err)
	}
	if !strings.Contains(out.CSRString, "BEGIN CERTIFICATE REQUEST") {
		return nil, fmt.Errorf("bmc: GenerateCSR answered without a certificate request")
	}
	return []byte(out.CSRString), nil
}

// oemGenerateCSR drives the iBMC's on-manager CSR generation, the proven
// parameter set: Country and CommonName required, SubjectAltName in openssl
// syntax (real on 6.72, undocumented). The pending CSR then appears in
// HttpsCert.CertificateSigningRequest — polled, the generation is slow.
func (c *client) oemGenerateCSR(ctx context.Context, uri string, subject bmc.CSRSubject) ([]byte, error) {
	var sans []string
	for _, d := range subject.DNSNames {
		sans = append(sans, "DNS:"+d)
	}
	for _, ip := range subject.IPAddresses {
		sans = append(sans, "IP:"+ip)
	}
	payload := map[string]any{
		"Country":    subject.Country,
		"CommonName": subject.CommonName,
	}
	for k, v := range map[string]string{
		"State":          subject.State,
		"City":           subject.Locality,
		"OrgName":        subject.Organization,
		"OrgUnit":        subject.OrganizationalUnit,
		"SubjectAltName": strings.Join(sans, ","),
	} {
		if v != "" {
			payload[k] = v
		}
	}
	// The resource keeps serving the previous request while the manager
	// makes the new key, which takes a while. Only a request that differs
	// from the one served before the POST is the new one: a signed copy of
	// the old one would not match the key the manager now holds.
	before, err := c.pendingCSR(uri)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.Post(uri+"/Actions/HttpsCert.GenerateCSR", payload)
	if err != nil {
		return nil, fmt.Errorf("bmc: GenerateCSR: %w", err)
	}
	resp.Body.Close()

	var csr []byte
	err = poll.Until(ctx, taskBackoff, func(context.Context) (bool, error) {
		pending, err := c.pendingCSR(uri)
		if err != nil {
			return false, err
		}
		if pending != "" && pending != before {
			csr = []byte(pending)
			return true, nil
		}
		return false, nil
	})
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("bmc: no new CSR appeared: %w", err)
	}
	return csr, err
}

// pendingCSR reads the request HttpsCert serves; empty when it serves none.
func (c *client) pendingCSR(uri string) (string, error) {
	var doc struct {
		CertificateSigningRequest string
	}
	if _, err := c.getJSON(uri, &doc); err != nil {
		return "", fmt.Errorf("bmc: HttpsCert: %w", err)
	}
	if !strings.Contains(doc.CertificateSigningRequest, "BEGIN CERTIFICATE REQUEST") {
		return "", nil
	}
	return doc.CertificateSigningRequest, nil
}

// ImportServerCertificate hands the signed chain back to the manager that
// generated the request.
func (c *client) ImportServerCertificate(ctx context.Context, chainPEM []byte) error {
	if c.readOnly {
		return errReadOnly
	}
	uri, err := c.httpsCertURI()
	if err != nil {
		return err
	}
	if uri != "" {
		// The iBMC: PEM text, the parameter named Certificate (Content is
		// refused), Type text.
		resp, err := c.api.Post(uri+"/Actions/HttpsCert.ImportServerCertificate",
			map[string]any{"Type": "text", "Certificate": string(chainPEM)})
		if err != nil {
			return fmt.Errorf("bmc: ImportServerCertificate: %w", err)
		}
		defer resp.Body.Close()
		return c.pollAccepted(ctx, resp)
	}
	if c.hasSmcSSLCert() {
		return fmt.Errorf("bmc: endpoint exposes no certificate import")
	}
	cs, err := c.certificateService()
	if err != nil {
		return err
	}
	if cs == nil || cs.certificate == "" {
		return fmt.Errorf("bmc: endpoint exposes no certificate import")
	}
	// CertificateService.ReplaceCertificate. A raw POST: the action takes
	// CertificateUri as a link object, which gofish's typed parameters send
	// as a plain string.
	certType := "PEM"
	if bytes.Count(chainPEM, []byte("-----BEGIN CERTIFICATE-----")) > 1 {
		certType = "PEMchain"
	}
	resp, err := c.api.Post(cs.replace, map[string]any{
		"CertificateUri":    ref{ODataID: cs.certificate},
		"CertificateString": string(chainPEM),
		"CertificateType":   certType,
	})
	if err != nil {
		return fmt.Errorf("bmc: ReplaceCertificate: %w", err)
	}
	defer resp.Body.Close()
	return c.pollAccepted(ctx, resp)
}

// UploadServerIdentity is the X12 OEM route: one multipart POST, parts
// cert_file and key_file exactly as the action advertises, leaf-only PEM.
func (c *client) UploadServerIdentity(ctx context.Context, certPEM, keyPEM []byte) error {
	if c.readOnly {
		return errReadOnly
	}
	if !c.hasSmcSSLCert() {
		return fmt.Errorf("bmc: endpoint exposes no identity upload")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, part := range []struct {
		name, filename string
		data           []byte
	}{
		{"cert_file", "cert.pem", certPEM},
		{"key_file", "key.pem", keyPEM},
	} {
		fw, err := mw.CreateFormFile(part.name, part.filename)
		if err != nil {
			return err
		}
		if _, err := fw.Write(part.data); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	resp, err := c.api.RunRawRequestWithHeaders(http.MethodPost,
		smcSSLCertURI+"/Actions/SmcSSLCert.Upload",
		bytes.NewReader(buf.Bytes()), mw.FormDataContentType(), nil)
	if err != nil {
		return fmt.Errorf("bmc: SmcSSLCert.Upload: %w", err)
	}
	resp.Body.Close()
	return nil
}
