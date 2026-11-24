package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/drain"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// The Talos and Kubernetes steps. They act through the node's machine API or
// its cluster's Kubernetes API, never through the BMC, so an operation made
// only of them runs while the BMC is down (see usesBMC).
//
// Each step checks its goal against the node before it acts, as the BMC
// steps do, and a step that restarts the node records the node's boot time
// before it asks (status.talosStep): the restart shows as a later boot time,
// whatever the reboot path and however short the outage, and a resumed pass
// that finds the boot time moved on does not ask a second time.
//
// A node that does not answer is a wait, not a failure: a reboot is exactly
// that, and the step's budget (talosStepBudget) ends a wait that lasts too
// long. A node that answers and refuses fails the step.

// Default budgets of the Talos steps; the waits name theirs in the action.
const (
	// talosRestartTimeout bounds a reboot, a reset, a shutdown, or a config
	// apply that reboots, from the first look to the node running again.
	talosRestartTimeout = 20 * time.Minute
	// talosUpgradeTimeout adds the image pull and the install to a restart.
	talosUpgradeTimeout = 45 * time.Minute
	// talosUpgradePass is the pass budget of an operation that upgrades: the
	// node acknowledges the request only once it pulled the image, which
	// machineapi bounds at 30 minutes.
	talosUpgradePass = 35 * time.Minute
	// defaultDrainTimeout, defaultNodeReadyTimeout and
	// defaultEtcdHealthyTimeout mirror the schema defaults.
	defaultDrainTimeout       = 900 * time.Second
	defaultNodeReadyTimeout   = 1800 * time.Second
	defaultEtcdHealthyTimeout = 900 * time.Second
)

// talosNode resolves an action's nodeRef: the TalosNode, its cluster, and
// the access to both. The node must be the one the Host runs: the name is
// fixed at proposal, and an operation on one host never acts on another
// host's node.
func (x *exec) talosNode(ctx context.Context, ref metalv1alpha1.Reference) (*osv1alpha1.TalosNode, *talosAccess, error) {
	if os := x.host.Spec.OS; os == nil || os.NodeRef.Name != ref.Name {
		runs := "no node"
		if os != nil {
			runs = "TalosNode " + os.NodeRef.Name
		}
		return nil, nil, fmt.Errorf("the step names TalosNode %s, but host %s runs %s", ref.Name, x.host.Name, runs)
	}
	node := &osv1alpha1.TalosNode{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: ref.Name}, node); err != nil {
		return nil, nil, fmt.Errorf("TalosNode %s: %w", ref.Name, err)
	}
	if x.talos != nil && x.talos.cluster.Name == node.Spec.ClusterRef.Name {
		return node, x.talos, nil
	}
	cluster := &osv1alpha1.TalosCluster{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		return nil, nil, fmt.Errorf("TalosCluster %s of node %s: %w", node.Spec.ClusterRef.Name, node.Name, err)
	}
	acc, err := newTalosAccess(ctx, x.r.Client, x.r.CredentialsNamespace, x.r.TalosDial, cluster)
	if err != nil {
		return nil, nil, err
	}
	x.talos = acc
	return node, acc, nil
}

// workload opens the Kubernetes API of the node's cluster, once per pass.
func (x *exec) workload(ctx context.Context, acc *talosAccess) (client.Client, error) {
	if x.kube != nil {
		return x.kube, nil
	}
	c, err := workloadClient(ctx, x.r.Client, x.r.CredentialsNamespace, acc.cluster, x.r.Workload)
	if err != nil {
		return nil, err
	}
	x.kube = c
	return c, nil
}

// progress is the recorded progress of step i, or nil.
func (x *exec) progress(i int) *metalv1alpha1.TalosStepProgress {
	if p := x.op.Status.TalosStep; p != nil && int(p.Step) == i {
		return p
	}
	return nil
}

// record persists the progress of step i before the step goes on: what a
// resumed pass continues from.
func (x *exec) record(p *metalv1alpha1.TalosStepProgress) error {
	x.op.Status.TalosStep = p
	if err := x.flush(); err != nil {
		return &persistError{err}
	}
	return nil
}

