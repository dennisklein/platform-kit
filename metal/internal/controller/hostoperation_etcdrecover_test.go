package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup/etcdbackuptest"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	talosfake "github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// recoverWorld is a cluster of two control planes whose etcd is gone, a
// verified snapshot of it, and an approved etcdRecover operation for cp-1.
type recoverWorld struct {
	r     *HostOperationReconciler
	cps   map[string]*talosfake.Machine
	fleet *talosfake.Fleet
	snap  []byte
	sha   string
}

func newRecoverWorld(t *testing.T, bootstrapped bool) *recoverWorld {
	t.Helper()
	snap := etcdbackuptest.Snapshot(9)
	sum := sha256.Sum256(snap)
	sha := "sha256:" + hex.EncodeToString(sum[:])

	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: "v1.14.6", KubernetesVersion: "v1.36.2", Endpoint: "https://192.0.2.10:6443",
			SecretsRef: osv1alpha1.SecretReference{Namespace: "pk-metal", Name: "dr-secrets"},
		},
	}
	if bootstrapped {
		cluster.Status.BootstrappedAt = &metav1.Time{Time: metav1.Now().Add(-24 * 3600e9)}
	}
	objs := []client.Object{
		cluster,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "dr-secrets"},
			Data: map[string][]byte{render.BundleKey: bundle}},
		&osv1alpha1.EtcdSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-daily-20261123-060000"},
			Spec:       osv1alpha1.EtcdSnapshotSpec{PolicyRef: osv1alpha1.Reference{Name: "dr-daily"}},
			Status: osv1alpha1.EtcdSnapshotStatus{ClusterName: "dr", SHA256: sha, Size: int64(len(snap)),
				Conditions: []metav1.Condition{
					{Type: osv1alpha1.ConditionComplete, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonStored},
					{Type: osv1alpha1.ConditionVerified, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonIntact},
				}},
		},
		preClearedOp(metalv1alpha1.HostOperationAction{EtcdRecover: &metalv1alpha1.EtcdRecoverAction{
			NodeRef:     metalv1alpha1.Reference{Name: "cp-1"},
			SnapshotRef: metalv1alpha1.Reference{Name: "dr-daily-20261123-060000"},
			SHA256:      sha,
		}}),
	}
	fleet := talosfake.NewFleet()
	cps := map[string]*talosfake.Machine{}
	for i, name := range []string{"cp-1", "cp-2"} {
		addr := "192.0.2.1" + string(rune('1'+i))
		objs = append(objs, &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "dr"},
				Role: osv1alpha1.RoleControlPlane, Address: addr},
		})
		m := talosfake.NewMachine("v1.14.6")
		m.Configure([]byte("config"))
		m.SetStage("running", false)
		fleet.Add(addr, name, m)
		cps[name] = m
	}

	r, _, _ := executorFor(t, "huawei-2288h-v5", objs...)
	r.TalosDial = fleet.Dialer()
	r.CredentialsNamespace = "pk-metal"
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Spec.OS = &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: "cp-1"}}
	require.NoError(t, r.Update(t.Context(), host))
	return &recoverWorld{r: r, cps: cps, fleet: fleet, snap: snap, sha: sha}
}

// pass runs one reconcile and returns the operation and its Executed condition.
func (w *recoverWorld) pass(t *testing.T) (*metalv1alpha1.HostOperation, *metav1.Condition) {
	t.Helper()
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	_, err := w.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	op := &metalv1alpha1.HostOperation{}
	require.NoError(t, w.r.Get(t.Context(), key, op))
	return op, meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
}

// upload is what pk-metal restore upload does: the plaintext to the node,
// then the record on the operation.
func (w *recoverWorld) upload(t *testing.T, sha string) {
	t.Helper()
	require.NoError(t, talos.With(t.Context(), w.fleet.Dialer(),
		talos.Target{Endpoint: "192.0.2.11", Talosconfig: []byte("person's talosconfig")},
		func(c talos.Client) error { return c.EtcdRecover(t.Context(), bytes.NewReader(w.snap)) }))
	op := &metalv1alpha1.HostOperation{}
	require.NoError(t, w.r.Get(t.Context(), client.ObjectKey{Namespace: "pk-metal", Name: "op"}, op))
	op.Annotations = map[string]string{metalv1alpha1.AnnotationEtcdSnapshotUploaded: sha}
	require.NoError(t, w.r.Update(t.Context(), op))
}

