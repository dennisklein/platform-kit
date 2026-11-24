package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup/etcdbackuptest"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	talosfake "github.com/dennisklein/platform-kit/metal/internal/talos/fake"
)

// drWorld is a cluster "dr" of three control planes, each run by a Host,
// with the talosconfig Secret the operator publishes.
type drWorld struct {
	objs  []client.Object
	fleet *talosfake.Fleet
	cps   map[string]*talosfake.Machine
	etcd  *talosfake.EtcdCluster
}

func newDRWorld(bootstrapped bool) *drWorld {
	w := &drWorld{fleet: talosfake.NewFleet(), cps: map[string]*talosfake.Machine{}, etcd: talosfake.NewEtcdCluster()}
	cluster := &osv1alpha1.TalosCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Status: osv1alpha1.TalosClusterStatus{
			TalosconfigSecretRef: &osv1alpha1.SecretReference{Namespace: operatorNamespace, Name: "dr-talosconfig"},
		},
	}
	if bootstrapped {
		cluster.Status.BootstrappedAt = &metav1.Time{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	w.objs = append(w.objs, cluster, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: "dr-talosconfig"},
		Data:       map[string][]byte{"talosconfig": []byte("admin talosconfig")},
	})
	for i := 1; i <= 3; i++ {
		name, addr := fmt.Sprintf("cp-%d", i), fmt.Sprintf("192.0.2.1%d", i)
		w.objs = append(w.objs,
			&osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: osv1alpha1.TalosNodeSpec{
				ClusterRef: osv1alpha1.Reference{Name: "dr"}, Role: osv1alpha1.RoleControlPlane, Address: addr}},
			&metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-" + strconv.Itoa(i)},
				Spec: metalv1alpha1.HostSpec{OS: &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: name}}}})
		m := talosfake.NewMachine("v1.14.6")
		m.Configure([]byte("config"))
		w.fleet.Add(addr, name, m)
		w.cps[name] = m
	}
	return w
}

func (w *drWorld) joinAll() {
	for i := 1; i <= 3; i++ {
		w.cps[fmt.Sprintf("cp-%d", i)].JoinEtcd(w.etcd, false)
	}
}

func verifiedSnapshot(name string, takenAt time.Time, plain []byte) *osv1alpha1.EtcdSnapshot {
	sum := sha256.Sum256(plain)
	return &osv1alpha1.EtcdSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{osv1alpha1.LabelSnapshotCluster: "dr"}},
		Spec:       osv1alpha1.EtcdSnapshotSpec{PolicyRef: osv1alpha1.Reference{Name: "dr-daily"}},
		Status: osv1alpha1.EtcdSnapshotStatus{
			ClusterName: "dr", TakenAt: &metav1.Time{Time: takenAt}, TakenFrom: "cp-2", Size: int64(len(plain)),
			SHA256: "sha256:" + hex.EncodeToString(sum[:]), Key: "dr/" + name + ".db.age",
			Location: "pvc://pk-metal-etcd-backups/dr/" + name + ".db.age",
			Conditions: []metav1.Condition{
				{Type: osv1alpha1.ConditionComplete, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonStored},
				{Type: osv1alpha1.ConditionVerified, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonIntact},
			},
		},
	}
}

func TestRestorePlanHealthyClusterProposesNothing(t *testing.T) {
	w := newDRWorld(true)
	w.joinAll()
	p, err := planRestore(t.Context(), fakeClient(w.objs...), w.fleet.Dialer(), []byte("tc"), restoreInput{Cluster: "dr"})
	require.NoError(t, err)
	assert.Empty(t, p.Ops)
	assert.Contains(t, p.render(), "healthy")
}

