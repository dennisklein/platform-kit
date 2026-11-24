package sim

import (
	"net/http"
	"slices"
)

// The AccountService. Served only when the fixture seeds accounts — an open
// fixture serves none, so its absence reads as absence on every transport.

func (s *Server) accountRoutes() {
	if len(s.f.Accounts) == 0 {
		return
	}
	s.mux.HandleFunc("GET "+accountServiceURI, s.accountService)
	s.mux.HandleFunc("GET "+accountServiceURI+"/Accounts", s.accounts)
	s.mux.HandleFunc("GET "+accountServiceURI+"/Accounts/{id}", s.account)
	s.mux.HandleFunc("PATCH "+accountServiceURI+"/Accounts/{id}", s.patchAccount)
	s.mux.HandleFunc("GET "+accountServiceURI+"/Roles", s.roles)
	s.mux.HandleFunc("GET "+accountServiceURI+"/Roles/{id}", s.role)
	if s.f.AccountSlots == 0 {
		// A fixed-slot collection takes no POST: the method is simply not
		// allowed there, and the catch-all says so with a 405.
		s.mux.HandleFunc("POST "+accountServiceURI+"/Accounts", s.createAccount)
	}
}

func (s *Server) accountService(w http.ResponseWriter, r *http.Request) {
	doc := resource("#AccountService.v1_16_0.AccountService", accountServiceURI, "AccountService", "Account Service")
	doc["ServiceEnabled"] = true
	doc["MinPasswordLength"] = 8
	doc["MaxPasswordLength"] = 64
	doc["Accounts"] = link(accountServiceURI + "/Accounts")
	doc["Roles"] = link(accountServiceURI + "/Roles")
	s.reply(w, r, doc)
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
	s.reply(w, r, collection("ManagerAccount", accountServiceURI+"/Accounts", ids))
}

func accountDoc(a account) map[string]any {
	doc := resource("#ManagerAccount.v1_13_0.ManagerAccount", accountServiceURI+"/Accounts/"+a.id, a.id, "User Account")
	doc["UserName"] = a.username
	doc["RoleId"] = a.role
	doc["Enabled"] = a.enabled
	doc["Locked"] = a.locked
	// Password is write-only on every account service: always null on read.
	doc["Password"] = nil
	doc["AccountTypes"] = []string{"Redfish"}
	if a.role != "" {
		doc["Links"] = map[string]any{"Role": link(accountServiceURI + "/Roles/" + a.role)}
	}
	return doc
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	view, err := s.st.accountsView()
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	for _, a := range view {
		if a.id == id {
			s.reply(w, r, accountDoc(a))
			return
		}
	}
	s.fail(w, notFound("ManagerAccount", id))
}

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request) {
	var body accountPatch
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.st.patchAccount(r.PathValue("id"), body); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
		RoleID   string `json:"RoleId"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	for _, p := range []struct{ name, value string }{
		{"UserName", body.UserName}, {"Password", body.Password}, {"RoleId", body.RoleID},
	} {
		if p.value == "" {
			s.fail(w, &Error{
				Status: http.StatusBadRequest, MessageID: "CreateFailedMissingReqProperties",
				Message: "the create operation failed because the required property " + p.name + " was missing from the request",
				Args:    []string{p.name},
			})
			return
		}
	}
	id, err := s.st.createAccount(body.UserName, body.Password, body.RoleID)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Location", accountServiceURI+"/Accounts/"+id)
	writeJSON(w, http.StatusCreated, accountDoc(account{id: id, username: body.UserName, role: body.RoleID, enabled: true}))
}

func (s *Server) roles(w http.ResponseWriter, r *http.Request) {
	s.reply(w, r, collection("Role", accountServiceURI+"/Roles", roles))
}

// rolePrivileges are the privileges the specification assigns its predefined
// roles.
var rolePrivileges = map[string][]string{
	"Administrator": {"Login", "ConfigureManager", "ConfigureUsers", "ConfigureSelf", "ConfigureComponents"},
	"Operator":      {"Login", "ConfigureSelf", "ConfigureComponents"},
	"ReadOnly":      {"Login", "ConfigureSelf"},
}

func (s *Server) role(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !slices.Contains(roles, id) {
		s.fail(w, notFound("Role", id))
		return
	}
	doc := resource("#Role.v1_3_3.Role", accountServiceURI+"/Roles/"+id, id, id)
	doc["RoleId"] = id
	doc["IsPredefined"] = true
	doc["AssignedPrivileges"] = rolePrivileges[id]
	s.reply(w, r, doc)
}
