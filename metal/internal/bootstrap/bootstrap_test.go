package bootstrap

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
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

	// Every control plane answers and none runs etcd: after a total loss this
	// looks exactly like a cluster never bootstrapped. Remembered, it is a
	// loss, and only a person may start etcd again.
	t.Run("a cluster bootstrapped before is never bootstrapped again", func(t *testing.T) {
		fleet := fake.NewFleet()
		cp1, cp2 := configured("booting", false), configured("booting", false)
		fleet.Add("10.77.0.11", "node-1", cp1)
		fleet.Add("10.77.0.12", "node-2", cp2)
		members := []Member{{"node-1", "10.77.0.11"}, {"node-2", "10.77.0.12"}}
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		for _, obs := range []Observation{
			{BootstrappedAt: at},
			// Even with a request outstanding past its window, the case in
			// which the old code asked again.
			{BootstrappedAt: at, RequestedAt: at, Now: at.Add(RequestGrace + time.Hour)},
		} {
			r := Ensure(ctx, fleet.Dialer(), tc, members, obs)
			assert.False(t, r.Bootstrapped)
			assert.Equal(t, osv1alpha1.ReasonEtcdDataLost, r.Reason)
			assert.Contains(t, r.Message, "2026-01-01T00:00:00Z")
			assert.Contains(t, r.Message, RestoreProcedure)
		}
		assert.Zero(t, cp1.BootstrapCalls()+cp2.BootstrapCalls())

		// A silent member may still hold the data: that is not a loss yet,
		// and still no bootstrap.
		cp2.SetUnreachable(true)
		r := Ensure(ctx, fleet.Dialer(), tc, members, Observation{BootstrappedAt: at})
		assert.Equal(t, osv1alpha1.ReasonEtcdUnavailable, r.Reason)
		assert.Contains(t, r.Message, "not answering: node-2")
		assert.Zero(t, cp1.BootstrapCalls())

		// A member back with etcd is observed as before.
		cp2.SetUnreachable(false)
		cp2.SetEtcdRunning(true)
		r = Ensure(ctx, fleet.Dialer(), tc, members, Observation{BootstrappedAt: at})
		assert.True(t, r.Bootstrapped)
	})

	// A cluster re-created to be restored was never seen bootstrapped, and
	// looks exactly like a new one; the mark keeps it from starting empty.
	// Once the recovery starts etcd it reads as bootstrapped as usual.
	t.Run("a cluster marked for restore is never bootstrapped", func(t *testing.T) {
		fleet := fake.NewFleet()
		cp1 := configured("booting", false)
		fleet.Add("10.77.0.11", "node-1", cp1)
		members := []Member{{"node-1", "10.77.0.11"}}

		r := Ensure(ctx, fleet.Dialer(), tc, members, Observation{RestorePending: true})
		assert.False(t, r.Bootstrapped)
		assert.Equal(t, osv1alpha1.ReasonRestorePending, r.Reason)
		assert.Contains(t, r.Message, RestoreProcedure)
		assert.Zero(t, cp1.BootstrapCalls())

		cp1.SetEtcdRunning(true)
		r = Ensure(ctx, fleet.Dialer(), tc, members, Observation{RestorePending: true})
		assert.True(t, r.Bootstrapped)
	})

	// node-2 runs an etcd that answers its status but cannot list members:
	// it holds the data of a cluster that lost quorum. Bootstrapping node-1
	// would start a second etcd beside it.
	t.Run("a member holding etcd data blocks the first bootstrap", func(t *testing.T) {
		fleet := fake.NewFleet()
		cp1, cp2 := configured("booting", false), configured("booting", false)
		cp2.SetEtcdRunning(true)
		cp2.SetFailure("EtcdMembers", errors.New("etcdserver: request timed out"))
		fleet.Add("10.77.0.11", "node-1", cp1)
		fleet.Add("10.77.0.12", "node-2", cp2)
		members := []Member{{"node-1", "10.77.0.11"}, {"node-2", "10.77.0.12"}}

		r := Ensure(ctx, fleet.Dialer(), tc, members, Observation{})
		assert.False(t, r.Bootstrapped)
		assert.Equal(t, osv1alpha1.ReasonEtcdDataPresent, r.Reason)
		assert.Contains(t, r.Message, "node-2")
		assert.Contains(t, r.Message, RestoreProcedure)
		assert.Zero(t, cp1.BootstrapCalls(), "no second etcd beside the one node-2 holds")

		// Remembered as bootstrapped, the same member reads as quorum lost.
		r = Ensure(ctx, fleet.Dialer(), tc, members, Observation{BootstrappedAt: time.Now()})
		assert.Equal(t, osv1alpha1.ReasonEtcdUnavailable, r.Reason)
		assert.Contains(t, r.Message, "quorum lost")
		assert.Zero(t, cp1.BootstrapCalls())
	})
}

