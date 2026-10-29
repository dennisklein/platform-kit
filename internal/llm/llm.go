// Package llm is the LLM layer of the bench: an OpenAI-compatible client and
// a deterministic mock, the redaction filter that runs before every call, the
// append-only LLM log and the retrieval index.
package llm

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// Role of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one chat message (OpenAI chat completions format).
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call the model asks for.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Tool is a function the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// Request is a completion request.
type Request struct {
	Messages    []Message
	Tools       []Tool
	Temperature float64
	MaxTokens   int
	// JSON asks for a JSON object answer.
	JSON bool
}

// Usage is token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Response is a completion.
type Response struct {
	Message Message
	Model   string
	Usage   Usage
}

// Client is a chat completion and embedding client. Every implementation
// calls Redact on all message contents before sending.
type Client interface {
	// Model is the model name ("mock model" in mock mode).
	Model() string
	// Mock reports the deterministic mock mode (no PK_LLM_BASE_URL).
	Mock() bool
	Complete(ctx context.Context, req Request) (*Response, error)
	// Embed returns embeddings, or ErrNoEmbeddings without PK_LLM_EMBED_MODEL.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Ping checks the endpoint.
	Ping(ctx context.Context) error
}

// Redactor removes secrets before a call: SOPS ENC[...] values, schema
// x-pk-secret fields, sops metadata blocks, PEM private keys, AGE-SECRET-KEY-
// and bearer/API token patterns. IPs and serial numbers stay.
type Redactor interface {
	// Redact returns the redacted text and the number of removed values.
	Redact(text string) (string, int)
}

// Source is where a point or an answer comes from.
type Source struct {
	Label string `json:"label"`
	Ref   string `json:"ref,omitempty"`
	// Kind: trailer, mr, kb, lab, file, policy, impact, change, history, entity, search.
	Kind string `json:"kind"`
}

// Surface of an LLM call.
type Surface string

const (
	SurfaceAdvisor       Surface = "advisor"
	SurfaceChat          Surface = "chat"
	SurfaceCommitMessage Surface = "commit-message"
	SurfaceMCP           Surface = "mcp"
)

// LogEntry is one LLM call in the log (<data>/llm/log.jsonl).
type LogEntry struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	User       string    `json:"user"`
	Surface    Surface   `json:"surface"`
	Recipe     string    `json:"recipe,omitempty"`
	ChangeID   string    `json:"changeId,omitempty"`
	Model      string    `json:"model"`
	Mock       bool      `json:"mock,omitempty"`
	Prompt     string    `json:"prompt"` // redacted
	Answer     string    `json:"answer"`
	Sources    []Source  `json:"sources,omitempty"`
	Redactions int       `json:"redactions,omitempty"`
	Rating     string    `json:"rating,omitempty"` // up, down
	DurationMs int64     `json:"durationMs,omitempty"`
	// Error is set when the model call failed (the answer then comes from
	// the deterministic findings).
	Error string `json:"error,omitempty"`
}

// Log is the append-only LLM log. Delete writes an audit entry (caller).
type Log interface {
	Append(ctx context.Context, e LogEntry) (string, error)
	Get(ctx context.Context, id string) (*LogEntry, error)
	// List searches entries (newest first); q matches prompt, answer, user, recipe.
	List(ctx context.Context, q string, limit int) ([]LogEntry, int, error)
	Rate(ctx context.Context, id, rating string) (*LogEntry, error)
	Delete(ctx context.Context, id string) error
	Export(ctx context.Context, w io.Writer) error
}

// Document is one indexed text.
type Document struct {
	// Kind: kb, file, trailer, mr, lab.
	Kind   string
	Ref    string
	Title  string
	Text   string
	Path   string
	Change string
	// Redactions is the number of values the filter removed from Text.
	Redactions int
}

// Hit is a search result.
type Hit struct {
	Document
	Snippet string
	Score   float64
}

// Index is the retrieval index (BM25 keyword, or embeddings when
// PK_LLM_EMBED_MODEL is set). It holds main (kb/**, YAML files), merge
// trailers, MR discussions and lab reports. Live state is not indexed.
type Index interface {
	// Mode is bm25 or embeddings.
	Mode() string
	Search(ctx context.Context, q string, limit int) ([]Hit, error)
	// Rebuild re-reads all sources.
	Rebuild(ctx context.Context) error
}
