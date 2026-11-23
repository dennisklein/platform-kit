package change

import (
	"context"
	"testing"
)

// One failed forge poll does not mark the forge offline; two in a row do,
// and one that works clears it again.
func TestForgeOfflineAfterTwoFailures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if h.svc.ForgeOffline() {
		t.Fatal("offline with a working forge")
	}

	h.forge.Unreachable = true
	if err := h.svc.Refresh(ctx); err == nil {
		t.Fatal("refresh worked with the forge down")
	}
	if h.svc.ForgeOffline() {
		t.Fatal("offline after one failed poll")
	}
	_ = h.svc.Refresh(ctx)
	if !h.svc.ForgeOffline() {
		t.Fatal("not offline after two failed polls in a row")
	}

	h.forge.Unreachable = false
	if err := h.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if h.svc.ForgeOffline() {
		t.Fatal("still offline after a poll that worked")
	}

	// A single failure between two good polls never shows.
	h.forge.Unreachable = true
	_ = h.svc.Refresh(ctx)
	h.forge.Unreachable = false
	if err := h.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if h.svc.ForgeOffline() {
		t.Fatal("offline after a single blip")
	}
}
