package controller

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// factoryAsset is a Ready factory BootAsset served at url.
func factoryAsset(url string) *osv1alpha1.BootAsset {
	return &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "installer"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyFactory, TalosVersion: "v1.9.0"},
		Status:     osv1alpha1.BootAssetStatus{URL: url, Conditions: []metav1.Condition{readyCond()}},
	}
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

func insertURL(url string) metalv1alpha1.HostOperationAction {
	return metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{MediaID: mediaSlot, ImageURL: url}}
}

// isoServer serves any .iso path over https, as an image server would.
func isoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 2048))
	}))
	t.Cleanup(srv.Close)
	return srv
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
		"factory asset without url": {
			objs: func() []client.Object {
				ba := factoryAsset("")
				ba.Name = "node-config"
				return []client.Object{ba}
			},
			want: "carries no url",
		},
		"configISO without a media service": {
			objs: func() []client.Object {
				ba, artifact := configAsset(t, iso)
				return []client.Object{ba, artifact}
			},
			want: "runs no media service",
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

func TestPlainImageURLConvergesOnTheSlot(t *testing.T) {
	srv := isoServer(t)
	want := srv.URL + "/installer.iso"

	t.Run("already mounted", func(t *testing.T) {
		r, st, rec := executorFor(t, "supermicro-x12", preClearedOp(insertURL(want)))
		require.NoError(t, st.InsertMedia(t.Context(), mediaSlot, want, nil))
		require.True(t, opSucceeded(runOp(t, r)))
		assert.Empty(t, rec.ejects, "the goal was met: no eject")
		assert.Empty(t, rec.inserts, "and no insert")
	})
	t.Run("another image mounted", func(t *testing.T) {
		r, st, rec := executorFor(t, "supermicro-x12", preClearedOp(insertURL(want)))
		require.NoError(t, st.InsertMedia(t.Context(), mediaSlot, srv.URL+"/other.iso", nil))
		require.True(t, opSucceeded(runOp(t, r)))
		assert.Equal(t, []string{mediaSlot}, rec.ejects, "the slot is emptied first")
		assert.Equal(t, []string{want}, rec.inserts)
		assert.Equal(t, want, mounted(t.Context(), st, mediaSlot))
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

// TestFactoryURLIsNotPinned records that a factory asset carries no digest
// pin: the step mounts the URL the asset names when it runs, not the one it
// named at approval. The URL is derived from the schematic id and the Talos
// version, so it names one build, and the image holds no secret.
func TestFactoryURLIsNotPinned(t *testing.T) {
	srv := isoServer(t)
	ba := factoryAsset(srv.URL + "/image/aaaa/v1.9.0/metal-amd64.iso")
	op := preClearedOp(insertAsset(ba.Name, ""))
	r, st, _ := executorFor(t, "supermicro-x12", op, ba)

	// The asset moves to another version after approval.
	ba.Status.URL = srv.URL + "/image/aaaa/v1.9.1/metal-amd64.iso"
	require.NoError(t, r.Update(t.Context(), ba))

	require.True(t, opSucceeded(runOp(t, r)))
	assert.Equal(t, ba.Status.URL, mounted(t.Context(), st, mediaSlot), "the URL at run time is mounted")
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