func TestEtcdRecoverRestoresALostCluster(t *testing.T) {
	w := newRecoverWorld(t, true)

	// etcd still runs on cp-2: the restore waits for it to be wiped.
	w.cps["cp-2"].SetEtcdRunning(true)
	_, ex := w.pass(t)
	require.NotNil(t, ex)
	assert.Equal(t, metav1.ConditionUnknown, ex.Status)
	assert.Contains(t, ex.Message, "etcd still runs on cp-2")
	w.cps["cp-2"].SetEtcdRunning(false)

	// A control plane that does not answer may hold data: wait for it too.
	w.cps["cp-2"].SetUnreachable(true)
	_, ex = w.pass(t)
	assert.Contains(t, ex.Message, "cp-2 does not")
	w.cps["cp-2"].SetUnreachable(false)

	// Every control plane answers without etcd: wait for the person's
	// upload, which the operator cannot do.
	_, ex = w.pass(t)
	assert.Contains(t, ex.Message, "pk-metal restore upload pk-metal/op")
	assert.Zero(t, w.cps["cp-1"].BootstrapCalls())

	// An upload recorded for other bytes is not the approved one.
	w.upload(t, "sha256:"+hex.EncodeToString(make([]byte, 32)))
	_, ex = w.pass(t)
	assert.Contains(t, ex.Message, "waiting for the snapshot upload")
	assert.Zero(t, w.cps["cp-1"].BootstrapCalls())

	w.upload(t, w.sha)
	_, ex = w.pass(t)
	assert.Contains(t, ex.Message, "accepted the recovery")
	assert.Equal(t, []talos.BootstrapOptions{{RecoverEtcd: true}}, w.cps["cp-1"].Bootstraps())
	assert.Equal(t, w.snap, w.cps["cp-1"].Recovered())
	assert.Zero(t, w.cps["cp-2"].BootstrapCalls(), "only the chosen control plane")

	op, ex := w.pass(t)
	assert.Equal(t, metav1.ConditionTrue, ex.Status, ex.Message)
	assert.True(t, opSucceeded(op))
	assert.Equal(t, 1, w.cps["cp-1"].BootstrapCalls(), "asked once")
}

func TestEtcdRecoverRefusesWhatWasNotApproved(t *testing.T) {
	t.Run("a cluster never bootstrapped and not marked", func(t *testing.T) {
		w := newRecoverWorld(t, false)
		op := runOp(t, w.r)
		ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		assert.Equal(t, metav1.ConditionFalse, ex.Status)
		assert.Contains(t, ex.Message, osv1alpha1.AnnotationRestorePending)
		assert.Zero(t, w.cps["cp-1"].BootstrapCalls())
	})

	t.Run("a cluster marked restore-pending is restored", func(t *testing.T) {
		w := newRecoverWorld(t, false)
		c := &osv1alpha1.TalosCluster{}
		require.NoError(t, w.r.Get(t.Context(), client.ObjectKey{Name: "dr"}, c))
		c.Annotations = map[string]string{osv1alpha1.AnnotationRestorePending: "true"}
		require.NoError(t, w.r.Update(t.Context(), c))
		w.upload(t, w.sha)
		w.pass(t)
		op, ex := w.pass(t)
		assert.True(t, opSucceeded(op), ex.Message)
	})

	t.Run("a snapshot record that changed after approval", func(t *testing.T) {
		w := newRecoverWorld(t, true)
		s := &osv1alpha1.EtcdSnapshot{}
		require.NoError(t, w.r.Get(t.Context(), client.ObjectKey{Name: "dr-daily-20261123-060000"}, s))
		s.Status.SHA256 = "sha256:" + hex.EncodeToString(make([]byte, 32))
		require.NoError(t, w.r.Update(t.Context(), s))
		op := runOp(t, w.r)
		ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		assert.Equal(t, metav1.ConditionFalse, ex.Status)
		assert.Contains(t, ex.Message, "was approved")
	})

	t.Run("a host that runs another node", func(t *testing.T) {
		w := newRecoverWorld(t, true)
		host := &metalv1alpha1.Host{}
		require.NoError(t, w.r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
		host.Spec.OS.NodeRef.Name = "cp-2"
		require.NoError(t, w.r.Update(t.Context(), host))
		op := runOp(t, w.r)
		ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		assert.Equal(t, metav1.ConditionFalse, ex.Status)
		assert.Contains(t, ex.Message, "does not run TalosNode cp-1")
	})
}

// etcdHangs is a node whose etcd calls hang until their deadline, as a
// control plane whose etcd does not run can, while its machine API answers.
type etcdHangs struct{ talos.Client }

func (h etcdHangs) EtcdStatus(ctx context.Context) (talos.EtcdStatus, error) {
	<-ctx.Done()
	return talos.EtcdStatus{}, ctx.Err()
}

func (h etcdHangs) EtcdMembers(ctx context.Context) ([]talos.EtcdMember, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestProbeEtcdSeesAStoppedEtcdWhoseCallsHang: a node whose etcd calls run
// into their deadline still answers. With one budget for every call it
// counted as silent, and the recovery waited for it for ever.
func TestProbeEtcdSeesAStoppedEtcdWhoseCallsHang(t *testing.T) {
	old := probeTimeout
	probeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })

	m := talosfake.NewMachine("v1.14.6")
	m.Configure([]byte("config"))
	dial := func(ctx context.Context, target talos.Target) (talos.Client, error) {
		c, err := m.Dialer()(ctx, target)
		if err != nil {
			return nil, err
		}
		return etcdHangs{c}, nil
	}
	assert.Equal(t, etcdStopped, probeEtcd(t.Context(), dial, []byte("tc"), "192.0.2.11"))

	m.SetUnreachable(true)
	assert.Equal(t, etcdSilent, probeEtcd(t.Context(), dial, []byte("tc"), "192.0.2.11"))
}