// The day-0 ladder on machines whose config came on the media: bootstrap,
// ready. The fake's stage transitions play the machines. Run sends no config.
func TestRunLadder(t *testing.T) {
	fleet := fake.NewFleet()
	cp, w := configured("booting", false), configured("booting", false)
	fleet.Add("10.77.0.11", "node-1", cp)
	fleet.Add("10.77.0.12", "node-2", w)
	o := Options{
		Dial: fleet.Dialer(),
		Nodes: []Node{
			{Name: "node-1", Endpoint: "10.77.0.11", ControlPlane: true},
			{Name: "node-2", Endpoint: "10.77.0.12"},
		},
		Talosconfig: []byte("tc"),
		Poll:        5 * time.Millisecond, Logf: t.Logf,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()

	cp.SetStage("booting", false, "etcd")
	require.Eventually(t, cp.Bootstrapped, 5*time.Second, time.Millisecond)
	cp.SetStage("running", true)
	w.SetStage("running", true)

	require.NoError(t, <-done)
	assert.Equal(t, [][]byte{[]byte("cfg")}, cp.Applied(), "Run sends no config")
	assert.Equal(t, [][]byte{[]byte("cfg")}, w.Applied(), "Run sends no config")
	assert.Equal(t, 1, cp.BootstrapCalls())
	assert.Zero(t, w.BootstrapCalls(), "workers are never bootstrapped")
}

// The CLI loop polls every few milliseconds here while etcd takes its time:
// the request is still sent exactly once.
func TestRunLateEtcdBootstrapsOnce(t *testing.T) {
	m := configured("booting", false)
	m.SetEtcdStartsLate(true)
	// Every round dials the machine, so the dials count the rounds.
	var dials atomic.Int64
	dial := func(ctx context.Context, tgt talos.Target) (talos.Client, error) {
		dials.Add(1)
		return m.Dialer()(ctx, tgt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Dial: dial, Nodes: []Node{{Name: "node-1", Endpoint: "n1", ControlPlane: true}},
			Talosconfig: []byte("tc"), Poll: time.Millisecond, Logf: t.Logf,
		})
	}()

	require.Eventually(t, m.Bootstrapped, 5*time.Second, time.Millisecond)
	// Dozens of rounds with no etcd running.
	mark := dials.Load()
	require.Eventually(t, func() bool { return dials.Load() >= mark+30 }, 5*time.Second, time.Millisecond)
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
		Nodes:       []Node{{Name: "node-1", Endpoint: "n1", ControlPlane: true}},
		Talosconfig: []byte("tc"),
		Poll:        time.Millisecond, Logf: t.Logf,
	}
	require.NoError(t, Run(context.Background(), o))
	require.NoError(t, Run(context.Background(), o))
	assert.Len(t, m.Applied(), 1, "Run sends no config")
	assert.Equal(t, 1, m.BootstrapCalls())
}

// A blank machine never proceeds: the config must arrive on the media, Run
// never sends it in band, and the deadline reports that honestly.
func TestRunBlankMachineTimesOut(t *testing.T) {
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
