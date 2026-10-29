package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIClient(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"qwen3:8b"},{"id":"other"}]}`)
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &got)
			if strings.Contains(string(b), "ENC[") {
				t.Errorf("secret sent: %s", b)
			}
			io.WriteString(w, `{"model":"qwen3:8b","choices":[{"message":{"role":"assistant","content":"<think>hm</think>ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"cn-003\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
		case "/v1/embeddings":
			io.WriteString(w, `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL + "/v1/", APIKey: "sk-test", EmbedModel: "nomic"}, nil)
	if c.Mock() {
		t.Fatal("mock")
	}
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(ctx, Request{
		Messages: []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "token: ENC[AES256_GCM,data:x,type:str]"}},
		Tools:    []Tool{{Name: "search", Description: "search", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Model() != "qwen3:8b" || resp.Model != "qwen3:8b" {
		t.Errorf("model %q %q", c.Model(), resp.Model)
	}
	if resp.Message.Content != "ok" || len(resp.Message.ToolCalls) != 1 || string(resp.Message.ToolCalls[0].Arguments) != `{"q":"cn-003"}` {
		t.Errorf("message %+v", resp.Message)
	}
	if got["tool_choice"] != "auto" || len(got["tools"].([]any)) != 1 {
		t.Errorf("request %v", got)
	}
	vecs, err := c.Embed(ctx, []string{"a", "b"})
	if err != nil || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Errorf("embed %v %v", vecs, err)
	}
}

func TestOpenAIClientErrors(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream down")
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Model: "m"}, nil)
	_, err := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "502") || n != 2 {
		t.Fatalf("err %v after %d calls", err, n)
	}
	if _, err := c.Embed(context.Background(), []string{"x"}); err != ErrNoEmbeddings {
		t.Fatalf("embed without model: %v", err)
	}
}

func TestMockClient(t *testing.T) {
	c := New(Config{}, nil)
	if !c.Mock() || c.Model() != MockModel {
		t.Fatal("not mock")
	}
	resp, _ := c.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "password: hunter22"}}})
	if strings.Contains(resp.Message.Content, "hunter22") {
		t.Fatal("mock leaks")
	}
}

// An endpoint that accepts connections but never answers: the first call
// waits for the timeout, the next calls fail at once (breaker), and the
// breaker closes when the endpoint answers again.
func TestOpenAIBreaker(t *testing.T) {
	hang := make(chan struct{})
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"m"}]}`)
		default:
			io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		}
	}))
	defer srv.Close()
	defer close(hang)
	c := New(Config{BaseURL: srv.URL + "/v1", Model: "m", Timeout: 300 * time.Millisecond}, nil).(*OpenAI)
	ctx := context.Background()
	req := Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	start := time.Now()
	if _, err := c.Complete(ctx, req); err == nil {
		t.Fatal("hanging endpoint answered")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("first call took %v (no retry after a timeout)", d)
	}
	start = time.Now()
	if _, err := c.Complete(ctx, req); !errors.Is(err, ErrUnavailable) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("second call: %v after %v", err, time.Since(start))
	}
	healthy.Store(true)
	// The background probe (started by the short-circuited call) closes it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Complete(ctx, req); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("breaker did not close after the endpoint recovered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// A refused port fails fast and opens the breaker too.
	r := New(Config{BaseURL: "http://127.0.0.1:1/v1", Model: "m"}, nil)
	if err := r.Ping(ctx); err == nil {
		t.Fatal("ping of a closed port")
	}
	if _, err := r.Complete(ctx, req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("after failed ping: %v", err)
	}
}
