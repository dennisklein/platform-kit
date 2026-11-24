package envtest_test

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup/etcdbackuptest"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// roleLog records the Talos roles of the certificate each etcd call came
// with.
type roleLog struct {
	mu    sync.Mutex
	calls map[string][][]string // method → roles of each call
}

func (l *roleLog) add(method string, tc []byte) {
	roles, _ := render.TalosconfigRoles(tc)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls[method] = append(l.calls[method], roles)
}

func (l *roleLog) get(method string) [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls[method])
}

type loggedClient struct {
	talos.Client
	log *roleLog
	tc  []byte
}

func (c loggedClient) EtcdStatus(ctx context.Context) (talos.EtcdStatus, error) {
	c.log.add("EtcdStatus", c.tc)
	return c.Client.EtcdStatus(ctx)
}

func (c loggedClient) EtcdSnapshot(ctx context.Context, w io.Writer) (int64, error) {
	c.log.add("EtcdSnapshot", c.tc)
	return c.Client.EtcdSnapshot(ctx, w)
}

// A backup reads etcd's status with os:reader and streams the snapshot with
// os:etcd:backup. Neither certificate can change a machine; os:admin is not
// needed for a read.
func TestEtcdBackupLeastPrivilege(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()

	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	fleet := fake.NewFleet()
	etcdFleet(fleet, 83, []string{"rl-cp-1", "rl-cp-2"}, etcdbackuptest.Snapshot(7))
	createTalosCluster(t, cl, "rl", 83, []string{"rl-cp-1", "rl-cp-2"})

	log := &roleLog{calls: map[string][][]string{}}
	dial := fleet.Dialer()
	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		TalosDial: func(ctx context.Context, tg talos.Target) (talos.Client, error) {
			c, err := dial(ctx, tg)
			if err != nil {
				return nil, err
			}
			return loggedClient{Client: c, log: log, tc: tg.Talosconfig}, nil
		},
		EtcdBackupDir:    t.TempDir(),
		EtcdBackupVolume: "pk-metal-etcd-backups",
		CheckInterval:    200 * time.Millisecond,
	}, controller.LaneEtcdBackup))
	runManager(t, ctx, mgr)

	require.NoError(t, cl.Create(ctx, pvcPolicy("rl-daily", "rl", id.Recipient().String(), 2)))
	require.Eventually(t, func() bool {
		p := &osv1alpha1.EtcdBackupPolicy{}
		return cl.Get(ctx, types.NamespacedName{Name: "rl-daily"}, p) == nil &&
			meta.IsStatusConditionTrue(p.Status.Conditions, osv1alpha1.ConditionBackedUp)
	}, waitFor, tick, "the snapshot is taken and verified with the narrow roles")

	status, snaps := log.get("EtcdStatus"), log.get("EtcdSnapshot")
	require.NotEmpty(t, status)
	require.NotEmpty(t, snaps)
	for _, r := range status {
		assert.Equal(t, []string{"os:reader"}, r, "the status probe reads only")
	}
	for _, r := range snaps {
		assert.Equal(t, []string{"os:etcd:backup"}, r, "the stream may take a snapshot and nothing else")
	}
}
