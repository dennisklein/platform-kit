package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
)

// fakeReviewer knows one token per user and lets the users in may read.
type fakeReviewer struct {
	users map[string]string // token → user
	may   map[string]bool   // user → may get /metrics
	calls int
}

func (f *fakeReviewer) authenticate(_ context.Context, token string) (authenticationv1.UserInfo, bool, error) {
	f.calls++
	u, ok := f.users[token]
	return authenticationv1.UserInfo{Username: u}, ok, nil
}

func (f *fakeReviewer) authorize(_ context.Context, u authenticationv1.UserInfo, verb, path string) (bool, error) {
	f.calls++
	return verb == "get" && path == "/metrics" && f.may[u.Username], nil
}

func TestMetricsGate(t *testing.T) {
	rev := &fakeReviewer{
		users: map[string]string{"prom-token": "system:serviceaccount:monitoring:prometheus", "other-token": "alice"},
		may:   map[string]bool{"system:serviceaccount:monitoring:prometheus": true},
	}
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	g := &metricsGate{rev: rev, now: func() time.Time { return now }}
	h, err := g.filter(logr.Discard(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pk_metal_host_phase 1\n"))
	}))
	require.NoError(t, err)
	get := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusUnauthorized, get(""), "no token, no metrics")
	assert.Equal(t, http.StatusUnauthorized, get("forged"), "a token the API server does not know")
	assert.Equal(t, http.StatusForbidden, get("other-token"), "a known user without the role")
	assert.Equal(t, http.StatusOK, get("prom-token"))

	// An allowed token is remembered for a minute, then asked about again.
	calls := rev.calls
	assert.Equal(t, http.StatusOK, get("prom-token"))
	assert.Equal(t, calls, rev.calls, "a second scrape within the minute asks nobody")
	now = now.Add(metricsReviewTTL + time.Second)
	rev.may["system:serviceaccount:monitoring:prometheus"] = false
	assert.Equal(t, http.StatusForbidden, get("prom-token"), "a removed binding takes effect after the minute")
}

func TestMetricsSecureByDefault(t *testing.T) {
	c, err := parseFlags([]string{"--ledger=l", "--metrics-bind-address=:8080"}, &bytes.Buffer{})
	require.NoError(t, err)
	o := c.metricsOptions()
	assert.True(t, o.SecureServing, "metrics are served over HTTPS unless a person turns it off")
	assert.NotNil(t, o.FilterProvider, "and only to a caller the API server allows")

	c, err = parseFlags([]string{"--ledger=l", "--metrics-secure=false"}, &bytes.Buffer{})
	require.NoError(t, err)
	o = c.metricsOptions()
	assert.False(t, o.SecureServing)
	assert.Nil(t, o.FilterProvider)
}
