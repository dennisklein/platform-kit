package bench

import (
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
)

// FocusStore holds the server-side focus of each person: current change,
// file, object and view. Browser tabs, chat and agents read and move it;
// every change goes out as focus.changed to that person's sessions.
type FocusStore struct {
	mu  sync.Mutex
	m   map[string]api.Focus
	pub events.Publisher
	now func() time.Time
}

// NewFocusStore returns an empty store.
func NewFocusStore(pub events.Publisher) *FocusStore {
	return &FocusStore{m: map[string]api.Focus{}, pub: pub, now: time.Now}
}

// Get returns the focus of a person.
func (f *FocusStore) Get(principal string) api.Focus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fo, ok := f.m[principal]; ok {
		return fo
	}
	return api.Focus{By: principal, At: f.now().UTC()}
}

// Set replaces the focus of a person. by is the identity that moves it.
func (f *FocusStore) Set(principal, by string, u api.FocusUpdate) api.Focus {
	fo := api.Focus{
		ChangeId: u.ChangeId, Path: u.Path, Line: u.Line, Object: u.Object,
		View: u.View, Route: u.Route, Pinned: u.Pinned,
		By: by, At: f.now().UTC(),
	}
	f.mu.Lock()
	f.m[principal] = fo
	f.mu.Unlock()
	f.pub.PublishTo(principal, events.FocusChanged, fo)
	return fo
}
