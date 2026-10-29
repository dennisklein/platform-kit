package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteStreamTextAndToolCalls(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"model":"qwen3:8b","choices":[{"delta":{"role":"assistant","content":"<thi"}}]}`,
			`{"choices":[{"delta":{"content":"nk>plan</th"}}]}`,
			`{"choices":[{"delta":{"content":"ink>\n\nc-2hd7vw moved "}}]}`,
			`{"choices":[{"delta":{"content":"cn-003."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"port 37\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"history","arguments":"{}"}}]}}]}`,
			`[DONE]`,
		} {
			io.WriteString(w, "data: "+d+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Model: "m"}, nil).(*OpenAI)
	var pieces []string
	resp, err := c.CompleteStream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}}, func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Errorf("stream not asked: %v", body)
	}
	if got := strings.Join(pieces, ""); got != "c-2hd7vw moved cn-003." {
		t.Errorf("streamed %q (%q)", got, pieces)
	}
	if resp.Model != "qwen3:8b" || resp.Message.Content != "c-2hd7vw moved cn-003." {
		t.Errorf("response %+v", resp)
	}
	tc := resp.Message.ToolCalls
	if len(tc) != 2 || tc[0].ID != "call_1" || tc[0].Name != "search" || string(tc[0].Arguments) != `{"q":"port 37"}` || tc[1].Name != "history" {
		t.Errorf("tool calls %+v", tc)
	}
}

func TestCompleteStreamFallsBackToJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"<think>x</think>all at once"}}]}`)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Model: "m"}, nil).(*OpenAI)
	var got string
	resp, err := c.CompleteStream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}}, func(s string) { got += s })
	if err != nil {
		t.Fatal(err)
	}
	if got != "all at once" || resp.Message.Content != "all at once" {
		t.Errorf("got %q, response %q", got, resp.Message.Content)
	}
}

func TestCompleteStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"error\":{\"message\":\"model not found\"}}\n\n")
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Model: "m"}, nil).(*OpenAI)
	_, err := c.CompleteStream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "x"}}}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err %v", err)
	}
}

func TestThinkFilter(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"a<think>b</think>c"}, "ac"},
		{[]string{"<", "think>", "x", "</", "think", ">", "\n\nok"}, "ok"},
		{[]string{"a < b", " and <t", "ag>"}, "a < b and <tag>"},
		{[]string{"<think>never closed"}, ""},
	} {
		var f thinkFilter
		got := ""
		for _, s := range tc.in {
			got += f.push(s)
		}
		got += f.flush()
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}
