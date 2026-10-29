package events

import "testing"

func TestHubAudienceAndReplay(t *testing.T) {
	h := NewHub()
	h.Publish(ChangeCreated, "a")
	h.PublishTo("anna", FocusChanged, "b")
	h.PublishTo("dennis", FocusChanged, "c")

	ch, cancel := h.Subscribe("dennis", 1)
	defer cancel()
	ev := <-ch
	if ev.ID != 3 || ev.Data != "c" {
		t.Fatalf("replay: got %+v", ev)
	}
	h.PublishTo("anna", Presence, "x")
	h.Publish(LabStep, "d")
	ev = <-ch
	if ev.Type != LabStep {
		t.Fatalf("live: got %+v", ev)
	}
}

func TestHubDropsSlowSubscriber(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe("dennis", 0)
	defer cancel()
	for i := 0; i < chanSize+1; i++ {
		h.Publish(LabStep, i)
	}
	n := 0
	for range ch {
		n++
	}
	if n != chanSize {
		t.Fatalf("got %d events before close, want %d", n, chanSize)
	}
}

func TestHubKeepsTransientEventsOutOfReplay(t *testing.T) {
	h := NewHub()
	live, cancel := h.Subscribe("dennis", 0)
	defer cancel()
	h.PublishTo("dennis", ChatDelta, "a")
	h.PublishTo("dennis", ChatStep, "b")
	h.Publish(ChangeCreated, "c")
	for _, want := range []Type{ChatDelta, ChatStep, ChangeCreated} {
		if ev := <-live; ev.Type != want {
			t.Fatalf("live: got %s, want %s", ev.Type, want)
		}
	}
	replay, cancel2 := h.Subscribe("dennis", 1)
	defer cancel2()
	if ev := <-replay; ev.Type != ChangeCreated || ev.ID != 3 {
		t.Fatalf("replay: got %+v, want only change.created", ev)
	}
}
