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

	t.Run("unknown token refused", func(t *testing.T) {
		resp := get(t, srv.URL+"/grants/00000000000000000000000000000000/node-x12.iso", nil)
		assertRefused(t, resp)
	})

	t.Run("filename mismatch refused", func(t *testing.T) {
		resp := get(t, srv.URL+"/grants/"+grant.Token+"/other.iso", nil)
		assertRefused(t, resp)
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
		assertRefused(t, resp)

		req, err := http.NewRequest(http.MethodGet, authedURL, nil)
		require.NoError(t, err)
		req.SetBasicAuth("media", "wrong")
		resp, err = cl.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		assertRefused(t, resp)

		req.SetBasicAuth("media", "s3cret")
		resp, err = cl.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("expired grant refused", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Minute, nil)
		require.NoError(t, err)
		expURL := srv.URL + g.Path()

		resp := get(t, expURL, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "live before the TTL")

		now = now.Add(2 * time.Minute)
		resp = get(t, expURL, nil)
		assertRefused(t, resp)
	})

	t.Run("revoked grant refused", func(t *testing.T) {
		g, err := table.Issue("node-x12-config", "node-x12.iso", content, time.Hour, nil)
		require.NoError(t, err)
		table.RevokeAsset("node-x12-config")
		resp := get(t, srv.URL+g.Path(), nil)
		assertRefused(t, resp)
	})
}

// assertRefused: the one answer to every refused grant request. A live
// token without credentials, a wrong password, an unknown token and an
// expired one cannot be told apart.
func assertRefused(t *testing.T, resp *http.Response) {
	t.Helper()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, `Basic realm="pk-metal"`, resp.Header.Get("WWW-Authenticate"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "unauthorized\n", string(body))
}

// TestGrantBindsToItsFirstSource: the first admitted request binds a grant
// to its source address. Another address is refused with the common answer,
// and the refusal is recorded. Each record names the grant by its handle
// and file, never by its token or credentials, and the end of the grant
// reports what it served.
func TestGrantBindsToItsFirstSource(t *testing.T) {
	var recs []GrantRecord
	table := NewGrants()
	table.Record = func(r GrantRecord) { recs = append(recs, r) }
	srv := &Server{Grants: table}
	content := []byte("the config medium")
	creds := &Credentials{Username: "media", Password: "s3cret-password"}
	g, err := table.Issue("hostoperation:pk-metal/op", "node-config.iso", content, time.Hour, creds)
	require.NoError(t, err)

	fetch := func(from string, user, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "https://media"+g.Path(), nil)
		req.RemoteAddr = from
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	assert.Equal(t, http.StatusUnauthorized, fetch("192.0.2.10:4000", "", "").Code, "the challenge round")
	assert.Empty(t, recs, "a challenge round is no refusal worth a record")

	assert.Equal(t, http.StatusOK, fetch("192.0.2.10:4001", "media", "s3cret-password").Code, "the BMC fetches")
	assert.Equal(t, http.StatusOK, fetch("192.0.2.10:4002", "media", "s3cret-password").Code, "and again, from another port")
	stolen := fetch("198.51.100.7:5000", "media", "s3cret-password")
	assert.Equal(t, http.StatusUnauthorized, stolen.Code, "a copy of the URL and credentials used elsewhere")
	assert.Equal(t, "unauthorized\n", stolen.Body.String(), "with the common answer")
	assert.Equal(t, http.StatusUnauthorized, fetch("192.0.2.10:4003", "media", "wrong").Code)

	table.RevokeAsset("hostoperation:pk-metal/op")

	require.Len(t, recs, 4)
	assert.Equal(t, GrantRecord{Event: GrantFetched, Handle: "hostoperation:pk-metal/op", Filename: "node-config.iso",
		Source: "192.0.2.10"}, recs[0])
	assert.Equal(t, GrantRefused, recs[1].Event)
	assert.Equal(t, "198.51.100.7", recs[1].Source)
	assert.Contains(t, recs[1].Reason, "bound to another source address, 192.0.2.10")
	assert.Equal(t, GrantRefused, recs[2].Event)
	assert.Equal(t, "wrong credentials", recs[2].Reason)
	assert.Equal(t, GrantRecord{Event: GrantEnded, Handle: "hostoperation:pk-metal/op", Filename: "node-config.iso",
		Source: "192.0.2.10", Reason: "revoked", Requests: 2, Bytes: int64(2 * len(content)), Refused: 2}, recs[3])
	for _, r := range recs {
		assert.NotContains(t, fmt.Sprintf("%+v", r), g.Token)
		assert.NotContains(t, fmt.Sprintf("%+v", r), creds.Password)
	}
}

// TestRefusalRecordsAreBounded: a client that retries without end gets a
// bounded number of records; the end of the grant counts every refusal.
func TestRefusalRecordsAreBounded(t *testing.T) {
	var recs []GrantRecord
	table := NewGrants()
	table.Record = func(r GrantRecord) { recs = append(recs, r) }
	g, err := table.Issue("h", "x.iso", []byte("x"), time.Hour, &Credentials{Username: "u", Password: "p"})
	require.NoError(t, err)
	for range 3 * maxRefusalRecords {
		table.refuse(g, "192.0.2.1", "wrong credentials")
	}
	table.Revoke(g.Token)
	require.Len(t, recs, maxRefusalRecords+1)
	assert.Equal(t, 3*maxRefusalRecords, recs[len(recs)-1].Refused)
}
