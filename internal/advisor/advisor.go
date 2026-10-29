// Package advisor holds the advisor recipes: fixed questions with one click,
// no freeform input. Each recipe is Go code (prompt, tools, output form). It
// first computes structured findings deterministically from tools, then asks
// the model to write points with sources (real LLM) or renders the findings
// directly (mock).
package advisor

import (
	"context"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// Recipe ids.
const (
	Risks         = "risks"
	MergeOrder    = "merge-order"
	LabGaps       = "lab-gaps"
	ExplainDiff   = "explain-diff"
	CheckCommit   = "check-commit"
	CommitMessage = "commit-message"
)

// Output form of a recipe.
type Output string

const (
	OutputPoints     Output = "points"
	OutputMessage    Output = "message"
	OutputSuggestion Output = "suggestion"
)

// Recipe is one advisor.
type Recipe struct {
	ID       string
	Title    string
	Question string
	// Tools the recipe reads (impact, policy, lab, history, mr, search).
	Tools  []string
	Output Output
	// Scope: change or commit.
	Scope string
}

// Severity of a point.
type Severity string

const (
	SevInfo Severity = "info"
	SevWarn Severity = "warn"
	SevRisk Severity = "risk"
	SevOK   Severity = "ok"
)

// Point is one finding with sources.
type Point struct {
	Title    string
	Text     string
	Severity Severity
	Sources  []llm.Source
}

// Suggestion is a suggested edit (the admin accepts or rejects it).
type Suggestion struct {
	ID       string
	ChangeID string
	Path     string
	Title    string
	Before   string
	After    string
	Diff     string
	// Status: pending, accepted, rejected, stale.
	Status    string
	By        string
	LogID     string
	CreatedAt time.Time
	DecidedBy string
	DecidedAt time.Time
}

// Suggestion states.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusRejected = "rejected"
	StatusStale    = "stale"
)

// Result of a recipe run.
type Result struct {
	Recipe      string
	Title       string
	ChangeID    string
	Points      []Point
	Note        string
	Reading     []llm.Source
	Suggestions []Suggestion
	Model       string
	Mock        bool
	// Fallback: the answer comes from the bench data, not from a model (no
	// LLM endpoint, or the model call failed or gave no usable answer).
	Fallback  bool
	LogID     string
	CreatedAt time.Time
}

// CommitMessageResult is a suggested commit message.
type CommitMessageResult struct {
	Message string
	Subject string
	Body    string
	Model   string
	Mock    bool
	// Fallback: the answer comes from the bench data, not from a model (no
	// LLM endpoint, or the model call failed or gave no usable answer).
	Fallback bool
	LogID    string
	Sources  []llm.Source
}

// Runner runs recipes as the identity in ctx.
type Runner interface {
	Recipes() []Recipe
	Run(ctx context.Context, recipe, changeID string) (*Result, error)
	// CommitMessage suggests a Conventional Commit message from the
	// uncommitted diff (or paths) and the change intent.
	CommitMessage(ctx context.Context, changeID string, paths []string) (*CommitMessageResult, error)
}

// Tools is what recipes read. The bench implementation (APITools) calls the
// bench API in-process as the caller, so secrets arrive masked and the
// caller's rights apply. Tests use fakes.
type Tools interface {
	Change(ctx context.Context, id string) (*api.Change, error)
	Changes(ctx context.Context) ([]api.ChangeSummary, error)
	Impact(ctx context.Context, id string) (*api.Impact, error)
	// Diff returns the source diff of a change against its base.
	Diff(ctx context.Context, id string) (*api.Diff, error)
	Status(ctx context.Context, id string) (*api.WorktreeStatus, error)
	Problems(ctx context.Context, id string) ([]api.Problem, error)
	// Tree lists files of a change worktree (ref is a change id) or of a
	// ref (main, sha).
	Tree(ctx context.Context, ref string) ([]api.FileNode, error)
	// ReadFile reads a file of a change worktree (ref is a change id) or at
	// a ref (main, sha, branch).
	ReadFile(ctx context.Context, ref, path string) (*api.FileContent, error)
	WriteFile(ctx context.Context, changeID, path, content string) (*api.WriteFileResult, error)
	History(ctx context.Context, p api.ListHistoryParams) ([]api.HistoryEntry, error)
	HistoryEntry(ctx context.Context, sha string) (*api.HistoryDetail, error)
	// LabRuns lists runs of a change, newest first.
	LabRuns(ctx context.Context, changeID string) ([]api.LabRun, error)
	Entity(ctx context.Context, kind, name, ref string) (*api.Entity, error)
	Search(ctx context.Context, q string, limit int) ([]llm.Hit, error)
}

// SuggestionStore keeps suggestions (the chat service implements it).
type SuggestionStore interface {
	AddSuggestion(ctx context.Context, s Suggestion) (*Suggestion, error)
}
