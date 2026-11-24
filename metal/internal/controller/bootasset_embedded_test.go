package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
)

const testNS = "pk-metal"

// embedFixture is a rendered node, its config Secret and an embeddedInstaller
// asset for it, with a reconciler over a fake API and a real store.
type embedFixture struct {
	r     *BootAssetReconciler
	cl    client.Client
	store *asset.Store
	ba    *osv1alpha1.BootAsset
}

func newEmbedFixture(t *testing.T, config string) *embedFixture {
	t.Helper()
	store, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: osv1alpha1.TalosNodeStatus{
			ConfigSecretRef: &osv1alpha1.SecretReference{Namespace: testNS, Name: "node-2-config"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "node-2-config"},
		Data:       map[string][]byte{asset.ConfigFilename: []byte(config)},
	}
	ba := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2-installer", UID: "uid-ba", Generation: 1},
		Spec: osv1alpha1.BootAssetSpec{
			Strategy:     osv1alpha1.StrategyEmbeddedInstaller,
			NodeRef:      &osv1alpha1.Reference{Name: "node-2"},
			TalosVersion: "v1.14.6",
			Schematic: &osv1alpha1.Schematic{Customization: osv1alpha1.SchematicCustomization{
				ExtraKernelArgs: []string{"console=ttyS0", bootConfigKernelArg},
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(node, sec, ba).WithStatusSubresource(ba).Build()
	return &embedFixture{
		r: &BootAssetReconciler{
			Client: cl, Scheme: testScheme(t), Namespace: testNS, Store: store,
			Imager: &Imager{Repository: DefaultImagerRepository, Digests: DefaultImagerDigests(), StoreClaim: "media",
				HelperImage: "registry.example/pk-metal:test"},
		},
		cl: cl, store: store, ba: ba,
	}
}

func (f *embedFixture) reconcile(t *testing.T) *osv1alpha1.BootAsset {
	t.Helper()
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.ba)})
	require.NoError(t, err)
	ba := &osv1alpha1.BootAsset{}
	require.NoError(t, f.cl.Get(context.Background(), client.ObjectKeyFromObject(f.ba), ba))
	return ba
}

func (f *embedFixture) jobs(t *testing.T) []batchv1.Job {
	t.Helper()
	l := &batchv1.JobList{}
	require.NoError(t, f.cl.List(context.Background(), l, client.InNamespace(testNS)))
	return l.Items
}

// finish plays the imager: it writes an ISO into the build's output and
// marks the Job as the Job controller would.
func (f *embedFixture) finish(t *testing.T, job *batchv1.Job, ok bool, iso string) {
	t.Helper()
	if iso != "" {
		dir := filepath.Join(f.store.Dir, asset.BuildDir(job.Name))
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "metal-amd64.iso"), []byte(iso), 0o644))
	}
	cond := batchv1.JobComplete
	if !ok {
		cond = batchv1.JobFailed
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: cond, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"})
	require.NoError(t, f.cl.Status().Update(context.Background(), job))
}

func ready(ba *osv1alpha1.BootAsset) *metav1.Condition {
	return meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
}

