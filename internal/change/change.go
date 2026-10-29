// Package change is the change model: one change is one worktree, one branch
// and one merge request. It holds ids, the metadata file, worktrees, state
// derivation, trailers, rebase and overlap.
package change

import (
	"context"
	"time"

	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// State of a change. It is derived from git, lab runs, the forge and Flux,
// never stored.
type State string

const (
	Draft    State = "draft"    // worktree or branch exists; no MR or a draft MR
	Lab      State = "lab"      // a lab run for the head is running or failed
	Review   State = "review"   // MR open and ready; lab gate met if needed
	Approved State = "approved" // approvals meet the policy
	Merged   State = "merged"   // merge commit with Pk-Change trailer on main
	Deployed State = "deployed" // Flux applied the merge commit (or later) on all impacted clusters
	Verified State = "verified" // health checks pass on all impacted clusters
	Closed   State = "closed"
)

// Lifecycle is the order of the states on the lifecycle track.
var Lifecycle = []State{Draft, Lab, Review, Approved, Merged, Deployed, Verified}

// Group of a change in the rail.
type Group string

const (
	GroupMine   Group = "mine"   // author is me or my agent
	GroupOthers Group = "others" // pushed by others
	GroupReview Group = "review" // ready, not mine, I have not approved
	GroupBots   Group = "bots"   // author is a bot
)

// LabResult of a lab run.
type LabResult string

const (
	LabRunning LabResult = "running"
	LabPass    LabResult = "pass"
	LabFail    LabResult = "fail"
	// LabCancelled: the run stopped before a result (bench restart).
	LabCancelled LabResult = "cancelled"
)

// Metadata file kind and directory.
const (
	MetaAPIVersion = "pk/v1alpha1"
	MetaKind       = "Change"
	MetaDir        = ".pk/changes"
)

// Meta is the metadata file .pk/changes/<id>.yaml in the branch. At "mark
// ready" it moves into the MR description as trailers and is deleted.
type Meta struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind" yaml:"kind"`
	ID         string      `json:"id" yaml:"id"`
	Title      string      `json:"title" yaml:"title"`
	Intent     string      `json:"intent,omitempty" yaml:"intent,omitempty"`
	Author     string      `json:"author" yaml:"author"`
	Agent      string      `json:"agent,omitempty" yaml:"agent,omitempty"`
	Target     string      `json:"target" yaml:"target"`
	Parent     string      `json:"parent,omitempty" yaml:"parent,omitempty"`
	WorkItems  []string    `json:"workItems,omitempty" yaml:"workItems,omitempty"`
	LabRuns    []LabRunRef `json:"labRuns,omitempty" yaml:"labRuns,omitempty"`
}

// MetaPath returns the metadata file path of a change.
func MetaPath(id string) string { return MetaDir + "/" + id + ".yaml" }

// LabRunRef is a lab run recorded on a change.
type LabRunRef struct {
	Run    string    `json:"run" yaml:"run"` // c-k7m2qz/run-1
	SHA    string    `json:"sha,omitempty" yaml:"sha,omitempty"`
	Result LabResult `json:"result" yaml:"result"`
	Report string    `json:"report,omitempty" yaml:"report,omitempty"`
}

// MR is the merge request of a change, forge-neutral.
type MR struct {
	Number int
	URL    string
	Draft  bool
	// State: open, merged, closed.
	State string
	Title string
	Body  string
}

// Rollout is the Flux state of a merged change on one cluster.
type Rollout struct {
	Cluster  string
	Ring     string
	Deployed bool
	Verified bool
	SHA      string
	At       time.Time
}

// Summary is one change in the rail.
type Summary struct {
	ID         string
	Title      string
	Intent     string
	Branch     string
	Target     string
	Parent     string
	Author     string
	Agent      string
	AuthorKind identity.Kind
	State      State
	// LabState: none, running, pass, fail (last run on the head).
	LabState string
	LabRun   string
	Group    Group
	Local    bool // a local worktree exists
	Dirty    bool // saved, uncommitted files
	Pushed   bool // the branch exists on the forge
	MR       *MR
	// Against the target branch and the remote branch.
	AheadTarget, BehindTarget int
	AheadRemote, BehindRemote int
	Files                     int
	Commits                   int
	WorkItems                 []string
	ApprovalsHave             int
	ApprovalsNeed             int
	Risk                      policy.RiskLevel
	Overlaps                  []string
	Rollout                   []Rollout
	CreatedAt, UpdatedAt      time.Time
	MergedAt                  time.Time
	MergeSHA                  string
}

