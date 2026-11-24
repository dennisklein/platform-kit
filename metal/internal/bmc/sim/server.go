package sim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Server exposes a State as a Redfish HTTP tree — the same world the in-process
// client mutates, viewed the way the production client and curl see it. The
// tree is the specification's; where a fixture departs from it (an OEM action,
// a resource in its pre-deprecation place) the departure is fixture data and
// the route is registered from that data.
//
// The handlers live next to the resources they serve: server_root.go (service
// root, sessions, tasks, registries), server_systems.go, server_managers.go,
// server_chassis.go, server_media.go, server_update.go, server_accounts.go and
// server_certs.go. This file holds what every resource shares: routing,
// authentication, ETags and the error shape.
type Server struct {
	st  *State
	f   *Fixture
	mux *http.ServeMux
	// conns holds one token per request in flight (Protocol.MaxConnections);
	// nil is no limit.
	conns chan struct{}
}

const (
	rootURI              = "/redfish/v1"
	managerURI           = rootURI + "/Managers/1"
	chassisURI           = rootURI + "/Chassis/1"
	systemsURI           = rootURI + "/Systems"
	updateServiceURI     = rootURI + "/UpdateService"
	firmwareInventoryURI = updateServiceURI + "/FirmwareInventory"
	accountServiceURI    = rootURI + "/AccountService"
	sessionServiceURI    = rootURI + "/SessionService"
	taskServiceURI       = rootURI + "/TaskService"
	certServiceURI       = rootURI + "/CertificateService"
	eventServiceURI      = rootURI + "/EventService"
	registriesURI        = rootURI + "/Registries"
	httpsCertsURI        = managerURI + "/NetworkProtocol/HTTPS/Certificates"
)

