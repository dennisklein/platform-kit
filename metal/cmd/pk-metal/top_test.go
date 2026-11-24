package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden views under testdata/top")

// labRack is a lab in the middle of its installs: node-1 runs, node-2 is
// being installed and its VM hung in the firmware once, node-3 waits for
// approval, and a machine outside the fleet failed its operation.
func labRack() topInput {
	t0 := metav1.NewTime(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	host := func(name, serial, ep, claim, node string, phase metalv1alpha1.HostPhase, blocked string) metalv1alpha1.Host {
		h := metalv1alpha1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: metalv1alpha1.HostStatus{
				Identity:  &metalv1alpha1.HostIdentity{Serial: serial, Vendor: "QEMU"},
				ManagedBy: []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: ep}, SystemID: "1", Reachable: true}},
				Phase:     phase, BlockedOn: blocked,
			},
		}
		if claim != "" {
			h.Spec.ClaimRef = &metalv1alpha1.ClaimReference{Namespace: "default", Name: claim}
		}
		if node != "" {
			h.Spec.OS = &metalv1alpha1.HostOS{NodeRef: metalv1alpha1.Reference{Name: node}}
		}
		return h
	}
	endpoint := func(name, addr string, phase metalv1alpha1.EndpointPhase) metalv1alpha1.ManagementEndpoint {
		return metalv1alpha1.ManagementEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       metalv1alpha1.ManagementEndpointSpec{AssetRef: metalv1alpha1.Reference{Name: name}, Address: addr},
			Status:     metalv1alpha1.ManagementEndpointStatus{Phase: phase},
		}
	}
	claim := func(name string, held bool) metalv1alpha1.HostClaim {
		c := metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Status: metalv1alpha1.HostClaimStatus{Phase: metalv1alpha1.ClaimBound}}
		if held {
			c.Annotations = map[string]string{metalv1alpha1.AnnotationHold: "disk swap"}
		}
		return c
	}
	install, _ := bootPlan()
	op := func(name, host string, steps int, approvedBy string, conds ...metav1.Condition) metalv1alpha1.HostOperation {
		o := *install.DeepCopy()
		o.Name, o.Spec.HostRef.Name, o.CreationTimestamp = name, host, t0
		o.Spec.Approval.ApprovedBy = approvedBy
		o.Status = metalv1alpha1.HostOperationStatus{Conditions: conds}
		for i := 0; i < steps; i++ {
			o.Status.Steps = append(o.Status.Steps, metalv1alpha1.OperationStep{Action: actionKey(o.Spec.Actions[i]), Phase: "Succeeded"})
		}
		return o
	}
	node := func(name, addr string, ready metav1.ConditionStatus, reason string) osv1alpha1.TalosNode {
		return osv1alpha1.TalosNode{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       osv1alpha1.TalosNodeSpec{Address: addr},
			Status: osv1alpha1.TalosNodeStatus{Conditions: []metav1.Condition{
				{Type: osv1alpha1.ConditionReady, Status: ready, Reason: reason}}},
		}
	}
	failedOp := op("bios-real1-9f8e7d6c", "pk-real-1", 3, "bob", executed(metav1.ConditionFalse))
	failedOp.Status.Steps[2].Phase = "Failed"
	failedOp.Status.Conditions[0].Message = "the BMC answered 500 to the media mount"

	return topInput{
		Hosts: []metalv1alpha1.Host{
			host("pklab0001", "PKLAB0001", "node-1-bmc", "node-1", "node-1", metalv1alpha1.HostProvisioned, ""),
			host("pklab0002", "PKLAB0002", "node-2-bmc", "node-2", "node-2", metalv1alpha1.HostProvisioning, ""),
			host("pklab0003", "PKLAB0003", "node-3-bmc", "node-3", "node-3", metalv1alpha1.HostAssigned, "Approval(Provisioning)"),
			host("pk-real-1", "R0001", "real-1-bmc", "real-1", "", metalv1alpha1.HostAssigned, ""),
		},
		Endpoints: []metalv1alpha1.ManagementEndpoint{
			endpoint("node-1-bmc", "https://10.77.0.1:8441", metalv1alpha1.EndpointReady),
			endpoint("node-2-bmc", "https://10.77.0.1:8442", metalv1alpha1.EndpointReady),
			endpoint("node-3-bmc", "https://10.77.0.1:8443", metalv1alpha1.EndpointReady),
			endpoint("real-1-bmc", "https://192.0.2.10", metalv1alpha1.EndpointReady),
		},
		Claims: []metalv1alpha1.HostClaim{claim("node-1", false), claim("node-2", false), claim("node-3", false), claim("real-1", true)},
		Ops: []metalv1alpha1.HostOperation{
			op("provision-node1-0a0b0c0d", "pklab0001", 9, "alice", executed(metav1.ConditionTrue)),
			op("provision-node2-1a2b3c4d", "pklab0002", 6, "alice",
				metav1.Condition{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionTrue, Reason: "Approved"},
				metav1.Condition{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting}),
			op("provision-node3-5e6f7a8b", "pklab0003", 0, "",
				metav1.Condition{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionFalse, Reason: metalv1alpha1.ReasonAwaitingApproval}),
			failedOp,
		},
		Nodes: []osv1alpha1.TalosNode{
			node("node-1", "10.77.0.11", metav1.ConditionTrue, osv1alpha1.ReasonNodeReady),
			node("node-2", "10.77.0.12", metav1.ConditionUnknown, osv1alpha1.ReasonMaintenanceMode),
			node("node-3", "10.77.0.13", metav1.ConditionUnknown, osv1alpha1.ReasonAwaitingContact),
		},
		Clusters: []osv1alpha1.TalosCluster{{
			ObjectMeta: metav1.ObjectMeta{Name: "lab"},
			Status: osv1alpha1.TalosClusterStatus{
				ControlPlanes: 1, ReadyControlPlanes: 1, Workers: 2, KubernetesVersion: "v1.36.0",
				Conditions: []metav1.Condition{
					{Type: osv1alpha1.ConditionBootstrapped, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonEtcdRunning},
					{Type: osv1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonNodesNotReady},
				},
			},
		}},
		K8sNodes: []corev1.Node{{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		}},
		FleetURL: "http://127.0.0.1:8440/state",
		Fleet: &fleet.State{Version: 1, Machines: []fleet.MachineState{
			{
				Name: "node-1", Personality: "generic", BMC: "https://10.77.0.1:8441", Serial: "PKLAB0001",
				Power: "On", BootOverride: sim.BootOverrideSnapshot{Target: "Cd", Enabled: "Disabled"},
				Media:      []sim.MediaSnapshot{{ID: "CD"}, {ID: "USBStick"}},
				Requests:   []fleet.Request{{Method: "GET", Path: "/redfish/v1/Systems/1", Status: 200}},
				Backend:    fleet.BackendInfo{Kind: "qemu", Running: true, BootProgress: "OSRunning"},
				ConsoleLog: ".lab/state/node-1/console.log",
			},
			{
				Name: "node-2", Personality: "generic", BMC: "https://10.77.0.1:8442", Serial: "PKLAB0002",
				Power: "On", BootOverride: sim.BootOverrideSnapshot{Target: "Cd", Enabled: "Once"},
				Media: []sim.MediaSnapshot{
					{ID: "CD", Inserted: true, ImageURL: "https://factory.talos.dev/image/abc/v1.14.6/metal-amd64.iso"},
					{ID: "USBStick", Inserted: true, ImageURL: "https://10.77.0.1:30443/media/node-2-config.iso"},
				},
				Requests:   []fleet.Request{{Method: "POST", Path: "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", Status: 204}},
				Backend:    fleet.BackendInfo{Kind: "qemu", Running: true, BootProgress: "OSBootStarted", HangCycles: 1},
				ConsoleLog: ".lab/state/node-2/console.log",
			},
			{
				// node-3's identity differs from its Host's serial; the
				// BMC address still ties them together.
				Name: "node-3", Personality: "supermicro-x12", BMC: "https://10.77.0.1:8443",
				Power: "Off", BootOverride: sim.BootOverrideSnapshot{Target: "None", Enabled: "Disabled"},
				Media:      []sim.MediaSnapshot{{ID: "VirtualMedia1"}, {ID: "VirtualMedia2"}, {ID: "VirtualMedia3"}},
				Backend:    fleet.BackendInfo{Kind: "qemu"},
				ConsoleLog: ".lab/state/node-3/console.log",
			},
		}},
		Consoles: map[string][]string{
			"node-1": {"[talos] task startAllServices (1/1): done", "[talos] service[kubelet](Running): Health check successful"},
			"node-2": {"BdsDxe: loading Boot0002 \"UEFI QEMU DVD-ROM QM00001\"", "[    0.000000] Linux version 6.12.40-talos"},
		},
	}
}

