package sim

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The account model: fixture-seeded ManagerAccounts with Basic-auth
// enforcement in the HTTP tier. An empty accounts list serves an OPEN sim —
// no AccountService, nothing enforced (the historical posture every consumer
// without credentials relies on); any seeded account turns both on. Real
// trees always carry accounts (both capture trees list members from id 2),
// so enforcement-off is a sim-only convenience, not a vendor shape.

type account struct {
	id       string
	username string
	password string
	role     string
	enabled  bool
}

// authEnforced reports whether the fixture models accounts at all.
func (s *State) authEnforced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts) > 0
}

// Authenticate checks Basic-auth credentials against the enabled accounts.
// An open sim accepts everything.
func (s *State) Authenticate(username, password string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.accounts) == 0 {
		return true
	}
	for _, a := range s.accounts {
		if a.enabled && a.username == username && a.password == password {
			return true
		}
	}
	return false
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
		return fmt.Errorf("sim: fixture models no account service")
	}
	if a := s.accountByUser(username); a != nil {
		a.password = password
		a.enabled = true
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

// accountByUser finds an account by username under the caller's lock.
func (s *State) accountByUser(username string) *account {
	for _, a := range s.accounts {
		if a.username == username {
			return a
		}
	}
	return nil
}

// nextAccountID picks the next numeric member id (captures start at 2 on
// both vendors) under the caller's lock.
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
	if a := s.accountByUser(username); a != nil {
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
	if a == nil {
		return fmt.Errorf("sim: no account %q", username)
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
	out := make([]account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b account) int { return strings.Compare(a.id, b.id) })
	return out, nil
}

// patchAccount applies a member PATCH: password and/or enabled.
func (s *State) patchAccount(id string, password *string, enabled *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	for _, a := range s.accounts {
		if a.id != id {
			continue
		}
		if password != nil {
			a.password = *password
		}
		if enabled != nil {
			a.enabled = *enabled
		}
		return nil
	}
	return fmt.Errorf("sim: no account %q", id)
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
		return "", fmt.Errorf("sim: account %q already exists", username)
	}
	a := &account{id: s.nextAccountID(), username: username, password: password, role: role, enabled: true}
	s.accounts = append(s.accounts, a)
	return a.id, nil
}
