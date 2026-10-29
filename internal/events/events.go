// Package events is the in-process event bus. Services publish typed events;
// the bench streams them to browsers and clients over SSE.
package events

import (
	"sync"
	"time"
)

// Type is an event type. The payload of each type is listed in the OpenAPI
// schema EventType.
type Type string

const (
	ChangeCreated     Type = "change.created"     // data: api.ChangeEvent
	ChangeUpdated     Type = "change.updated"     // data: api.ChangeEvent
	LabStep           Type = "lab.step"           // data: api.LabStepEvent
	LabFinished       Type = "lab.finished"       // data: api.LabRun
	RenderUpdated     Type = "render.updated"     // data: api.RenderEvent
	FocusChanged      Type = "focus.changed"      // data: api.Focus
	ForgeUpdated      Type = "forge.updated"      // data: api.ForgeEvent
	ForgeReset        Type = "forge.reset"        // data: change.ForgeResetEvent (API schema ForgeResetEvent)
	FluxUpdated       Type = "flux.updated"       // data: api.Cluster
	Presence          Type = "presence"           // data: api.Presence
	SuggestionCreated Type = "suggestion.created" // data: api.Suggestion
	// ChatStep and ChatDelta are the live progress of one chat answer, for
	// the person who asked (PublishTo).
	ChatStep  Type = "chat.step"  // data: api.ChatStepEvent
	ChatDelta Type = "chat.delta" // data: api.ChatDeltaEvent
)

// Transient reports event types that only matter while they happen: the hub
// sends them live but keeps them out of the replay history, so a streamed
// answer does not push the state events out of it.
func (t Type) Transient() bool { return t == ChatStep || t == ChatDelta }

// Event is one published event.
type Event struct {
	ID   uint64    `json:"id"`
	Type Type      `json:"type"`
	Time time.Time `json:"time"`
	// Audience is the person the event is for; empty means everyone.
	Audience string `json:"-"`
	Data     any    `json:"data"`
}

// Publisher publishes events.
type Publisher interface {
	// Publish sends an event to everyone.
	Publish(t Type, data any)
	// PublishTo sends an event only to the sessions of one person (and the agents that act for them).
	PublishTo(audience string, t Type, data any)
}

// Bus is a Publisher with subscriptions.
type Bus interface {
	Publisher
	// Subscribe returns events for principal after lastID (replayed from a
	// short history) and then live events. Call cancel to unsubscribe. The
	// channel is closed when the subscriber is too slow; the client then
	// reconnects with Last-Event-ID.
	Subscribe(principal string, lastID uint64) (ch <-chan Event, cancel func())
}

// Nop is a Publisher that drops all events.
type Nop struct{}

func (Nop) Publish(Type, any)           {}
func (Nop) PublishTo(string, Type, any) {}

const (
	historySize = 512
	chanSize    = 256
)

// Hub is the in-memory Bus.
type Hub struct {
	mu      sync.Mutex
	nextID  uint64
	history []Event
	subs    map[*sub]struct{}
	now     func() time.Time
}

type sub struct {
	principal string
	ch        chan Event
	closed    bool
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[*sub]struct{}{}, now: time.Now}
}

func (h *Hub) Publish(t Type, data any) { h.PublishTo("", t, data) }

func (h *Hub) PublishTo(audience string, t Type, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	ev := Event{ID: h.nextID, Type: t, Time: h.now().UTC(), Audience: audience, Data: data}
	if !t.Transient() {
		h.history = append(h.history, ev)
		if len(h.history) > historySize {
			h.history = h.history[len(h.history)-historySize:]
		}
	}
	for s := range h.subs {
		if !visible(ev, s.principal) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			h.drop(s)
		}
	}
}

func (h *Hub) Subscribe(principal string, lastID uint64) (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &sub{principal: principal, ch: make(chan Event, chanSize)}
	if lastID > 0 {
		for _, ev := range h.history {
			if ev.ID > lastID && visible(ev, principal) && len(s.ch) < chanSize {
				s.ch <- ev
			}
		}
	}
	h.subs[s] = struct{}{}
	return s.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.drop(s)
	}
}

// LastID returns the id of the newest event.
func (h *Hub) LastID() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextID
}

func (h *Hub) drop(s *sub) {
	if s.closed {
		return
	}
	s.closed = true
	delete(h.subs, s)
	close(s.ch)
}

func visible(ev Event, principal string) bool {
	return ev.Audience == "" || ev.Audience == principal
}