// talosStep runs action i when it is one of the Talos or Kubernetes actions;
// ok is false otherwise.
func (x *exec) talosStep(ctx context.Context, i int) (st stepState, ok bool, err error) {
	a := x.op.Spec.Actions[i]
	switch {
	case a.Cordon != nil:
		st, err = x.cordon(ctx, a.Cordon.NodeRef)
	case a.Drain != nil:
		st, err = x.drain(ctx, a.Drain)
	case a.Uncordon != nil:
		st, err = x.uncordon(ctx, a.Uncordon.NodeRef)
	case a.TalosReboot != nil:
		st, err = x.talosReboot(ctx, i, a.TalosReboot)
	case a.TalosShutdown != nil:
		st, err = x.talosShutdown(ctx, i, a.TalosShutdown)
	case a.TalosUpgrade != nil:
		st, err = x.talosUpgrade(ctx, i, a.TalosUpgrade)
	case a.TalosApplyConfig != nil:
		st, err = x.talosApplyConfig(ctx, i, a.TalosApplyConfig)
	case a.EtcdLeave != nil:
		st, err = x.etcdLeave(ctx, a.EtcdLeave)
	case a.EtcdRemoveMember != nil:
		st, err = x.etcdRemoveMember(ctx, a.EtcdRemoveMember)
	case a.TalosReset != nil:
		st, err = x.talosReset(ctx, i, a.TalosReset)
	case a.WaitNodeReady != nil:
		st, err = x.waitNodeReady(ctx, a.WaitNodeReady)
	case a.WaitEtcdHealthy != nil:
		st, err = x.waitEtcdHealthy(ctx, a.WaitEtcdHealthy)
	case a.EtcdForfeitLeadership != nil:
		st, err = x.etcdForfeitLeadership(ctx, a.EtcdForfeitLeadership)
	case a.DeleteKubernetesNode != nil:
		st, err = x.deleteKubernetesNode(ctx, a.DeleteKubernetesNode)
	default:
		return stepState{}, false, nil
	}
	return st, true, err
}

// talosStepBudget is how long a Talos or Kubernetes step may keep waiting;
// ok is false for the other actions.
func talosStepBudget(a metalv1alpha1.HostOperationAction) (time.Duration, bool) {
	secs := func(s int32, def time.Duration) time.Duration {
		if s > 0 {
			return time.Duration(s) * time.Second
		}
		return def
	}
	switch {
	case a.Drain != nil:
		return secs(a.Drain.TimeoutSeconds, defaultDrainTimeout), true
	case a.WaitNodeReady != nil:
		return secs(a.WaitNodeReady.TimeoutSeconds, defaultNodeReadyTimeout), true
	case a.WaitEtcdHealthy != nil:
		return secs(a.WaitEtcdHealthy.TimeoutSeconds, defaultEtcdHealthyTimeout), true
	case a.TalosUpgrade != nil:
		return talosUpgradeTimeout, true
	case a.TalosReboot != nil, a.TalosShutdown != nil, a.TalosReset != nil, a.TalosApplyConfig != nil:
		return talosRestartTimeout, true
	}
	return 0, false
}

// cordon makes the node unschedulable, and records it when this operation
// did, so that an abort hands back only its own cordon.
func (x *exec) cordon(ctx context.Context, ref metalv1alpha1.Reference) (stepState, error) {
	_, acc, err := x.talosNode(ctx, ref)
	if err != nil {
		return stepState{}, err
	}
	kube, err := x.workload(ctx, acc)
	if err != nil {
		return stepState{}, err
	}
	changed, err := drain.Cordon(ctx, kube, ref.Name)
	if err != nil {
		return stepState{}, err
	}
	return stepDone, x.noteCordon(ref.Name, changed)
}

func (x *exec) noteCordon(node string, changed bool) error {
	if !changed || x.op.Status.CordonedNode == node {
		return nil
	}
	x.op.Status.CordonedNode = node
	if err := x.flush(); err != nil {
		return &persistError{err}
	}
	return nil
}

func (x *exec) uncordon(ctx context.Context, ref metalv1alpha1.Reference) (stepState, error) {
	_, acc, err := x.talosNode(ctx, ref)
	if err != nil {
		return stepState{}, err
	}
	kube, err := x.workload(ctx, acc)
	if err != nil {
		return stepState{}, err
	}
	if _, err := drain.Uncordon(ctx, kube, ref.Name); err != nil {
		return stepState{}, err
	}
	if x.op.Status.CordonedNode == ref.Name {
		x.op.Status.CordonedNode = ""
	}
	return stepDone, nil
}

