package bench

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"sync"
	"time"
)

// AgentMode says how an agent steers the UI.
type AgentMode string

const (
	// ModeNavigate: the agent moves the focus (presence chip shows who).
	ModeNavigate AgentMode = "navigate"
	// ModeSuggest: the agent sends a "look here" card; the admin clicks to follow.
	ModeSuggest AgentMode = "suggest"
)

// Grant is the approval of an agent for a limited time.
type Grant struct {
	ID         string    `json:"id"`
	ActsFor    string    `json:"actsFor"`
	Can        []string  `json:"can"`
	Mode       AgentMode `json:"mode"`
	ApprovedBy string    `json:"approvedBy"`
	ApprovedAt time.Time `json:"approvedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt,omitzero"`
	LastTool   string    `json:"lastTool,omitempty"`
	// TokenHash is sha256 of the token; the token itself is shown once.
	TokenHash string `json:"tokenHash"`
	Revoked   bool   `json:"revoked,omitempty"`
}

// Active reports whether the grant can be used now.
func (g Grant) Active(now time.Time) bool { return !g.Revoked && now.Before(g.ExpiresAt) }

// GrantStore holds agent grants in <data>/state/agents.json.
type GrantStore struct {
	mu     sync.Mutex
	path   string
	grants map[string]*Grant
	now    func() time.Time
}

// NewGrantStore loads the grants at path.
func NewGrantStore(path string) (*GrantStore, error) {
	gs := &GrantStore{path: path, grants: map[string]*Grant{}, now: time.Now}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return gs, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Grant
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, g := range list {
		gs.grants[g.ID] = g
	}
	return gs, nil
}

// Approve creates a new grant for an agent and returns the token. An older
// grant of the same agent is replaced.
func (gs *GrantStore) Approve(agentID, actsFor string, can []string, mode AgentMode, ttl time.Duration, by string) (string, Grant, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", Grant{}, err
	}
	token := "pk_agt_" + hex.EncodeToString(raw)
	if mode == "" {
		mode = ModeNavigate
	}
	now := gs.now().UTC()
	g := &Grant{
		ID: agentID, ActsFor: actsFor, Can: can, Mode: mode,
		ApprovedBy: by, ApprovedAt: now, ExpiresAt: now.Add(ttl),
		TokenHash: hashToken(token),
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	gs.grants[agentID] = g
	return token, *g, gs.save()
}

// Revoke ends the grant of an agent.
func (gs *GrantStore) Revoke(agentID string) (Grant, bool, error) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	g, ok := gs.grants[agentID]
	if !ok {
		return Grant{}, false, nil
	}
	g.Revoked = true
	return *g, true, gs.save()
}

// Verify returns the active grant of a token.
func (gs *GrantStore) Verify(token string) (Grant, bool) {
	h := hashToken(token)
	gs.mu.Lock()
	defer gs.mu.Unlock()
	for _, g := range gs.grants {
		if g.TokenHash == h && g.Active(gs.now()) {
			return *g, true
		}
	}
	return Grant{}, false
}

// Get returns the grant of an agent.
func (gs *GrantStore) Get(agentID string) (Grant, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	g, ok := gs.grants[agentID]
	if !ok {
		return Grant{}, false
	}
	return *g, true
}

// List returns all grants, sorted by id.
func (gs *GrantStore) List() []Grant {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	out := make([]Grant, 0, len(gs.grants))
	for _, g := range gs.grants {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Seen records the last use of a grant (tool may be empty). It is kept in
// memory; the file is written on approve and revoke only.
func (gs *GrantStore) Seen(agentID, tool string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if g, ok := gs.grants[agentID]; ok {
		g.LastSeenAt = gs.now().UTC()
		if tool != "" {
			g.LastTool = tool
		}
	}
}

// SetMode changes the steering mode of an agent.
func (gs *GrantStore) SetMode(agentID string, mode AgentMode) error {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if g, ok := gs.grants[agentID]; ok {
		g.Mode = mode
		return gs.save()
	}
	return nil
}

func (gs *GrantStore) save() error {
	list := make([]*Grant, 0, len(gs.grants))
	for _, g := range gs.grants {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := gs.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, gs.path)
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}