// fakeActions records what the keys asked for.
type fakeActions struct {
	plan     string
	planErr  error
	approved []string
	expected []string
	uids     []types.UID
	holds    []string
	retried  []string
}

func (f *fakeActions) Plan(_ context.Context, ns, name string) (shownPlan, error) {
	return shownPlan{Text: f.plan, User: "carol", UID: "uid-" + types.UID(name)}, f.planErr
}

func (f *fakeActions) Approve(_ context.Context, ns, name string, p shownPlan) (string, error) {
	f.approved = append(f.approved, ns+"/"+name+" as "+p.User)
	f.expected = append(f.expected, p.Text)
	f.uids = append(f.uids, p.UID)
	return "approved " + ns + "/" + name + " as " + p.User + "; the operator starts it now.\n", nil
}

func (f *fakeActions) Hold(_ context.Context, ns, name string, on bool) (string, error) {
	f.holds = append(f.holds, ns+"/"+name+map[bool]string{true: " on", false: " off"}[on])
	return "claim " + ns + "/" + name + " changed\n", nil
}

func (f *fakeActions) Retry(_ context.Context, ns, name string) (string, error) {
	f.retried = append(f.retried, ns+"/"+name)
	return "", errors.New("operation " + ns + "/" + name + " is Running, not Failed")
}

