package oci

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registry serves one single-layer artifact the way the internal Zot would.
func registry(t *testing.T, blob []byte, layers int) (srv *httptest.Server, ref string) {
	t.Helper()
	blobDigest := digest.FromBytes(blob).String()
	layer := fmt.Sprintf(`{"mediaType":"application/octet-stream","digest":%q,"size":%d}`, blobDigest, len(blob))
	man := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[%s]}`,
		strings.TrimSuffix(strings.Repeat(layer+",", layers), ","))
	manDigest := digest.FromBytes([]byte(man)).String()

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
		bad := digest.FromBytes([]byte("something else")).String()
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

	t.Run("malformed layer digest refused before the blob GET", func(t *testing.T) {
		man := `{"schemaVersion":2,"layers":[{"digest":"sha256:not-hex"}]}`
		var blobGets int
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/firmware/x12-bmc/manifests/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(man))
		})
		mux.HandleFunc("GET /v2/firmware/x12-bmc/blobs/", func(http.ResponseWriter, *http.Request) { blobGets++ })
		bad := httptest.NewTLSServer(mux)
		t.Cleanup(bad.Close)
		ref := strings.TrimPrefix(bad.URL, "https://") + "/firmware/x12-bmc@" + digest.FromString(man).String()
		_, err := Pull(ctx, bad.Client(), ref)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "layer digest")
		assert.Zero(t, blobGets, "a malformed layer digest must not reach a URL")
	})

	t.Run("missing artifact is an error", func(t *testing.T) {
		_, err := Pull(ctx, srv.Client(), strings.TrimPrefix(srv.URL, "https://")+"/firmware/nope@"+digest.FromBytes([]byte("x")).String())
		require.Error(t, err)
	})
}

func TestParseRef(t *testing.T) {
	pin := digest.FromString("firmware").String()
	for _, tc := range []struct {
		ref        string
		host, repo string
	}{
		{"registry.pk.internal:5000/firmware/x12-bmc@" + pin, "registry.pk.internal:5000", "firmware/x12-bmc"},
		{"[fd00::1]:5000/fw@" + pin, "[fd00::1]:5000", "fw"},
		{"localhost/fw@" + pin, "localhost", "fw"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			host, repo, d, err := parseRef(tc.ref)
			require.NoError(t, err)
			assert.Equal(t, tc.host, host)
			assert.Equal(t, tc.repo, repo)
			assert.Equal(t, pin, d.String())
		})
	}

	for name, ref := range map[string]string{
		"tag only":             "registry.pk.internal:5000/firmware/x12-bmc:latest",
		"short digest":         "registry.pk.internal:5000/firmware/x12-bmc@sha256:abc",
		"uppercase repository": "registry.pk.internal:5000/Firmware/X12@" + pin,
		"no registry":          "firmware/x12-bmc@" + pin,
		"no repository":        "registry.pk.internal:5000@" + pin,
		"two digests":          "registry.pk.internal:5000/fw@" + pin + "@" + pin,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := parseRef(ref)
			assert.Error(t, err, "a reference that is not host/repo@sha256:<64 hex> must be refused")
		})
	}
}