func TestEmbeddedInstallerBuildsInAJobAndImportsTheResult(t *testing.T) {
	config := "version: v1alpha1 # node-2, with its secrets"
	f := newEmbedFixture(t, config)

	ba := f.reconcile(t)
	assert.Equal(t, osv1alpha1.ReasonBuilding, ready(ba).Reason)
	jobs := f.jobs(t)
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, ba.Status.BuildJob, job.Name)
	require.Len(t, job.OwnerReferences, 1, "the Job dies with its BootAsset")
	assert.Equal(t, "BootAsset", job.OwnerReferences[0].Kind)

	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	assert.Equal(t, "ghcr.io/siderolabs/imager:v1.14.6@"+DefaultImagerDigests()["v1.14.6"], c.Image, "pinned by digest, version from the asset")
	all := strings.Join(c.Command, " ")
	// The imager runs as a child of the manager binary's imager-run, which
	// keeps the profile dump (it holds the config) out of the log.
	require.Len(t, pod.InitContainers, 1)
	assert.Equal(t, "registry.example/pk-metal:test", pod.InitContainers[0].Image)
	assert.Equal(t, []string{"/manager", "imager-stage", "/helper/manager"}, pod.InitContainers[0].Command)
	assert.Equal(t, []string{"/helper/manager", "imager-run", "/config/config.yaml", "/bin/imager", "iso"}, c.Command[:5])
	assert.NotContains(t, all, "version: v1alpha1", "the config never travels in arguments")
	for _, e := range c.Env {
		assert.NotContains(t, e.Value, "v1alpha1", "nor in the environment")
	}
	assert.Contains(t, all, "--embedded-config-path /config/config.yaml")
	assert.Contains(t, all, "--extra-kernel-arg console=ttyS0")
	assert.NotContains(t, all, bootConfigKernelArg, "an embedded config needs no config medium")
	assert.Contains(t, all, "--output /store/builds/"+job.Name)

	// The config arrives as a Secret volume, read-only and owner-only.
	var cfgVol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Secret != nil {
			cfgVol = &pod.Volumes[i]
		}
	}
	require.NotNil(t, cfgVol)
	assert.Equal(t, "node-2-config", cfgVol.Secret.SecretName)
	assert.EqualValues(t, 0o400, *cfgVol.Secret.DefaultMode)

	// Least privilege: the restricted Pod Security profile holds.
	assert.True(t, *pod.SecurityContext.RunAsNonRoot)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
	assert.False(t, *pod.AutomountServiceAccountToken)
	for _, ctr := range append(pod.InitContainers, c) {
		assert.False(t, *ctr.SecurityContext.AllowPrivilegeEscalation, ctr.Name)
		assert.Equal(t, []corev1.Capability{"ALL"}, ctr.SecurityContext.Capabilities.Drop, ctr.Name)
		assert.Nil(t, ctr.SecurityContext.Privileged, ctr.Name)
	}
	for _, v := range pod.Volumes {
		assert.Nil(t, v.HostPath, "no host paths: no loop devices are needed for an ISO")
		if v.Name == "tmp" {
			assert.Equal(t, corev1.StorageMediumMemory, v.EmptyDir.Medium, "what the imager derives from the config stays off disk")
		}
	}

	// Running: nothing changes.
	ba = f.reconcile(t)
	assert.Equal(t, osv1alpha1.ReasonBuilding, ready(ba).Reason)
	assert.Contains(t, ready(ba).Message, "kubectl -n pk-metal logs job/"+job.Name)

	f.finish(t, &job, true, "the installer")
	ba = f.reconcile(t)
	require.Equal(t, metav1.ConditionTrue, ready(ba).Status, "%+v", ready(ba))
	assert.Equal(t, digest.FromString("the installer").String(), ba.Status.Digest)
	assert.EqualValues(t, len("the installer"), ba.Status.Size)
	assert.Equal(t, digest.FromString(config).String(), ba.Status.ConfigDigest)
	assert.True(t, f.store.Has(ba.Name, ba.Status.BuildKey))
	assert.Empty(t, f.jobs(t), "a finished build's Job goes")

	t.Run("a new render is a new build; the old artifact stays until it lands", func(t *testing.T) {
		oldKey, oldDigest := ba.Status.BuildKey, ba.Status.Digest
		sec := &corev1.Secret{}
		require.NoError(t, f.cl.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "node-2-config"}, sec))
		sec.Data[asset.ConfigFilename] = []byte("version: v1alpha1 # node-2, rotated")
		require.NoError(t, f.cl.Update(context.Background(), sec))

		ba := f.reconcile(t)
		assert.Equal(t, osv1alpha1.ReasonBuilding, ready(ba).Reason)
		assert.Equal(t, oldDigest, ba.Status.Digest, "status names what is in the store")
		jobs := f.jobs(t)
		require.Len(t, jobs, 1)
		assert.NotEqual(t, job.Name, jobs[0].Name)

		f.finish(t, &jobs[0], true, "the rotated installer")
		ba = f.reconcile(t)
		assert.Equal(t, metav1.ConditionTrue, ready(ba).Status)
		assert.NotEqual(t, oldKey, ba.Status.BuildKey)
		assert.False(t, f.store.Has(ba.Name, oldKey), "the superseded installer is removed")
	})
}

