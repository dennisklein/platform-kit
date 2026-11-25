package sim

import (
	"context"
	"net/http"
	"slices"
	"strconv"
)

// The account model: fixture-seeded ManagerAccounts with authentication
// enforced in the HTTP tier. An empty accounts list serves an OPEN sim — no
// AccountService, nothing enforced (the posture every consumer without
// credentials relies on); any seeded account turns both on. Real trees always
// carry accounts, so enforcement-off is a sim-only convenience.
//
// Two collection shapes exist in the field and both are modelled. A growable
// collection takes a POST for a new account. A fixed-slot collection serves a
// constant number of members, the unused ones with an empty UserName; a new
// account is a PATCH of a vacant member and POST answers 405.

type account struct {
	id       string
	username string
	password string
	role     string
	enabled  bool
	locked   bool
	failures int // failed logins in a row
}

// roles are the specification's predefined roles — the only ones served.
var roles = []string{"Administrator", "Operator", "ReadOnly"}

// seedAccounts builds the account list from the fixture, padding a fixed-slot
// collection with vacant members.
func (s *State) seedAccounts() {
	for _, a := range s.f.Accounts {
		s.accounts = append(s.accounts, &account{
			id: a.ID, username: a.Username, password: a.Password, role: a.Role, enabled: a.Enabled,
		})
	}
	for n := 1; len(s.accounts) < s.f.AccountSlots && len(s.f.Accounts) > 0; n++ {
		id := strconv.Itoa(n)
		if !slices.ContainsFunc(s.accounts, func(a *account) bool { return a.id == id }) {
			s.accounts = append(s.accounts, &account{id: id})
		}
	}
}

// authEnforced reports whether the fixture models accounts at all.
func (s *State) authEnforced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts) > 0
}

// Authenticate checks a username and password against the enabled accounts.
// An open sim accepts everything.
// A failed login counts against the account of that name, and locks it at
// the fixture's lockout threshold.
func (s *State) Authenticate(username, password string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authenticateLocked(username, password)
}

func (s *State) authenticateLocked(username, password string) bool {
	if len(s.accounts) == 0 {
		return true
	}
	if username == "" {
		return false
	}
	a := s.accountByUser(username)
	if a == nil {
		return false
	}
	if a.enabled && !a.locked && a.password == password {
		a.failures = 0
		return true
	}
	a.failures++
	if t := s.f.LockoutThreshold; t > 0 && a.failures >= t {
		a.locked = true
	}
	return false
}

// AccountLocked reports whether the named account is locked (test
// observability).
func (s *State) AccountLocked(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accountByUser(username)
	return a != nil && username != "" && a.locked
}

// EnsureAccount is the seam entrance: create-or-reset the named account,
// role Administrator, enabled. Refused on an open sim — a fixture that
// models no account service cannot grow one mid-flight (enforcement would
// flip on under every other connection).
func (s *State) EnsureAccount(_ context.Context, username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if len(s.accounts) == 0 {
		return refuse("fixture models no account service")
	}
	if a := s.accountByUser(username); a != nil {
		a.password = password
		a.enabled, a.locked, a.failures = true, false, 0
		return nil
	}
	if s.f.AccountSlots > 0 {
		var vacant *account
		sorted := slices.Clone(s.accounts)
		slices.SortFunc(sorted, byAccountID)
		for _, a := range sorted {
			if a.username == "" && !slices.Contains(s.f.ReservedAccountSlots, a.id) {
				vacant = a
				break
			}
		}
		if vacant == nil {
			return refuse("every account slot is in use")
		}
		vacant.username, vacant.password, vacant.role, vacant.enabled = username, password, "Administrator", true
		return nil
	}
	s.accounts = append(s.accounts, &account{
		id:       s.nextAccountID(),
		username: username,
		password: password,
		role:     "Administrator",
		enabled:  true,
	})
	return nil
}

// DisableAccount is the seam entrance: the named account logs in no more.
// An absent account is already off.
func (s *State) DisableAccount(_ context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if len(s.accounts) == 0 {
		return refuse("fixture models no account service")
	}
	if a := s.accountByUser(username); a != nil && username != "" {
		if slices.Contains(s.f.ReservedAccountSlots, a.id) {
			return refuse("the account slot %s is reserved and cannot be changed", a.id)
		}
		a.enabled = false
	}
	return nil
}

// AccountEnabled reports whether the named account exists and is enabled
// (test observability).
func (s *State) AccountEnabled(username string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accountByUser(username)
	return a != nil && username != "" && a.enabled
}

