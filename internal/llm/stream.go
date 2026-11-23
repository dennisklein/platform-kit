package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Streamer is a Client that streams the text of an answer while the model
// writes it. Callers check for it with a type assertion; a client without it
// answers with Complete.
type Streamer interface {
	// CompleteStream is Complete, and calls onText with each new piece of
	// the answer text as it arrives (reasoning <think> blocks removed).
	// Tool calls come in the returned response only.
	CompleteStream(ctx context.Context, req Request, onText func(string)) (*Response, error)
}

var _ Streamer = (*OpenAI)(nil)

// CompleteStream runs one chat completion with stream:true. A server that
// does not stream (it answers with one JSON object) works too: onText then
// gets the whole text at once.
func (c *OpenAI) CompleteStream(ctx context.Context, req Request, onText func(string)) (*Response, error) {
	if onText == nil {
		return c.Complete(ctx, req)
	}
	if err := c.down(); err != nil {
		return nil, err
	}
	model, err := c.resolveModel(ctx)
	if err != nil {
		return nil, err
	}
	body := c.request(model, req)
	body.Stream = true
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	const path = "/chat/completions"
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		c.auth(hreq)
		resp, err := c.http.Do(hreq)
		if err != nil {
			last = fmt.Errorf("llm: %w", err)
			if unreachable(err) {
				c.note(last)
			}
			return nil, last
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			last = fmt.Errorf("llm: POST %s: %s: %s", path, resp.Status, short(string(rb)))
			continue
		}
		if resp.StatusCode/100 != 2 {
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("llm: POST %s: %s: %s", path, resp.Status, short(string(rb)))
		}
		c.note(nil)
		res, err := readStream(resp, model, onText)
		resp.Body.Close()
		if err != nil && unreachable(err) && !errors.Is(err, context.DeadlineExceeded) {
			c.note(err)
		}
		return res, err
	}
	return nil, last
}

type oaChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type toolBuild struct {
	id, name string
	args     strings.Builder
}

// readStream reads an answer: server-sent "data:" lines with deltas, or one
// JSON object from a server that ignores stream:true.
func readStream(resp *http.Response, model string, onText func(string)) (*Response, error) {
	br := bufio.NewReaderSize(io.LimitReader(resp.Body, 32<<20), 64<<10)
	sse := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
	if !sse {
		// Some servers send SSE with another content type.
		for {
			p, err := br.Peek(1)
			if err != nil || (p[0] != ' ' && p[0] != '\n' && p[0] != '\r' && p[0] != '\t') {
				break
			}
			br.ReadByte()
		}
		p, _ := br.Peek(5)
		sse = string(p) == "data:"
	}
	if !sse {
		rb, err := io.ReadAll(br)
		if err != nil {
			return nil, fmt.Errorf("llm: %w", err)
		}
		var out oaResponse
		if err := json.Unmarshal(rb, &out); err != nil {
			return nil, fmt.Errorf("llm: POST /chat/completions: %w", err)
		}
		res, err := toResponse(model, out)
		if err != nil {
			return nil, err
		}
		if res.Message.Content != "" {
			onText(res.Message.Content)
		}
		return res, nil
	}

	res := &Response{Model: model}
	var full strings.Builder
	var think thinkFilter
	tools := map[int]*toolBuild{}
	chunks := 0
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(line[len("data:"):])
			if data == "[DONE]" {
				break
			}
			var ch oaChunk
			if jerr := json.Unmarshal([]byte(data), &ch); jerr != nil {
				return nil, fmt.Errorf("llm: stream: %w", jerr)
			}
			if ch.Error != nil {
				return nil, fmt.Errorf("llm: %s", ch.Error.Message)
			}
			chunks++
			if ch.Model != "" {
				res.Model = ch.Model
			}
			if ch.Usage != nil {
				res.Usage = *ch.Usage
			}
			for _, c := range ch.Choices {
				if c.Delta.Content != nil && *c.Delta.Content != "" {
					full.WriteString(*c.Delta.Content)
					if t := think.push(*c.Delta.Content); t != "" {
						onText(t)
					}
				}
				for _, tc := range c.Delta.ToolCalls {
					b := tools[tc.Index]
					if b == nil {
						b = &toolBuild{}
						tools[tc.Index] = b
					}
					if tc.ID != "" {
						b.id = tc.ID
					}
					if tc.Function.Name != "" && b.name == "" {
						b.name = tc.Function.Name
					}
					b.args.WriteString(tc.Function.Arguments)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("llm: stream: %w", err)
		}
	}
	if chunks == 0 {
		return nil, errors.New("llm: empty answer (no stream data)")
	}
	if t := think.flush(); t != "" {
		onText(t)
	}
	res.Message = Message{Role: RoleAssistant, Content: stripThinking(full.String())}
	idx := make([]int, 0, len(tools))
	for i := range tools {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		b := tools[i]
		if b.name == "" {
			continue
		}
		res.Message.ToolCalls = append(res.Message.ToolCalls, toolCall(b.id, b.name, b.args.String()))
	}
	return res, nil
}

// thinkFilter removes <think>...</think> blocks from streamed text, also
// when a tag is split across pieces, and drops the blank lines a model
// writes before its answer.
type thinkFilter struct {
	in      bool
	pend    string
	started bool
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

func (f *thinkFilter) push(s string) string {
	f.pend += s
	var out strings.Builder
	for {
		if f.in {
			i := strings.Index(f.pend, thinkClose)
			if i < 0 {
				f.pend = f.pend[len(f.pend)-partialTag(f.pend, thinkClose):]
				break
			}
			f.pend, f.in = f.pend[i+len(thinkClose):], false
			continue
		}
		i := strings.Index(f.pend, thinkOpen)
		if i < 0 {
			k := len(f.pend) - partialTag(f.pend, thinkOpen)
			out.WriteString(f.pend[:k])
			f.pend = f.pend[k:]
			break
		}
		out.WriteString(f.pend[:i])
		f.pend, f.in = f.pend[i+len(thinkOpen):], true
	}
	return f.emit(out.String())
}

func (f *thinkFilter) flush() string {
	if f.in {
		return ""
	}
	s := f.pend
	f.pend = ""
	return f.emit(s)
}

func (f *thinkFilter) emit(s string) string {
	if !f.started {
		s = strings.TrimLeft(s, " \t\r\n")
		f.started = s != ""
	}
	return s
}

// partialTag is the length of the longest end of s that starts tag.
func partialTag(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
