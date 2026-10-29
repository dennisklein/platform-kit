package change

import (
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
)

// ChangeEvent is the payload of change.created and change.updated (API
// schema ChangeEvent; the generator skips schemas no operation uses).
type ChangeEvent struct {
	ChangeID string             `json:"changeId"`
	Reason   string             `json:"reason,omitempty"`
	Change   *api.ChangeSummary `json:"change,omitempty"`
}

// ForgeEvent is the payload of forge.updated (API schema ForgeEvent).
type ForgeEvent struct {
	What     string               `json:"what"`
	ChangeID string               `json:"changeId,omitempty"`
	MR       *api.MergeRequestRef `json:"mr,omitempty"`
	SHA      string               `json:"sha,omitempty"`
}

// ForgeResetEvent is the payload of forge.reset (API schema
// ForgeResetEvent): the forge state was rebuilt and the local worktrees and
// change branches opened before Since were dropped. Clients reload.
type ForgeResetEvent struct {
	Since time.Time `json:"since"`
	Main  string    `json:"main,omitempty"`
}

func changeEvent(sum Summary, reason string) ChangeEvent {
	ev := ChangeEvent{ChangeID: sum.ID, Reason: reason}
	if sum.ID != "" {
		a := SummaryToAPI(sum)
		ev.Change = &a
	}
	return ev
}

func forgeEvent(what, id string, mr *MR, sha string) ForgeEvent {
	ev := ForgeEvent{What: what, ChangeID: id, SHA: sha}
	if mr != nil {
		a := MRToAPI(mr)
		ev.MR = &a
	}
	return ev
}
