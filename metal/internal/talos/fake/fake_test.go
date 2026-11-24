package fake

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// The blank-machine ladder: insecure dial in maintenance, apply, then the
// secure dial takes over and the insecure one dies with the maintenance service.
func TestLadder(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	dial := m.Dialer()

	// Secure dial before any config: nothing serves the cluster PKI.
	_, err := dial(ctx, talos.Target{Endpoint: "n1", Talosconfig: []byte("tc")})
	require.Error(t, err)

	c, err := dial(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.NoError(t, err)

	v, err := c.Version(ctx)
	require.NoError(t, err)
	assert.Equal(t, "v1.14.6", v)

	st, err := c.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, "maintenance", st.Stage)

	// Bootstrap before a config is a scripting error, as on the real node.
	require.Error(t, c.Bootstrap(ctx, talos.BootstrapOptions{}))

	_, err = c.ApplyConfiguration(ctx, []byte("cfg"), talos.ApplyOptions{})
	require.NoError(t, err)
	m.SetStage("booting", false, "etcd")

	// The maintenance service is gone now.
	_, err = dial(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.Error(t, err)

	sc, err := dial(ctx, talos.Target{Endpoint: "n1", Talosconfig: []byte("tc")})
	require.NoError(t, err)
	require.NoError(t, sc.Bootstrap(ctx, talos.BootstrapOptions{}))
	assert.True(t, m.Bootstrapped())

	m.SetStage("running", true)
	st, err = sc.Status(ctx)
	require.NoError(t, err)
	assert.True(t, st.Ready)
	assert.Empty(t, st.UnmetConditions)

	assert.Equal(t, [][]byte{[]byte("cfg")}, m.Applied())
}

// Talos refuses a second bootstrap of a node with etcd data, and only a node
// running etcd lists members.
func TestBootstrapOnceAndEtcd(t *testing.T) {
	ctx := context.Background()
	fleet := NewFleet()
	m := NewMachine("v1.14.6")
	m.Configure([]byte("cfg"))
	fleet.Add("10.77.0.11", "node-1", m)
	dial := fleet.Dialer()

	c, err := dial(ctx, talos.Target{Endpoint: "10.77.0.11:50000", Talosconfig: []byte("tc")})
	require.NoError(t, err, "the fleet routes host:port by host")
	_, err = c.EtcdMembers(ctx)
	require.Error(t, err, "no etcd before bootstrap")

	require.NoError(t, c.Bootstrap(ctx, talos.BootstrapOptions{}))
	members, err := c.EtcdMembers(ctx)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, "node-1", members[0].Hostname)
	assert.False(t, members[0].IsLearner)
	require.ErrorIs(t, c.Bootstrap(ctx, talos.BootstrapOptions{}), talos.ErrAlreadyBootstrapped)
	assert.Equal(t, 2, m.BootstrapCalls())

	_, err = dial(ctx, talos.Target{Endpoint: "10.77.0.99", Talosconfig: []byte("tc")})
	require.Error(t, err, "an address with no machine has no route")
}

func TestUnreachable(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	m.SetUnreachable(true)

	// The real client connects lazily: the dial succeeds and the call fails
	// with Unavailable, which is what the callers must recognise.
	c, err := m.Dialer()(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.NoError(t, err)
	_, err = c.Version(ctx)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, codes.Unavailable, status.Code(c.Bootstrap(ctx, talos.BootstrapOptions{})),
		"a call that never arrives is not a refusal")
	assert.Zero(t, m.BootstrapCalls(), "an unreachable node counts no call")

	m.SetUnreachable(false)
	_, err = c.Version(ctx)
	require.NoError(t, err, "the same client answers once the machine is back")
}

func TestTargetValidation(t *testing.T) {
	ctx := context.Background()
	dial := NewMachine("v").Dialer()

	_, err := dial(ctx, talos.Target{Endpoint: "n1", Insecure: true, Talosconfig: []byte("tc")})
	assert.Error(t, err)

	_, err = dial(ctx, talos.Target{Endpoint: "n1"})
	assert.Error(t, err)
}

// threeMembers is a bootstrapped node-1 that node-2 and node-3 joined.
func threeMembers(t *testing.T) (*Fleet, []*Machine) {
	t.Helper()
	ctx := context.Background()
	fleet := NewFleet()
	var ms []*Machine
	for i, name := range []string{"node-1", "node-2", "node-3"} {
		m := NewMachine("v1.14.6")
		m.Configure([]byte("cfg"))
		fleet.Add("10.77.0.1"+string(rune('1'+i)), name, m)
		ms = append(ms, m)
	}
	c, err := fleet.Dialer()(ctx, talos.Target{Endpoint: "10.77.0.11", Talosconfig: []byte("tc")})
	require.NoError(t, err)
	require.NoError(t, c.Bootstrap(ctx, talos.BootstrapOptions{}))
	ms[1].JoinEtcd(ms[0].Etcd(), false)
	ms[2].JoinEtcd(ms[0].Etcd(), true)
	return fleet, ms
}