// feed runs msgs through the model in order and returns the model and the
// last command.
func feed(t *testing.T, m tea.Model, msgs ...tea.Msg) (topModel, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, msg := range msgs {
		m, cmd = m.Update(msg)
	}
	tm, ok := m.(topModel)
	require.True(t, ok)
	return tm, cmd
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// screen is the view as a person reads it: the colours removed.
func screen(m topModel) string {
	return ansi.Strip(m.render())
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "top", name)
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run: go test ./cmd/pk-metal -run %s -update", t.Name())
	assert.Equal(t, string(want), got, "the view changed; if that is intended: go test ./cmd/pk-metal -run %s -update", t.Name())
}

func TestTopViewWithFleet(t *testing.T) {
	m, _ := feed(t, newTopModel(nil, &fakeActions{}, nil, 0),
		tea.WindowSizeMsg{Width: 100, Height: 50}, topDataMsg(labRack()))
	golden(t, "fleet-100.golden", screen(m))
}

func TestTopViewRealHardware(t *testing.T) {
	in := labRack()
	in.Fleet, in.FleetURL, in.Consoles, in.K8sNodes = nil, "", nil, nil
	m, _ := feed(t, newTopModel(nil, &fakeActions{}, nil, 0),
		tea.WindowSizeMsg{Width: 100, Height: 50}, topDataMsg(in))
	out := screen(m)
	golden(t, "hardware-100.golden", out)
	assert.NotRegexp(t, `[│┃] (media|last|VM) `, out, "no simulator panels without the fleet")
	assert.NotContains(t, out, "console")
}

func TestTopViewNarrowAndEmpty(t *testing.T) {
	m, _ := feed(t, newTopModel(nil, &fakeActions{}, nil, 0), tea.WindowSizeMsg{Width: 60, Height: 50}, topDataMsg(labRack()))
	for _, l := range strings.Split(screen(m), "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(l), 60, "no line is wider than the terminal: %q", l)
	}

	m, _ = feed(t, newTopModel(nil, nil, nil, 0), topDataMsg(topInput{Loading: []string{"Host"}}))
	out := screen(m)
	assert.Contains(t, out, "no machines")
	assert.Contains(t, out, "still reading: Host")
	assert.Contains(t, out, "tab select  q quit", "without actions, no action keys are offered")
	assert.Contains(t, screen(newTopModel(nil, nil, nil, 0)), "reading the cluster")
}

