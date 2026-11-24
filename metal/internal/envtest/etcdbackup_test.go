// The etcd backup lane against a real apiserver: a policy schedules
// snapshots, each is streamed from a follower of a scripted etcd through age
// encryption onto the backup volume and verified, retention prunes the
// record and the stored object together, and a pvc sink is refused for the
// cluster the operator runs on.
package envtest_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup/etcdbackuptest"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// etcdFleet scripts a cluster of control planes that run one etcd, at
// 10.<net>.0.1x, with the given snapshot bytes.
func etcdFleet(fleet *fake.Fleet, net int, names []string, snap []byte) map[string]*fake.Machine {
	etcd := fake.NewEtcdCluster()
	out := map[string]*fake.Machine{}
	for i, name := range names {
		m := fake.NewMachine(labTalos)
		m.Configure([]byte("config of " + name))
		m.SetStage("running", true)
		m.SetSnapshot(snap)
		fleet.Add(fmt.Sprintf("10.%d.0.1%d", net, i+1), name, m)
		m.JoinEtcd(etcd, false)
		out[name] = m
	}
	return out
}

// createTalosCluster creates a cluster and its control-plane TalosNodes at
// the addresses etcdFleet uses, with the bundle Secret it names.
func createTalosCluster(t *testing.T, cl client.Client, name string, net int, nodes []string) {
	t.Helper()
	ctx := t.Context()
	bundle, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	require.NoError(t, cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-talos-secrets", Namespace: "default"},
		Data:       map[string][]byte{render.BundleKey: bundle},
	}))
	require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: osv1alpha1.TalosClusterSpec{
			TalosVersion: labTalos, KubernetesVersion: "v1.36.2",
			Endpoint:   fmt.Sprintf("https://10.%d.0.10:6443", net),
			SecretsRef: osv1alpha1.SecretReference{Namespace: "default", Name: name + "-talos-secrets"},
		},
	}))
	for i, n := range nodes {
		require.NoError(t, cl.Create(ctx, &osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: n},
			Spec: osv1alpha1.TalosNodeSpec{
				ClusterRef: osv1alpha1.Reference{Name: name}, Role: osv1alpha1.RoleControlPlane,
				Address:             fmt.Sprintf("10.%d.0.1%d", net, i+1),
				InstallDiskSelector: osv1alpha1.DiskSelector{Serial: "disk-" + n},
			},
		}))
	}
}

func pvcPolicy(name, cluster, recipient string, count int32) *osv1alpha1.EtcdBackupPolicy {
	return &osv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: osv1alpha1.EtcdBackupPolicySpec{
			ClusterRef: osv1alpha1.Reference{Name: cluster},
			Interval:   metav1.Duration{Duration: time.Hour},
			Retention:  osv1alpha1.EtcdBackupRetention{Count: count},
			Sink:       osv1alpha1.EtcdBackupSink{PVC: &osv1alpha1.PVCSink{}},
			Encryption: osv1alpha1.EtcdBackupEncryption{AgeRecipients: []string{recipient}},
		},
	}
}

// manualSnapshot is what pk-metal backup now creates.
func manualSnapshot(policy, cluster, name string) *osv1alpha1.EtcdSnapshot {
	return &osv1alpha1.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			osv1alpha1.LabelSnapshotPolicy: policy, osv1alpha1.LabelSnapshotCluster: cluster,
		}},
		Spec: osv1alpha1.EtcdSnapshotSpec{PolicyRef: osv1alpha1.Reference{Name: policy}},
	}
}

