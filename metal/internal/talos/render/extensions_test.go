package render_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The imager takes extension images, the schematic names. The factory's own
// catalog for the release joins the two, pinned by digest, and is asked once
// per release.
func TestExtensionImages(t *testing.T) {
	ctx := context.Background()
	var asks atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		assert.Equal(t, "/version/v1.14.6/extensions/official", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "siderolabs/drbd", "ref": "registry.example/siderolabs/drbd:9.2.0-v1.14.6", "digest": "sha256:aaaa"},
			{"name": "siderolabs/iscsi-tools", "ref": "registry.example/siderolabs/iscsi-tools:v0.2.0", "digest": "sha256:bbbb"},
		})
	}))
	t.Cleanup(srv.Close)
	f := &render.Factory{RegistryURL: strings.TrimPrefix(srv.URL, "https://"), HTTPClient: srv.Client()}

	refs, err := f.ExtensionImages(ctx, "v1.14.6", []string{"siderolabs/iscsi-tools", "siderolabs/drbd"})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"registry.example/siderolabs/iscsi-tools:v0.2.0@sha256:bbbb",
		"registry.example/siderolabs/drbd:9.2.0-v1.14.6@sha256:aaaa",
	}, refs, "in the order asked, pinned by digest")

	_, err = f.ExtensionImages(ctx, "v1.14.6", []string{"siderolabs/nope"})
	assert.ErrorContains(t, err, "not an official extension")
	assert.EqualValues(t, 1, asks.Load(), "a release's catalog is asked for once")

	refs, err = f.ExtensionImages(ctx, "v1.14.6", nil)
	require.NoError(t, err)
	assert.Empty(t, refs)
	assert.EqualValues(t, 1, asks.Load(), "no extensions, no question")
}
