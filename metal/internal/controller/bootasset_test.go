package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The factory strategy hands a URL to a BMC, which mounts it hours later and
// far away. A schematic the factory never heard of is a 404 there and nowhere
// else, so the URL is only worth stamping once the factory has been told.
func TestFactoryStrategyHoldsWhenTheSchematicCannotBeRegistered(t *testing.T) {
	ctx := context.Background()
	factoryAsset := func() *osv1alpha1.BootAsset {
		return &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "install", Generation: 3},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyFactory,
				TalosVersion: "v1.13.4",
			},
		}
	}

	for name, factory := range map[string]*render.Factory{
		// Nothing listens on port 1; this is a factory that cannot be reached.
		"unreachable": render.NewFactory("127.0.0.1:1"),
		// A manager wired without one — the URL would still compute.
		"unconfigured": nil,
	} {
		t.Run(name, func(t *testing.T) {
			ba := factoryAsset()
			(&BootAssetReconciler{Factory: factory}).fetchImage(ctx, ba)

			c := meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, osv1alpha1.ReasonSchematicUnavailable, c.Reason)
			assert.EqualValues(t, 3, c.ObservedGeneration)
			assert.Empty(t, ba.Status.URL,
				"an image that may not exist must not be handed out as a URL")
		})
	}
}

// imageFactory answers schematic registrations with the locally computed id
// and serves image as every .iso, counting the downloads.
func imageFactory(t *testing.T, image []byte) (*render.Factory, *int) {
	t.Helper()
	downloads := new(int)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /schematics", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s, err := schematic.Unmarshal(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, _ := s.ID()
		normalized, _ := s.Marshal()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "schematic": string(normalized)})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".iso") {
			http.NotFound(w, r)
			return
		}
		*downloads++
		_, _ = w.Write(image)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	f := render.NewFactory(strings.TrimPrefix(srv.URL, "https://"))
	f.HTTPClient = srv.Client()
	return f, downloads
}

// TestFactoryStrategyDownloadsTheInstaller: the operator downloads the
// factory installer into the media store and records its digest, so that the
// media service serves it, pinned, and no BMC fetches it from the factory.
// A second look downloads nothing.
func TestFactoryStrategyDownloadsTheInstaller(t *testing.T) {
	ctx := t.Context()
	image := []byte("the factory installer")
	factory, downloads := imageFactory(t, image)
	store, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	r := &BootAssetReconciler{Factory: factory, Store: store}
	ba := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "installer", Generation: 1},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: "v1.14.0"},
	}

	res := r.fetchImage(ctx, ba)
	c := meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "not Ready while the download runs")
	assert.Contains(t, c.Message, "downloads the installer")
	assert.Positive(t, res.RequeueAfter)

	require.Eventually(t, func() bool {
		r.fetchImage(ctx, ba)
		return meta.IsStatusConditionTrue(ba.Status.Conditions, osv1alpha1.ConditionReady)
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, godigest.FromBytes(image).String(), ba.Status.Digest)
	assert.Equal(t, int64(len(image)), ba.Status.Size)
	assert.True(t, store.Has(ba.Name, ba.Status.BuildKey))
	assert.Contains(t, ba.Status.URL, "/metal-amd64.iso", "the URL says where the operator downloaded it from")

	r.fetchImage(ctx, ba)
	assert.Equal(t, 1, *downloads, "an installer in the store is not downloaded again")
}

func TestFactoryStrategyNeedsAMediaStore(t *testing.T) {
	factory, _ := imageFactory(t, []byte("x"))
	ba := &osv1alpha1.BootAsset{Spec: osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: "v1.14.0"}}
	(&BootAssetReconciler{Factory: factory}).fetchImage(t.Context(), ba)
	c := meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, osv1alpha1.ReasonBuilderUnavailable, c.Reason)
	assert.Contains(t, c.Message, "--media-store")
}