// drain cordons the node and evicts its pods, one pass per look; a budget
// that refuses an eviction is waited out until the step's timeout.
func (x *exec) drain(ctx context.Context, a *metalv1alpha1.DrainAction) (stepState, error) {
	_, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	kube, err := x.workload(ctx, acc)
	if err != nil {
		return stepState{}, err
	}
	opts := drain.Options{DeleteEmptyDirData: a.DeleteEmptyDirData}
	if a.GracePeriodSeconds != nil {
		opts.GracePeriodSeconds = ptr.To(int64(*a.GracePeriodSeconds))
	}
	p, cordoned, err := drain.Pass(ctx, kube, a.NodeRef.Name, opts)
	if nerr := x.noteCordon(a.NodeRef.Name, cordoned); nerr != nil {
		return stepState{}, nerr
	}
	if err != nil {
		return stepState{}, err
	}
	if p.Done {
		return stepDone, nil
	}
	st := waitingOn("draining node %s: %s", a.NodeRef.Name, p.Waiting())
	st.hint = ". The node stays cordoned. Relax the disruption budget or move the pods, then retry; " +
		"to give the node back: kubectl uncordon " + a.NodeRef.Name
	return st, nil
}

// restartRequest is what a restarting step asks of the node. It reports
// whether the node restarts because of it, and the mode the node chose.
type restartRequest func(ctx context.Context, c talos.Client) (restarts bool, mode string, err error)

// restart asks the node, once, for something that restarts it, and waits
// until the node booted again and runs ready. ready, when set, is asked last
// on the restarted node and can still keep the step waiting (or fail it).
//
// The boot time is recorded before the request, so a pass that died between
// the request and its record finds out from the node: a boot time that moved
// on means the request took, and it is not sent again.
func (x *exec) restart(ctx context.Context, i int, node *osv1alpha1.TalosNode, acc *talosAccess, what string,
	request restartRequest, ready func(ctx context.Context, c talos.Client, st talos.Status) (stepState, error)) (stepState, error) {
	endpoint := nodeEndpoint(node)
	p := x.progress(i)
	if p == nil || p.BootTime == nil {
		var booted time.Time
		err := acc.with(ctx, endpoint, func(c talos.Client) (err error) {
			booted, err = c.BootTime(ctx)
			return err
		})
		if err != nil {
			if unavailable(err) {
				return waitingOn("node %s does not answer before the %s: %v", node.Name, what, err), nil
			}
			return stepState{}, fmt.Errorf("read the boot time of node %s: %w", node.Name, err)
		}
		p = &metalv1alpha1.TalosStepProgress{Step: int32(i), BootTime: &metav1.Time{Time: booted}}
		if err := x.record(p); err != nil {
			return stepState{}, err
		}
	}
	before := p.BootTime.Time
	if p.RequestedAt == nil {
		var (
			restarts = true
			mode     string
			took     bool
		)
		err := acc.with(ctx, endpoint, func(c talos.Client) error {
			booted, err := c.BootTime(ctx)
			if err != nil {
				return err
			}
			if booted.After(before) {
				took = true // asked by a pass that died before recording it
				return nil
			}
			restarts, mode, err = request(ctx, c)
			return err
		})
		if err != nil {
			if unavailable(err) {
				// Down mid-call is what a restart looks like; whether the
				// request took shows in the boot time on the next look.
				return waitingOn("asked node %s for the %s; it does not answer now (%v)", node.Name, what, err), nil
			}
			return stepState{}, fmt.Errorf("node %s refused the %s: %w", node.Name, what, err)
		}
		p = p.DeepCopy()
		p.RequestedAt = ptrTime(metav1.Now())
		if !took && !restarts {
			p.Mode = cmpOr(mode, "auto")
		}
		if err := x.record(p); err != nil {
			return stepState{}, err
		}
		if !took && !restarts {
			return stepDone, nil
		}
	}
	if p.Mode != "" {
		// The node applied without a restart (recorded above): done.
		return stepDone, nil
	}
	var (
		booted time.Time
		st     talos.Status
		res    stepState
		rerr   error
	)
	err := acc.with(ctx, endpoint, func(c talos.Client) error {
		var err error
		if booted, err = c.BootTime(ctx); err != nil {
			return err
		}
		if st, err = c.Status(ctx); err != nil {
			return err
		}
		if !booted.After(before) || st.Stage != "running" || !st.Ready || ready == nil {
			return nil
		}
		res, rerr = ready(ctx, c, st)
		return nil
	})
	switch {
	case err != nil:
		return waitingOn("node %s is restarting for the %s (%v)", node.Name, what, err), nil
	case !booted.After(before):
		return waitingOn("asked node %s for the %s; it has not restarted yet (booted at %s)",
			node.Name, what, before.UTC().Format(time.RFC3339)), nil
	case st.Stage != "running" || !st.Ready:
		return waitingOn("node %s restarted at %s and is %s, not ready yet%s", node.Name,
			booted.UTC().Format(time.RFC3339), cmpOr(st.Stage, "booting"), unmet(st)), nil
	case ready != nil:
		return res, rerr
	}
	return stepDone, nil
}

