package controller

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

const mediaSlot = "VirtualMedia1"

// configAsset is a Ready configISO BootAsset whose artifact Secret holds iso.
func configAsset(t *testing.T, iso []byte) (*osv1alpha1.BootAsset, *corev1.Secret) {
	t.Helper()
	artifact := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config-artifact", Namespace: "pk-metal"},
		Data:       map[string][]byte{artifactKey: iso},
	}
	ba := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node"}},
		Status: osv1alpha1.BootAssetStatus{
			Digest:            godigest.FromBytes(iso).String(),
			ArtifactSecretRef: &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: artifact.Name},
			Conditions:        []metav1.Condition{readyCond()},
		},
	}
	return ba, artifact
}

func readyCond() metav1.Condition {
	return metav1.Condition{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Built", LastTransitionTime: metav1.Now()}
}

// factoryAsset is a Ready factory BootAsset downloaded from url. With a
// store, the download (image) is in it under the asset's key.
func factoryAsset(t *testing.T, store *asset.Store, url string, image []byte) *osv1alpha1.BootAsset {
	t.Helper()
	ba := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "installer"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: "v1.9.0"},
		Status: osv1alpha1.BootAssetStatus{URL: url, BuildKey: imageKey(url), Digest: godigest.FromBytes(image).String(),
			Size: int64(len(image)), Conditions: []metav1.Condition{readyCond()}},
	}
	if store != nil {
		require.NoError(t, store.PrepareBuild("download"))
		require.NoError(t, os.WriteFile(filepath.Join(store.Dir, asset.BuildDir("download"), "image.iso"), image, 0o600))
		_, _, err := store.Import("download", ba.Name, ba.Status.BuildKey)
		require.NoError(t, err)
	}
	return ba
}

func buildISO(t *testing.T, config string) []byte {
	t.Helper()
	iso, err := asset.BuildConfigISO([]byte(config))
	require.NoError(t, err)
	return iso
}

func insertAsset(name, digest string) metalv1alpha1.HostOperationAction {
	return metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
		MediaID: mediaSlot, BootAssetRef: &metalv1alpha1.Reference{Name: name}, Digest: digest,
	}}
}

// isoImage is what isoServer serves for every path.
var isoImage = make([]byte, 2048)

// isoDigest is the digest of isoImage.
var isoDigest = godigest.FromBytes(isoImage).String()

// insertURL mounts url pinned to isoDigest, the digest of what isoServer
// serves.
func insertURL(url string) metalv1alpha1.HostOperationAction {
	return insertImage(url, isoDigest)
}

func insertImage(url, digest string) metalv1alpha1.HostOperationAction {
	return metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{MediaID: mediaSlot, ImageURL: url, Digest: digest}}
}

// isoServer serves isoImage at any path over https, as an image server would.
func isoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(isoImage)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withImages gives r a media service and a media store, and trusts srv for
// the images the operator downloads.
func withImages(t *testing.T, r *HostOperationReconciler, srv *httptest.Server) *asset.Store {
	t.Helper()
	store, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	r.Media, r.Store, r.RegistryClient = startMedia(t), store, srv.Client()
	return store
}

// downloaded puts what url serves into the store of operation "op", as a
// finished download would: the steps then do not wait for one.
func downloaded(t *testing.T, r *HostOperationReconciler, url, digest string) {
	t.Helper()
	name := urlStoreName(client.ObjectKey{Namespace: "pk-metal", Name: "op"})
	_, _, err := r.Store.Fetch(t.Context(), r.imageClient(), url, name, strings.TrimPrefix(digest, "sha256:"), digest)
	require.NoError(t, err)
}

func requireFailed(t *testing.T, op *metalv1alpha1.HostOperation, want string) {
	t.Helper()
	ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	require.Equal(t, metav1.ConditionFalse, ex.Status, "%+v", op.Status)
	assert.Contains(t, ex.Message, want)
}

func TestRebuiltArtifactIsNotMounted(t *testing.T) {
	approved := buildISO(t, "version: v1alpha1\n# approved\n")
	ba, artifact := configAsset(t, buildISO(t, "version: v1alpha1\n# rebuilt\n"))
	op := preClearedOp(insertAsset(ba.Name, godigest.FromBytes(approved).String()))
	r, st, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)

	got := runOp(t, r)
	requireFailed(t, got, "rebuilt since approval")
	assert.Empty(t, rec.inserts, "nothing mounted")
	assert.Empty(t, rec.ejects, "the slot is left as it was")
	assert.Empty(t, mounted(t.Context(), st, mediaSlot))
	_, live := r.Media.Grants.Live(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso")
	assert.False(t, live, "no grant serves the rebuilt bytes")
}

