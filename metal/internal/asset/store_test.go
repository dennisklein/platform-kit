package asset

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeBuild plays the build Job: it writes an imager-shaped output
// directory into the store.
func writeBuild(t *testing.T, s *Store, job string, content []byte) {
	t.Helper()
	require.NoError(t, s.PrepareBuild(job))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, BuildDir(job), "metal-amd64.iso"), content, 0o644))
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestStore(t *testing.T) {
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	iso := []byte("an installer with a config in it")

	t.Run("import moves the build output into place", func(t *testing.T) {
		writeBuild(t, s, "node-a-build-1", iso)
		d, size, err := s.Import("node-a-build-1", "node-a", "k1")
		require.NoError(t, err)
		assert.Equal(t, sha(iso), d)
		assert.EqualValues(t, len(iso), size)
		assert.True(t, s.Has("node-a", "k1"))
		assert.NoDirExists(t, filepath.Join(s.Dir, BuildDir("node-a-build-1")), "the build directory goes")

		st, err := os.Stat(s.path("node-a", "k1"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(artifactMode), st.Mode().Perm(), "an artifact carries secrets: owner read only")
	})

	t.Run("import of a key already there keeps it", func(t *testing.T) {
		writeBuild(t, s, "node-a-build-2", []byte("another build of the same inputs"))
		d, _, err := s.Import("node-a-build-2", "node-a", "k1")
		require.NoError(t, err)
		assert.Equal(t, sha(iso), d, "files are never rewritten in place")
	})

	t.Run("import refuses output that is not one iso", func(t *testing.T) {
		require.NoError(t, s.PrepareBuild("empty"))
		_, _, err := s.Import("empty", "node-b", "k1")
		assert.ErrorContains(t, err, "want exactly one")
		assert.False(t, s.Has("node-b", "k1"))
	})

	t.Run("open checks the approved digest", func(t *testing.T) {
		f, err := s.Open("node-a", "k1", sha(iso))
		require.NoError(t, err)
		got, err := io.ReadAll(f)
		require.NoError(t, err)
		assert.Equal(t, iso, got, "handed back rewound")
		require.NoError(t, f.Close())

		_, err = s.Open("node-a", "k1", sha([]byte("something else")))
		assert.ErrorContains(t, err, "approved")
	})

	t.Run("names stay inside the store", func(t *testing.T) {
		for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden"} {
			_, _, err := s.Import("job", bad, "k")
			assert.Error(t, err, bad)
			assert.False(t, s.Has(bad, "k"), bad)
		}
	})

	t.Run("prune keeps the current key only", func(t *testing.T) {
		writeBuild(t, s, "node-a-build-3", []byte("newer render"))
		_, _, err := s.Import("node-a-build-3", "node-a", "k2")
		require.NoError(t, err)
		require.NoError(t, s.Prune("node-a", "k2"))
		assert.False(t, s.Has("node-a", "k1"))
		assert.True(t, s.Has("node-a", "k2"))
	})

	t.Run("sweep removes what nothing claims", func(t *testing.T) {
		writeBuild(t, s, "node-c-build-1", iso)
		_, _, err := s.Import("node-c-build-1", "node-c", "k1")
		require.NoError(t, err)
		writeBuild(t, s, "orphan-build", iso)
		writeBuild(t, s, "running-build", iso)

		require.NoError(t, s.Sweep(
			func(a string) bool { return a == "node-a" },
			func(j string) bool { return j == "running-build" }))
		assert.True(t, s.Has("node-a", "k2"))
		assert.False(t, s.Has("node-c", "k1"), "its BootAsset is gone")
		assert.NoDirExists(t, filepath.Join(s.Dir, BuildDir("orphan-build")))
		assert.DirExists(t, filepath.Join(s.Dir, BuildDir("running-build")))
	})
}

func TestNewStoreRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := NewStore(filepath.Join(dir, "store"))
	assert.Error(t, err)
}

// A grant on a file serves ranges from the descriptor it was minted with,
// and lets go of it when it ends.
func TestFileGrant(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	table := NewGrants()
	table.now = func() time.Time { return now }
	srv := httptest.NewTLSServer(&Server{Grants: table})
	t.Cleanup(srv.Close)

	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	content := make([]byte, 5000)
	for i := range content {
		content[i] = byte(i * 7)
	}
	writeBuild(t, s, "b", content)
	_, _, err = s.Import("b", "node-a", "k1")
	require.NoError(t, err)

	f, err := s.Open("node-a", "k1", sha(content))
	require.NoError(t, err)
	g, err := table.IssueFile("op", "node-a.iso", f, time.Hour, nil)
	require.NoError(t, err)

	// The store moves on (a newer render); the grant still serves what it
	// was minted with.
	require.NoError(t, s.Prune("node-a", "k2"))
	assert.False(t, s.Has("node-a", "k1"))

	req, err := http.NewRequest(http.MethodGet, srv.URL+g.Path(), nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=4096-4999")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, fmt.Sprintf("bytes 4096-4999/%d", len(content)), resp.Header.Get("Content-Range"))
	assert.Equal(t, content[4096:], body)

	assert.True(t, table.Revoke(g.Token))
	_, err = f.Stat()
	assert.ErrorIs(t, err, os.ErrClosed, "a revoked grant closes its file")

	t.Run("expiry closes the file too", func(t *testing.T) {
		writeBuild(t, s, "b2", content)
		_, _, err := s.Import("b2", "node-x", "k1")
		require.NoError(t, err)
		f, err := s.Open("node-x", "k1", "")
		require.NoError(t, err)
		_, err = table.IssueFile("op", "node-x.iso", f, time.Minute, nil)
		require.NoError(t, err)
		now = now.Add(time.Minute)
		_, ok := table.Live("op", "node-x.iso")
		assert.False(t, ok)
		_, err = f.Stat()
		assert.ErrorIs(t, err, os.ErrClosed)
	})
}