func unmet(st talos.Status) string {
	if len(st.UnmetConditions) == 0 {
		return ""
	}
	return " (waiting for " + strings.Join(st.UnmetConditions, ", ") + ")"
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (x *exec) talosReboot(ctx context.Context, i int, a *metalv1alpha1.TalosRebootAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	opts := talos.RebootOptions{Mode: talos.RebootDefault}
	if a.Mode == "PowerCycle" {
		opts.Mode = talos.RebootPowerCycle
	}
	return x.restart(ctx, i, node, acc, "reboot", func(ctx context.Context, c talos.Client) (bool, string, error) {
		return true, "", c.Reboot(ctx, opts)
	}, nil)
}

// talosUpgrade installs the pinned image and waits for the node to run the
// version the approver saw. A node that already runs it is not upgraded.
func (x *exec) talosUpgrade(ctx context.Context, i int, a *metalv1alpha1.TalosUpgradeAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	if x.progress(i) == nil {
		var version string
		err := acc.with(ctx, nodeEndpoint(node), func(c talos.Client) (err error) {
			version, err = c.Version(ctx)
			return err
		})
		if err == nil && version == a.Version {
			return stepDone, nil // goal already met
		}
	}
	opts := talos.UpgradeOptions{Image: a.Image, Stage: a.Stage, Force: a.Force, Preserve: a.Preserve == nil || *a.Preserve}
	if a.RebootMode == "PowerCycle" {
		opts.RebootMode = talos.UpgradeRebootPowerCycle
	}
	return x.restart(ctx, i, node, acc, "upgrade to "+a.Version, func(ctx context.Context, c talos.Client) (bool, string, error) {
		_, err := c.Upgrade(ctx, opts)
		return true, "", err
	}, func(ctx context.Context, c talos.Client, _ talos.Status) (stepState, error) {
		version, err := c.Version(ctx)
		if err != nil {
			return waitingOn("node %s restarted; its version does not read yet (%v)", node.Name, err), nil
		}
		if version != a.Version {
			return stepState{}, fmt.Errorf("node %s restarted and runs %s, not %s: the upgrade did not take "+
				"(Talos boots the previous image when the new one fails)", node.Name, version, a.Version)
		}
		return stepDone, nil
	})
}

// talosApplyConfig sends the rendered config the approver saw: the Secret's
// bytes must hash to the pinned digest, or nothing is sent.
func (x *exec) talosApplyConfig(ctx context.Context, i int, a *metalv1alpha1.TalosApplyConfigAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	var cfg []byte
	if p := x.progress(i); p == nil || p.RequestedAt == nil {
		if cfg, err = x.pinnedConfig(ctx, a); err != nil {
			return stepState{}, err
		}
	}
	opts := talos.ApplyOptions{Mode: applyModes[a.Mode]}
	if a.TryTimeoutSeconds > 0 {
		opts.TryTimeout = time.Duration(a.TryTimeoutSeconds) * time.Second
	}
	return x.restart(ctx, i, node, acc, "config apply", func(ctx context.Context, c talos.Client) (bool, string, error) {
		res, err := c.ApplyConfiguration(ctx, cfg, opts)
		if err != nil {
			return false, "", err
		}
		return res.Mode == talos.ApplyReboot, string(res.Mode), nil
	}, nil)
}

// applyModes maps the action's modes onto the machine API's.
var applyModes = map[string]talos.ApplyMode{
	"Auto": talos.ApplyAuto, "NoReboot": talos.ApplyNoReboot, "Reboot": talos.ApplyReboot,
	"Staged": talos.ApplyStaged, "Try": talos.ApplyTry, "": talos.ApplyNoReboot,
}

// pinnedConfig reads the config Secret and checks it against the digest.
func (x *exec) pinnedConfig(ctx context.Context, a *metalv1alpha1.TalosApplyConfigAction) ([]byte, error) {
	sec := &corev1.Secret{}
	key := client.ObjectKey{Namespace: x.r.CredentialsNamespace, Name: a.ConfigSecretRef.Name}
	if err := x.r.Get(ctx, key, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("config Secret %s not found", key)
		}
		return nil, err
	}
	cfg := sec.Data[asset.ConfigFilename]
	if len(cfg) == 0 {
		return nil, fmt.Errorf("config Secret %s holds no %s", key, asset.ConfigFilename)
	}
	if got := digest.FromBytes(cfg).String(); got != a.Digest {
		return nil, fmt.Errorf("config Secret %s changed since approval (holds %s, approved %s); nothing was sent",
			key, got, a.Digest)
	}
	return cfg, nil
}