// accountByUser finds an account by username under the caller's lock; the
// empty name finds the first vacant slot.
func (s *State) accountByUser(username string) *account {
	sorted := slices.Clone(s.accounts)
	slices.SortFunc(sorted, byAccountID)
	for _, a := range sorted {
		if a.username == username {
			return a
		}
	}
	return nil
}

// byAccountID orders accounts numerically where the ids are numbers.
func byAccountID(a, b *account) int {
	an, aerr := strconv.Atoi(a.id)
	bn, berr := strconv.Atoi(b.id)
	if aerr == nil && berr == nil {
		return an - bn
	}
	if a.id < b.id {
		return -1
	}
	if a.id > b.id {
		return 1
	}
	return 0
}

// nextAccountID picks the next numeric member id under the caller's lock.
// Growable collections start at 2 on both captured vendors.
func (s *State) nextAccountID() string {
	next := 2
	for _, a := range s.accounts {
		if n, err := strconv.Atoi(a.id); err == nil && n >= next {
			next = n + 1
		}
	}
	return strconv.Itoa(next)
}

// AccountPassword reports an account's current password (test observability).
func (s *State) AccountPassword(username string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.accountByUser(username); a != nil && username != "" {
		return a.password, true
	}
	return "", false
}

// SetAccountPassword changes an account's password out-of-band — the
// someone-changed-it-behind-the-operator's-back state the repair path heals.
func (s *State) SetAccountPassword(username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accountByUser(username)
	if a == nil || username == "" {
		return notFound("ManagerAccount", username)
	}
	a.password = password
	return nil
}

// accountsView clones the account list for the HTTP tier, sorted by id.
func (s *State) accountsView() ([]account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	sorted := slices.Clone(s.accounts)
	slices.SortFunc(sorted, byAccountID)
	out := make([]account, 0, len(sorted))
	for _, a := range sorted {
		out = append(out, *a)
	}
	return out, nil
}

// accountPatch is a member PATCH; nil fields keep their value.
type accountPatch struct {
	UserName *string `json:"UserName"`
	Password *string `json:"Password"`
	RoleID   *string `json:"RoleId"`
	Enabled  *bool   `json:"Enabled"`
	Locked   *bool   `json:"Locked"`
}

// patchAccount applies a member PATCH. Renaming is how a fixed-slot collection
// creates and deletes accounts: a name claims a vacant slot, the empty name
// vacates it.
func (s *State) patchAccount(id string, p accountPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	var a *account
	for _, cand := range s.accounts {
		if cand.id == id {
			a = cand
		}
	}
	if a == nil {
		return notFound("ManagerAccount", id)
	}
	if slices.Contains(s.f.ReservedAccountSlots, id) {
		return &Error{
			Status: http.StatusBadRequest, MessageID: "PropertyNotWritable",
			Message: "the account slot " + id + " is reserved and cannot be changed",
			Args:    []string{"UserName"},
		}
	}
	if p.Locked != nil && *p.Locked {
		// The specification lets a client clear the lock, never set it.
		return &Error{
			Status: http.StatusBadRequest, MessageID: "PropertyValueNotInList",
			Message: "Locked can only be set to false", Args: []string{"true", "Locked"},
		}
	}
	if p.UserName != nil && *p.UserName != a.username {
		if *p.UserName != "" && s.accountByUser(*p.UserName) != nil {
			return &Error{
				Status: http.StatusBadRequest, MessageID: "ResourceAlreadyExists",
				Message: "an account named " + strconv.Quote(*p.UserName) + " already exists",
				Args:    []string{"ManagerAccount", "UserName", *p.UserName},
			}
		}
	}
	if p.RoleID != nil && !slices.Contains(roles, *p.RoleID) {
		return notInList("PropertyValueNotInList", "RoleId", *p.RoleID)
	}
	if p.UserName != nil {
		a.username = *p.UserName
		if a.username == "" {
			a.password, a.role, a.enabled = "", "", false
			return nil
		}
	}
	if p.Password != nil {
		a.password = *p.Password
	}
	if p.RoleID != nil {
		a.role = *p.RoleID
	}
	if p.Enabled != nil {
		a.enabled = *p.Enabled
	}
	if p.Locked != nil {
		a.locked, a.failures = false, 0
	}
	return nil
}

// createAccount applies a collection POST; duplicate usernames are rejected
// the way real account services reject them.
func (s *State) createAccount(username, password, role string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return "", err
	}
	if s.accountByUser(username) != nil {
		return "", refuse("account %q already exists", username)
	}
	a := &account{id: s.nextAccountID(), username: username, password: password, role: role, enabled: true}
	s.accounts = append(s.accounts, a)
	return a.id, nil
}
