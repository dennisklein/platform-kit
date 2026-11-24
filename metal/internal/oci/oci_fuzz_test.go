package oci

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzParseRef: an accepted reference names a host, pins a valid sha256
// digest, and host/repo@digest parses back to the same three parts. A
// reference without a digest is never accepted.
func FuzzParseRef(f *testing.F) {
	pin := digest.FromString("x").String()
	for _, seed := range []string{
		"registry.example:5000/firmware/x12-bmc@" + pin,
		"[2001:db8::1]:5000/firmware/x12-bmc@" + pin,
		"10.0.0.5/firmware/x12-bmc@" + pin,
		"registry.example/firmware/x12-bmc:v1@" + pin,
		"registry.example/firmware/x12-bmc:v1",
		"firmware/x12-bmc@" + pin,
		"registry.example/firmware/x12-bmc@sha256:abc",
		"registry.example/firmware/x12-bmc@sha512:" + strings.Repeat("a", 128) + "@sha256:",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		host, repo, d, err := parseRef(ref)
		if err != nil {
			return
		}
		assert.NotEmpty(t, host, "ref %q", ref)
		assert.NotEmpty(t, repo, "ref %q", ref)
		require.NoError(t, d.Validate(), "ref %q", ref)
		assert.Equal(t, digest.SHA256, d.Algorithm(), "ref %q", ref)
		assert.Contains(t, ref, "@"+d.String(), "the digest is not the one written")

		h2, r2, d2, err := parseRef(host + "/" + repo + "@" + d.String())
		require.NoError(t, err, "ref %q", ref)
		assert.Equal(t, []string{host, repo, d.String()}, []string{h2, r2, d2.String()}, "ref %q", ref)
	})
}

// TestPullRefusesOversizedBody: a manifest or blob over the limit is
// refused even when its digest matches, so a pull holds a bounded amount.
func TestPullRefusesOversizedBody(t *testing.T) {
	old := maxBytes
	maxBytes = 512 // above the manifest, below the big blob
	t.Cleanup(func() { maxBytes = old })

	srv, ref := registry(t, []byte("fits"), 1)
	_, err := Pull(t.Context(), srv.Client(), ref)
	require.NoError(t, err, "a blob under the limit is pulled")

	srv, ref = registry(t, []byte(strings.Repeat("x", 513)), 1)
	_, err = Pull(t.Context(), srv.Client(), ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// FuzzPullManifest serves an arbitrary manifest and blob. Pull may return
// bytes only when the manifest hashes to the pinned digest and the blob to
// the single layer digest the manifest names.
func FuzzPullManifest(f *testing.F) {
	blob := []byte("SIMFW 02.00.00 test image")
	good := fmt.Sprintf(`{"schemaVersion":2,"layers":[{"digest":%q,"size":%d}]}`, digest.FromBytes(blob), len(blob))
	f.Add([]byte(good), blob, true)
	f.Add([]byte(good), blob, false)
	f.Add([]byte(good), []byte("tampered"), true)
	f.Add([]byte(`{"layers":[]}`), blob, true)
	f.Add([]byte(`{"layers":[{"digest":"sha256:../../x"}]}`), blob, true)
	f.Add([]byte(`null`), blob, true)

	var manifestBody, blobBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/firmware/x12-bmc/manifests/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifestBody)
	})
	mux.HandleFunc("GET /v2/firmware/x12-bmc/blobs/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blobBody)
	})
	srv := httptest.NewTLSServer(mux)
	f.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")

	f.Fuzz(func(t *testing.T, man, b []byte, pinManifest bool) {
		manifestBody, blobBody = man, b
		pinned := digest.FromBytes(man)
		if !pinManifest {
			pinned = digest.FromBytes(append([]byte("other "), man...))
		}
		got, err := Pull(t.Context(), srv.Client(), host+"/firmware/x12-bmc@"+pinned.String())
		if err != nil {
			return
		}
		assert.Equal(t, pinned, digest.FromBytes(man), "pulled through a manifest that is not the pinned one")
		var m manifest
		require.NoError(t, json.Unmarshal(man, &m))
		require.Len(t, m.Layers, 1)
		assert.Equal(t, m.Layers[0].Digest, m.Layers[0].Digest.Algorithm().FromBytes(b), "pulled a blob that is not the layer")
		assert.Equal(t, b, got)
	})
}