// talosReset wipes the node. Its goal shows as the node leaving: it stops
// answering with the cluster's credentials (its config is gone, or it is
// off), or it restarted with the partitions wiped that hold no config.
func (x *exec) talosReset(ctx context.Context, i int, a *metalv1alpha1.TalosResetAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	opts := talos.ResetOptions{Graceful: a.Graceful == nil || *a.Graceful, Reboot: a.Reboot}
	switch a.Mode {
	case "SystemDisk":
		opts.Mode = talos.WipeSystemDisk
	case "UserDisks":
		opts.Mode = talos.WipeUserDisks
	}
	for _, p := range a.SystemPartitions {
		opts.SystemPartitions = append(opts.SystemPartitions, string(p))
	}
	endpoint := nodeEndpoint(node)
	if p := x.progress(i); p != nil && p.RequestedAt != nil {
		var booted time.Time
		err := acc.with(ctx, endpoint, func(c talos.Client) (err error) {
			booted, err = c.BootTime(ctx)
			return err
		})
		switch {
		case unavailable(err):
			// Wiped or off: the cluster's credentials no longer open it. The
			// machine API client connects lazily, so a node that is gone shows
			// as a call that fails Unavailable, not as a dial that fails.
			return stepDone, nil
		case err == nil && p.BootTime != nil && booted.After(p.BootTime.Time):
			return stepDone, nil
		}
		return waitingOn("asked node %s to reset; it still answers with the cluster's credentials", node.Name), nil
	}
	var booted time.Time
	err = acc.with(ctx, endpoint, func(c talos.Client) error {
		var err error
		if booted, err = c.BootTime(ctx); err != nil {
			return err
		}
		return c.Reset(ctx, opts)
	})
	if err != nil && !unavailable(err) {
		return stepState{}, fmt.Errorf("node %s refused the reset: %w", node.Name, err)
	}
	if err != nil && booted.IsZero() {
		// Not even the boot time read: the reset was never sent.
		st := waitingOn("node %s does not answer, so it cannot be reset through Talos (%v)", node.Name, err)
		st.hint = ". A node that cannot answer is wiped by installing it again instead"
		return st, nil
	}
	p := &metalv1alpha1.TalosStepProgress{Step: int32(i), RequestedAt: ptrTime(metav1.Now())}
	if !booted.IsZero() {
		p.BootTime = &metav1.Time{Time: booted}
	}
	if err := x.record(p); err != nil {
		return stepState{}, err
	}
	return waitingOn("asked node %s to reset", node.Name), nil
}

