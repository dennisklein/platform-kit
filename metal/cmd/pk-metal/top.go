package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
)

func init() {
	verbHelp["top"] = `
A live view of every machine, updated as the cluster changes. One tile per
machine shows its BMC, the Host phase and what blocks it, the claim, the
operation that runs (for example "5/9 setBootOverride") and whether it waits
for approval, and the TalosNode. The header shows each TalosCluster and how
many nodes are Ready, and each TalosRollout that has not succeeded: its nodes
done and where it stands (approve and resume it with pk-metal rollout).

With --fleet (the simulator's state endpoint, as in the virtual lab) each
tile also shows what the simulated BMC holds (power, media, boot override,
the last Redfish call), whether the VM runs, and the last console lines.
Without it, top works the same against real hardware.

With --workload-kubeconfig the tiles also show whether each node is Ready in
the workload cluster itself.

keys:
  tab, arrows  select a machine
  a            show the plan of the operation that waits for approval, as
               pk-metal approve does, and approve it on y
  h            hold the claim, or remove the hold
  r            retry the failed operation (pk-metal retry)
  q            quit

examples:
  pk-metal top
  pk-metal --kubeconfig .lab/kubeconfig top --fleet http://127.0.0.1:8440/state`
}

func topCmd(args []string) {
	fs := newFlags("top")
	fleetURL := fs.String("fleet", "", "the simulator's state endpoint, for example http://127.0.0.1:8440/state (default: none, real hardware)")
	workload := fs.String("workload-kubeconfig", "", "kubeconfig of the workload cluster, to show its Nodes' Ready condition (default: none)")
	every := fs.Duration("interval", 2*time.Second, "how often the fleet and the console logs are read again; cluster objects update as they change")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "top takes no arguments")
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		dief("top needs a terminal; in a script use: pk-metal status")
	}
	stateURL, err := fleetStateURL(*fleetURL)
	if err != nil {
		badUsage(fs, err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg, err := restConfig()
	if err != nil {
		die(err)
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	changes := make(chan struct{}, 1)
	src, err := newWatchSource(ctx, cfg, *workload, stateURL, changes)
	if err != nil {
		die(err)
	}
	m := newTopModel(src, clusterActions{cl: cl}, changes, *every)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		die(err)
	}
}

// fleetStateURL accepts the endpoint with or without its path.
func fleetStateURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("--fleet %q is not an http URL, for example http://127.0.0.1:8440/state", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = fleet.StatePath
	}
	return u.String(), nil
}

// watchSource reads the cluster from informer caches, which watches keep
// current, and the fleet over HTTP. A kind whose watch has not synced yet is
// named as loading instead of blocking the screen; a kind the cluster does
// not have (the OS kinds not installed) is left out.
type watchSource struct {
	mgmt      cache.Cache
	informers map[string]cache.Informer // by kind, only kinds the cluster has
	workload  cache.Cache
	nodes     cache.Informer
	fleetURL  string
	http      *http.Client
}

// topKinds are the kinds top watches in the management cluster.
var topKinds = []struct {
	name string
	obj  client.Object
}{
	{"Host", &metalv1alpha1.Host{}},
	{"ManagementEndpoint", &metalv1alpha1.ManagementEndpoint{}},
	{"HostClaim", &metalv1alpha1.HostClaim{}},
	{"HostOperation", &metalv1alpha1.HostOperation{}},
	{"TalosNode", &osv1alpha1.TalosNode{}},
	{"TalosCluster", &osv1alpha1.TalosCluster{}},
	{"TalosRollout", &osv1alpha1.TalosRollout{}},
}

func newWatchSource(ctx context.Context, cfg *rest.Config, workloadKubeconfig, fleetURL string,
	changes chan<- struct{}) (*watchSource, error) {
	notify := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { poke(changes) },
		UpdateFunc: func(any, any) { poke(changes) },
		DeleteFunc: func(any) { poke(changes) },
	}
	mgmt, err := cache.New(cfg, cache.Options{Scheme: newScheme()})
	if err != nil {
		return nil, fmt.Errorf("cannot watch the cluster: %w", err)
	}
	s := &watchSource{mgmt: mgmt, informers: map[string]cache.Informer{}, fleetURL: fleetURL,
		http: &http.Client{Timeout: 2 * time.Second}}
	for _, k := range topKinds {
		inf, err := mgmt.GetInformer(ctx, k.obj, cache.BlockUntilSynced(false))
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot watch %s: %w", k.name, err)
		}
		if _, err := inf.AddEventHandler(notify); err != nil {
			return nil, err
		}
		s.informers[k.name] = inf
	}
	go func() { _ = mgmt.Start(ctx) }()

	if workloadKubeconfig != "" {
		wcfg, err := clientcmd.BuildConfigFromFlags("", workloadKubeconfig)
		if err != nil {
			return nil, fmt.Errorf("--workload-kubeconfig: %w", err)
		}
		sch := runtime.NewScheme()
		if err := scheme.AddToScheme(sch); err != nil {
			return nil, err
		}
		s.workload, err = cache.New(wcfg, cache.Options{Scheme: sch})
		if err != nil {
			return nil, fmt.Errorf("cannot watch the workload cluster: %w", err)
		}
		s.nodes, err = s.workload.GetInformer(ctx, &corev1.Node{}, cache.BlockUntilSynced(false))
		if err != nil {
			return nil, fmt.Errorf("cannot watch the workload cluster's Nodes: %w", err)
		}
		if _, err := s.nodes.AddEventHandler(notify); err != nil {
			return nil, err
		}
		go func() { _ = s.workload.Start(ctx) }()
	}
	return s, nil
}

