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