// talosShutdown powers the node off through Talos; its goal shows as the
// node no longer answering.
func (x *exec) talosShutdown(ctx context.Context, i int, a *metalv1alpha1.TalosShutdownAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	endpoint := nodeEndpoint(node)
	asked := x.progress(i) != nil
	err = acc.with(ctx, endpoint, func(c talos.Client) error {
		if asked {
			_, err := c.Version(ctx)
			return err
		}
		return c.Shutdown(ctx, talos.ShutdownOptions{Force: a.Force})
	})
	var unreach *unreachableError
	switch {
	case asked && unavailable(err):
		// Off. Like a reset, this shows as a failing call: the client
		// connects lazily, so its dial succeeds whether the node is there.
		return stepDone, nil
	case asked:
		return waitingOn("asked node %s to shut down; it still answers", node.Name), nil
	case err != nil && errors.As(err, &unreach):
		return waitingOn("node %s does not answer, so it cannot be asked to shut down (%v)", node.Name, err), nil
	case err != nil && !unavailable(err):
		return stepState{}, fmt.Errorf("node %s refused to shut down: %w", node.Name, err)
	}
	if err := x.record(&metalv1alpha1.TalosStepProgress{Step: int32(i), RequestedAt: ptrTime(metav1.Now())}); err != nil {
		return stepState{}, err
	}
	return waitingOn("asked node %s to shut down", node.Name), nil
}

// controlPlanePeers lists the control planes of the node's cluster that the
// executor asks about etcd: every one but the node itself, which is asked
// last when included.
func (x *exec) controlPlanePeers(ctx context.Context, node *osv1alpha1.TalosNode, includeSelf bool) ([]osv1alpha1.TalosNode, []osv1alpha1.TalosNode, error) {
	all, err := clusterMembers(ctx, x.r.Client, node.Spec.ClusterRef.Name)
	if err != nil {
		return nil, nil, err
	}
	via := controlPlanes(all, node.Name)
	if !includeSelf {
		via = slices.DeleteFunc(via, func(n osv1alpha1.TalosNode) bool { return n.Name == node.Name })
	}
	return via, all, nil
}

// etcdLeave makes the node's member leave etcd, asked on the node itself.
// Its goal is the member's absence from the list a peer keeps.
func (x *exec) etcdLeave(ctx context.Context, a *metalv1alpha1.EtcdLeaveAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	if node.Spec.Role != osv1alpha1.RoleControlPlane {
		return stepState{}, fmt.Errorf("node %s is a %s, not a control plane: it runs no etcd member", node.Name, node.Spec.Role)
	}
	peers, _, err := x.controlPlanePeers(ctx, node, false)
	if err != nil {
		return stepState{}, err
	}
	if len(peers) == 0 {
		return stepState{}, fmt.Errorf("node %s is the only control plane of cluster %s: its member leaving would end etcd", node.Name, acc.cluster.Name)
	}
	members, via, err := acc.membersVia(ctx, peers)
	if err != nil {
		return waitingOn("%v", err), nil
	}
	if !slices.ContainsFunc(members, func(m talos.EtcdMember) bool { return m.Hostname == node.Name }) {
		return stepDone, nil // goal already met
	}
	err = acc.with(ctx, nodeEndpoint(node), func(c talos.Client) error {
		// Handing leadership over first spares the cluster an election
		// under load; a follower is left as it is.
		if err := c.EtcdForfeitLeadership(ctx); err != nil {
			return err
		}
		return c.EtcdLeaveCluster(ctx)
	})
	switch {
	case err != nil && unavailable(err):
		st := waitingOn("node %s does not answer, so its member cannot leave by itself (%v)", node.Name, err)
		st.hint = ". For a node that cannot answer, remove its member through a peer instead (etcdRemoveMember)"
		return st, nil
	case err != nil:
		return stepState{}, fmt.Errorf("node %s could not leave etcd: %w", node.Name, err)
	}
	return waitingOn("node %s left etcd; waiting for %s to stop listing it", node.Name, via), nil
}

