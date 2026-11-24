package sim

import (
	"net/http"
	"time"
)

// The service root and the services that hang directly off it: sessions,
// tasks, events and the registries.

func (s *Server) rootRoutes() {
	s.mux.HandleFunc("GET /redfish", s.versions)
	s.mux.HandleFunc("GET "+rootURI, s.serviceRoot)
	s.mux.HandleFunc("GET "+rootURI+"/odata", s.odata)
	s.mux.HandleFunc("GET "+rootURI+"/$metadata", s.metadata)

	s.mux.HandleFunc("GET "+taskServiceURI, s.taskService)
	s.mux.HandleFunc("GET "+taskServiceURI+"/Tasks", s.tasks)
	s.mux.HandleFunc("GET "+taskServiceURI+"/Tasks/{id}", s.task)

	s.mux.HandleFunc("GET "+eventServiceURI, s.eventService)
	s.mux.HandleFunc("GET "+eventServiceURI+"/Subscriptions", s.subscriptions)

	s.mux.HandleFunc("GET "+registriesURI, s.registries)
	s.mux.HandleFunc("GET "+registriesURI+"/{name}", s.registryFile)
	if len(s.f.Bios.Schema) > 0 {
		// The registry FILE lives wherever Location.Uri points — inside the
		// tree by default, outside it on the X12 (/registries/…).
		s.mux.HandleFunc("GET "+s.f.registryURI(), s.registryEntries)
	}

	if s.f.Protocol.Sessions {
		s.mux.HandleFunc("GET "+sessionServiceURI, s.sessionService)
		s.mux.HandleFunc("GET "+sessionServiceURI+"/Sessions", s.sessions)
		s.mux.HandleFunc("POST "+sessionServiceURI+"/Sessions", s.createSession)
		s.mux.HandleFunc("GET "+sessionServiceURI+"/Sessions/{id}", s.session)
		s.mux.HandleFunc("DELETE "+sessionServiceURI+"/Sessions/{id}", s.deleteSession)
	}
}

// versions is the unversioned entry point: which protocol versions exist.
func (s *Server) versions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"v1": rootURI + "/"})
}

func (s *Server) serviceRoot(w http.ResponseWriter, r *http.Request) {
	doc := resource("#ServiceRoot.v1_17_0.ServiceRoot", rootURI, "RootService", "Root Service")
	doc["RedfishVersion"] = s.f.redfishVersion()
	doc["UUID"] = s.f.Systems[0].UUID
	doc["Systems"] = link(systemsURI)
	doc["Managers"] = link(rootURI + "/Managers")
	doc["Chassis"] = link(rootURI + "/Chassis")
	doc["Tasks"] = link(taskServiceURI)
	doc["UpdateService"] = link(updateServiceURI)
	doc["EventService"] = link(eventServiceURI)
	doc["Registries"] = link(registriesURI)
	if s.f.Protocol.RootVendor {
		doc["Vendor"] = s.f.Vendor
	}
	if s.f.Product != "" {
		doc["Product"] = s.f.Product
	}
	if len(s.f.Accounts) > 0 {
		doc["AccountService"] = link(accountServiceURI)
	}
	if s.f.Identity.CertificateService {
		doc["CertificateService"] = link(certServiceURI)
	}
	if s.f.Protocol.Sessions {
		doc["SessionService"] = link(sessionServiceURI)
		doc["Links"] = map[string]any{"Sessions": link(sessionServiceURI + "/Sessions")}
	}
	doc["ProtocolFeaturesSupported"] = map[string]any{
		"ExpandQuery":     map[string]any{"ExpandAll": false, "Levels": false, "Links": false, "NoLinks": false},
		"FilterQuery":     false,
		"SelectQuery":     false,
		"ExcerptQuery":    false,
		"OnlyMemberQuery": false,
	}
	s.reply(w, r, doc)
}

// odata is the OData service document every Redfish service carries.
func (s *Server) odata(w http.ResponseWriter, _ *http.Request) {
	entries := []map[string]string{
		{"name": "Service", "kind": "Singleton", "url": rootURI},
		{"name": "Systems", "kind": "Singleton", "url": systemsURI},
		{"name": "Managers", "kind": "Singleton", "url": rootURI + "/Managers"},
		{"name": "Chassis", "kind": "Singleton", "url": rootURI + "/Chassis"},
	}
	writeJSON(w, http.StatusOK, map[string]any{"@odata.context": rootURI + "/$metadata", "value": entries})
}

// metadata is the OData metadata document: the schema references a generic
// OData client needs to resolve the service root's type.
func (s *Server) metadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<edmx:Edmx xmlns:edmx="http://docs.oasis-open.org/odata/ns/edmx" Version="4.0">
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/ServiceRoot_v1.xml">
    <edmx:Include Namespace="ServiceRoot"/>
    <edmx:Include Namespace="ServiceRoot.v1_17_0"/>
  </edmx:Reference>
  <edmx:DataServices>
    <Schema xmlns="http://docs.oasis-open.org/odata/ns/edm" Namespace="Service">
      <EntityContainer Name="Service" Extends="ServiceRoot.v1_17_0.ServiceContainer"/>
    </Schema>
  </edmx:DataServices>
