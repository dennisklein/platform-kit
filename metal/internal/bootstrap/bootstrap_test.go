package bootstrap

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
)

// The container-tier ladder: blank machine, config over the maintenance
// service, bootstrap, ready. The fake's stage transitions play the machine.
func TestLadderApplyInsecure(t *testing.T) {
	m := fake.NewMachine("v1.13.4")
	o := Options{
		Dial: m.Dialer(), Endpoint: "n1",
		MachineConfig: []byte("cfg"), Talosconfig: []byte("tc"),
		ApplyInsecure: true,
		Poll:          5 * time.Millisecond,
		Logf:          t.Logf,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()

	// The machine converges after the config lands, like the real one.
	require.Eventually(t, func() bool { return len(m.Applied()) == 1 }, 5*time.Second, time.Millisecond)
	m.SetStage("booting", false, "etcd")
	require.Eventually(t, m.Bootstrapped, 5*time.Second, time.Millisecond,
		"bootstrap must be retried until the machine takes it")
	m.SetStage("running", true)

	require.NoError(t, <-done)
	assert.Equal(t, [][]byte{[]byte("cfg")}, m.Applied())
}

// The metal flow: the config rode the media, the machine is already configured
// and converging — bootstrap only bootstraps and waits.
func TestMetalFlowNoApply(t *testing.T) {
	m := fake.NewMachine("v1.13.4")
	// Media boot: config applied out of band, machine converging.
	c, err := m.Dialer()(context.Background(), talos.Target{Endpoint: "n1", Insecure: true})
	require.NoError(t, err)
	require.NoError(t, c.ApplyConfiguration(context.Background(), []byte("media-cfg")))
	require.NoError(t, c.Close())
	m.SetStage("booting", false)

	o := Options{
		Dial: m.Dialer(), Endpoint: "n1", Talosconfig: []byte("tc"),
		Poll: 5 * time.Millisecond, Logf: t.Logf,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()

	require.Eventually(t, m.Bootstrapped, 5*time.Second, time.Millisecond)
	m.SetStage("running", true)
	require.NoError(t, <-done)
	assert.Len(t, m.Applied(), 1, "the metal flow must not re-apply")
}

// A Ready node is a no-op — the idempotence rung.
func TestAlreadyReady(t *testing.T) {
	m := fake.NewMachine("v1.13.4")
	c, err := m.Dialer()(context.Background(), talos.Target{Endpoint: "n1", Insecure: true})
	require.NoError(t, err)
	require.NoError(t, c.ApplyConfiguration(context.Background(), []byte("cfg")))
	require.NoError(t, c.Close())
	m.SetStage("running", true)

	err = Run(context.Background(), Options{
		Dial: m.Dialer(), Endpoint: "n1", Talosconfig: []byte("tc"),
		ApplyInsecure: true, MachineConfig: []byte("other"),
		Poll: time.Millisecond, Logf: t.Logf,
	})
	require.NoError(t, err)
	assert.Len(t, m.Applied(), 1, "a ready node must not be re-applied")
}

// Without ApplyInsecure a blank machine never proceeds — the metal flow's
// config must arrive by media, and the deadline reports that honestly.
func TestBlankMachineWithoutApplyTimesOut(t *testing.T) {
	m := fake.NewMachine("v1.13.4")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Run(ctx, Options{
		Dial: m.Dialer(), Endpoint: "n1", Talosconfig: []byte("tc"),
		Poll: 5 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Empty(t, m.Applied())
	assert.False(t, m.Bootstrapped())
}

func TestApplyInsecureNeedsConfig(t *testing.T) {
	m := fake.NewMachine("v1.13.4")
	err := Run(context.Background(), Options{
		Dial: m.Dialer(), Endpoint: "n1", Talosconfig: []byte("tc"), ApplyInsecure: true,
	})
	require.Error(t, err)
}
