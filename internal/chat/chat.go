// Package chat is the freeform chat as the logged-in admin. With a real LLM
// it runs a tool loop with read-only tools (search, read file, history,
// entity, impact) plus suggest_edit, which creates a Suggestion in the open
// change and never commits. The mock answers known intents from data.
package chat

import (
	"context"
	"errors"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// Message is one chat turn.
type Message struct {
	// Role: user or assistant.
	Role    string
	Content string
}

// Request is a chat request.
type Request struct {
	ChangeID string
	Path     string
	Messages []Message
	// RequestID is the client id of the question. With it, the progress of
	// the answer (steps and text) goes live to the asking person as
	// chat.step and chat.delta events.
	RequestID string
}

// Step is one lookup an answer made, for example "history cn-003" with
// the summary "4 merges".
type Step struct {
	ID      int
	Tool    string
	Label   string
	Summary string
	// State: running, ok or failed.
	State string
	Ms    int64
}

// Answer is a chat answer.
type Answer struct {
	Text        string
	Sources     []llm.Source
	Suggestions []advisor.Suggestion
	Refused     bool
	Model       string
	Mock        bool
	// Fallback: the answer comes from the bench data, not from a model (no
	// LLM endpoint, or the model call failed); false for a refusal).
	Fallback bool
	// FallbackReason says why the data answered although a model is set,
	// for example "the model took longer than 25 s".
	FallbackReason string
	// Steps are the lookups of the answer, in order.
	Steps []Step
	LogID string
}

// Service answers chat and manages suggestions (from chat and advisors).
type Service interface {
	Ask(ctx context.Context, req Request) (*Answer, error)
	Suggestions(ctx context.Context, changeID string) ([]advisor.Suggestion, error)
	// Get returns one suggestion.
	Get(ctx context.Context, id string) (*advisor.Suggestion, error)
	// AddSuggestion stores a suggestion (advisors use it) and publishes
	// suggestion.created.
	AddSuggestion(ctx context.Context, s advisor.Suggestion) (*advisor.Suggestion, error)
	// Accept writes the suggestion to the worktree as a normal edit of the
	// caller. Stale when the file changed since.
	Accept(ctx context.Context, id string) (*advisor.Suggestion, error)
	Reject(ctx context.Context, id string) (*advisor.Suggestion, error)
}

// Errors of the suggestion store.
var (
	ErrNotFound = errors.New("suggestion not found")
	// ErrStale: the file changed since the suggestion was made.
	ErrStale = errors.New("the file changed since the suggestion was made; ask again")
	// ErrDecided: the suggestion was accepted or rejected already.
	ErrDecided = errors.New("the suggestion is decided already")
	// ErrNoQuestion: the request has no user message.
	ErrNoQuestion = errors.New("no question: the last message must be from the user")
)