func TestRestorePlanReplacesOneLostMember(t *testing.T) {
	w := newDRWorld(true)
	w.joinAll()
	w.cps["cp-3"].SetEtcdRunning(false) // its data is gone; the member stays listed
	var cp3 talos.EtcdMember
	for _, m := range w.etcd.Members() {
		if m.Hostname == "cp-3" {
			cp3 = m
		}
	}

	cl := fakeClient(w.objs...)
	p, err := planRestore(t.Context(), cl, w.fleet.Dialer(), []byte("tc"), restoreInput{Cluster: "dr", User: "alice"})
	require.NoError(t, err)
	require.Len(t, p.Ops, 1)
	op := p.Ops[0]
	assert.Equal(t, "dr-etcd-replace-cp-3", op.Name)
	assert.Equal(t, "host-3", op.Spec.HostRef.Name)
	require.Len(t, op.Spec.Actions, 3)
	assert.Equal(t, strconv.FormatUint(cp3.ID, 16), op.Spec.Actions[0].EtcdRemoveMember.MemberID,
		"the member ID is frozen at proposal")
	assert.Equal(t, []metalv1alpha1.SystemPartition{"EPHEMERAL"}, op.Spec.Actions[1].TalosReset.SystemPartitions)
	assert.False(t, *op.Spec.Actions[1].TalosReset.Graceful, "a node without etcd cannot leave gracefully")
	assert.NotNil(t, op.Spec.Actions[2].WaitEtcdHealthy)

	var out bytes.Buffer
	require.NoError(t, p.propose(t.Context(), cl, &out))
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKey{Namespace: operatorNamespace, Name: op.Name}, got))
	assert.Equal(t, "alice", got.Spec.ProposedBy)
	assert.Empty(t, got.Spec.Approval.ApprovedBy, "proposed, never approved")
	assert.Contains(t, got.Annotations[metalv1alpha1.AnnotationOverrideClusterGuard], "pk-metal restore",
		"a repair of a broken etcd would otherwise wait for the health it restores")
	assert.Contains(t, out.String(), "pk-metal approve")
}

func TestRestorePlanRecoversALostCluster(t *testing.T) {
	plain := etcdbackuptest.Snapshot(3)
	w := newDRWorld(true)
	// cp-2 still runs an etcd without a quorum; the others run none.
	w.cps["cp-2"].SetEtcdRunning(true)
	w.cps["cp-2"].SetFailure("EtcdMembers", fmt.Errorf("etcdserver: no leader"))
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	newest := verifiedSnapshot("dr-daily-new", now.Add(-time.Hour), plain)
	objs := append(w.objs, newest, verifiedSnapshot("dr-daily-old", now.Add(-25*time.Hour), []byte("x")))

	p, err := planRestore(t.Context(), fakeClient(objs...), w.fleet.Dialer(), []byte("tc"),
		restoreInput{Cluster: "dr", User: "alice", Now: now})
	require.NoError(t, err)
	require.Len(t, p.Ops, 2)
	assert.Equal(t, "dr-etcd-reset-cp-2", p.Ops[0].Name)
	assert.NotNil(t, p.Ops[0].Spec.Actions[0].TalosReset)
	rec := p.Ops[1]
	assert.Equal(t, "dr-etcd-recover-cp-1", rec.Name)
	assert.Equal(t, "host-1", rec.Spec.HostRef.Name)
	assert.Equal(t, "dr-daily-new", rec.Spec.Actions[0].EtcdRecover.SnapshotRef.Name, "the newest verified snapshot")
	assert.Equal(t, newest.Status.SHA256, rec.Spec.Actions[0].EtcdRecover.SHA256)
	for _, op := range p.Ops {
		assert.NotEmpty(t, op.Annotations[metalv1alpha1.AnnotationOverrideClusterGuard], "%s overrides the cluster guard", op.Name)
		assert.NotEmpty(t, op.Annotations[metalv1alpha1.AnnotationIgnoreWindow], "%s ignores maintenance windows", op.Name)
	}
	text := p.render()
	assert.Contains(t, text, "RESTORE etcd")
	assert.Contains(t, text, "pk-metal restore upload pk-metal/dr-etcd-recover-cp-1")
	assert.NotContains(t, text, "restore-pending", "a cluster seen bootstrapped needs no mark")

	// A cluster never seen bootstrapped must be marked before anything.
	w2 := newDRWorld(false)
	p, err = planRestore(t.Context(), fakeClient(append(w2.objs, newest)...), w2.fleet.Dialer(), []byte("tc"),
		restoreInput{Cluster: "dr"})
	require.NoError(t, err)
	assert.Contains(t, p.render(), "kubectl annotate taloscluster dr os.pk.io/restore-pending=true")

	// No verified snapshot: nothing to restore from.
	_, err = planRestore(t.Context(), fakeClient(w2.objs...), w2.fleet.Dialer(), []byte("tc"), restoreInput{Cluster: "dr"})
	require.ErrorContains(t, err, "no verified etcd snapshot")
}

// encryptedFile writes plain, encrypted to id, to a file.
func encryptedFile(t *testing.T, id *age.X25519Identity, plain []byte) string {
	t.Helper()
	rs, err := etcdbackup.ParseRecipients([]string{id.Recipient().String()})
	require.NoError(t, err)
	var buf bytes.Buffer
	enc, err := etcdbackup.Encrypt(&buf, rs)
	require.NoError(t, err)
	_, err = enc.Write(plain)
	require.NoError(t, err)
	require.NoError(t, enc.Close())
	path := filepath.Join(t.TempDir(), "snap.db.age")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

func identityFile(t *testing.T, id *age.X25519Identity) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.txt")
	require.NoError(t, os.WriteFile(path, []byte("# test identity\n"+id.String()+"\n"), 0o600))
	return path
}

