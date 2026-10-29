package events

import "testing"

func TestHubRecallsToolCallsForNewSubscribers(t *testing.T) {
	h := NewHub()
	for i := 0; i < recallSize+3; i++ {
		PublishRecalled(h, "dennis", Presence, i)
	}
	PublishRecalled(h, "anna", Presence, "anna's agent")
	h.Publish(ChangeCreated, "not recalled")

	ch, cancel := h.Subscribe("dennis", 0)
	defer cancel()
	newest := h.LastID()
	// The recall keeps 20 events of all people: anna's call took one slot.
	for i := 4; i < recallSize+3; i++ {
		ev := <-ch
		if ev.Type != Presence || ev.Data != i || !ev.Replay || ev.ID != newest {
			t.Fatalf("recall %d: got %+v", i, ev)
		}
	}
	h.Publish(LabStep, "live")
	if ev := <-ch; ev.Type != LabStep || ev.Replay {
		t.Fatalf("live: got %+v", ev)
	}

	// A reconnect (Last-Event-ID) gets the history, not the recall.
	re, cancel2 := h.Subscribe("dennis", newest)
	defer cancel2()
	if ev := <-re; ev.Type != LabStep || ev.Replay {
		t.Fatalf("reconnect: got %+v", ev)
	}
}

type plain struct {
	Nop
	got []Type
}

func (p *plain) PublishTo(_ string, t Type, _ any) { p.got = append(p.got, t) }

func TestPublishRecalledFallsBack(t *testing.T) {
	p := &plain{}
	PublishRecalled(p, "dennis", Presence, "x")
	if len(p.got) != 1 || p.got[0] != Presence {
		t.Fatalf("got %v", p.got)
	}
}
