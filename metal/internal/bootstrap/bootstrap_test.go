package bootstrap

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/fake"
)

const version = "v1.14.6"

// configured returns a machine whose config already arrived (by media).
func configured(stage string, ready bool) *fake.Machine {
	m := fake.NewMachine(version)
	m.Configure([]byte("cfg"))
	m.SetStage(stage, ready)
	return m
}

func TestEnsure(t *testing.T) {
	ctx := context.Background()
	tc := []byte("tc")

	t.Run("bootstraps the first member once all answer, then observes", func(t *testing.T) {
		fleet := fake.NewFleet()
		cp1, cp2 := configured("booting", false), configured("booting", false)
		fleet.Add("10.77.0.11", "node-1", cp1)
		fleet.Add("10.77.0.12", "node-2", cp2)
		members := []Member{{"node-2", "10.77.0.12"}, {"node-1", "10.77.0.11"}}

		r := Ensure(ctx, fleet.Dialer(), tc, members, Observation{})
		assert.Equal(t, osv1alpha1.ReasonBootstrapRequested, r.Reason)
		assert.False(t, r.Bootstrapped, "accepting the call is not etcd running")
		assert.True(t, cp1.Bootstrapped(), "the first member by name is the one bootstrapped")
		assert.Zero(t, cp2.BootstrapCalls())

		for range 3 {
			r = Ensure(ctx, fleet.Dialer(), tc, members, Observation{})
			assert.True(t, r.Bootstrapped)
			assert.Equal(t, osv1alpha1.ReasonEtcdRunning, r.Reason)
		}
		assert.Equal(t, 1, cp1.BootstrapCalls(), "an observed etcd is never bootstrapped again")
	})

	t.Run("a silent member holds the bootstrap", func(t *testing.T) {
		fleet := fake.NewFleet()
		cp1, cp2 := configured("booting", false), configured("booting", false)
		cp2.SetUnreachable(true)
		fleet.Add("10.77.0.11", "node-1", cp1)
		fleet.Add("10.77.0.12", "node-2", cp2)

		r := Ensure(ctx, fleet.Dialer(), tc, []Member{{"node-1", "10.77.0.11"}, {"node-2", "10.77.0.12"}}, Observation{})
		assert.Equal(t, osv1alpha1.ReasonAwaitingControlPlane, r.Reason)
		assert.Contains(t, r.Message, "node-2")
		assert.Zero(t, cp1.BootstrapCalls(), "node-2 may already run etcd; it must answer first")
	})

	t.Run("an answering Kubernetes API is proof", func(t *testing.T) {
		fleet := fake.NewFleet()
		cp1 := configured("running", true)
		fleet.Add("10.77.0.11", "node-1", cp1)
		r := Ensure(ctx, fleet.Dialer(), tc, []Member{{"node-1", "10.77.0.11"}}, Observation{KubernetesAnswers: true})
		assert.True(t, r.Bootstrapped)
		assert.Zero(t, cp1.BootstrapCalls())
	})

	t.Run("AlreadyExists reads as bootstrapped", func(t *testing.T) {
		m := configured("booting", false)
		r := Ensure(ctx, m.Dialer(), tc, []Member{{"node-1", "10.77.0.11"}}, Observation{})
		require.Equal(t, osv1alpha1.ReasonBootstrapRequested, r.Reason)
		// etcd has data but is not listing members right now (a reboot), and
		// the request is older than the grace window, so Ensure asks again.
		m.SetEtcdRunning(false)
		r = Ensure(ctx, m.Dialer(), tc, []Member{{"node-1", "10.77.0.11"}}, Observation{
			RequestedAt: r.RequestedAt, Now: r.RequestedAt.Add(RequestGrace + time.Second),
		})
		assert.True(t, r.Bootstrapped)
		assert.Equal(t, 2, m.BootstrapCalls())
	})

	t.Run("a late etcd is waited for, not bootstrapped again", func(t *testing.T) {
		m := configured("booting", false)
		m.SetEtcdStartsLate(true)
		members := []Member{{"node-1", "10.77.0.11"}}
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		r := Ensure(ctx, m.Dialer(), tc, members, Observation{Now: start})
		require.Equal(t, osv1alpha1.ReasonBootstrapRequested, r.Reason)
		require.Equal(t, start, r.RequestedAt)

		// Every round inside the window observes no etcd yet and only waits.
		for i := range 20 {
			now := start.Add(time.Duration(i) * 30 * time.Second)
			r = Ensure(ctx, m.Dialer(), tc, members, Observation{RequestedAt: r.RequestedAt, Now: now})
			assert.Equal(t, osv1alpha1.ReasonBootstrapRequested, r.Reason)
			assert.Equal(t, start, r.RequestedAt, "the request time carries forward")
		}
		assert.Equal(t, 1, m.BootstrapCalls(), "no second call while etcd starts")

		m.SetEtcdRunning(true)
		r = Ensure(ctx, m.Dialer(), tc, members, Observation{RequestedAt: r.RequestedAt, Now: start.Add(11 * time.Minute)})
		assert.True(t, r.Bootstrapped)
		assert.Equal(t, 1, m.BootstrapCalls())
	})

	t.Run("a silent member inside the window keeps the request", func(t *testing.T) {
		m := configured("booting", false)
		m.SetEtcdStartsLate(true)
		members := []Member{{"node-1", "10.77.0.11"}}
		r := Ensure(ctx, m.Dialer(), tc, members, Observation{})
		m.SetUnreachable(true)
		r = Ensure(ctx, m.Dialer(), tc, members, Observation{RequestedAt: r.RequestedAt})
		assert.Equal(t, osv1alpha1.ReasonBootstrapRequested, r.Reason)
		assert.Contains(t, r.Message, "not answering: node-1")
		assert.False(t, r.RequestedAt.IsZero())
	})

	t.Run("no control plane", func(t *testing.T) {
		r := Ensure(ctx, fake.NewFleet().Dialer(), tc, nil, Observation{})
		assert.Equal(t, osv1alpha1.ReasonNoControlPlane, r.Reason)
	})
}