func TestRestoreUploadSendsOnlyTheApprovedSnapshot(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	plain := etcdbackuptest.Snapshot(5)
	snap := verifiedSnapshot("dr-daily-new", time.Now(), plain)
	w := newDRWorld(true)
	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: "dr-etcd-recover-cp-1"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef: metalv1alpha1.Reference{Name: "host-1"}, ProposedBy: "alice",
			Actions: []metalv1alpha1.HostOperationAction{{EtcdRecover: &metalv1alpha1.EtcdRecoverAction{
				NodeRef: metalv1alpha1.Reference{Name: "cp-1"}, SnapshotRef: metalv1alpha1.Reference{Name: snap.Name},
				SHA256: snap.Status.SHA256,
			}}},
			Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true)},
		},
	}
	cl := fakeClient(append(w.objs, snap, op)...)
	in := uploadInput{Namespace: operatorNamespace, Name: op.Name, Identity: identityFile(t, id),
		FromFile: encryptedFile(t, id, plain)}
	ctx := context.Background()

	var out bytes.Buffer
	err = uploadSnapshot(ctx, cl, w.fleet.Dialer(), in, &out)
	require.ErrorContains(t, err, "not approved yet")
	assert.Nil(t, w.cps["cp-1"].Recovered())

	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(op), got))
	got.Spec.Approval.ApprovedBy = "bob"
	require.NoError(t, cl.Update(ctx, got))

	// Another snapshot's file is refused before anything reaches the node.
	wrong := in
	wrong.FromFile = encryptedFile(t, id, etcdbackuptest.Snapshot(6))
	err = uploadSnapshot(ctx, cl, w.fleet.Dialer(), wrong, &out)
	require.ErrorContains(t, err, "was approved")
	assert.Nil(t, w.cps["cp-1"].Recovered())

	// Somebody else's identity does not decrypt it.
	other, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	stranger := in
	stranger.Identity = identityFile(t, other)
	require.Error(t, uploadSnapshot(ctx, cl, w.fleet.Dialer(), stranger, &out))
	assert.Nil(t, w.cps["cp-1"].Recovered())

	require.NoError(t, uploadSnapshot(ctx, cl, w.fleet.Dialer(), in, &out))
	assert.Equal(t, plain, w.cps["cp-1"].Recovered(), "the node got exactly the snapshot")
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(op), got))
	assert.Equal(t, snap.Status.SHA256, got.Annotations[metalv1alpha1.AnnotationEtcdSnapshotUploaded])
	assert.Contains(t, out.String(), "the operator now starts etcd")

	// Talos refuses an upload to a node whose etcd runs; so does the result.
	w.cps["cp-2"].SetEtcdRunning(true)
	got.Annotations = nil
	got.Spec.Actions[0].EtcdRecover.NodeRef.Name = "cp-2"
	require.NoError(t, cl.Update(ctx, got))
	err = uploadSnapshot(ctx, cl, w.fleet.Dialer(), in, &out)
	require.ErrorContains(t, err, "etcd is running")
}

func TestBackupNowAndList(t *testing.T) {
	policy := &osv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-daily"},
		Spec:       osv1alpha1.EtcdBackupPolicySpec{ClusterRef: osv1alpha1.Reference{Name: "dr"}},
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cl := fakeClient(policy, verifiedSnapshot("dr-daily-20261124-060000", now.Add(-6*time.Hour), []byte("x")))
	ctx := context.Background()
	var out bytes.Buffer

	name, err := backupNow(ctx, cl, "dr", now, &out)
	require.NoError(t, err)
	assert.Equal(t, "dr-daily-manual-20261124-120000", name)
	s := &osv1alpha1.EtcdSnapshot{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: name}, s))
	assert.Equal(t, "dr-daily", s.Spec.PolicyRef.Name)
	assert.Equal(t, "dr", s.Labels[osv1alpha1.LabelSnapshotCluster])

	_, err = backupNow(ctx, cl, "nope", now, &out)
	require.ErrorContains(t, err, "no EtcdBackupPolicy")

	out.Reset()
	require.NoError(t, listSnapshots(ctx, cl, "dr", now, &out))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[1], "dr-daily-manual-20261124-120000")
	assert.Contains(t, lines[1], "Taking")
	assert.Contains(t, lines[2], "Verified")
	assert.Contains(t, lines[2], "6h ago")
}

