// Package forge is the thin, forge-neutral interface to the git forge: pull
// requests, reviews, comments, commit statuses, merges and branches. The
// gitea adapter is in forge/gitea; an in-memory fake for tests in forge/fake.
// No forge type leaks into the core.
package forge

import (
	"context"
	"errors"
	"time"
)

// Errors of adapters.
var (
	ErrNotFound    = errors.New("forge: not found")
	ErrForbidden   = errors.New("forge: forbidden")
	ErrConflict    = errors.New("forge: conflict")
	ErrUnreachable = errors.New("forge: unreachable")
)

// PRState of a pull request.
type PRState string

const (
	PROpen   PRState = "open"
	PRClosed PRState = "closed"
	PRMerged PRState = "merged"
	PRAll    PRState = "all"
)

// DraftPrefix marks a draft pull request in the title (Gitea convention).
const DraftPrefix = "WIP: "

// PR is a pull request (merge request).
type PR struct {
	Number    int
	Title     string // without DraftPrefix
	Body      string
	Draft     bool
	State     PRState
	Head      string // branch
	HeadSHA   string
	Base      string
	Author    string
	URL       string
	Mergeable bool
	MergedSHA string
	MergedBy  string
	CreatedAt time.Time
	UpdatedAt time.Time
	MergedAt  time.Time
}

// PRCreate creates a pull request.
type PRCreate struct {
	Title string
	Body  string
	Head  string
	Base  string
	Draft bool
}

// PRUpdate changes a pull request; nil fields stay.
type PRUpdate struct {
	Title *string
	Body  *string
	Draft *bool
	Base  *string
	State *PRState // open or closed
}

// ReviewState of a review.
type ReviewState string

const (
	ReviewApproved         ReviewState = "approved"
	ReviewChangesRequested ReviewState = "changes_requested"
	ReviewComment          ReviewState = "comment"
	ReviewDismissed        ReviewState = "dismissed"
)

// Review on a pull request.
type Review struct {
	ID    int64
	User  string
	State ReviewState
	SHA   string // commit the review was made on
	Body  string
	At    time.Time
	Stale bool // made on an older head
}

// Comment on a pull request (discussion).
type Comment struct {
	ID   int64
	User string
	Body string
	At   time.Time
	Path string // review comments only
	Line int
}

// StatusState of a commit status.
type StatusState string

const (
	StatusPending StatusState = "pending"
	StatusSuccess StatusState = "success"
	StatusFailure StatusState = "failure"
	StatusError   StatusState = "error"
)

// Commit status contexts used by platform-kit.
const (
	ContextPolicy = "pk/policy"
	ContextLab    = "pk/lab"
)

// CommitStatus is a status on a commit.
type CommitStatus struct {
	Context     string
	State       StatusState
	Description string
	TargetURL   string
	Creator     string
	At          time.Time
}

// MergeOptions of a merge. Style is always a merge commit.
type MergeOptions struct {
	Title   string
	Message string
	// HeadSHA must match the current head (protects against a race).
	HeadSHA      string
	DeleteBranch bool
}

// MergeResult of a merge.
type MergeResult struct {
	SHA string
}

// Branch on the forge.
type Branch struct {
	Name string
	SHA  string
}

// Repo is the state repo on the forge.
type Repo struct {
	Owner         string
	Name          string
	CloneURL      string
	HTMLURL       string
	DefaultBranch string
}

// Forge is the interface the bench, pk ci and the seed builder use. "as" is
// the forge user that acts (dennis, anna, dennis-agent, sync-bot, pk-ci);
// the adapter holds a token per user.
type Forge interface {
	// Name is the display name (Gitea).
	Name() string
	// Ping checks that the forge is reachable.
	Ping(ctx context.Context) error
	Repo() Repo
	// WebURL is the URL of a pull request.
	WebURL(number int) string

	ListPRs(ctx context.Context, state PRState) ([]PR, error)
	GetPR(ctx context.Context, number int) (*PR, error)
	// FindPR returns the newest PR with head branch (any state), or ErrNotFound.
	FindPR(ctx context.Context, branch string) (*PR, error)
	CreatePR(ctx context.Context, as string, req PRCreate) (*PR, error)
	UpdatePR(ctx context.Context, as string, number int, req PRUpdate) (*PR, error)

	ListReviews(ctx context.Context, number int) ([]Review, error)
	Approve(ctx context.Context, as string, number int, body string) (*Review, error)
	ListComments(ctx context.Context, number int) ([]Comment, error)
	Comment(ctx context.Context, as string, number int, body string) (*Comment, error)

	SetStatus(ctx context.Context, as, sha string, st CommitStatus) error
	// Statuses returns the newest status per context.
	Statuses(ctx context.Context, sha string) ([]CommitStatus, error)

	Merge(ctx context.Context, as string, number int, opts MergeOptions) (*MergeResult, error)
	ListBranches(ctx context.Context, prefix string) ([]Branch, error)
	DeleteBranch(ctx context.Context, as, branch string) error
}

// User is a forge account.
type User struct {
	Login    string
	FullName string
	Email    string
	Password string
	Admin    bool
	// Bot marks non-human accounts (agents, bots, CI).
	Bot bool
}

// BranchProtection on the forge (generated from the policy).
type BranchProtection struct {
	Branch             string
	EnablePush         bool
	RequiredChecks     []string
	MergeWhitelist     []string
	ApprovalsWhitelist []string
	RequiredApprovals  int
	BlockOnOutdated    bool
	DismissStale       bool
}

// Admin configures the forge (pk forge-init, pk policy sync).
type Admin interface {
	EnsureUser(ctx context.Context, u User) error
	// CreateToken creates an access token for a user (with its password).
	CreateToken(ctx context.Context, user, password, name string) (string, error)
	EnsureOrg(ctx context.Context, org string) error
	EnsureRepo(ctx context.Context, owner, name string, private bool) (*Repo, error)
	DeleteRepo(ctx context.Context, owner, name string) error
	// AddCollaborator grants read, write or admin on the repo.
	AddCollaborator(ctx context.Context, user, permission string) error
	SetBranchProtection(ctx context.Context, bp BranchProtection) error
	GetBranchProtection(ctx context.Context, branch string) (*BranchProtection, error)
}