// etcdForfeitLeadership hands etcd leadership to another member when the
// node's member leads, asked on the node itself; Talos leaves a follower as
// it is, so the request is its own goal check. A node whose member is gone
// already (a resumed removal) has nothing to hand on.
func (x *exec) etcdForfeitLeadership(ctx context.Context, a *metalv1alpha1.EtcdForfeitLeadershipAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	if node.Spec.Role != osv1alpha1.RoleControlPlane {
		return stepState{}, fmt.Errorf("node %s is a %s, not a control plane: it runs no etcd member", node.Name, node.Spec.Role)
	}
	peers, _, err := x.controlPlanePeers(ctx, node, false)
	if err != nil {
		return stepState{}, err
	}
	if len(peers) == 0 {
		// No member to hand leadership to: the step changes nothing, and
		// refusing it would only stop an operation the guard allowed.
		return stepDone, nil
	}
	members, _, err := acc.membersVia(ctx, peers)
	if err != nil {
		return waitingOn("%v", err), nil
	}
	if !slices.ContainsFunc(members, func(m talos.EtcdMember) bool { return m.Hostname == node.Name }) {
		return stepDone, nil // no member, no leadership
	}
	err = acc.with(ctx, nodeEndpoint(node), func(c talos.Client) error { return c.EtcdForfeitLeadership(ctx) })
	switch {
	case err != nil && unavailable(err):
		st := waitingOn("node %s does not answer, so its member cannot hand leadership on (%v)", node.Name, err)
		st.hint = ". A node that cannot answer leads no healthy etcd; its member is removed through a peer instead (etcdRemoveMember)"
		return st, nil
	case err != nil:
		return stepState{}, fmt.Errorf("node %s could not hand etcd leadership on: %w", node.Name, err)
	}
	return stepDone, nil
}

// deleteKubernetesNode deletes the node's Node object through its cluster's
// Kubernetes API. Already gone is the goal met; an API that does not answer
// is a wait, as for the drain.
func (x *exec) deleteKubernetesNode(ctx context.Context, a *metalv1alpha1.DeleteKubernetesNodeAction) (stepState, error) {
	_, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	kube, err := x.workload(ctx, acc)
	if err != nil {
		return stepState{}, err
	}
	err = kube.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: a.NodeRef.Name}})
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return stepDone, nil
	case unavailable(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err), apierrors.IsServiceUnavailable(err):
		return waitingOn("the Kubernetes API of cluster %s does not answer to delete Node %s (%v)", acc.cluster.Name, a.NodeRef.Name, err), nil
	}
	return stepState{}, fmt.Errorf("delete Kubernetes Node %s from cluster %s: %w", a.NodeRef.Name, acc.cluster.Name, err)
}

// membersVia lists the etcd members through the first node in via that
// answers.
func (a *talosAccess) membersVia(ctx context.Context, via []osv1alpha1.TalosNode) ([]talos.EtcdMember, string, error) {
	var errs []string
	for i := range via {
		var members []talos.EtcdMember
		err := a.with(ctx, nodeEndpoint(&via[i]), func(c talos.Client) (err error) {
			members, err = c.EtcdMembers(ctx)
			return err
		})
		if err == nil {
			return members, via[i].Name, nil
		}
		errs = append(errs, via[i].Name+": "+err.Error())
	}
	return nil, "", fmt.Errorf("no control plane of cluster %s lists the etcd members (%s)", a.cluster.Name, strings.Join(errs, "; "))
}

// etcdRemoveMember removes exactly the member the approver saw, by ID,
// through a peer. A member already gone is success.
func (x *exec) etcdRemoveMember(ctx context.Context, a *metalv1alpha1.EtcdRemoveMemberAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	id, err := strconv.ParseUint(a.MemberID, 16, 64)
	if err != nil {
		return stepState{}, fmt.Errorf("member ID %q is not hex: %w", a.MemberID, err)
	}
	peers, _, err := x.controlPlanePeers(ctx, node, false)
	if err != nil {
		return stepState{}, err
	}
	var unanswered []string
	for i := range peers {
		var (
			gone   bool
			listed bool
		)
		err := acc.with(ctx, nodeEndpoint(&peers[i]), func(c talos.Client) error {
			members, err := c.EtcdMembers(ctx)
			if err != nil {
				return err
			}
			listed = true
			if !slices.ContainsFunc(members, func(m talos.EtcdMember) bool { return m.ID == id }) {
				gone = true
				return nil
			}
			return c.EtcdRemoveMemberByID(ctx, id)
		})
		switch {
		case err == nil && gone:
			return stepDone, nil
		case err == nil:
			return waitingOn("removed member %x through %s; waiting for it to drop from the list", id, peers[i].Name), nil
		case listed && !unavailable(err):
			// A peer whose etcd runs refused: that is a verdict.
			return stepState{}, fmt.Errorf("%s refused to remove member %x: %w", peers[i].Name, id, err)
		}
		// This peer could not be asked (down, or its etcd is not running):
		// the next one may.
		unanswered = append(unanswered, peers[i].Name+": "+err.Error())
	}
	if len(peers) == 0 {
		return stepState{}, fmt.Errorf("cluster %s has no control plane other than %s to remove member %x through", acc.cluster.Name, node.Name, id)
	}
	return waitingOn("no other control plane of cluster %s answers to remove member %x (%s)",
		acc.cluster.Name, id, strings.Join(unanswered, "; ")), nil
}