func TestBackupDecryptChecksTheSnapshot(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	plain := etcdbackuptest.Snapshot(8)
	in := encryptedFile(t, id, plain)
	ctx := context.Background()
	var out bytes.Buffer

	// Without -o nothing is written; the check alone runs.
	require.NoError(t, decryptSnapshot(ctx, nil, "", in, identityFile(t, id), "", &out))
	assert.Contains(t, out.String(), "intact etcd snapshot")

	dst := filepath.Join(t.TempDir(), "snap.db")
	require.NoError(t, decryptSnapshot(ctx, nil, "", in, identityFile(t, id), dst, &out))
	b, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, plain, b)
	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	require.ErrorContains(t, decryptSnapshot(ctx, nil, "", in, identityFile(t, id), dst, &out), "exists",
		"never overwrites")

	// A damaged plaintext (encrypted intact) is reported, and not kept.
	bad := encryptedFile(t, id, plain[:len(plain)-1])
	dst2 := filepath.Join(t.TempDir(), "bad.db")
	require.Error(t, decryptSnapshot(ctx, nil, "", bad, identityFile(t, id), dst2, &out))
	_, err = os.Stat(dst2)
	assert.True(t, os.IsNotExist(err))
}

// The help and the hints print flags after the cluster (pk-metal restore
// plan lab --propose); that must parse like flags in front.
func TestParseAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"lab", "--snapshot", "s1", "--propose"},
		{"--snapshot", "s1", "lab", "--propose"},
		{"--propose", "--snapshot=s1", "lab"},
	} {
		fs := newFlags("restore plan")
		snapshot := fs.String("snapshot", "", "")
		propose := fs.Bool("propose", false, "")
		pos := parseAnywhere(fs, args)
		assert.Equal(t, []string{"lab"}, pos, args)
		assert.Equal(t, "s1", *snapshot, args)
		assert.True(t, *propose, args)
	}
	fs := newFlags("backup decrypt")
	in := fs.String("in", "", "")
	assert.Equal(t, []string{"-weird", "x"}, parseAnywhere(fs, []string{"--in", "f", "--", "-weird", "x"}))
	assert.Equal(t, "f", *in)
	assert.Empty(t, argAt(nil, 0))
}

// hangingEtcd is a node whose etcd calls hang until their deadline, as a
// control plane whose etcd does not run can: the machine API answers, etcd
// does not.
type hangingEtcd struct{ talos.Client }

func (h hangingEtcd) EtcdStatus(ctx context.Context) (talos.EtcdStatus, error) {
	<-ctx.Done()
	return talos.EtcdStatus{}, ctx.Err()
}

func (h hangingEtcd) EtcdMembers(ctx context.Context) ([]talos.EtcdMember, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRestorePlanSeesNodesWhoseEtcdHangs: a node whose etcd calls run into
// their deadline still answers, so the restore has a target. With one budget
// for every call, the etcd calls used it up and the node looked silent.
func TestRestorePlanSeesNodesWhoseEtcdHangs(t *testing.T) {
	old := probeTimeout
	probeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })

	w := newDRWorld(true)
	inner := w.fleet.Dialer()
	dial := func(ctx context.Context, target talos.Target) (talos.Client, error) {
		c, err := inner(ctx, target)
		if err != nil {
			return nil, err
		}
		return hangingEtcd{c}, nil
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	objs := append(w.objs, verifiedSnapshot("dr-daily-new", now.Add(-time.Hour), etcdbackuptest.Snapshot(3)))
	p, err := planRestore(t.Context(), fakeClient(objs...), dial, []byte("tc"), restoreInput{Cluster: "dr", User: "alice", Now: now})
	require.NoError(t, err)
	for _, v := range p.Views {
		assert.Equal(t, "stopped", v.State, "%s answers; only its etcd does not", v.Node.Name)
	}
	require.Len(t, p.Ops, 1)
	assert.Equal(t, "dr-etcd-recover-cp-1", p.Ops[0].Name)

	// When none answers, the error says what each one said.
	w.cps["cp-1"].SetUnreachable(true)
	w.cps["cp-2"].SetUnreachable(true)
	w.cps["cp-3"].SetUnreachable(true)
	_, err = planRestore(t.Context(), fakeClient(objs...), dial, []byte("tc"), restoreInput{Cluster: "dr", User: "alice", Now: now})
	require.ErrorContains(t, err, "no control plane answers")
	assert.ErrorContains(t, err, "cp-2 at 192.0.2.12")
	assert.ErrorContains(t, err, "connection refused")
}
