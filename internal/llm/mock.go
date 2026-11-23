package llm

import (
	"context"
	"strings"
)

// MockModel is the model name in mock mode.
const MockModel = "mock"

// Mock is the deterministic client without a model. Advisors and chat do not
// ask it to write; they render their data-driven findings directly. Complete
// answers with the redacted last user message, so a caller that still calls
// it gets a stable, visible answer.
type Mock struct {
	red Redactor
}

// NewMock returns the mock client.
func NewMock(r Redactor) *Mock {
	if r == nil {
		r = NewRedactor()
	}
	return &Mock{red: r}
}

func (m *Mock) Model() string { return MockModel }
func (m *Mock) Mock() bool    { return true }

func (m *Mock) Complete(ctx context.Context, req Request) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			last = req.Messages[i].Content
			break
		}
	}
	last, _ = m.red.Redact(last)
	if len(last) > 400 {
		last = last[:400] + "…"
	}
	return &Response{Model: MockModel, Message: Message{Role: RoleAssistant, Content: strings.TrimSpace("mock answer: " + last)}}, nil
}

func (m *Mock) Embed(context.Context, []string) ([][]float32, error) { return nil, ErrNoEmbeddings }

func (m *Mock) Ping(context.Context) error { return nil }