func TestBuildTop(t *testing.T) {
	d := buildTop(labRack())
	require.Len(t, d.Tiles, 4)
	titles := []string{}
	for _, tl := range d.Tiles {
		titles = append(titles, tl.Title)
	}
	assert.Equal(t, []string{"pklab0001 · node-1", "pklab0002 · node-2", "pklab0003 · node-3", "pk-real-1"}, titles,
		"fleet order first, matched by serial or by BMC address; other hosts after")

	assert.Nil(t, d.Tiles[0].Op, "a succeeded operation is history")
	assert.Equal(t, &opLine{Namespace: "pk-metal", Name: "provision-node2-1a2b3c4d", Phase: "Running",
		Step: "7/9 awaitCheckIn", Approval: "approved by alice"}, d.Tiles[1].Op)
	assert.True(t, d.Tiles[2].Op.Waits)
	assert.Equal(t, "1/9 ejectMedia", d.Tiles[2].Op.Step)
	assert.True(t, d.Tiles[3].Op.Failed)
	assert.Equal(t, "3/9 insertMedia failed", d.Tiles[3].Op.Step)
	assert.True(t, d.Tiles[3].OnHold)
	assert.Equal(t, 2, d.Attention, "node-3 waits for approval, real-1's operation failed")

	assert.Equal(t, []stage{{"BMC", "on", stOK}, {"media", "2 mounted", stOK}, {"VM", "running OSBootStarted", stBad},
		{"node", "MaintenanceMode", stWait}}, d.Tiles[1].Flow)
	assert.Equal(t, []stage{{"BMC", "ready", stOK}, {"host", "Assigned", stBad}, {"node", "-", stIdle}}, d.Tiles[3].Flow,
		"the failed operation makes the host bad")
	assert.Equal(t, stWait, d.Tiles[2].hostState(), "a host that waits for approval waits; it has not failed")

	// A fleet machine with no Host yet still gets a tile.
	in := labRack()
	in.Hosts = nil
	d = buildTop(in)
	require.Len(t, d.Tiles, 3)
	assert.Equal(t, "no Host yet", d.Tiles[0].Phase)
}

func TestTopKeys(t *testing.T) {
	act := &fakeActions{plan: "operation pk-metal/provision-node3-5e6f7a8b\nsteps, in this order:\n   1. eject …\n"}
	m, _ := feed(t, newTopModel(nil, act, nil, 0), tea.WindowSizeMsg{Width: 100}, topDataMsg(labRack()))

	// Selection moves with tab and back with shift+tab, and wraps.
	m, _ = feed(t, m, key("tab"), key("tab"))
	assert.Equal(t, 2, m.sel)
	m, _ = feed(t, m, key("shift+tab"), key("shift+tab"), key("shift+tab"))
	assert.Equal(t, 3, m.sel)

	// a on a machine without a waiting operation says so and does nothing.
	m, _ = feed(t, m, key("tab"))
	m, cmd := feed(t, m, key("a"))
	assert.Nil(t, cmd)
	assert.Contains(t, m.status, "no operation waits for approval")

	// a on node-3 shows the plan; anything but y leaves it unapproved.
	m, _ = feed(t, m, key("tab"), key("tab"))
	m, cmd = feed(t, m, key("a"))
	require.NotNil(t, cmd)
	m, _ = feed(t, m, cmd())
	assert.Equal(t, modePlan, m.mode)
	out := screen(m)
	assert.Contains(t, out, "steps, in this order:")
	assert.Contains(t, out, "Approve as carol? [y/N]")
	golden(t, "plan-100.golden", out)
	m, cmd = feed(t, m, key("n"))
	assert.Nil(t, cmd)
	assert.Equal(t, modeTiles, m.mode)
	assert.Equal(t, errDeclined.Error(), m.status)
	assert.Empty(t, act.approved)

	// y approves the very plan that was shown.
	m, cmd = feed(t, m, key("a"))
	m, _ = feed(t, m, cmd())
	m, cmd = feed(t, m, key("y"))
	require.NotNil(t, cmd)
	m, _ = feed(t, m, cmd())
	assert.Equal(t, []string{"pk-metal/provision-node3-5e6f7a8b as carol"}, act.approved)
	assert.Equal(t, []string{act.plan}, act.expected)
	assert.Equal(t, []types.UID{"uid-provision-node3-5e6f7a8b"}, act.uids, "and the very object it was read from")
	assert.Contains(t, screen(m), "approved pk-metal/provision-node3-5e6f7a8b as carol")

	// A plan left on the screen past the approval window approves nothing.
	clock := time.Now()
	m.now = func() time.Time { return clock }
	m, cmd = feed(t, m, key("a"))
	m, _ = feed(t, m, cmd())
	clock = clock.Add(approvalWindow + time.Second)
	m, cmd = feed(t, m, key("y"))
	assert.Nil(t, cmd)
	assert.Equal(t, modeTiles, m.mode)
	assert.True(t, m.statusBad)
	assert.Contains(t, m.status, "was shown more than 5m ago; nothing changed")
	assert.Len(t, act.approved, 1)
	m.now = nil

	// h holds a claim that is not held, and lifts a hold.
	m, cmd = feed(t, m, key("h"))
	m, _ = feed(t, m, cmd())
	m, _ = feed(t, m, key("tab"))
	m, cmd = feed(t, m, key("h"))
	m, _ = feed(t, m, cmd())
	assert.Equal(t, []string{"default/node-3 on", "default/real-1 off"}, act.holds)

	// r retries a failed operation only; its error shows.
	m, cmd = feed(t, m, key("r"))
	m, _ = feed(t, m, cmd())
	assert.Equal(t, []string{"pk-metal/bios-real1-9f8e7d6c"}, act.retried)
	assert.True(t, m.statusBad)
	assert.Contains(t, screen(m), "is Running, not Failed")
	m, _ = feed(t, m, key("tab"))
	_, cmd = feed(t, m, key("r"))
	assert.Nil(t, cmd, "node-1 has no failed operation")

	// A refused plan shows why, and no plan.
	act.planErr = errors.New("operation pk-metal/x was proposed by you (carol); another person must approve it")
	m, _ = feed(t, m, key("tab"), key("tab"))
	m, cmd = feed(t, m, key("a"))
	m, _ = feed(t, m, cmd())
	assert.Equal(t, modeTiles, m.mode)
	assert.Contains(t, m.status, "another person must approve it")

	_, cmd = feed(t, m, key("q"))
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
}

