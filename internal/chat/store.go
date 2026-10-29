package chat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/inproc"
	"github.com/dennisklein/platform-kit/internal/render"
)

// store keeps suggestions in memory and in a JSON file.
type store struct {
	mu   sync.Mutex
	path string
	m    map[string]*advisor.Suggestion
}

func newStore(path string) (*store, error) {
	st := &store{path: path, m: map[string]*advisor.Suggestion{}}
	if path == "" {
		return st, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*advisor.Suggestion
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range list {
		st.m[s.ID] = s
	}
	return st, nil
}

func (st *store) save() error {
	if st.path == "" {
		return nil
	}
	list := make([]*advisor.Suggestion, 0, len(st.m))
	for _, s := range st.m {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

func newSuggestionID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "s-" + string(b)
}

// Suggestions lists the suggestions of a change, newest first.
func (s *Svc) Suggestions(ctx context.Context, changeID string) ([]advisor.Suggestion, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	out := []advisor.Suggestion{}
	for _, sg := range s.st.m {
		if changeID == "" || sg.ChangeID == changeID {
			out = append(out, *sg)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Get returns one suggestion.
func (s *Svc) Get(ctx context.Context, id string) (*advisor.Suggestion, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	sg, ok := s.st.m[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	c := *sg
	return &c, nil
}

// AddSuggestion stores a pending suggestion. The same pending edit of the
// same file is kept once.
func (s *Svc) AddSuggestion(ctx context.Context, sg advisor.Suggestion) (*advisor.Suggestion, error) {
	if sg.ChangeID == "" || sg.Path == "" {
		return nil, errors.New("a suggestion needs a change and a path")
	}
	if sg.Diff == "" {
		sg.Diff = render.Unified(sg.Path, sg.Before, sg.After)
	}
	s.st.mu.Lock()
	for _, old := range s.st.m {
		if old.Status == advisor.StatusPending && old.ChangeID == sg.ChangeID && old.Path == sg.Path && old.After == sg.After && old.Before == sg.Before {
			c := *old
			s.st.mu.Unlock()
			return &c, nil
		}
	}
	sg.ID = newSuggestionID()
	sg.Status = advisor.StatusPending
	sg.CreatedAt = s.now().UTC()
	c := sg
	s.st.m[sg.ID] = &c
	err := s.st.save()
	s.st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.publish(ctx, events.SuggestionCreated, ToAPI(sg))
	return &sg, nil
}

// Accept writes the suggested content to the worktree as an edit of the
// caller, through the files API (masking, render and events as for a hand
// edit).
func (s *Svc) Accept(ctx context.Context, id string) (*advisor.Suggestion, error) {
	sg, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sg.Status != advisor.StatusPending {
		return nil, fmt.Errorf("%w (%s)", ErrDecided, sg.Status)
	}
	cur := ""
	fc, err := s.o.Tools.ReadFile(ctx, sg.ChangeID, sg.Path)
	switch {
	case err == nil:
		cur = fc.Content
		if !fc.Exists {
			cur = ""
		}
	case inproc.StatusOf(err) == 404 && sg.Before == "":
	default:
		return nil, err
	}
	if cur != sg.Before {
		s.decide(ctx, id, advisor.StatusStale)
		return nil, ErrStale
	}
	if _, err := s.o.Tools.WriteFile(ctx, sg.ChangeID, sg.Path, sg.After); err != nil {
		return nil, err
	}
	return s.decide(ctx, id, advisor.StatusAccepted)
}

// Reject marks the suggestion rejected.
func (s *Svc) Reject(ctx context.Context, id string) (*advisor.Suggestion, error) {
	sg, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sg.Status != advisor.StatusPending && sg.Status != advisor.StatusStale {
		return nil, fmt.Errorf("%w (%s)", ErrDecided, sg.Status)
	}
	return s.decide(ctx, id, advisor.StatusRejected)
}

func (s *Svc) decide(ctx context.Context, id, status string) (*advisor.Suggestion, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	sg, ok := s.st.m[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	sg.Status = status
	sg.DecidedBy = identity.MustFrom(ctx).User
	sg.DecidedAt = s.now().UTC()
	c := *sg
	return &c, s.st.save()
}

func (s *Svc) publish(ctx context.Context, t events.Type, data any) {
	if s.o.Events == nil {
		return
	}
	if id, ok := identity.From(ctx); ok {
		s.o.Events.PublishTo(id.Principal(), t, data)
		return
	}
	s.o.Events.Publish(t, data)
}

// ToAPI converts a suggestion.
func ToAPI(s advisor.Suggestion) api.Suggestion {
	return api.Suggestion{
		Id: s.ID, ChangeId: s.ChangeID, Path: s.Path, Title: s.Title, Before: s.Before, After: s.After,
		Diff: s.Diff, Status: api.SuggestionStatus(s.Status), By: s.By, LogId: s.LogID,
		CreatedAt: s.CreatedAt, DecidedBy: s.DecidedBy, DecidedAt: s.DecidedAt,
	}
}