func TestEmbeddedInstallerFailedBuildSaysHowToRetry(t *testing.T) {
	f := newEmbedFixture(t, "version: v1alpha1")
	f.reconcile(t)
	jobs := f.jobs(t)
	require.Len(t, jobs, 1)
	f.finish(t, &jobs[0], false, "")

	ba := f.reconcile(t)
	c := ready(ba)
	assert.Equal(t, osv1alpha1.ReasonBuildFailed, c.Reason)
	assert.Contains(t, c.Message, "kubectl -n pk-metal delete job "+jobs[0].Name)
	assert.Len(t, f.jobs(t), 1, "the failed Job stays, so its log can be read")
}

func TestEmbeddedInstallerHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(f *embedFixture)
		reason string
		want   string
	}{
		"no media store": {
			mutate: func(f *embedFixture) { f.r.Store = nil },
			reason: osv1alpha1.ReasonBuilderUnavailable, want: "--media-store",
		},
		"no helper image": {
			mutate: func(f *embedFixture) { f.r.Imager.HelperImage = "" },
			reason: osv1alpha1.ReasonBuilderUnavailable, want: "--imager-helper-image",
		},
		"no pinned imager": {
			mutate: func(f *embedFixture) { f.r.Imager.Digests = map[string]string{} },
			reason: osv1alpha1.ReasonBuilderUnavailable, want: "--imager-digest=v1.14.6=sha256:",
		},
		"a release without embedded config": {
			mutate: func(f *embedFixture) {
				f.ba.Spec.TalosVersion = "v1.11.0"
				require.NoError(t, f.cl.Update(context.Background(), f.ba))
			},
			reason: osv1alpha1.ReasonBuilderUnavailable, want: "Talos 1.12 or later",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEmbedFixture(t, "version: v1alpha1")
			tc.mutate(f)
			ba := f.reconcile(t)
			c := ready(ba)
			require.NotNil(t, c)
			assert.Equal(t, tc.reason, c.Reason)
			assert.Contains(t, c.Message, tc.want)
			assert.Empty(t, f.jobs(t))
		})
	}
}

// builtFixture is a fixture whose installer is built and imported.
func builtFixture(t *testing.T) (*embedFixture, *osv1alpha1.BootAsset) {
	t.Helper()
	f := newEmbedFixture(t, "version: v1alpha1")
	f.reconcile(t)
	jobs := f.jobs(t)
	require.Len(t, jobs, 1)
	f.finish(t, &jobs[0], true, "the installer")
	ba := f.reconcile(t)
	require.Equal(t, metav1.ConditionTrue, ready(ba).Status)
	require.Empty(t, f.jobs(t))
	return f, ba
}

// After a move to another cluster the BootAsset's status says Ready, and the
// store there is a new, empty volume. The asset must not go on naming a file
// that is not there: it builds again, and names no artifact until then.
func TestEmbeddedInstallerMissingFromTheStoreIsBuiltAgain(t *testing.T) {
	f, ba := builtFixture(t)
	key, built := ba.Status.BuildKey, ba.Status.Digest
	empty, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	f.r.Store, f.store = empty, empty

	ba = f.reconcile(t)
	c := ready(ba)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, osv1alpha1.ReasonBuilding, c.Reason)
	assert.Contains(t, c.Message, "is not in the media store")
	assert.Contains(t, c.Message, built)
	assert.Empty(t, ba.Status.Digest, "no artifact is named while none is there")
	assert.Empty(t, ba.Status.BuildKey)
	jobs := f.jobs(t)
	require.Len(t, jobs, 1, "a new build")

	// The rebuilt image need not be byte for byte the old one.
	f.finish(t, &jobs[0], true, "the installer, built again")
	ba = f.reconcile(t)
	require.Equal(t, metav1.ConditionTrue, ready(ba).Status, "%+v", ready(ba))
	assert.Equal(t, key, ba.Status.BuildKey)
	assert.Equal(t, digest.FromString("the installer, built again").String(), ba.Status.Digest)

	ba = f.reconcile(t)
	assert.Equal(t, metav1.ConditionTrue, ready(ba).Status, "a rebuilt artifact is not taken for a changed one")
	assert.Empty(t, f.jobs(t))
}