func TestTopReadsOnChangeAndTick(t *testing.T) {
	src := &countingSource{in: labRack()}
	changes := make(chan struct{}, 1)
	m := newTopModel(src, nil, changes, time.Second)
	// A change while a read is under way does not start a second read.
	m, cmd := feed(t, m, topChangeMsg{})
	require.NotNil(t, cmd)
	m, _ = feed(t, m, topChangeMsg{})
	assert.True(t, m.reading)
	m, cmd = feed(t, m, topDataMsg(src.Read(context.Background())))
	assert.False(t, m.reading)
	assert.NotNil(t, cmd, "the next tick is scheduled")
	_, cmd = feed(t, m, topTickMsg{})
	assert.NotNil(t, cmd, "a tick reads again")
}

// The program runs the model end to end: it reads, draws and quits on q.
func TestTopProgramRunsAndQuits(t *testing.T) {
	src := &countingSource{in: labRack()}
	var out strings.Builder
	in, w := io.Pipe()
	p := tea.NewProgram(newTopModel(src, &fakeActions{}, nil, time.Hour),
		tea.WithInput(in), tea.WithOutput(&out), tea.WithWindowSize(100, 50))
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	require.Eventually(t, func() bool { return src.calls() > 0 }, 5*time.Second, 10*time.Millisecond)
	_, err := w.Write([]byte("q"))
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		p.Kill()
		t.Fatal("q did not quit")
	}
	assert.Contains(t, ansi.Strip(out.String()), "pklab0002")
}

type countingSource struct {
	in    topInput
	reads atomic.Int32
}

func (s *countingSource) Read(context.Context) topInput { s.reads.Add(1); return s.in }

func (s *countingSource) calls() int32 { return s.reads.Load() }

func TestLastTextLines(t *testing.T) {
	raw := "\x1b[2J\x1b[01;01HBdsDxe: loading Boot0001\r\n" +
		"progress 10%\rprogress 100%\r\n\r\n" +
		"\x1b[0m[talos] service[apid](Running)\n"
	assert.Equal(t, []string{"progress 100%", "[talos] service[apid](Running)"}, lastTextLines([]byte(raw), 2))
	assert.Equal(t, []string{"BdsDxe: loading Boot0001", "progress 100%", "[talos] service[apid](Running)"},
		lastTextLines([]byte(raw), 5))
	assert.Empty(t, lastTextLines([]byte("\x1b[2J\x1b[H\r\n"), 3), "screen control alone is no line")
}

func TestFleetStateURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 "",
		"http://127.0.0.1:8440":            "http://127.0.0.1:8440/state",
		"http://127.0.0.1:8440/":           "http://127.0.0.1:8440/state",
		"http://127.0.0.1:8440/state":      "http://127.0.0.1:8440/state",
		"https://lab.example:9000/x/state": "https://lab.example:9000/x/state",
	} {
		got, err := fleetStateURL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := fleetStateURL("127.0.0.1:8440")
	assert.ErrorContains(t, err, "not an http URL")
}

// The source reads a running fleet's state endpoint, and says so when it
// does not answer.
func TestWatchSourceReadsTheFleet(t *testing.T) {
	ctx := context.Background()
	cfg, err := fleet.Parse([]byte("machines:\n  - { name: node-1, listen: \"127.0.0.1:0\" }\n"))
	require.NoError(t, err)
	f, err := fleet.Start(ctx, cfg, fleet.Options{StateListen: "127.0.0.1:0"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close(context.Background()) })

	src := &watchSource{fleetURL: f.StateURL(), http: &http.Client{Timeout: time.Second}}
	in := src.Read(ctx)
	assert.Empty(t, in.FleetErr)
	require.NotNil(t, in.Fleet)
	require.Len(t, in.Fleet.Machines, 1)
	assert.Equal(t, "node-1", in.Fleet.Machines[0].Name)
	d := buildTop(in)
	require.Len(t, d.Tiles, 1)
	assert.Equal(t, "no Host yet", d.Tiles[0].Phase)

	require.NoError(t, f.Close(ctx))
	in = src.Read(ctx)
	assert.Nil(t, in.Fleet)
	assert.Equal(t, "not answering", in.FleetErr)
	assert.Contains(t, buildTop(in).FleetLine, "not answering")
}

// top approves the plan a person saw on the screen, or nothing.
func TestApproveTheShownPlanOnly(t *testing.T) {
	ctx := context.Background()
	op, objs := bootPlan()
	cl := fakeClient(objs...)
	shownOp, plan, err := approvalPlan(ctx, cl, op.Namespace, op.Name, "alice")
	require.NoError(t, err)
	assert.Equal(t, op.Name, shownOp.Name)

	var out strings.Builder
	err = approve(ctx, cl, approval{Namespace: op.Namespace, Name: op.Name, User: "alice", Yes: true, Out: &out,
		Expect: plan + "   10. something the person did not see\n"})
	assert.ErrorContains(t, err, "changed since its plan was shown")
	assert.Empty(t, approvedBy(t, cl, op))

	act := clusterActions{cl: cl}
	_, err = act.Approve(ctx, op.Namespace, op.Name, shownPlan{Text: plan, User: "alice", UID: "an-earlier-object"})
	assert.ErrorContains(t, err, "changed since its plan was shown",
		"the same plan on an operation deleted and created again under the name is another object")
	assert.Empty(t, approvedBy(t, cl, op))

	text, err := act.Approve(ctx, op.Namespace, op.Name, shownPlan{Text: plan, User: "alice", UID: shownOp.UID})
	require.NoError(t, err)
	assert.Equal(t, "alice", approvedBy(t, cl, op))
	assert.NotContains(t, text, "steps, in this order", "the status line gets the outcome, not the plan again")
	assert.Contains(t, text, "approved pk-metal/provision-node1-1a2b3c4d as alice")

	_, _, err = approvalPlan(ctx, cl, op.Namespace, op.Name, "bob")
	assert.ErrorContains(t, err, "already approved by alice")
}

// The keys' actions are the verbs' code: hold and retry through a fake
// cluster change what the verbs change.
func TestClusterActions(t *testing.T) {
	ctx := context.Background()
	c := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "node-1"}}
	failed := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "provision-node1-1"},
		Spec:       metalv1alpha1.HostOperationSpec{Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true)}},
		Status:     metalv1alpha1.HostOperationStatus{Conditions: []metav1.Condition{executed(metav1.ConditionFalse)}},
	}
	cl := fakeClient(c, failed)
	act := clusterActions{cl: cl}

	out, err := act.Hold(ctx, "default", "node-1", true)
	require.NoError(t, err)
	assert.Contains(t, out, "is on hold")
	got := &metalv1alpha1.HostClaim{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(c), got))
	assert.Equal(t, "held in pk-metal top", got.Annotations[metalv1alpha1.AnnotationHold])

	out, err = act.Retry(ctx, "pk-metal", "provision-node1-1")
	require.NoError(t, err)
	assert.Contains(t, out, "deleted pk-metal/provision-node1-1")
}
