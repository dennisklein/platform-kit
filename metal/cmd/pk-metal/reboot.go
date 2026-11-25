package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func init() {
	verbHelp["reboot"] = `
reboot proposes a clean reboot of one Talos node: it cordons the node, drains
it (pods are evicted so that every PodDisruptionBudget holds; DaemonSet and
static pods stay), reboots it through Talos, waits until it runs ready again
in Talos and in Kubernetes, and uncordons it. For a control plane it also
waits until etcd is healthy again before it uncordons.

Nothing happens until a person approves the operation (you, if the site
allows self-approval; otherwise another person):
  pk-metal approve pk-metal/<operation>

A control plane is rebooted only while every other etcd member is healthy and
no other control plane of its cluster is being taken down; until then the
approved operation waits (pk-metal status shows ClusterDisruption).

A drain that a disruption budget holds up waits up to --drain-timeout, then
fails and leaves the node cordoned. To stop a reboot: pk-metal abort --reason.

examples:
  pk-metal reboot w-1
  pk-metal reboot --drain-timeout 30m --grace-period 60 cp-2
  pk-metal reboot --mode powercycle --delete-emptydir-data w-1`
}

func rebootCmd(args []string) {
	fs := newFlags("reboot")
	mode := fs.String("mode", "default", "how the reboot ends: default (kexec or warm reboot) or powercycle (cold start, for firmware)")
	drainTimeout := fs.Duration("drain-timeout", 15*time.Minute, "how long the drain may wait for pods to leave (30s..24h)")
	grace := fs.Int("grace-period", -1, "seconds each evicted pod gets to stop; -1 keeps each pod's own")
	emptyDir := fs.Bool("delete-emptydir-data", false, "also evict pods with emptyDir volumes, whose data is lost")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one node (the TalosNode name, which is the Kubernetes node name)")
	}
	opts := rebootOptions{Node: fs.Arg(0), DrainTimeout: *drainTimeout, DeleteEmptyDirData: *emptyDir}
	switch *mode {
	case "default":
	case "powercycle":
		opts.PowerCycle = true
	default:
		badUsage(fs, "--mode is default or powercycle")
	}
	if *grace >= 0 {
		opts.GracePeriod = ptr.To(int32(*grace)) //nolint:gosec // bounded by the schema, checked below
		if *grace > 3600 {
			badUsage(fs, "--grace-period is at most 3600 seconds")
		}
	}
	if *drainTimeout < 30*time.Second || *drainTimeout > 24*time.Hour {
		badUsage(fs, "--drain-timeout is between 30s and 24h")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx, stop := signalContext()
	defer stop()
	if opts.User, err = whoAmI(ctx, cl); err != nil {
		die(err)
	}
	if _, err := proposeReboot(ctx, cl, opts, os.Stdout); err != nil {
		die(err)
	}
}

// rebootOptions is one reboot proposal.
type rebootOptions struct {
	Node               string
	User               string
	PowerCycle         bool
	DrainTimeout       time.Duration
	GracePeriod        *int32
	DeleteEmptyDirData bool
}

// rebootPlan is the reboot's actions, in order, for a node of role.
func rebootPlan(node string, role osv1alpha1.NodeRole, o rebootOptions) []metalv1alpha1.HostOperationAction {
	ref := metalv1alpha1.Reference{Name: node}
	mode := "Default"
	if o.PowerCycle {
		mode = "PowerCycle"
	}
	plan := []metalv1alpha1.HostOperationAction{
		{Cordon: &metalv1alpha1.CordonAction{NodeRef: ref}},
		{Drain: &metalv1alpha1.DrainAction{NodeRef: ref, TimeoutSeconds: int32(o.DrainTimeout / time.Second), //nolint:gosec // at most 24h
			GracePeriodSeconds: o.GracePeriod, DeleteEmptyDirData: o.DeleteEmptyDirData}},
		{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: ref, Mode: mode}},
		{WaitNodeReady: &metalv1alpha1.WaitNodeReadyAction{NodeRef: ref}},
	}
	if role == osv1alpha1.RoleControlPlane {
		// The node is ready once its kubelet is; its etcd member may still
		// be catching up. The next control plane must not go down before.
		plan = append(plan, metalv1alpha1.HostOperationAction{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{NodeRef: ref}})
	}
	return append(plan, metalv1alpha1.HostOperationAction{Uncordon: &metalv1alpha1.UncordonAction{NodeRef: ref}})
}

// proposeReboot creates the reboot operation for the host that runs the
// node, waiting for a person's approval.
func proposeReboot(ctx context.Context, cl client.Client, o rebootOptions, w io.Writer) (*metalv1alpha1.HostOperation, error) {
	node := &osv1alpha1.TalosNode{}
	if err := cl.Get(ctx, client.ObjectKey{Name: o.Node}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no TalosNode %s (kubectl get talosnodes lists them)", o.Node)
		}
		return nil, err
	}
	hosts := &metalv1alpha1.HostList{}
	if err := cl.List(ctx, hosts); err != nil {
		return nil, err
	}
	var host *metalv1alpha1.Host
	for i := range hosts.Items {
		if os := hosts.Items[i].Spec.OS; os != nil && os.NodeRef.Name == o.Node {
			if host != nil {
				return nil, fmt.Errorf("hosts %s and %s both run TalosNode %s; fix that first", host.Name, hosts.Items[i].Name, o.Node)
			}
			host = &hosts.Items[i]
		}
	}
	if host == nil {
		return nil, fmt.Errorf("no host runs TalosNode %s (a Host names it in spec.os.nodeRef once a claim placed it)", o.Node)
	}
	op := &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, GenerateName: "reboot-" + o.Node + "-"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: host.Name},
			Reason:     "Reboot",
			ProposedBy: o.User,
			Actions:    rebootPlan(o.Node, node.Spec.Role, o),
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(true)},
		},
	}
	if err := cl.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("propose the reboot of %s: %w", o.Node, err)
	}
	ref := op.Namespace + "/" + op.Name
	fmt.Fprintf(w, "proposed %s: a clean reboot of TalosNode %s on host %s (%d steps).\n", ref, o.Node, host.Name, len(op.Spec.Actions))
	fmt.Fprintf(w, "Approve it after reading the plan (another person must, if the site forbids self-approval): pk-metal approve %s\n", ref)
	if node.Spec.Role == osv1alpha1.RoleControlPlane {
		fmt.Fprintf(w, "It is a control plane of cluster %s: once approved, it waits until every other etcd member is healthy "+
			"and no other control plane is down.\n", node.Spec.ClusterRef.Name)
	}
	fmt.Fprintf(w, "To withdraw it: pk-metal abort --reason \"<reason>\" %s\n", ref)
	return op, nil
}