// The container-tier ladder: blank machines, configs over the maintenance
// service, bootstrap, ready. The fake's stage transitions play the machines.
func TestRunApplyInsecure(t *testing.T) {
	fleet := fake.NewFleet()
	cp, w := fake.NewMachine(version), fake.NewMachine(version)
	fleet.Add("10.77.0.11", "node-1", cp)
	fleet.Add("10.77.0.12", "node-2", w)
	o := Options{
		Dial: fleet.Dialer(),
		Nodes: []Node{
			{Name: "node-1", Endpoint: "10.77.0.11", ControlPlane: true, MachineConfig: []byte("cp")},
			{Name: "node-2", Endpoint: "10.77.0.12", MachineConfig: []byte("w")},
		},
		Talosconfig: []byte("tc"), ApplyInsecure: true,
		Poll: 5 * time.Millisecond, Logf: t.Logf,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()

	require.Eventually(t, func() bool { return len(cp.Applied()) == 1 && len(w.Applied()) == 1 }, 5*time.Second, time.Millisecond)
	cp.SetStage("booting", false, "etcd")
	w.SetStage("booting", false)
	require.Eventually(t, cp.Bootstrapped, 5*time.Second, time.Millisecond)
	cp.SetStage("running", true)
	w.SetStage("running", true)

	require.NoError(t, <-done)
	assert.Equal(t, [][]byte{[]byte("cp")}, cp.Applied())
	assert.Equal(t, 1, cp.BootstrapCalls())
	assert.Zero(t, w.BootstrapCalls(), "workers are never bootstrapped")
}

// The CLI loop polls every few milliseconds here while etcd takes its time:
// the request is still sent exactly once.
func TestRunLateEtcdBootstrapsOnce(t *testing.T) {
	m := configured("booting", false)
	m.SetEtcdStartsLate(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Dial: m.Dialer(), Nodes: []Node{{Name: "node-1", Endpoint: "n1", ControlPlane: true}},
			Talosconfig: []byte("tc"), Poll: time.Millisecond, Logf: t.Logf,
		})
	}()

	require.Eventually(t, m.Bootstrapped, 5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond) // dozens of rounds with no etcd running
	m.SetEtcdRunning(true)
	m.SetStage("running", true)
	require.NoError(t, <-done)
	assert.Equal(t, 1, m.BootstrapCalls())
}

// A re-run against a ready cluster changes nothing.
func TestRunIdempotent(t *testing.T) {
	m := configured("running", true)
	o := Options{
		Dial:        m.Dialer(),
		Nodes:       []Node{{Name: "node-1", Endpoint: "n1", ControlPlane: true, MachineConfig: []byte("other")}},
		Talosconfig: []byte("tc"), ApplyInsecure: true,
		Poll: time.Millisecond, Logf: t.Logf,
	}
	require.NoError(t, Run(context.Background(), o))
	require.NoError(t, Run(context.Background(), o))
	assert.Len(t, m.Applied(), 1, "a configured node is not re-applied")
	assert.Equal(t, 1, m.BootstrapCalls())
}

// Without ApplyInsecure a blank machine never proceeds: the metal flow's
// config must arrive by media, and the deadline reports that honestly.
func TestRunBlankMachineWithoutApplyTimesOut(t *testing.T) {
	m := fake.NewMachine(version)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Run(ctx, Options{
		Dial: m.Dialer(), Nodes: []Node{{Name: "node-1", Endpoint: "n1", ControlPlane: true}},
		Talosconfig: []byte("tc"), Poll: 5 * time.Millisecond,
	})
	require.Error(t, err)
	assert.Empty(t, m.Applied())
	assert.False(t, m.Bootstrapped())
}

func TestRunApplyInsecureNeedsConfig(t *testing.T) {
	m := fake.NewMachine(version)
	err := Run(context.Background(), Options{
		Dial: m.Dialer(), Nodes: []Node{{Name: "node-1", Endpoint: "n1", ControlPlane: true}},
		Talosconfig: []byte("tc"), ApplyInsecure: true,
	})
	require.Error(t, err)
}