func TestBootAssetPreconditionsFailBeforeAnyBMCWrite(t *testing.T) {
	iso := buildISO(t, "version: v1alpha1\n")
	for name, tc := range map[string]struct {
		objs     func() []client.Object
		media    bool
		unpinned bool
		want     string
	}{
		"asset missing": {
			objs: func() []client.Object { return nil },
			want: "boot asset node-config",
		},
		"asset not ready, mounted unpinned": {
			objs: func() []client.Object {
				ba, artifact := configAsset(t, iso)
				ba.Status.Conditions = nil
				return []client.Object{ba, artifact}
			},
			media:    true,
			unpinned: true,
			want:     "is not Ready",
		},
		"factory asset without a media store": {
			objs: func() []client.Object {
				ba := factoryAsset(t, nil, "https://factory.test/image.iso", iso)
				ba.Name = "node-config"
				return []client.Object{ba}
			},
			media: true,
			want:  "this operator runs none (--media-store)",
		},
		"configISO without a media service": {
			objs: func() []client.Object {
				ba, artifact := configAsset(t, iso)
				return []client.Object{ba, artifact}
			},
			want: "this operator runs none (--media-url)",
		},
		"artifact secret gone": {
			objs: func() []client.Object {
				ba, _ := configAsset(t, iso)
				return []client.Object{ba}
			},
			media: true,
			want:  "not found",
		},
	} {
		t.Run(name, func(t *testing.T) {
			digest := godigest.FromBytes(iso).String()
			if tc.unpinned {
				digest = ""
			}
			op := preClearedOp(insertAsset("node-config", digest))
			r, _, rec := executorFor(t, "supermicro-x12", append(tc.objs(), op)...)
			if tc.media {
				r.Media = startMedia(t)
			}
			requireFailed(t, runOp(t, r), tc.want)
			assert.Empty(t, rec.inserts)
			assert.Empty(t, rec.ejects)
			assert.Empty(t, rec.powers)
		})
	}
}

// TestPinnedMediumMountsWhileANewerBuildRuns: while a newer build of the
// asset runs, its status keeps naming the artifact approved, with
// Ready=False. A medium pinned to that artifact's digest mounts it: the
// digest check is the rail, and it passes. It used to fail every approved
// install that had not mounted yet, whenever the build key moved.
func TestPinnedMediumMountsWhileANewerBuildRuns(t *testing.T) {
	iso := buildISO(t, "version: v1alpha1\n")
	ba, artifact := configAsset(t, iso)
	ba.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: osv1alpha1.ReasonBuilding, Message: "a newer build runs", LastTransitionTime: metav1.Now()}}
	op := preClearedOp(insertAsset(ba.Name, godigest.FromBytes(iso).String()))
	r, st, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)

	op = runOp(t, r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.True(t, rec.insertedGranted(t, ba.Name, st), "the approved artifact is mounted")
}

// TestPinnedMediumWaitsForItsArtifact: an asset that names no artifact yet
// makes the step wait, without touching the slot, instead of failing an
// approved operation. Whether what appears is the approved artifact is
// decided at the grant, by digest.
func TestPinnedMediumWaitsForItsArtifact(t *testing.T) {
	iso := buildISO(t, "version: v1alpha1\n")
	ba, artifact := configAsset(t, iso)
	ba.Status.ArtifactSecretRef = nil
	ba.Status.Conditions = nil
	op := preClearedOp(insertAsset(ba.Name, godigest.FromBytes(iso).String()))
	r, st, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)
	key := client.ObjectKeyFromObject(op)

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	require.False(t, got.Finished(), "%+v", got.Status)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Contains(t, ex.Message, "has no artifact yet")
	assert.Empty(t, rec.ejects, "the slot is left as it was")
	assert.Empty(t, rec.inserts)

	// The artifact appears: the step goes on.
	ba.Status.ArtifactSecretRef = &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: artifact.Name}
	require.NoError(t, r.Update(t.Context(), ba))
	got = runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.True(t, rec.insertedGranted(t, ba.Name, st))
}

// insertedGranted: one medium was mounted, at a grant URL of the operation,
// and the slot holds it (the grant itself ends with the operation).
func (c *recordingClient) insertedGranted(t *testing.T,
	asset string, st bmc.Client) bool {
	t.Helper()
	if !assert.Len(t, c.inserts, 1) {
		return false
	}
	return assert.Contains(t, c.inserts[0], asset+".iso") && assert.Equal(t, c.inserts[0], mountedIn(t, st))
}

