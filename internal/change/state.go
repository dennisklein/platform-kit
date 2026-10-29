package change

import (
	"path"
	"strings"

	"github.com/dennisklein/platform-kit/internal/identity"
)

// StateInput is what the state of a change is derived from.
type StateInput struct {
	Merged bool // merge commit with Pk-Change on main, or the MR is merged
	Closed bool // the MR is closed without merge
	HasMR  bool
	Draft  bool // the MR is a draft
	// Lab is the lab state of the head: none, running, pass, fail.
	Lab           string
	ApprovalsHave int
	ApprovalsNeed int
	// Rollout is the Flux state per impacted cluster (merged changes).
	Rollout []Rollout
}

// DeriveState derives the state. It is never stored.
func DeriveState(in StateInput) State {
	switch {
	case in.Merged:
		if len(in.Rollout) == 0 {
			return Merged
		}
		deployed, verified := true, true
		for _, r := range in.Rollout {
			deployed = deployed && r.Deployed
			verified = verified && r.Verified
		}
		switch {
		case verified && deployed:
			return Verified
		case deployed:
			return Deployed
		}
		return Merged
	case in.Closed:
		return Closed
	case in.Lab == "running" || in.Lab == "fail":
		return Lab
	case in.HasMR && !in.Draft:
		if in.ApprovalsNeed > 0 && in.ApprovalsHave >= in.ApprovalsNeed {
			return Approved
		}
		return Review
	}
	return Draft
}

// Open reports whether the state is an open change.
func (s State) Open() bool {
	switch s {
	case Merged, Deployed, Verified, Closed:
		return false
	}
	return true
}

// LabCoversHead reports whether a lab run on a tested sha still counts for
// the head: the only paths that differ are change metadata and the run's
// own reports (kb/labs/<id>-run-<n>.md).
func LabCoversHead(id string, changed []string) bool {
	for _, p := range changed {
		if strings.HasPrefix(p, MetaDir+"/") {
			continue
		}
		dir, file := path.Split(p)
		if dir == "kb/labs/" && strings.HasPrefix(file, id+"-run-") && strings.HasSuffix(file, ".md") {
			continue
		}
		return false
	}
	return true
}

// RecordOnly reports whether a commit's files are only records of the
// change: its metadata and its own lab reports. Such commits do not make
// the committer a writer of the change.
func RecordOnly(id string, files []string) bool {
	return len(files) > 0 && LabCoversHead(id, files)
}

// GroupInput is what the rail group is derived from.
type GroupInput struct {
	Me         string // the person who looks
	Author     string
	AuthorKind identity.Kind
	// AuthorActsFor is the person behind an agent author.
	AuthorActsFor string
	Ready         bool // MR open and ready
	ApprovedByMe  bool
	// CanApprove: me is an eligible approver.
	CanApprove bool
}

// GroupOf returns the rail group.
func GroupOf(in GroupInput) Group {
	switch {
	case in.AuthorKind == identity.Bot:
		return GroupBots
	case in.Author == in.Me, in.AuthorActsFor != "" && in.AuthorActsFor == in.Me:
		return GroupMine
	case in.Ready && !in.ApprovedByMe && in.CanApprove:
		return GroupReview
	}
	return GroupOthers
}
