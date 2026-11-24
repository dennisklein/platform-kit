package render_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// fakeFactory answers schematic POSTs. answer rewrites the id it replies with,
// so a factory that disagrees with us can be played; nil answers honestly.
func fakeFactory(t *testing.T, answer func(id string) string) (*render.Factory, *atomic.Int64, *atomic.Value) {
	t.Helper()
	var posts atomic.Int64
	var lastBody atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/schematics", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		lastBody.Store(string(body))
		var sch schematic.Schematic
		require.NoError(t, yaml.Unmarshal(body, &sch))
		id, err := sch.ID()
		require.NoError(t, err)
		if answer != nil {
			id = answer(id)
		}
		normalized, err := sch.Marshal()
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "schematic": string(normalized)})
	}))
	t.Cleanup(srv.Close)
	return &render.Factory{
		RegistryURL: strings.TrimPrefix(srv.URL, "https://"),
		HTTPClient:  srv.Client(),
	}, &posts, &lastBody
}

func TestRegisterSchematic(t *testing.T) {
	ctx := context.Background()
	args := &osv1alpha1.Schematic{
		Customization: osv1alpha1.SchematicCustomization{
			ExtraKernelArgs: []string{"talos.config=metal-iso", "console=ttyS0"},
		},
	}

	t.Run("the registered id is the one the URL addresses", func(t *testing.T) {
		f, posts, body := fakeFactory(t, nil)
		id, err := f.Register(ctx, args)
		require.NoError(t, err)
		assert.Len(t, id, 64)
		assert.EqualValues(t, 1, posts.Load())
		// What is posted is the schematic, not our CRD wrapper around it.
		assert.Contains(t, body.Load().(string), "console=ttyS0")

		url, err := f.ISOURL(args, "v1.14.6")
		require.NoError(t, err)
		assert.Contains(t, url, id,
			"the image URL must address the schematic that was registered")
		installer, err := f.InstallerImage(args, "v1.14.6")
		require.NoError(t, err)
		assert.Equal(t, f.RegistryURL+"/metal-installer/"+id+":v1.14.6", installer,
			"the installer a config names must be the boot medium's build")
	})

	t.Run("registration is remembered, not repeated", func(t *testing.T) {
		// The render path re-runs on every event; the factory should not be
		// asked the same question on every one of them.
		f, posts, _ := fakeFactory(t, nil)
		for range 5 {
			_, err := f.Register(ctx, args)
			require.NoError(t, err)
		}
		assert.EqualValues(t, 1, posts.Load())

		// A different schematic is a different question.
		_, err := f.Register(ctx, nil)
		require.NoError(t, err)
		assert.EqualValues(t, 2, posts.Load())
	})

	t.Run("a disagreeing factory is an error, not a shrug", func(t *testing.T) {
		// If the factory hashes the schematic differently than we do, our
		// mapping has drifted and every URL we hand out addresses an image
		// built from something other than what was asked for.
		f, _, _ := fakeFactory(t, func(string) string { return strings.Repeat("a", 64) })
		_, err := f.Register(ctx, args)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "drifted")
	})

	t.Run("a refusal is an error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		f := &render.Factory{RegistryURL: strings.TrimPrefix(srv.URL, "https://"), HTTPClient: srv.Client()}
		_, err := f.Register(ctx, args)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "503")
	})

	t.Run("an unreachable factory is an error", func(t *testing.T) {
		f := render.NewFactory("127.0.0.1:1")
		_, err := f.Register(ctx, args)
		require.Error(t, err)
	})

	t.Run("no factory at all is an error", func(t *testing.T) {
		var f *render.Factory
		_, err := f.Register(ctx, args)
		require.Error(t, err)
	})
}

// TestRegisterWire pins what goes over the wire for one schematic: the body
// and the id. The id names every image URL already handed out, so a change in
// how the request is built must not change it.
func TestRegisterWire(t *testing.T) {
	const wantID = "316db6ac7fce93457e38a70ace023e57e4b76853a8c1f0108437e7a06e330d53"
	const wantBody = "customization:\n    extraKernelArgs:\n        - talos.config=metal-iso\n        - console=ttyS0\n" +
		"    systemExtensions:\n        officialExtensions:\n            - siderolabs/intel-ucode\n"
	f, _, body := fakeFactory(t, nil)
	id, err := f.Register(context.Background(), &osv1alpha1.Schematic{
		Customization: osv1alpha1.SchematicCustomization{
			ExtraKernelArgs: []string{"talos.config=metal-iso", "console=ttyS0"},
			SystemExtensions: osv1alpha1.SystemExtensions{
				OfficialExtensions: []string{"siderolabs/intel-ucode"},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, wantID, id, "the schematic id must not change for the same input")
	assert.Equal(t, wantBody, body.Load(), "the posted schematic must not change for the same input")
}

// The host the operator is given must reach both halves — the URL it hands out
// and the endpoint it posts to. A self-hosted factory that only got one of them
// would serve images nobody registered with it.
func TestFactoryHostReachesBothHalves(t *testing.T) {
	f, _, _ := fakeFactory(t, nil)
	url, err := f.ISOURL(nil, "v1.14.6")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(url, "https://"+f.RegistryURL+"/image/"), "got %s", url)

	_, err = f.Register(context.Background(), nil)
	require.NoError(t, err, "the same host must answer the schematic endpoint")
}
