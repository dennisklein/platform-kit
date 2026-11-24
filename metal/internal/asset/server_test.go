package asset

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	table := NewGrants()
	table.now = func() time.Time { return now }
	srv := httptest.NewTLSServer(&Server{Grants: table})
	t.Cleanup(srv.Close)
	cl := srv.Client()

	content := make([]byte, 3000)
	for i := range content {
		content[i] = byte(i)
	}
	grant, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Hour, nil)
	require.NoError(t, err)
	url := srv.URL + grant.Path()

	get := func(t *testing.T, url string, hdr map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, url, nil)
		require.NoError(t, err)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := cl.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("full fetch", func(t *testing.T) {
		resp := get(t, url, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, content, body)
	})

	t.Run("ranged fetch", func(t *testing.T) {
		resp := get(t, url, map[string]string{"Range": "bytes=512-1023"})
		assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
		assert.Equal(t, fmt.Sprintf("bytes 512-1023/%d", len(content)), resp.Header.Get("Content-Range"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, content[512:1024], body)
	})

	t.Run("head keep-alive", func(t *testing.T) {
		resp, err := cl.Head(url)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, fmt.Sprintf("%d", len(content)), resp.Header.Get("Content-Length"))
	})

	t.Run("unknown token 403", func(t *testing.T) {
		resp := get(t, srv.URL+"/grants/00000000000000000000000000000000/node-x12.iso", nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("filename mismatch 403", func(t *testing.T) {
		resp := get(t, srv.URL+"/grants/"+grant.Token+"/other.iso", nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("post 405", func(t *testing.T) {
		resp, err := cl.Post(url, "text/plain", nil)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})

	t.Run("credentials", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Hour,
			&Credentials{Username: "media", Password: "s3cret"})
		require.NoError(t, err)
		authedURL := srv.URL + g.Path()

		resp := get(t, authedURL, nil)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Basic")

		req, err := http.NewRequest(http.MethodGet, authedURL, nil)
		require.NoError(t, err)
		req.SetBasicAuth("media", "wrong")
		resp, err = cl.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		req.SetBasicAuth("media", "s3cret")
		resp, err = cl.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("expired grant 403", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Minute, nil)
		require.NoError(t, err)
		expURL := srv.URL + g.Path()

		resp := get(t, expURL, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "live before the TTL")

		now = now.Add(2 * time.Minute)
		resp = get(t, expURL, nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("revoked grant 403", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Hour, nil)
		require.NoError(t, err)
		table.RevokeAsset("node-x12-config")
		resp := get(t, srv.URL+g.Path(), nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}
