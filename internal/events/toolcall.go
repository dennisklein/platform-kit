package events

import "context"

// ToolCall marks a request that an MCP tool call makes. The MCP layer sends
// one presence event per call; a handler that has its own presence to send
// (a "look here" suggestion) sets Sent and adds the tool, so the call still
// makes one event.
type ToolCall struct {
	Tool string
	Sent bool
}

type toolCallKey struct{}

// WithToolCall returns a context for the requests of one tool call.
func WithToolCall(ctx context.Context, tool string) (context.Context, *ToolCall) {
	tc := &ToolCall{Tool: tool}
	return context.WithValue(ctx, toolCallKey{}, tc), tc
}

// ToolCallOf returns the tool call of a request, or nil.
func ToolCallOf(ctx context.Context) *ToolCall {
	tc, _ := ctx.Value(toolCallKey{}).(*ToolCall)
	return tc
}