</edmx:Edmx>
`))
}

func (s *Server) taskService(w http.ResponseWriter, r *http.Request) {
	doc := resource("#TaskService.v1_2_1.TaskService", taskServiceURI, "TaskService", "Task Service")
	doc["ServiceEnabled"] = true
	doc["CompletedTaskOverWritePolicy"] = "Oldest"
	doc["LifeCycleEventOnTaskStateChange"] = false
	doc["Tasks"] = link(taskServiceURI + "/Tasks")
	s.reply(w, r, doc)
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, t := range s.st.tasksView() {
		ids = append(ids, t.id)
	}
	s.reply(w, r, collection("Task", taskServiceURI+"/Tasks", ids))
}

func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, t := range s.st.tasksView() {
		if t.id == id {
			s.reply(w, r, taskDoc(t))
			return
		}
	}
	s.fail(w, notFound("Task", id))
}

func taskDoc(t task) map[string]any {
	uri := taskServiceURI + "/Tasks/" + t.id
	doc := resource("#Task.v1_7_4.Task", uri, t.id, t.name)
	doc["TaskMonitor"] = uri
	doc["TaskState"] = t.state
	doc["StartTime"] = t.started.UTC().Format(time.RFC3339)
	switch t.state {
	case "Completed":
		doc["TaskStatus"] = "OK"
		doc["PercentComplete"] = 100
		doc["EndTime"] = t.ended.UTC().Format(time.RFC3339)
	case "Exception":
		doc["TaskStatus"] = "Critical"
		doc["EndTime"] = t.ended.UTC().Format(time.RFC3339)
		doc["Messages"] = []map[string]any{{
			"@odata.type": "#Message.v1_2_1.Message",
			"MessageId":   baseRegistry + "GeneralError",
			"Message":     t.message,
		}}
	default:
		doc["TaskStatus"] = "OK"
		doc["PercentComplete"] = 0
	}
	return doc
}

// eventService is served disabled: the tree is complete, and no consumer
// subscribes to events.
func (s *Server) eventService(w http.ResponseWriter, r *http.Request) {
	doc := resource("#EventService.v1_10_3.EventService", eventServiceURI, "EventService", "Event Service")
	doc["ServiceEnabled"] = false
	doc["DeliveryRetryAttempts"] = 3
	doc["DeliveryRetryIntervalSeconds"] = 30
	doc["EventFormatTypes"] = []string{"Event"}
	doc["Subscriptions"] = link(eventServiceURI + "/Subscriptions")
	s.reply(w, r, doc)
}

func (s *Server) subscriptions(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("EventDestination", eventServiceURI+"/Subscriptions", nil))
}

// The attribute registry — served only when the fixture carries a schema, so
// an endpoint without one exercises the consumer's absence path.
func (s *Server) registries(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if len(s.f.Bios.Schema) > 0 {
		ids = append(ids, s.f.registryID())
	}
	s.reply(w, r, collection("MessageRegistryFile", registriesURI, ids))
}

func (s *Server) registryFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if len(s.f.Bios.Schema) == 0 || name != s.f.registryID() {
		s.fail(w, notFound("MessageRegistryFile", name))
		return
	}
	doc := resource("#MessageRegistryFile.v1_1_5.MessageRegistryFile", registriesURI+"/"+name, name, "BIOS Attribute Registry")
	doc["Registry"] = s.f.registryName()
	doc["Languages"] = []string{"en"}
	doc["Location"] = []map[string]string{{"Language": "en", "Uri": s.f.registryURI()}}
	s.reply(w, r, doc)
}

// registryEntries is the registry file itself. It is a file, not a resource:
// it has no @odata.id of its own.
func (s *Server) registryEntries(w http.ResponseWriter, _ *http.Request) {
	attrs := make([]map[string]any, 0, len(s.f.Bios.Schema))
	for _, a := range s.f.Bios.Schema {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.type":     "#AttributeRegistry.v1_3_9.AttributeRegistry",
		"Id":              s.f.registryName(),
		"Name":            "BIOS Attribute Registry",
		"Language":        "en",
		"RegistryVersion": "1.0.0",
		"OwningEntity":    s.f.Vendor,
		"RegistryEntries": map[string]any{"Attributes": attrs},
	})
}

func (s *Server) sessionService(w http.ResponseWriter, r *http.Request) {
	doc := resource("#SessionService.v1_1_9.SessionService", sessionServiceURI, "SessionService", "Session Service")
	doc["ServiceEnabled"] = true
	doc["SessionTimeout"] = 1800
	doc["Sessions"] = link(sessionServiceURI + "/Sessions")
	s.reply(w, r, doc)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, sess := range s.st.sessionsView() {
		ids = append(ids, sess.id)
	}
	s.reply(w, r, collection("Session", sessionServiceURI+"/Sessions", ids))
}

func sessionDoc(sess session) map[string]any {
	doc := resource("#Session.v1_7_2.Session", sessionServiceURI+"/Sessions/"+sess.id, sess.id, "User Session")
	doc["UserName"] = sess.username
	doc["SessionType"] = "Redfish"
	doc["CreatedTime"] = sess.created.UTC().Format(time.RFC3339)
	return doc
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, sess := range s.st.sessionsView() {
		if sess.id == id {
			s.reply(w, r, sessionDoc(sess))
			return
		}
	}
	s.fail(w, notFound("Session", id))
}

// createSession is the login: credentials in, token out. The token travels in
// the X-Auth-Token header only, never in the body.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	sess, err := s.st.createSession(body.UserName, body.Password)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("X-Auth-Token", sess.token)
	w.Header().Set("Location", sessionServiceURI+"/Sessions/"+sess.id)
	writeJSON(w, http.StatusCreated, sessionDoc(*sess))
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.st.deleteSession(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