func secure(t *testing.T, fleet *Fleet, addr string) talos.Client {
	t.Helper()
	c, err := fleet.Dialer()(context.Background(), talos.Target{Endpoint: addr, Talosconfig: []byte("tc")})
	require.NoError(t, err)
	return c
}

// Members share one membership: a learner shows as one until promoted, and
// leadership moves off a member that forfeits it.
func TestEtcdMembershipIsShared(t *testing.T) {
	ctx := context.Background()
	fleet, ms := threeMembers(t)

	members, err := secure(t, fleet, "10.77.0.12").EtcdMembers(ctx)
	require.NoError(t, err)
	require.Len(t, members, 3)
	assert.Equal(t, []bool{false, false, true}, []bool{members[0].IsLearner, members[1].IsLearner, members[2].IsLearner})
	ms[0].Etcd().Promote("node-3")
	members, err = secure(t, fleet, "10.77.0.13").EtcdMembers(ctx)
	require.NoError(t, err)
	assert.False(t, members[2].IsLearner)

	c1 := secure(t, fleet, "10.77.0.11")
	st, err := c1.EtcdStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, st.MemberID, st.LeaderID, "the bootstrapping member leads")
	require.NoError(t, c1.EtcdForfeitLeadership(ctx))
	st, err = c1.EtcdStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, members[1].ID, st.LeaderID)
}

// Leaving removes the member and its data; a removed member's etcd stops
// answering; removing an unknown ID is an error.
func TestEtcdLeaveAndRemove(t *testing.T) {
	ctx := context.Background()
	fleet, ms := threeMembers(t)

	require.NoError(t, secure(t, fleet, "10.77.0.13").EtcdLeaveCluster(ctx))
	assert.False(t, ms[2].Bootstrapped(), "a node that left keeps no etcd data")
	assert.Len(t, ms[0].Etcd().Members(), 2)

	c1 := secure(t, fleet, "10.77.0.11")
	id2 := ms[0].Etcd().Members()[1].ID
	require.NoError(t, c1.EtcdRemoveMemberByID(ctx, id2))
	assert.Equal(t, []uint64{id2}, ms[0].RemovedByID())
	_, err := secure(t, fleet, "10.77.0.12").EtcdMembers(ctx)
	require.Error(t, err, "a removed member no longer serves")
	require.Error(t, c1.EtcdRemoveMemberByID(ctx, id2), "already gone")
}

// A forced reset of a member's EPHEMERAL leaves its member listed (the stale
// member); a graceful one leaves etcd first. Wiping STATE returns the node to
// maintenance, and a reset without reboot ends powered off.
func TestReset(t *testing.T) {
	ctx := context.Background()
	fleet, ms := threeMembers(t)

	require.NoError(t, secure(t, fleet, "10.77.0.13").Reset(ctx, talos.ResetOptions{
		Graceful: false, Reboot: true, SystemPartitions: []string{"EPHEMERAL"},
	}))
	assert.Len(t, ms[0].Etcd().Members(), 3, "a forced reset leaves the stale member")
	assert.False(t, ms[2].Bootstrapped())

	require.NoError(t, secure(t, fleet, "10.77.0.12").Reset(ctx, talos.ResetOptions{Graceful: true}))
	assert.Len(t, ms[0].Etcd().Members(), 2, "a graceful reset leaves etcd")
	assert.Empty(t, ms[1].Applied(), "STATE wiped")
	off, err := fleet.Dialer()(ctx, talos.Target{Endpoint: "10.77.0.12", Insecure: true})
	require.NoError(t, err)
	_, err = off.Version(ctx)
	assert.Equal(t, codes.Unavailable, status.Code(err), "no reboot: the machine is off")
	assert.Len(t, ms[1].Resets(), 1)
}