func mountedIn(t *testing.T, st bmc.Client) string {
	t.Helper()
	media, err := st.Media(t.Context())
	require.NoError(t, err)
	for _, m := range media {
		if m.ID == mediaSlot && m.Inserted {
			return m.ImageURL
		}
	}
	return ""
}

// TestImageURLIsServedUnderAGrant: an image named by URL is downloaded by
// the operator, checked against its digest, and mounted from the media
// service under a grant without credentials. The BMC never gets the origin
// URL. The download goes when the operation ends.
func TestImageURLIsServedUnderAGrant(t *testing.T) {
	srv := isoServer(t)
	origin := srv.URL + "/installer.iso"
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}

	t.Run("downloaded, then mounted under a grant", func(t *testing.T) {
		r, st, rec := executorFor(t, "supermicro-x12", preClearedOp(insertURL(origin),
			metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}}))
		store := withImages(t, r, srv)
		var creds []*bmc.MediaCredentials
		rec.onInsert = func(_ string, c *bmc.MediaCredentials) { creds = append(creds, c) }

		// The download runs in the background: the step waits for it.
		op := &metalv1alpha1.HostOperation{}
		require.Eventually(t, func() bool {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), key, op))
			return op.Finished()
		}, 10*time.Second, 20*time.Millisecond)
		require.True(t, opSucceeded(op), "%+v", op.Status)
		require.Len(t, rec.inserts, 1)
		assert.Contains(t, rec.inserts[0], "/grants/", "the media service serves it")
		assert.NotContains(t, rec.inserts[0], srv.Listener.Addr().String(), "the BMC does not fetch from the origin")
		assert.Equal(t, isoImage, st.BootImage(mediaSlot), "the machine booted the downloaded bytes")
		require.Len(t, creds, 1)
		assert.Nil(t, creds[0], "an image without a secret gets no credentials")
		assert.False(t, store.Has(urlStoreName(key), strings.TrimPrefix(isoDigest, "sha256:")),
			"the download goes with the operation")
	})
	t.Run("the grant mounted is a no-op", func(t *testing.T) {
		op := preClearedOp(insertURL(origin))
		r, _, rec := executorFor(t, "supermicro-x12", op)
		withImages(t, r, srv)
		downloaded(t, r, origin, isoDigest)
		x := &exec{r: r, op: op, cl: rec, systemID: "1"}
		step := func() error { return x.insertMedia(t.Context(), 0, op.Spec.Actions[0].InsertMedia) }
		require.NoError(t, step())
		require.NoError(t, step())
		assert.Len(t, rec.inserts, 1, "nothing mounted again")
		assert.Empty(t, rec.ejects)
	})
	t.Run("the origin URL mounted is replaced", func(t *testing.T) {
		r, st, rec := executorFor(t, "supermicro-x12", preClearedOp(insertURL(origin)))
		withImages(t, r, srv)
		downloaded(t, r, origin, isoDigest)
		require.NoError(t, st.InsertMedia(t.Context(), mediaSlot, origin, nil))
		require.True(t, opSucceeded(runOp(t, r)))
		assert.Equal(t, []string{mediaSlot}, rec.ejects, "the slot is emptied first")
		require.Len(t, rec.inserts, 1)
		assert.Contains(t, rec.inserts[0], "/grants/")
	})
	t.Run("another digest fails the step", func(t *testing.T) {
		r, _, rec := executorFor(t, "supermicro-x12", preClearedOp(insertImage(origin, godigest.FromString("another").String())))
		withImages(t, r, srv)
		op := &metalv1alpha1.HostOperation{}
		require.Eventually(t, func() bool {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), key, op))
			return op.Finished()
		}, 10*time.Second, 20*time.Millisecond)
		requireFailed(t, op, "want sha256:")
		assert.Empty(t, rec.inserts, "nothing is mounted")
	})
	t.Run("no digest fails the step", func(t *testing.T) {
		r, _, rec := executorFor(t, "supermicro-x12", preClearedOp(insertImage(origin, "")))
		withImages(t, r, srv)
		requireFailed(t, runOp(t, r), "has no digest")
		assert.Empty(t, rec.inserts)
	})
	t.Run("no media service fails the step", func(t *testing.T) {
		r, _, rec := executorFor(t, "supermicro-x12", preClearedOp(insertURL(origin)))
		requireFailed(t, runOp(t, r), "runs no media service")
		assert.Empty(t, rec.inserts)
	})
}