// poke tells the screen something changed; one pending poke is enough.
func poke(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *watchSource) Read(ctx context.Context) topInput {
	var in topInput
	lists := map[string]client.ObjectList{
		"Host":               &metalv1alpha1.HostList{},
		"ManagementEndpoint": &metalv1alpha1.ManagementEndpointList{},
		"HostClaim":          &metalv1alpha1.HostClaimList{},
		"HostOperation":      &metalv1alpha1.HostOperationList{},
		"TalosNode":          &osv1alpha1.TalosNodeList{},
		"TalosCluster":       &osv1alpha1.TalosClusterList{},
		"TalosRollout":       &osv1alpha1.TalosRolloutList{},
	}
	for _, k := range topKinds {
		inf, ok := s.informers[k.name]
		if !ok {
			continue
		}
		if !inf.HasSynced() {
			in.Loading = append(in.Loading, k.name)
			continue
		}
		if err := s.mgmt.List(ctx, lists[k.name]); err != nil {
			in.Loading = append(in.Loading, k.name)
		}
	}
	in.Hosts = lists["Host"].(*metalv1alpha1.HostList).Items
	in.Endpoints = lists["ManagementEndpoint"].(*metalv1alpha1.ManagementEndpointList).Items
	in.Claims = lists["HostClaim"].(*metalv1alpha1.HostClaimList).Items
	in.Ops = lists["HostOperation"].(*metalv1alpha1.HostOperationList).Items
	in.Nodes = lists["TalosNode"].(*osv1alpha1.TalosNodeList).Items
	in.Clusters = lists["TalosCluster"].(*osv1alpha1.TalosClusterList).Items
	in.Rollouts = lists["TalosRollout"].(*osv1alpha1.TalosRolloutList).Items

	if s.nodes != nil {
		nodes := &corev1.NodeList{}
		if s.nodes.HasSynced() && s.workload.List(ctx, nodes) == nil {
			in.K8sNodes = nodes.Items
			if in.K8sNodes == nil {
				in.K8sNodes = []corev1.Node{}
			}
		} else {
			in.Loading = append(in.Loading, "workload Nodes")
		}
	}

	if s.fleetURL != "" {
		in.FleetURL = s.fleetURL
		st, err := s.readFleet(ctx)
		if err != nil {
			in.FleetErr = err.Error()
		} else {
			in.Fleet = st
			in.Consoles = map[string][]string{}
			for _, m := range st.Machines {
				if m.ConsoleLog != "" {
					in.Consoles[m.Name] = consoleTail(m.ConsoleLog, consoleLines)
				}
			}
		}
	}
	return in
}

func (s *watchSource) readFleet(ctx context.Context) (*fleet.State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.fleetURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, errors.New("not answering")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answers %s", resp.Status)
	}
	var st fleet.State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("answers something that is not its state: %w", err)
	}
	return &st, nil
}

// consoleTail reads the last lines of a console capture as text: escape
// sequences, carriage returns and blank lines removed.
func consoleTail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []string{"(cannot read: " + err.Error() + ")"}
	}
	defer func() { _ = f.Close() }()
	const window = 16 << 10
	if st, err := f.Stat(); err == nil && st.Size() > window {
		_, _ = f.Seek(-window, io.SeekEnd)
	}
	raw, _ := io.ReadAll(f)
	return lastTextLines(raw, n)
}

// lastTextLines is the pure part of consoleTail.
func lastTextLines(raw []byte, n int) []string {
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		// A carriage return starts the line over: what follows the last one
		// is what the screen shows.
		if i := strings.LastIndex(strings.TrimRight(l, "\r"), "\r"); i >= 0 {
			l = l[i+1:]
		}
		l = strings.TrimSpace(ansi.Strip(l))
		l = strings.Map(func(r rune) rune {
			if r < ' ' || r == 0x7f {
				return -1
			}
			return r
		}, l)
		if l != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// clusterActions runs the keys' actions with the code of the verbs.
type clusterActions struct {
	cl client.Client
}

func (a clusterActions) Plan(ctx context.Context, namespace, name string) (shownPlan, error) {
	user, err := whoAmI(ctx, a.cl)
	if err != nil {
		return shownPlan{}, err
	}
	op, plan, err := approvalPlan(ctx, a.cl, namespace, name, user)
	if err != nil {
		return shownPlan{}, err
	}
	return shownPlan{Text: plan, User: user, UID: op.UID}, nil
}

func (a clusterActions) Approve(ctx context.Context, namespace, name string, p shownPlan) (string, error) {
	var out bytes.Buffer
	err := approve(ctx, a.cl, approval{Namespace: namespace, Name: name, User: p.User, Yes: true, Out: &out,
		Expect: p.Text, ExpectUID: p.UID})
	if err != nil {
		return "", err
	}
	// approve printed the plan again, then its outcome.
	return strings.TrimPrefix(out.String(), p.Text), nil
}

func (a clusterActions) Hold(ctx context.Context, namespace, name string, on bool) (string, error) {
	var out bytes.Buffer
	err := hold(ctx, a.cl, namespace, name, "held in pk-metal top", on, &out)
	return out.String(), err
}

func (a clusterActions) Retry(ctx context.Context, namespace, name string) (string, error) {
	var out bytes.Buffer
	err := retry(ctx, a.cl, namespace, name, &out)
	return out.String(), err
}
