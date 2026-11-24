package gofish

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// taskURI is where the fuzz harness serves its task monitor.
const taskURI = "/redfish/v1/TaskService/Tasks/fuzz"

// taskServer serves the sim tree, with one task monitor whose answer the
// test sets.
type taskServer struct {
	mu     sync.Mutex
	status int
	body   []byte
}

func (s *taskServer) set(status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *taskServer) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != taskURI {
			next.ServeHTTP(w, r)
			return
		}
		s.mu.Lock()
		status, body := s.status, s.body
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

// newTaskClient connects the production client to a sim tree with a task
// monitor the test controls.
func newTaskClient(t testing.TB) (*client, *taskServer) {
	f, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	ts := &taskServer{status: http.StatusOK}
	srv := httptest.NewServer(ts.wrap(sim.NewServer(sim.NewState(f))))
	t.Cleanup(srv.Close)
	bc, err := Connect(context.Background(), Config{Endpoint: srv.URL})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close() })
	return bc.(*client), ts
}

// taskStates reads the TaskState values a body declares, the way
// encoding/json matches them (keys compare without case, so a body can name
// it more than once). A value that is not a string reads as "". ok is false
// when the body is not a JSON object at all.
func taskStates(body []byte) (states []string, ok bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	for k, v := range doc {
		if strings.EqualFold(k, "TaskState") {
			var s string
			_ = json.Unmarshal(v, &s)
			states = append(states, s)
		}
	}
	return states, true
}

// FuzzPollTask: a task ends in success only when its monitor says
// Completed, or when the monitor answers with the operation's own response
// (no Task, not 202). A failed, killed or cancelled task is always an error.
func FuzzPollTask(f *testing.F) {
	for _, seed := range []struct {
		body   string
		status uint16
	}{
		{`{"TaskState":"Completed"}`, 200},
		{`{"TaskState":"Running"}`, 202},
		{`{"TaskState":"Exception","Messages":[{"Message":"image rejected"}]}`, 200},
		{`{"TaskState":"Killed"}`, 200},
		{`{"TaskState":"Cancelled"}`, 202},
		{`{"@odata.id":"/redfish/v1/Systems/1"}`, 200},
		{``, 204},
		{``, 202},
		{`{"error":{"code":"Base.1.8.GeneralError"}}`, 500},
	} {
		f.Add([]byte(seed.body), seed.status)
	}
	c, ts := newTaskClient(f)
	f.Fuzz(func(t *testing.T, body []byte, status uint16) {
		code := int(status)
		if code < 200 || code > 599 {
			code = 200 + code%400 // what a monitor can answer with
		}
		ts.set(code, body)
		served := body
		if code == http.StatusNoContent || code == http.StatusNotModified {
			served = nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		err := c.pollTask(ctx, taskURI)

		states, isObject := taskStates(served)
		failed := slices.ContainsFunc(states, func(s string) bool {
			return s == "Exception" || s == "Killed" || s == "Cancelled"
		})
		switch {
		case failed:
			require.Error(t, err, "a task in %v ended in success", states)
		case err != nil:
			return
		case len(states) > 0:
			for _, s := range states {
				assert.Equal(t, "Completed", s, "success on TaskState %q", s)
			}
		default:
			assert.NotEqual(t, http.StatusAccepted, code, "success on a 202 with no TaskState")
			assert.True(t, isObject || len(bytes.TrimSpace(served)) == 0,
				"success on an answer that is not a JSON object: %q", served)
		}
	})
}