// waitNodeReady waits for the node to run and be ready: Talos stage running
// with every condition met (and the version, when named), and its
// Kubernetes Node Ready.
func (x *exec) waitNodeReady(ctx context.Context, a *metalv1alpha1.WaitNodeReadyAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	var (
		st      talos.Status
		version string
	)
	err = acc.with(ctx, nodeEndpoint(node), func(c talos.Client) (err error) {
		if st, err = c.Status(ctx); err != nil {
			return err
		}
		version, err = c.Version(ctx)
		return err
	})
	switch {
	case err != nil:
		return waitingOn("node %s: %v", node.Name, err), nil
	case st.Stage != "running" || !st.Ready:
		return waitingOn("node %s is %s, not ready yet%s", node.Name, cmpOr(st.Stage, "booting"), unmet(st)), nil
	case a.Version != "" && version != a.Version:
		return waitingOn("node %s runs Talos %s, waiting for %s", node.Name, version, a.Version), nil
	}
	kube, err := x.workload(ctx, acc)
	if err != nil {
		return stepState{}, err
	}
	kn := &corev1.Node{}
	if err := kube.Get(ctx, client.ObjectKey{Name: node.Name}, kn); err != nil {
		if apierrors.IsNotFound(err) {
			return waitingOn("node %s runs, but Kubernetes lists no Node %s yet", node.Name, node.Name), nil
		}
		return stepState{}, err
	}
	for _, c := range kn.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				return stepDone, nil
			}
			return waitingOn("Kubernetes Node %s is not Ready: %s", node.Name, cmpOr(c.Message, c.Reason)), nil
		}
	}
	return waitingOn("Kubernetes Node %s reports no Ready condition yet", node.Name), nil
}

// waitEtcdHealthy waits until every member of the cluster's etcd answers,
// votes and reports no error, no alarm is raised, and the node's own member
// is among them.
func (x *exec) waitEtcdHealthy(ctx context.Context, a *metalv1alpha1.WaitEtcdHealthyAction) (stepState, error) {
	node, acc, err := x.talosNode(ctx, a.NodeRef)
	if err != nil {
		return stepState{}, err
	}
	via, all, err := x.controlPlanePeers(ctx, node, true)
	if err != nil {
		return stepState{}, err
	}
	rep, err := acc.etcdHealth(ctx, via, all)
	if err != nil {
		return waitingOn("%v", err), nil
	}
	if node.Spec.Role == osv1alpha1.RoleControlPlane && rep.member(node.Name) == nil {
		return waitingOn("etcd of cluster %s does not list node %s as a member yet", acc.cluster.Name, node.Name), nil
	}
	if probs := rep.problems(0); len(probs) > 0 {
		return waitingOn("etcd of cluster %s is not healthy: %s", acc.cluster.Name, strings.Join(probs, "; ")), nil
	}
	return stepDone, nil
}

// acted: step i already did something to the machine that only finishing
// the step completes: the off half of a power cycle seen, a graceful
// shutdown asked for, a Talos request on its way. Such a step is finished
// before anything stops the operation (an abort, the cluster guard).
func (x *exec) acted(i int) bool {
	a := x.op.Spec.Actions[i]
	switch {
	case a.PowerCycle != nil:
		return x.op.Status.OffObserved != nil
	case a.PowerOff != nil:
		return x.op.Status.ShutdownRequested != nil
	}
	return x.progress(i) != nil
}
