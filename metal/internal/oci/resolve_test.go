package oci

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const index = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`

func TestResolvePinsTheTag(t *testing.T) {
	want := digest.FromString(index)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/metal-installer/abc/manifests/v1.14.6" {
			http.NotFound(w, r)
			return
		}
		assert.Contains(t, r.Header.Get("Accept"), "image.index", "an index is acceptable: installers are multi-platform")
		w.Header().Set("Docker-Content-Digest", want.String())
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(index))
		}
	}))
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "https://") + "/metal-installer/abc:v1.14.6"

	pinned, err := Resolve(t.Context(), srv.Client(), ref)
	require.NoError(t, err)
	assert.Equal(t, ref+"@"+want.String(), pinned)

	again, err := Resolve(t.Context(), srv.Client(), pinned)
	require.NoError(t, err)
	assert.Equal(t, pinned, again, "a pinned reference stays as it is")
}

func TestResolveHashesWhenNoDigestHeader(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(index))
	}))
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "https://") + "/installer:v1"
	pinned, err := Resolve(t.Context(), srv.Client(), ref)
	require.NoError(t, err)
	assert.Equal(t, ref+"@"+digest.FromString(index).String(), pinned)
}

func TestResolveTakesAnAnonymousToken(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			assert.Equal(t, "repository:siderolabs/installer:pull", r.URL.Query().Get("scope"))
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case r.Header.Get("Authorization") != "Bearer anon":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry",scope="repository:siderolabs/installer:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Header().Set("Docker-Content-Digest", digest.FromString("x").String())
		}
	}))
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "https://") + "/siderolabs/installer:v1.14.6"
	pinned, err := Resolve(t.Context(), srv.Client(), ref)
	require.NoError(t, err)
	assert.Equal(t, ref+"@"+digest.FromString("x").String(), pinned)
}

func TestResolveRefusesWhatItCannotPin(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := Resolve(t.Context(), srv.Client(), strings.TrimPrefix(srv.URL, "https://")+"/installer:v1")
	assert.ErrorContains(t, err, "404")
	_, err = Resolve(t.Context(), srv.Client(), "not a reference")
	assert.Error(t, err)
}