// Commit is one commit of a change.
type Commit struct {
	SHA         string
	Subject     string
	Body        string
	Author      string // person, agent or bot id
	AuthorName  string
	AuthorEmail string
	Date        time.Time
	Agent       string
	OnBehalfOf  string
	Trailers    []gitx.Trailer
	Files       []string
	Pushed      bool
}

// FileStatus is a changed file of a change.
type FileStatus struct {
	Path    string
	OldPath string
	// Status: added, modified, deleted, renamed, untracked.
	Status    string
	Staged    bool
	Committed bool
}

// Change is one change with details.
type Change struct {
	Summary
	HeadSHA      string
	BaseSHA      string
	TargetSHA    string
	Worktree     string
	Meta         Meta
	Description  string
	Commits      []Commit
	LabRuns      []LabRunRef
	Approvals    []policy.Approval
	Gates        *policy.Gates
	Overlaps     []Overlap
	Children     []string
	ChangedFiles []FileStatus
}

// Overlap is another open change that touches the same rendered objects.
type Overlap struct {
	Change  string
	Title   string
	By      string
	State   State
	Objects []string // object keys
}

// CreateRequest creates a change.
type CreateRequest struct {
	Title     string
	Intent    string
	Target    string // default main
	Parent    string
	WorkItems []string
}

// PushResult of a push.
type PushResult struct {
	Change      Summary
	MR          *MR
	Created     bool
	RenamedFrom string
	Pushed      []string
}

// RebaseResult of a rebase.
type RebaseResult struct {
	OK        bool
	From, To  string
	Onto      string
	Conflicts []string
	Change    Summary
}

// MergeResult of a merge.
type MergeResult struct {
	SHA     string
	Message string
	MR      *MR
	Change  Summary
}

// Service runs the change lifecycle for the bench. All methods take the
// acting identity from ctx (identity.From) and check rights through the
// policy; they return errors from errors.go for the HTTP mapping.
type Service interface {
	// List returns open changes (and with all, recent merges and closed changes).
	List(ctx context.Context, all bool) ([]Summary, error)
	Get(ctx context.Context, id string) (*Change, error)
	Create(ctx context.Context, req CreateRequest) (*Change, error)
	// Checkout fetches the branch of a change into a local worktree.
	Checkout(ctx context.Context, id string) (*Summary, error)
	Close(ctx context.Context, id string) (*Summary, error)

	// Worktree returns the local worktree path of a change (ErrNoWorktree when none).
	Worktree(ctx context.Context, id string) (string, error)
	// MainDir returns the read-only worktree of origin/main.
	MainDir(ctx context.Context) (string, error)
	// Resolve returns a directory with the tree of ref (main, a change id,
	// a branch or a sha) and whether it is a live worktree.
	Resolve(ctx context.Context, ref string) (dir string, live bool, err error)

	Status(ctx context.Context, id string) ([]FileStatus, error)
	Commits(ctx context.Context, id string) ([]Commit, error)
	Commit(ctx context.Context, id, message string, paths []string) (*Commit, error)
	Push(ctx context.Context, id string) (*PushResult, error)
	MarkReady(ctx context.Context, id string) (*Change, error)
	Rebase(ctx context.Context, id string) (*RebaseResult, error)
	Approve(ctx context.Context, id, comment string) (*policy.Approval, error)
	Merge(ctx context.Context, id string) (*MergeResult, error)

	// RecordLabRun appends a lab run to the metadata file (or the MR
	// description after ready) and commits the report if given.
	RecordLabRun(ctx context.Context, id string, run LabRunRef, report []byte) error
	// Refresh fetches from the forge and publishes change events.
	Refresh(ctx context.Context) error

	// Gates evaluates the policy gates of a change (same evaluation as pk ci).
	Gates(ctx context.Context, id string) (*policy.Gates, error)
	// Heads lists the open changes with their heads (for overlap).
	Heads(ctx context.Context) ([]Head, error)
	// Sources returns the render sources of a change: base is the merge
	// base with the target, head the worktree (with saved files) or the
	// remote branch.
	Sources(ctx context.Context, id string) (base, head render.Source, err error)
	// Source returns the render source of a ref (main, a change id, a
	// branch or a sha).
	Source(ctx context.Context, ref string) (render.Source, error)
}

// Head is an open change with its head.
type Head struct {
	ID      string
	Title   string
	Author  string
	State   State
	Branch  string
	Target  string
	HeadSHA string
	// Worktree is the local worktree ("" when the change is only on the forge).
	Worktree string
	Dirty    bool
}