// A file whose bytes are not the ones built and recorded is nobody's
// approved artifact: it goes, and the installer is built again.
func TestEmbeddedInstallerWithAnotherDigestIsBuiltAgain(t *testing.T) {
	f, ba := builtFixture(t)
	path := filepath.Join(f.store.Dir, "assets", ba.Name, ba.Status.BuildKey+".iso")
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("not the installer"), 0o400))

	ba = f.reconcile(t)
	c := ready(ba)
	assert.Equal(t, osv1alpha1.ReasonBuilding, c.Reason)
	assert.Contains(t, c.Message, "not the "+digest.FromString("the installer").String()+" that was built")
	assert.Empty(t, ba.Status.Digest)
	assert.NoFileExists(t, path)
	assert.Len(t, f.jobs(t), 1)
}

// A store the imager Jobs cannot mount (an emptyDir after a move with
// --media-store=ephemeral) builds nothing, and the asset says so.
func TestEmbeddedInstallerWithoutAClaimSaysSo(t *testing.T) {
	f, _ := builtFixture(t)
	empty, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	f.r.Store, f.store = empty, empty
	f.r.Imager.StoreClaim = ""

	ba := f.reconcile(t)
	c := ready(ba)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, osv1alpha1.ReasonBuilderUnavailable, c.Reason)
	assert.Contains(t, c.Message, "not on a PersistentVolumeClaim")
	assert.Contains(t, c.Message, "--media-store-claim")
	assert.Empty(t, f.jobs(t))
}

// A deleted BootAsset has no owner reference to take its file along; the
// reconcile of its disappearance does.
func TestDeletedEmbeddedInstallerLeavesTheStore(t *testing.T) {
	f := newEmbedFixture(t, "version: v1alpha1")
	f.reconcile(t)
	jobs := f.jobs(t)
	f.finish(t, &jobs[0], true, "iso")
	ba := f.reconcile(t)
	require.True(t, f.store.Has(ba.Name, ba.Status.BuildKey))

	require.NoError(t, f.cl.Delete(context.Background(), ba))
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ba)})
	require.NoError(t, err)
	assert.False(t, f.store.Has(ba.Name, ba.Status.BuildKey))
}

func TestImagerJobNameFitsALabel(t *testing.T) {
	long := strings.Repeat("a", 200)
	name := imagerJobName(long, "0123456789abcdef")
	assert.LessOrEqual(t, len(name), 63)
	assert.True(t, strings.HasSuffix(name, "-0123456789"))
	assert.Equal(t, "build-node-2-installer-0123456789", imagerJobName("node-2-installer", "0123456789abcdef"))
}

func TestBuildKeyFollowsEveryInput(t *testing.T) {
	base := imagerInputs{ConfigDigest: "sha256:a", TalosVersion: "v1.14.6", Image: "img@sha256:1",
		KernelArgs: []string{"console=ttyS0"}, Extensions: []string{"ext@sha256:2"}}
	seen := map[string]string{base.key(): "base"}
	for name, in := range map[string]imagerInputs{
		"config":     {ConfigDigest: "sha256:b", TalosVersion: base.TalosVersion, Image: base.Image, KernelArgs: base.KernelArgs, Extensions: base.Extensions},
		"version":    {ConfigDigest: base.ConfigDigest, TalosVersion: "v1.14.7", Image: base.Image, KernelArgs: base.KernelArgs, Extensions: base.Extensions},
		"image":      {ConfigDigest: base.ConfigDigest, TalosVersion: base.TalosVersion, Image: "img@sha256:9", KernelArgs: base.KernelArgs, Extensions: base.Extensions},
		"kernelArgs": {ConfigDigest: base.ConfigDigest, TalosVersion: base.TalosVersion, Image: base.Image, Extensions: base.Extensions},
		"extensions": {ConfigDigest: base.ConfigDigest, TalosVersion: base.TalosVersion, Image: base.Image, KernelArgs: base.KernelArgs},
	} {
		k := in.key()
		_, dup := seen[k]
		assert.False(t, dup, "%s does not change the key", name)
		seen[k] = name
	}
}
