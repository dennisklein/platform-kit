package sim

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sessions are the specification's second authentication mechanism: a POST to
// the Sessions collection with a username and password answers with an
// X-Auth-Token header, which then authenticates requests until the session is
// deleted. A session is bound to the credential that opened it — it dies with
// a password change, like the Basic credential does.

type session struct {
	id       string
	token    string
	username string
	password string
	created  time.Time
}

// createSession opens a session for valid credentials.
func (s *State) createSession(username, password string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return nil, err
	}
	if !s.authenticateLocked(username, password) {
		return nil, &Error{
			Status: http.StatusUnauthorized, MessageID: "ResourceAtUriUnauthorized",
			Message: "the username or password is not valid for this service",
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	next := 1
	for _, existing := range s.sessions {
		if n, err := strconv.Atoi(existing.id); err == nil && n >= next {
			next = n + 1
		}
	}
	sess := &session{
		id: strconv.Itoa(next), token: hex.EncodeToString(raw),
		username: username, password: password, created: time.Now(),
	}
	s.sessions[sess.token] = sess
	return sess, nil
}

// sessionValid reports whether a token names a live session whose credential
// still authenticates.
func (s *State) sessionValid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[token]
	return sess != nil && s.authenticateLocked(sess.username, sess.password)
}

// sessionsView clones the session list for the HTTP tier, sorted by id.
func (s *State) sessionsView() []session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, *sess)
	}
	slices.SortFunc(out, func(a, b session) int { return strings.Compare(a.id, b.id) })
	return out
}

// deleteSession ends a session by id.
func (s *State) deleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, sess := range s.sessions {
		if sess.id == id {
			delete(s.sessions, token)
			return nil
		}
	}
	return notFound("Session", id)
}
