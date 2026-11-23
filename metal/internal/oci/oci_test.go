package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registry serves one single-layer artifact the way the internal Zot would.
func registry(t *testing.T, blob []byte, layers int) (srv *httptest.Server, ref string) {
	t.Helper()
	blobDigest := digest(blob)
	layer := fmt.Sprintf(`{"mediaType":"application/octet-stream","digest":%q,"size":%d}`, blobDigest, len(blob))
	man := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[%s]}`,
		strings.TrimSuffix(strings.Repeat(layer+",", layers), ","))
	manDigest := digest([]byte(man))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/firmware/x12-bmc/manifests/"+manDigest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(man))
	})
	mux.HandleFunc("GET /v2/firmware/x12-bmc/blobs/"+blobDigest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	return srv, host + "/firmware/x12-bmc@" + manDigest
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPull(t *testing.T) {
	image := []byte("SIMFW 02.00.00 test image")
	srv, ref := registry(t, image, 1)
	ctx := t.Context()

	t.Run("verified round-trip", func(t *testing.T) {
		got, err := Pull(ctx, srv.Client(), ref)
		require.NoError(t, err)
		assert.Equal(t, image, got)
	})

	t.Run("manifest digest mismatch refused", func(t *testing.T) {
		// Point the reference at a digest the served manifest does not hash to.
		host, _, _ := strings.Cut(strings.TrimPrefix(srv.URL, "https://")+"/", "/")
		bad := digest([]byte("something else"))
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"layers":[]}`))
		})
		liar := httptest.NewTLSServer(mux)
		t.Cleanup(liar.Close)
		_ = host
		_, err := Pull(ctx, liar.Client(), strings.TrimPrefix(liar.URL, "https://")+"/firmware/x12-bmc@"+bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "digest")
	})

	t.Run("multi-layer manifest refused", func(t *testing.T) {
		srv2, ref2 := registry(t, image, 2)
		_, err := Pull(ctx, srv2.Client(), ref2)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "single-layer")
	})

	t.Run("unpinned reference refused", func(t *testing.T) {
		_, err := Pull(ctx, srv.Client(), "registry.pk.internal:5000/firmware/x12-bmc:latest")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "digest-pinned")
	})

	t.Run("missing artifact is an error", func(t *testing.T) {
		_, err := Pull(ctx, srv.Client(), strings.TrimPrefix(srv.URL, "https://")+"/firmware/nope@"+digest([]byte("x")))
		require.Error(t, err)
	})
}
