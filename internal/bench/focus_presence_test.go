package bench

import (
	"context"
	"testing"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
)

// presenceOf collects the presence events that reach dennis within a moment.
func presenceOf(t *testing.T, s *Server, fn func()) []api.Presence {
	t.Helper()
	ch, cancel := s.hub.Subscribe("dennis", 0)
	defer cancel()
	fn()
	var out []api.Presence
	timeout := time.After(150 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if p, ok := ev.Data.(api.Presence); ok && ev.Type == events.Presence {
				out = append(out, p)
			}
		case <-timeout:
			return out
		}
	}
}

func TestSetFocusPresenceOncePerToolCall(t *testing.T) {
	s, _ := newTestServer(t, true)
	agent := identity.Identity{User: "dennis-agent", Kind: identity.Agent, ActsFor: "dennis"}
	upd := &api.FocusUpdate{ChangeId: "c-k7m2qz", Path: "control/inventory/cn-035.yaml", View: api.FocusViewEditor}
	set := func(ctx context.Context) {
		if _, err := s.SetFocus(identity.With(ctx, agent), api.SetFocusRequestObject{Body: upd}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.grants.Approve("dennis-agent", "dennis", []string{"push"}, ModeNavigate, time.Hour, "dennis"); err != nil {
		t.Fatal(err)
	}

	// Direct API call (CLI with an agent token): the bench tells the person.
	if got := presenceOf(t, s, func() { set(context.Background()) }); len(got) != 1 {
		t.Fatalf("direct call: %d presence events %+v", len(got), got)
	}
	// Navigate mode through MCP: the MCP layer sends the one event.
	ctx, tc := events.WithToolCall(context.Background(), "pk_file_write")
	if got := presenceOf(t, s, func() { set(ctx) }); len(got) != 0 || tc.Sent {
		t.Fatalf("mcp navigate: %d presence events, sent %v", len(got), tc.Sent)
	}

	// Suggest mode through MCP: the "look here" card is the call's event.
	if _, _, err := s.grants.Approve("dennis-agent", "dennis", []string{"push"}, ModeSuggest, time.Hour, "dennis"); err != nil {
		t.Fatal(err)
	}
	ctx, tc = events.WithToolCall(context.Background(), "pk_focus")
	got := presenceOf(t, s, func() { set(ctx) })
	if len(got) != 1 || !tc.Sent || got[0].Tool != "pk_focus" || got[0].Suggest.Path != upd.Path ||
		got[0].Text != "pk_focus control/inventory/cn-035.yaml (look here)" {
		t.Fatalf("mcp suggest: sent %v, %+v", tc.Sent, got)
	}
}
