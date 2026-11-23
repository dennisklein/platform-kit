package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.Error(t, c.Bootstrap(ctx))

	require.NoError(t, c.ApplyConfiguration(ctx, []byte("cfg")))
	m.SetStage("booting", false, "etcd")

	// The maintenance service is gone now.
	_, err = dial(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.Error(t, err)

	sc, err := dial(ctx, talos.Target{Endpoint: "n1", Talosconfig: []byte("tc")})
	require.NoError(t, err)
	require.NoError(t, sc.Bootstrap(ctx))
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

	require.NoError(t, c.Bootstrap(ctx))
	members, err := c.EtcdMembers(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"node-1"}, members)
	require.ErrorIs(t, c.Bootstrap(ctx), talos.ErrAlreadyBootstrapped)
	assert.Equal(t, 2, m.BootstrapCalls())

	_, err = dial(ctx, talos.Target{Endpoint: "10.77.0.99", Talosconfig: []byte("tc")})
	require.Error(t, err, "an address with no machine has no route")
}

func TestUnreachable(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("v1.14.6")
	m.SetUnreachable(true)

	_, err := m.Dialer()(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.Error(t, err)

	m.SetUnreachable(false)
	_, err = m.Dialer()(ctx, talos.Target{Endpoint: "n1", Insecure: true})
	require.NoError(t, err)
}

func TestTargetValidation(t *testing.T) {
	ctx := context.Background()
	dial := NewMachine("v").Dialer()

	_, err := dial(ctx, talos.Target{Endpoint: "n1", Insecure: true, Talosconfig: []byte("tc")})
	assert.Error(t, err)

	_, err = dial(ctx, talos.Target{Endpoint: "n1"})
	assert.Error(t, err)
}