// A snapshot streams the scripted bytes; recovery needs an upload, and only
// on a node whose etcd is down.
func TestSnapshotAndRecover(t *testing.T) {
	ctx := context.Background()
	fleet, ms := threeMembers(t)
	ms[0].SetSnapshot([]byte("bbolt"))

	var buf bytes.Buffer
	n, err := secure(t, fleet, "10.77.0.11").EtcdSnapshot(ctx, &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(5), n)
	assert.Equal(t, "bbolt", buf.String())

	require.Error(t, secure(t, fleet, "10.77.0.11").EtcdRecover(ctx, strings.NewReader("x")), "etcd runs here")

	blank := NewMachine("v1.14.6")
	blank.Configure([]byte("cfg"))
	fleet.Add("10.77.0.21", "node-9", blank)
	c := secure(t, fleet, "10.77.0.21")
	require.Error(t, c.Bootstrap(ctx, talos.BootstrapOptions{RecoverEtcd: true}), "nothing uploaded")
	require.NoError(t, c.EtcdRecover(ctx, bytes.NewReader(buf.Bytes())))
	require.NoError(t, c.Bootstrap(ctx, talos.BootstrapOptions{RecoverEtcd: true}))
	assert.Equal(t, []byte("bbolt"), blank.Recovered())
	assert.Equal(t, []talos.BootstrapOptions{{RecoverEtcd: true}}, blank.Bootstraps())
}

// A dry run reports a diff and records nothing; a real apply records its mode.
func TestApplyModes(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	m.Configure([]byte("a: 1\n"))
	c, err := m.Dialer()(ctx, talos.Target{Endpoint: "n1", Talosconfig: []byte("tc")})
	require.NoError(t, err)

	res, err := c.ApplyConfiguration(ctx, []byte("a: 2\n"), talos.ApplyOptions{Mode: talos.ApplyNoReboot, DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, res.Details, "+a: 2")
	assert.Len(t, m.Applied(), 1, "a dry run applies nothing")

	res, err = c.ApplyConfiguration(ctx, []byte("a: 1\n"), talos.ApplyOptions{DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, res.Details, "No changes.")

	_, err = c.ApplyConfiguration(ctx, []byte("a: 2\n"), talos.ApplyOptions{Mode: talos.ApplyStaged})
	require.NoError(t, err)
	assert.Equal(t, []talos.ApplyOptions{{Mode: talos.ApplyStaged}}, m.ApplyCalls())
}

// An upgrade that is not staged reports the image's tag as the new version;
// reboot and shutdown are recorded, and a shut-down machine stops answering.
func TestUpgradeRebootShutdown(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	m.Configure([]byte("cfg"))
	c, err := m.Dialer()(ctx, talos.Target{Endpoint: "n1", Talosconfig: []byte("tc")})
	require.NoError(t, err)

	_, err = c.Upgrade(ctx, talos.UpgradeOptions{Image: "factory.talos.dev/installer/abc:v1.14.7", Stage: true})
	require.NoError(t, err)
	assert.Equal(t, "v1.14.6", m.Version(), "staged: nothing changes before a reboot")
	_, err = c.Upgrade(ctx, talos.UpgradeOptions{Image: "registry:5000/installer:v1.14.7@sha256:" + strings.Repeat("a", 64)})
	require.NoError(t, err)
	assert.Equal(t, "v1.14.7", m.Version())

	require.NoError(t, c.Reboot(ctx, talos.RebootOptions{Mode: talos.RebootPowerCycle}))
	assert.Equal(t, []talos.RebootOptions{{Mode: talos.RebootPowerCycle}}, m.Reboots())
	require.NoError(t, c.Shutdown(ctx, talos.ShutdownOptions{}))
	_, err = c.Version(ctx)
	assert.Equal(t, codes.Unavailable, status.Code(err), "the machine is off")
}

// The boot time is how the executor proves a restart: every path that
// restarts the machine moves it on, and nothing else does.
func TestBootTimeMovesOnEveryRestart(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	m.Configure([]byte("cfg"))
	c, err := m.Dialer()(ctx, talos.Target{Endpoint: "n1", Talosconfig: []byte("tc")})
	require.NoError(t, err)
	at := func() time.Time {
		t.Helper()
		bt, err := c.BootTime(ctx)
		require.NoError(t, err)
		return bt
	}

	first := at()
	assert.Equal(t, first, at(), "reading it changes nothing")
	require.NoError(t, c.Reboot(ctx, talos.RebootOptions{}))
	afterReboot := at()
	assert.True(t, afterReboot.After(first))

	_, err = c.Upgrade(ctx, talos.UpgradeOptions{Image: "installer:v1.14.7", Stage: true})
	require.NoError(t, err)
	assert.Equal(t, afterReboot, at(), "a staged upgrade waits for the next boot")
	m.Boot()
	assert.True(t, at().After(afterReboot))
	assert.Equal(t, "v1.14.7", m.Version(), "the staged image took at the boot")
	assert.Equal(t, 2, m.Boots())
}

func TestSetFailure(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	boom := errors.New("boom")
	m.SetFailure("Version", boom)
	c, err := m.Dialer()(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.NoError(t, err)
	_, err = c.Version(ctx)
	require.ErrorIs(t, err, boom)
	m.SetFailure("Version", nil)
	_, err = c.Version(ctx)
	require.NoError(t, err)
}
