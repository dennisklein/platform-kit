package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config of the LLM layer (PK_LLM_*). An empty BaseURL means mock mode.
type Config struct {
	BaseURL    string
	Model      string
	APIKey     string
	EmbedModel string
	// Timeout of one call (default 45 s: a local model answers a short
	// prompt well within it; a hanging endpoint must not block a click
	// for minutes). PK_LLM_TIMEOUT sets it.
	Timeout time.Duration
}

// DefaultTimeout of one model call.
const DefaultTimeout = 45 * time.Second

// connectTimeout bounds the TCP connect: an endpoint that does not answer
// (a sleeping host, venue Wi-Fi) fails fast instead of after 30 s per try.
const connectTimeout = 3 * time.Second

// breakerWindow is how long a failed call or probe skips the model: calls
// fail at once (callers fall back to their deterministic answer) while a
// background probe checks the endpoint again.
const breakerWindow = 30 * time.Second

// ErrUnavailable is returned without a call while the endpoint is known to
// be down (a call or probe failed within the last 30 s).
var ErrUnavailable = errors.New("llm: endpoint unavailable")

// New returns the OpenAI-compatible client, or the mock without BaseURL.
// Every call goes through the redactor.
func New(cfg Config, r Redactor) Client {
	if r == nil {
		r = NewRedactor()
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return NewMock(r)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 5 * time.Second
	return &OpenAI{cfg: cfg, red: r, http: &http.Client{Timeout: cfg.Timeout, Transport: tr}, model: cfg.Model, now: time.Now}
}

// OpenAI is a client for OpenAI-compatible chat completions and embeddings
// (OpenAI, Ollama, vLLM, llama.cpp server, LiteLLM, ...).
type OpenAI struct {
	cfg  Config
	red  Redactor
	http *http.Client

	mu    sync.Mutex
	model string

	// Circuit breaker: downAt is the last failed call or probe.
	now     func() time.Time
	downAt  time.Time
	downErr error
	probing bool
}

// down returns ErrUnavailable while the endpoint failed recently, and starts
// one background probe that closes the breaker when the endpoint answers.
func (c *OpenAI) down() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.downAt.IsZero() || c.now().Sub(c.downAt) > breakerWindow {
		return nil
	}
	if !c.probing {
		c.probing = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), connectTimeout+2*time.Second)
			defer cancel()
			_, err := c.models(ctx)
			c.mu.Lock()
			c.probing = false
			c.mu.Unlock()
			c.note(err)
		}()
	}
	return fmt.Errorf("%w (%v)", ErrUnavailable, c.downErr)
}

// note records the outcome of a call: a failure to reach the endpoint opens
// the breaker, an answer closes it.
func (c *OpenAI) note(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.downAt, c.downErr = time.Time{}, nil
		return
	}
	c.downAt, c.downErr = c.now(), err
}

// unreachable reports errors that say the endpoint did not answer (as
// opposed to an answer with an error status).
func unreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var oe *net.OpError
	return errors.As(err, &oe) || strings.Contains(err.Error(), "connection refused") || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Model is the configured model, or the first model the server lists.
func (c *OpenAI) Model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.model == "" {
		return "default"
	}
	return c.model
}

func (c *OpenAI) Mock() bool { return false }

func (c *OpenAI) url(p string) string { return strings.TrimRight(c.cfg.BaseURL, "/") + p }

type oaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content"`
	Name       string       `json:"name,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaRequest struct {
	Model          string          `json:"model"`
	Messages       []oaMessage     `json:"messages"`
	Tools          []oaTool        `json:"tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *map[string]any `json:"response_format,omitempty"`
	Stream         bool            `json:"stream"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      oaMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete runs one chat completion. Message contents and tool arguments are
// redacted first.
func (c *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	if err := c.down(); err != nil {
		return nil, err
	}
	model, err := c.resolveModel(ctx)
	if err != nil {
		return nil, err
	}
	body := c.request(model, req)
	var out oaResponse
	if err := c.post(ctx, "/chat/completions", body, &out); err != nil {
		return nil, err
	}
	return toResponse(model, out)
}

// toResponse converts a non-streamed answer.
func toResponse(model string, out oaResponse) (*Response, error) {
	if out.Error != nil {
		return nil, fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("llm: empty answer (no choices)")
	}
	m := out.Choices[0].Message
	res := &Response{Model: model, Usage: out.Usage}
	if out.Model != "" {
		res.Model = out.Model
	}
	res.Message = Message{Role: RoleAssistant}
	if m.Content != nil {
		res.Message.Content = stripThinking(*m.Content)
	}
	for _, tc := range m.ToolCalls {
		res.Message.ToolCalls = append(res.Message.ToolCalls, toolCall(tc.ID, tc.Function.Name, tc.Function.Arguments))
	}
	return res, nil
}

func toolCall(id, name, args string) ToolCall {
	args = strings.TrimSpace(args)
	if args == "" || !json.Valid([]byte(args)) {
		args = "{}"
	}
	return ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// request builds the body of a chat completion (redacted).
func (c *OpenAI) request(model string, req Request) oaRequest {
	msgs := append([]Message(nil), req.Messages...)
	for i := range msgs {
		msgs[i].ToolCalls = append([]ToolCall(nil), msgs[i].ToolCalls...)
	}
	RedactMessages(c.red, msgs)
	body := oaRequest{Model: model, Stream: false, MaxTokens: req.MaxTokens}
	if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	if req.JSON && len(req.Tools) == 0 {
		body.ResponseFormat = &map[string]any{"type": "json_object"}
	}
	for _, m := range msgs {
		content := m.Content
		om := oaMessage{Role: string(m.Role), Content: &content, Name: m.Name, ToolCallID: m.ToolCallID}
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 && content == "" {
			om.Content = nil
		}
		for _, tc := range m.ToolCalls {
			otc := oaToolCall{ID: tc.ID, Type: "function"}
			otc.Function.Name = tc.Name
			otc.Function.Arguments = string(tc.Arguments)
			if otc.Function.Arguments == "" {
				otc.Function.Arguments = "{}"
			}
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, oaTool{Type: "function", Function: oaFunction{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}
	return body
}

// stripThinking removes <think>...</think> blocks of reasoning models.
func stripThinking(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// Embed returns embeddings of texts (redacted first).
func (c *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if c.cfg.EmbedModel == "" {
		return nil, ErrNoEmbeddings
	}
	if err := c.down(); err != nil {
		return nil, err
	}
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i], _ = c.red.Redact(t)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/embeddings", map[string]any{"model": c.cfg.EmbedModel, "input": in}, &out); err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		idx := d.Index
		if idx < 0 || idx >= len(vecs) {
			idx = i
		}
		if idx < len(vecs) {
			vecs[idx] = d.Embedding
		}
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("llm: no embedding for input %d", i)
		}
	}
	return vecs, nil
}

// Ping lists the models of the endpoint. It is the probe of the breaker:
// a failure makes the next calls fall back at once, an answer closes it.
func (c *OpenAI) Ping(ctx context.Context) error {
	_, err := c.models(ctx)
	if err == nil || unreachable(err) {
		c.note(err)
	}
	return err
}

func (c *OpenAI) models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/models"), nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("llm: GET /models: %s: %s", resp.Status, short(string(b)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("llm: GET /models: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

// resolveModel returns PK_LLM_MODEL, or the first model of the endpoint.
func (c *OpenAI) resolveModel(ctx context.Context) (string, error) {
	c.mu.Lock()
	m := c.model
	c.mu.Unlock()
	if m != "" {
		return m, nil
	}
	ids, err := c.models(ctx)
	if err != nil {
		if unreachable(err) {
			c.note(err)
		}
		return "", err
	}
	if len(ids) == 0 {
		return "", errors.New("llm: PK_LLM_MODEL is not set and the endpoint lists no models")
	}
	c.mu.Lock()
	c.model = ids[0]
	c.mu.Unlock()
	return ids[0], nil
}

func (c *OpenAI) auth(req *http.Request) {
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
}

// post sends JSON and decodes the answer. It retries once on 429 and 5xx.
func (c *OpenAI) post(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		c.auth(req)
		resp, err := c.http.Do(req)
		if err != nil {
			last = fmt.Errorf("llm: %w", err)
			// The endpoint did not answer: no retry, the breaker opens.
			if unreachable(err) {
				c.note(last)
			}
			return last
		}
		rb, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if rerr != nil {
			last = fmt.Errorf("llm: %w", rerr)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			last = fmt.Errorf("llm: POST %s: %s: %s", path, resp.Status, short(string(rb)))
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("llm: POST %s: %s: %s", path, resp.Status, short(string(rb)))
		}
		c.note(nil)
		if err := json.Unmarshal(rb, out); err != nil {
			return fmt.Errorf("llm: POST %s: %w", path, err)
		}
		return nil
	}
	return last
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
