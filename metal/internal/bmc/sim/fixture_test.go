package sim

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadFixtureRefusesUnknownKeys: a fixture given by path is a user's
// file. A misspelled quirk key must be an error, or the sim silently stops
// reproducing that quirk.
func TestLoadFixtureRefusesUnknownKeys(t *testing.T) {
	good, err := embedded.ReadFile("fixtures/generic.yaml")
	require.NoError(t, err)
	require.Contains(t, string(good), "require_if_match:")

	dir := t.TempDir()
	goodPath := filepath.Join(dir, "good.yaml")
	require.NoError(t, os.WriteFile(goodPath, good, 0o600))
	_, err = LoadFixture(goodPath)
	require.NoError(t, err, "the unchanged fixture loads from a path")

	typo := strings.Replace(string(good), "require_if_match:", "require_if_mach:", 1)
	typoPath := filepath.Join(dir, "typo.yaml")
	require.NoError(t, os.WriteFile(typoPath, []byte(typo), 0o600))
	_, err = LoadFixture(typoPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require_if_mach")
}

// TestETagGzipSuffix: behind mod_deflate the ETag header carries "-gzip",
// the body tag does not, and only the plain tag satisfies If-Match.
func TestETagGzipSuffix(t *testing.T) {
	f, err := LoadFixture("generic")
	require.NoError(t, err)
	f.Protocol.ETagGzipSuffix = true
	srv := httptest.NewServer(NewServer(NewState(f)))
	defer srv.Close()
	uri := srv.URL + "/redfish/v1/Systems/" + f.Systems[0].ID
	do := func(method string, headers map[string]string, body string) *http.Response {
		req, err := http.NewRequest(method, uri, strings.NewReader(body))
		require.NoError(t, err)
		req.SetBasicAuth(f.Accounts[0].Username, f.Accounts[0].Password)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp
	}

	resp := do(http.MethodGet, nil, "")
	header := resp.Header.Get("ETag")
	require.True(t, strings.HasSuffix(header, `-gzip"`), "header %s", header)
	plain := strings.TrimSuffix(header, `-gzip"`) + `"`

	change := `{"Boot":{"BootSourceOverrideEnabled":"Disabled"}}`
	assert.Equal(t, http.StatusPreconditionFailed, do(http.MethodPatch, map[string]string{"If-Match": header}, change).StatusCode,
		"the suffixed tag is not the one the backend compares")
	assert.Less(t, do(http.MethodPatch, map[string]string{"If-Match": plain}, change).StatusCode, 300,
		"the plain tag passes")
}

// TestEmbeddedFixturesLoad keeps every shipped fixture valid under strict
// decoding.
func TestEmbeddedFixturesLoad(t *testing.T) {
	for _, name := range Fixtures() {
		_, err := LoadFixture(name)
		assert.NoError(t, err, name)
	}
}