func TestGrantedMediumConvergesOnTheSlot(t *testing.T) {
	iso := buildISO(t, "version: v1alpha1\n")
	digest := godigest.FromBytes(iso).String()

	// Grants end with their operation, so these drive the step itself, as a
	// pass re-entering it before it was recorded would.
	stepOf := func(t *testing.T, r *HostOperationReconciler, rec *recordingClient, op *metalv1alpha1.HostOperation) func() error {
		x := &exec{r: r, op: op, cl: rec, systemID: "1"}
		return func() error { return x.insertMedia(t.Context(), 0, op.Spec.Actions[0].InsertMedia) }
	}

	t.Run("live grant mounted is a no-op", func(t *testing.T) {
		ba, artifact := configAsset(t, iso)
		op := preClearedOp(insertAsset(ba.Name, digest))
		r, _, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
		r.Media = startMedia(t)
		step := stepOf(t, r, rec, op)

		require.NoError(t, step())
		require.Len(t, rec.inserts, 1)
		g, ok := r.Media.Grants.Live(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso")
		require.True(t, ok)

		require.NoError(t, step())
		assert.Len(t, rec.inserts, 1, "nothing mounted again")
		assert.Empty(t, rec.ejects)
		g2, ok := r.Media.Grants.Live(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso")
		require.True(t, ok)
		assert.Equal(t, g.Token, g2.Token, "the grant is kept")
	})

	t.Run("live grant with another URL mounted is re-minted", func(t *testing.T) {
		ba, artifact := configAsset(t, iso)
		op := preClearedOp(insertAsset(ba.Name, digest))
		srv := isoServer(t)
		r, st, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
		r.Media = startMedia(t)
		creds, err := asset.MintCredentials()
		require.NoError(t, err)
		old, err := r.Media.Grants.Issue(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso", iso, time.Hour, creds)
		require.NoError(t, err)
		// Someone mounted something else since.
		require.NoError(t, st.InsertMedia(t.Context(), mediaSlot, srv.URL+"/other.iso", nil))

		require.NoError(t, stepOf(t, r, rec, op)())
		g, ok := r.Media.Grants.Live(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso")
		require.True(t, ok)
		assert.NotEqual(t, old.Token, g.Token, "the old grant is revoked, a new one minted")
		assert.Equal(t, []string{mediaSlot}, rec.ejects)
		assert.Equal(t, []string{r.Media.URL(g)}, rec.inserts)
		assert.Equal(t, r.Media.URL(g), mounted(t.Context(), st, mediaSlot))
	})

	t.Run("failed insert revokes the new grant", func(t *testing.T) {
		ba, artifact := configAsset(t, iso)
		op := preClearedOp(insertAsset(ba.Name, digest))
		r, _, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
		r.Media = startMedia(t)
		rec.insertErr = errors.New("the BMC refused the image")

		requireFailed(t, runOp(t, r), "the BMC refused the image")
		require.Len(t, rec.inserts, 1)
		_, live := r.Media.Grants.Live(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso")
		assert.False(t, live, "nothing keeps serving the config after a failed mount")
	})
}

// TestFactoryInstallerIsServedUnderAGrant: the installer the operator
// downloaded is mounted from the media service, under a grant of the
// operation and without credentials, never from the factory URL. The BMC
// fetches exactly the stored bytes. A stored file that is not the one the
// operation pins is not served.
func TestFactoryInstallerIsServedUnderAGrant(t *testing.T) {
	image := []byte("the factory installer")
	digest := godigest.FromBytes(image).String()
	store, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	ba := factoryAsset(t, store, "https://factory.test/image/aaaa/v1.9.0/metal-amd64.iso", image)

	t.Run("pinned, mounted under a grant", func(t *testing.T) {
		op := preClearedOp(insertAsset(ba.Name, digest))
		r, st, rec := executorFor(t, "supermicro-x12", op, ba.DeepCopy())
		r.Media, r.Store = startMedia(t), store
		var creds []*bmc.MediaCredentials
		rec.onInsert = func(_ string, c *bmc.MediaCredentials) { creds = append(creds, c) }

		require.True(t, opSucceeded(runOp(t, r)))
		assert.True(t, rec.insertedGranted(t, ba.Name, st), "the media service serves it")
		assert.NotContains(t, rec.inserts[0], "factory.test", "the BMC does not fetch from the factory")
		require.Len(t, creds, 1)
		assert.Nil(t, creds[0], "a medium without a secret gets no credentials")
	})
	t.Run("a stored file that is not the pinned one is refused", func(t *testing.T) {
		op := preClearedOp(insertAsset(ba.Name, godigest.FromBytes([]byte("another image")).String()))
		r, _, rec := executorFor(t, "supermicro-x12", op, ba.DeepCopy())
		r.Media, r.Store = startMedia(t), store
		requireFailed(t, runOp(t, r), "rebuilt since approval")
		assert.Empty(t, rec.inserts)
	})
}

// TestUnpinnedConfigISOMountsWhatIsBuilt records that the digest check runs
// only when the action carries a digest. The provisioner always pins one;
// an operation written by hand without it mounts the current build.
func TestUnpinnedConfigISOMountsWhatIsBuilt(t *testing.T) {
	ba, artifact := configAsset(t, buildISO(t, "version: v1alpha1\n"))
	op := preClearedOp(insertAsset(ba.Name, ""))
	r, _, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)

	require.True(t, opSucceeded(runOp(t, r)))
	assert.Len(t, rec.inserts, 1)
}

// mediaStub answers Media with a fixed slot list.
type mediaStub struct {
	bmc.Client
	media []bmc.VirtualMedia
	err   error
}

func (m mediaStub) Media(context.Context) ([]bmc.VirtualMedia, error) { return m.media, m.err }

func TestMountedStripsUserinfo(t *testing.T) {
	const clean = "https://203.0.113.10:8443/grants/tok/node-config.iso"
	cl := mediaStub{media: []bmc.VirtualMedia{
		{ID: "CD", Inserted: true, ImageURL: "https://user:secret@203.0.113.10:8443/grants/tok/node-config.iso"},
		{ID: "Floppy"},
	}}
	assert.Equal(t, clean, mounted(t.Context(), cl, "CD"), "the vmm dialect stores credentials in the URL")
	assert.Empty(t, mounted(t.Context(), cl, "Floppy"), "an empty slot")
	assert.Empty(t, mounted(t.Context(), cl, "USB"), "an unknown slot")
	assert.Empty(t, mounted(t.Context(), mediaStub{err: errors.New("unreachable")}, "CD"), "an unreadable BMC")
}

// TestFailedStepMasksTheBMCError: a BMC refusal that echoes the grant URL
// with its credentials is recorded masked in the step, the condition and the
// audit trail.
func TestFailedStepMasksTheBMCError(t *testing.T) {
	const (
		token    = "0123456789abcdef0123456789abcdef"
		password = "fedcba9876543210fedcba9876543210"
	)
	ba, artifact := configAsset(t, buildISO(t, "version: v1alpha1\n"))
	op := preClearedOp(insertAsset(ba.Name, ""))
	r, _, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)
	var trail bytes.Buffer
	r.Audit = audit.New(&trail, "operator")
	rec.insertErr = errors.New(`400: {"MessageArgs":["https://media:` + password + `@10.0.0.1:30443/grants/` + token + `/node-config.iso"]}`)

	got := runOp(t, r)
	requireFailed(t, got, "https://***@10.0.0.1:30443/grants/***/node-config.iso")
	require.NotEmpty(t, got.Status.Steps)
	for _, text := range []string{got.Status.Steps[len(got.Status.Steps)-1].Message,
		meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted).Message, trail.String()} {
		assert.NotContains(t, text, token)
		assert.NotContains(t, text, password)
	}
	assert.Contains(t, trail.String(), `"event":"step.failed"`)
}

// TestFailedStepEjectsWhatTheRunMounted: a step that fails after a medium
// was mounted leaves no medium behind. The trailing eject steps never run
// after a failure, so the failure path ejects, once, and says so.
func TestFailedStepEjectsWhatTheRunMounted(t *testing.T) {
	ba, artifact := configAsset(t, buildISO(t, "version: v1alpha1\n"))
	op := preClearedOp(insertAsset(ba.Name, ""),
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "NotARealTarget"}})
	r, st, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)

	got := runOp(t, r)
	requireFailed(t, got, "after the failure: ejected virtual media "+mediaSlot)
	assert.Equal(t, []string{mediaSlot}, rec.ejects[len(rec.ejects)-1:], "the mounted slot is ejected")
	assert.Empty(t, mountedIn(t, st), "the slot is empty")
	assert.Contains(t, got.Status.Steps[len(got.Status.Steps)-1].Message, "ejected virtual media")
}