func TestEtcdBackupLane(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()

	// A person's age identity: the operator gets only its public key.
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	recipient := id.Recipient().String()

	snap := etcdbackuptest.Snapshot(41)
	fleet := fake.NewFleet()
	cps := etcdFleet(fleet, 81, []string{"bk-cp-1", "bk-cp-2", "bk-cp-3"}, snap)
	createTalosCluster(t, cl, "bk", 81, []string{"bk-cp-1", "bk-cp-2", "bk-cp-3"})
	// The operator's own cluster: the manager runs on own-cp-1.
	etcdFleet(fleet, 82, []string{"own-cp-1"}, snap)
	createTalosCluster(t, cl, "own", 82, []string{"own-cp-1"})

	dir := t.TempDir()
	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		TalosDial:            fleet.Dialer(),
		EtcdBackupDir:        dir,
		EtcdBackupVolume:     "pk-metal-etcd-backups",
		NodeName:             "own-cp-1",
		CheckInterval:        200 * time.Millisecond,
	}, controller.LaneEtcdBackup))
	runManager(t, ctx, mgr)

	getSnap := func(name string) *osv1alpha1.EtcdSnapshot {
		s := &osv1alpha1.EtcdSnapshot{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, s); err != nil {
			return nil
		}
		return s
	}
	ended := func(name string) *osv1alpha1.EtcdSnapshot {
		var s *osv1alpha1.EtcdSnapshot
		require.Eventually(t, func() bool {
			s = getSnap(name)
			c := (*metav1.Condition)(nil)
			if s != nil {
				c = meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionComplete)
			}
			return c != nil && c.Status != metav1.ConditionUnknown
		}, waitFor, tick, "snapshot %s never ended", name)
		return s
	}
	policySnaps := func(policy string) []osv1alpha1.EtcdSnapshot {
		l := &osv1alpha1.EtcdSnapshotList{}
		require.NoError(t, cl.List(ctx, l, client.MatchingLabels{osv1alpha1.LabelSnapshotPolicy: policy}))
		return l.Items
	}

	var first string
	st.run(t, "a policy takes a verified, encrypted snapshot from a follower", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, pvcPolicy("bk-daily", "bk", recipient, 2)))
		require.Eventually(t, func() bool { return len(policySnaps("bk-daily")) == 1 }, waitFor, tick,
			"the first snapshot is taken at once")
		first = policySnaps("bk-daily")[0].Name
		s := ended(first)

		require.True(t, meta.IsStatusConditionTrue(s.Status.Conditions, osv1alpha1.ConditionComplete), "%+v", s.Status.Conditions)
		require.True(t, meta.IsStatusConditionTrue(s.Status.Conditions, osv1alpha1.ConditionVerified), "%+v", s.Status.Conditions)
		// bk-cp-1 joined first and leads; the snapshot comes off a follower.
		leader := cps["bk-cp-1"].Etcd().Leader()
		assert.NotEqual(t, fmt.Sprintf("%x", leader), s.Status.MemberID)
		assert.Equal(t, "bk-cp-2", s.Status.TakenFrom, "the first follower by name")
		assert.Equal(t, "bk", s.Status.ClusterName)
		assert.Equal(t, int64(len(snap)), s.Status.Size)
		assert.Equal(t, "bk/"+first+".db.age", s.Status.Key)
		assert.Equal(t, "pvc://pk-metal-etcd-backups/bk/"+first+".db.age", s.Status.Location)
		assert.Equal(t, []string{recipient}, s.Status.Recipients)
		assert.EqualValues(t, 42, s.Status.DatabaseTxID)

		// What the volume holds decrypts, with the person's identity, to
		// exactly what etcd streamed; it is not the plaintext.
		cipher, err := os.ReadFile(filepath.Join(dir, "bk", first+".db.age"))
		require.NoError(t, err)
		assert.False(t, bytes.Contains(cipher, snap[:64]))
		assert.Equal(t, s.Status.EncryptedSize, int64(len(cipher)))
		var plain bytes.Buffer
		rep, err := etcdbackup.Restore(bytes.NewReader(cipher), []age.Identity{id}, &plain)
		require.NoError(t, err)
		assert.Equal(t, snap, plain.Bytes())
		assert.Equal(t, s.Status.SHA256, "sha256:"+rep.SHA256)

		var p osv1alpha1.EtcdBackupPolicy
		require.Eventually(t, func() bool {
			require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "bk-daily"}, &p))
			return meta.IsStatusConditionTrue(p.Status.Conditions, osv1alpha1.ConditionBackedUp)
		}, waitFor, tick)
		require.NotNil(t, p.Status.LastSuccessfulSnapshot)
		assert.Equal(t, first, p.Status.LastSuccessfulSnapshot.Name)
		assert.Zero(t, p.Status.ConsecutiveFailures)
		assert.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, osv1alpha1.ConditionReady))

		// The interval has not passed: no second snapshot.
		st.settle(t, ctx, &p, 2)
		assert.Len(t, policySnaps("bk-daily"), 1)
	})

	st.run(t, "a damaged snapshot is stored but not verified, and counts as a failure", func(t *testing.T) {
		for _, m := range cps {
			m.SetSnapshot(snap[:len(snap)-100])
		}
		require.NoError(t, cl.Create(ctx, manualSnapshot("bk-daily", "bk", "bk-damaged")))
		s := ended("bk-damaged")
		assert.True(t, meta.IsStatusConditionTrue(s.Status.Conditions, osv1alpha1.ConditionComplete))
		v := meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionVerified)
		require.NotNil(t, v)
		assert.Equal(t, metav1.ConditionFalse, v.Status)
		assert.Equal(t, osv1alpha1.ReasonDamaged, v.Reason)

		require.Eventually(t, func() bool {
			var p osv1alpha1.EtcdBackupPolicy
			require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "bk-daily"}, &p))
			return p.Status.ConsecutiveFailures == 1 && p.Status.LastSuccessfulSnapshot != nil &&
				p.Status.LastSuccessfulSnapshot.Name == first
		}, waitFor, tick)
		for _, m := range cps {
			m.SetSnapshot(snap)
		}
	})

	st.run(t, "retention prunes the record and the stored object together", func(t *testing.T) {
		for i := range 3 {
			name := fmt.Sprintf("bk-manual-%d", i)
			require.NoError(t, cl.Create(ctx, manualSnapshot("bk-daily", "bk", name)))
			require.True(t, meta.IsStatusConditionTrue(ended(name).Status.Conditions, osv1alpha1.ConditionVerified))
			// Distinct take times, so the order is the creation order.
			time.Sleep(1100 * time.Millisecond)
		}
		require.Eventually(t, func() bool {
			return getSnap(first) == nil && getSnap("bk-manual-0") == nil
		}, waitFor, tick, "the two oldest verified snapshots must go (count 2)")
		assert.NotNil(t, getSnap("bk-manual-1"))
		assert.NotNil(t, getSnap("bk-manual-2"))
		assert.NotNil(t, getSnap("bk-damaged"), "failures are kept for reading, up to three")

		files, err := filepath.Glob(filepath.Join(dir, "bk", "*.db.age"))
		require.NoError(t, err)
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(f))
		}
		assert.ElementsMatch(t, []string{"bk-damaged.db.age", "bk-manual-1.db.age", "bk-manual-2.db.age"}, names)
	})

	st.run(t, "a pvc sink is refused for the operator's own cluster", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, pvcPolicy("own-daily", "own", recipient, 3)))
		var p osv1alpha1.EtcdBackupPolicy
		require.Eventually(t, func() bool {
			require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "own-daily"}, &p))
			c := meta.FindStatusCondition(p.Status.Conditions, osv1alpha1.ConditionReady)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == osv1alpha1.ReasonSinkRefused
		}, waitFor, tick)
		assert.Empty(t, policySnaps("own-daily"), "nothing is scheduled onto a refused sink")

		require.NoError(t, cl.Create(ctx, manualSnapshot("own-daily", "own", "own-manual")))
		s := ended("own-manual")
		c := meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionComplete)
		assert.Equal(t, metav1.ConditionFalse, c.Status)
		assert.Contains(t, c.Message, "use an s3 sink")
		_, err := os.Stat(filepath.Join(dir, "own"))
		assert.True(t, os.IsNotExist(err), "nothing written for the refused cluster")
	})

	st.run(t, "a snapshot waits for a healthy member and names what is wrong", func(t *testing.T) {
		for _, m := range cps {
			m.SetEtcdErrors("etcdserver: mvcc: database space exceeded")
		}
		require.NoError(t, cl.Create(ctx, manualSnapshot("bk-daily", "bk", "bk-unhealthy")))
		require.Eventually(t, func() bool {
			s := getSnap("bk-unhealthy")
			c := (*metav1.Condition)(nil)
			if s != nil {
				c = meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionComplete)
			}
			return c != nil && c.Status == metav1.ConditionUnknown && bytes.Contains([]byte(c.Message), []byte("database space exceeded"))
		}, waitFor, tick)
		for _, m := range cps {
			m.SetEtcdErrors()
		}
		require.Eventually(t, func() bool {
			s := getSnap("bk-unhealthy")
			return s != nil && meta.IsStatusConditionTrue(s.Status.Conditions, osv1alpha1.ConditionVerified)
		}, 2*waitFor, tick, "the snapshot is taken once a member is healthy again")
	})
}