func NewServer(st *State) *Server {
	s := &Server{st: st, f: st.f, mux: http.NewServeMux()}
	if n := st.f.Protocol.MaxConnections; n > 0 {
		s.conns = make(chan struct{}, n)
	}
	s.rootRoutes()
	s.systemRoutes()
	s.managerRoutes()
	s.chassisRoutes()
	s.mediaRoutes()
	s.updateRoutes()
	s.accountRoutes()
	s.certRoutes()
	// Everything else does not exist. Without this the mux would have no
	// answer of its own, and a client probing for an optional service must be
	// told "no" in the protocol's words.
	s.mux.HandleFunc("/", s.missing)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.conns != nil {
		select {
		case s.conns <- struct{}{}:
			defer func() { <-s.conns }()
		default:
			w.Header().Set("Retry-After", "1")
			s.fail(w, &Error{
				Status: http.StatusServiceUnavailable, MessageID: "ServiceTemporarilyUnavailable",
				Message: "the service has no free connection; retry later", Args: []string{"1"},
			})
			return
		}
	}
	if d := s.f.Protocol.Latency; d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("OData-Version", "4.0")
	w.Header().Set("Cache-Control", "no-cache")
	// A trailing slash names the same resource (the service root is commonly
	// requested both ways).
	if p := r.URL.Path; len(p) > 1 && strings.HasSuffix(p, "/") {
		r.URL.Path = strings.TrimRight(p, "/")
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="redfish"`)
		s.fail(w, &Error{
			Status: http.StatusUnauthorized, MessageID: "ResourceAtUriUnauthorized",
			Message: "while attempting to establish a connection to the service, the service denied access",
		})
		return
	}
	if r.Method == http.MethodPatch && !s.preconditionHolds(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized applies the specification's access rules: the service root and
// the version and metadata documents are public, creating a session is public
// (it carries its own credentials), everything else needs Basic credentials
// or a session token. An open sim (no accounts) authorizes everything.
func (s *Server) authorized(r *http.Request) bool {
	if !s.st.authEnforced() {
		return true
	}
	switch r.URL.Path {
	case "/redfish", rootURI, rootURI + "/odata", rootURI + "/$metadata":
		return true
	case sessionServiceURI + "/Sessions":
		if r.Method == http.MethodPost {
			return true
		}
	}
	if token := r.Header.Get("X-Auth-Token"); token != "" && s.f.Protocol.Sessions {
		return s.st.sessionValid(token)
	}
	user, pass, ok := r.BasicAuth()
	return ok && s.st.Authenticate(user, pass)
}

// preconditionHolds enforces If-Match on a PATCH: a stale tag is 412, and on a
// fixture that insists on the header its absence is 428. The current tag is
// read off the resource's own GET, so every resource is covered without each
// handler knowing about ETags.
func (s *Server) preconditionHolds(w http.ResponseWriter, r *http.Request) bool {
	if !s.f.Protocol.ETags {
		return true
	}
	probe := r.Clone(r.Context())
	probe.Method, probe.Body = http.MethodGet, http.NoBody
	probe.Header = http.Header{}
	rec := &recorder{header: http.Header{}}
	s.mux.ServeHTTP(rec, probe)
	// The proxy's suffix is not part of the tag the backend compares.
	current := strings.Replace(rec.header.Get("ETag"), `-gzip"`, `"`, 1)
	if current == "" {
		return true // not a resource; the handler answers
	}
	sent := r.Header.Get("If-Match")
	switch {
	case sent == "" && s.f.Protocol.RequireIfMatch:
		s.fail(w, &Error{
			Status: http.StatusPreconditionRequired, MessageID: "PreconditionRequired",
			Message: "a precondition header is required: send If-Match with the resource's current ETag",
		})
		return false
	case sent == "" || sent == "*":
		return true
	}
	for _, tag := range strings.Split(sent, ",") {
		if strings.TrimSpace(tag) == current {
			return true
		}
	}
	s.fail(w, &Error{
		Status: http.StatusPreconditionFailed, MessageID: "PreconditionFailed",
		Message: "the ETag supplied did not match the ETag required to change this resource",
	})
	return false
}

// recorder swallows a response and keeps its headers — the in-process GET
// behind preconditionHolds.
type recorder struct{ header http.Header }

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(int)             {}

// missing answers every path no resource claimed: 405 where the path exists
// under another method, 404 otherwise.
func (s *Server) missing(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, pattern := s.mux.Handler(probe); pattern != "/" {
			allow = append(allow, m)
		}
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		s.fail(w, &Error{
			Status: http.StatusMethodNotAllowed, MessageID: "OperationNotAllowed",
			Message: "the HTTP method is not allowed on this resource",
		})
		return
	}
	s.noResource(w, r)
}

// noResource is the 404 for a URI nothing lives at.
func (s *Server) noResource(w http.ResponseWriter, r *http.Request) {
	s.fail(w, &Error{
		Status: http.StatusNotFound, MessageID: "ResourceMissingAtURI",
		Message: "the resource at the URI " + r.URL.Path + " was not found",
		Args:    []string{r.URL.Path},
	})
}

// resource starts a resource document with the properties every one carries.
func resource(odataType, uri, id, name string) map[string]any {
	return map[string]any{
		"@odata.type": odataType,
		"@odata.id":   uri,
		"Id":          id,
		"Name":        name,
	}
}

// collection is a resource collection of the named member type.
func collection(memberType, uri string, ids []string) map[string]any {
	members := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		members = append(members, link(uri+"/"+id))
	}
	return map[string]any{
		"@odata.type":         "#" + memberType + "Collection." + memberType + "Collection",
		"@odata.id":           uri,
		"Name":                memberType + " Collection",
		"Members":             members,
		"Members@odata.count": len(members),
	}
}

func link(uri string) map[string]string { return map[string]string{"@odata.id": uri} }

func links(uris ...string) []map[string]string {
	out := make([]map[string]string, 0, len(uris))
	for _, u := range uris {
		out = append(out, link(u))
	}
	return out
}

// reply serves a resource. On a fixture that speaks ETags the document gets
// its tag, in the body and the header: a weak tag over the document's content,
// which is what the specification recommends and what changes exactly when the
// resource does.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, doc map[string]any) {
	if s.f.Protocol.ETags {
		raw, _ := json.Marshal(doc) // map keys marshal sorted: equal content, equal tag
		sum := sha256.Sum256(raw)
		tag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
		doc["@odata.etag"] = tag
		header := tag
		if s.f.Protocol.ETagGzipSuffix {
			header = strings.TrimSuffix(tag, `"`) + `-gzip"`
		}
		w.Header().Set("ETag", header)
		// mod_deflate does strip its suffix for If-None-Match, so a
		// conditional GET works either way; only If-Match trips over it.
		if inm := r.Header.Get("If-None-Match"); inm == tag || inm == header {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, http.StatusOK, doc)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// fail renders an error in the specification's shape.
func (s *Server) fail(w http.ResponseWriter, err error) {
	e := asError(err)
	writeJSON(w, e.Status, errorBody(e))
}

// decode reads a JSON request body into v; a body that is not JSON fails the
// request with the registry's MalformedJSON.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.fail(w, &Error{
			Status: http.StatusBadRequest, MessageID: "MalformedJSON",
			Message: "the request body submitted was malformed JSON and could not be parsed",
		})
		return false
	}
	return true
}

// accepted answers 202 for an asynchronous operation: the task monitor in the
// Location header, the task itself as the body.
func (s *Server) accepted(w http.ResponseWriter, t task) {
	if !s.f.Protocol.TaskInBody {
		w.Header().Set("Location", taskServiceURI+"/Tasks/"+t.id)
	}
	writeJSON(w, http.StatusAccepted, taskDoc(t))
}
