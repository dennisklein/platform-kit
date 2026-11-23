package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

func init() {
	verbHelp["approve"] = `
The operator never acts on a machine before a person approves the plan.
approve prints the plan first: the machine, its BMC, each step in order, the
media it mounts, the boot target, and for an OS install the node, its address
and the disk that will be overwritten. Then it asks "Approve? [y/N]".

The approval is recorded as your cluster user. You cannot approve an operation
that you proposed yourself, and an approval cannot be changed afterwards.

Operations live in the operator's namespace, pk-metal. You can give the name
as it appears in pk-metal status (pk-metal/<name>) or use -n.

Without a terminal (in a script or a pipe), approve does not ask: it refuses
unless you give --yes.

examples:
  pk-metal approve                                  list what waits for approval
  pk-metal approve pk-metal/provision-node1-1a2b3c4d
  pk-metal approve --yes provision-node1-1a2b3c4d   approve in a script`
}

// approveCmd stamps spec.approval.approvedBy with the caller's own cluster
// username — the only value admission accepts (approve as yourself,
// write-once). Who YOU are is asked of the apiserver (SelfSubjectReview), not
// guessed from the environment.
func approveCmd(args []string) {
	fs := newFlags("approve")
	ns := fs.String("n", operatorNamespace, "namespace of the operation")
	yes := fs.Bool("yes", false, "approve without asking (for scripts); the plan is still printed")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() > 1 {
		badUsage(fs, "give one operation, or none to list the pending ones")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx := context.Background()

	if fs.NArg() == 0 {
		if err := listPending(ctx, cl, os.Stdout, time.Now()); err != nil {
			die(err)
		}
		return
	}
	namespace, name, err := splitRef(fs.Arg(0), *ns, flagGiven(fs, "n"))
	if err != nil {
		badUsage(fs, err.Error())
	}
	user, err := whoAmI(ctx, cl)
	if err != nil {
		die(err)
	}
	if err := approve(ctx, cl, approval{
		Namespace: namespace, Name: name, User: user, Yes: *yes,
		In: os.Stdin, Interactive: isTerminal(os.Stdin), Out: os.Stdout,
	}); err != nil {
		die(err)
	}
}

// flagGiven reports whether the flag was set on the command line.
func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// isTerminal: stdin is a character device, so a person can answer a prompt.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func whoAmI(ctx context.Context, cl client.Client) (string, error) {
	ssr := &authenticationv1.SelfSubjectReview{}
	if err := cl.Create(ctx, ssr); err != nil {
		return "", fmt.Errorf("cannot ask the cluster who you are: %w", err)
	}
	if ssr.Status.UserInfo.Username == "" {
		return "", fmt.Errorf("the cluster did not say who you are; check your kubeconfig user")
	}
	return ssr.Status.UserInfo.Username, nil
}

// approval is one approve call: the operation, who approves, and how to ask.
type approval struct {
	Namespace, Name string
	User            string
	Yes             bool
	In              io.Reader
	Interactive     bool
	Out             io.Writer
}

// errDeclined: the person answered no. Nothing changed.
var errDeclined = errors.New("not approved; nothing changed")

func approve(ctx context.Context, cl client.Client, a approval) error {
	op := &metalv1alpha1.HostOperation{}
	if err := getNamed(ctx, cl, "operation", op, &metalv1alpha1.HostOperationList{}, a.Namespace, a.Name); err != nil {
		return err
	}
	ref := op.Namespace + "/" + op.Name
	switch {
	case op.Spec.Approval.ApprovedBy != "":
		return fmt.Errorf("operation %s is already approved by %s (an approval cannot be changed)",
			ref, op.Spec.Approval.ApprovedBy)
	case finished(op):
		return fmt.Errorf("operation %s has finished (%s); there is nothing to approve", ref, opPhase(op))
	case op.Spec.Approval.Required != nil && !*op.Spec.Approval.Required:
		return fmt.Errorf("operation %s does not need approval; the operator runs it on its own", ref)
	case op.Spec.ProposedBy == a.User:
		// Admission refuses this too; saying it here names the rule instead
		// of quoting a policy.
		return fmt.Errorf("operation %s was proposed by you (%s); another person must approve it", ref, a.User)
	}

	pc, err := loadPlanContext(ctx, cl, op)
	if err != nil {
		return err
	}
	fmt.Fprint(a.Out, renderPlan(op, pc))

	if !a.Yes {
		if !a.Interactive {
			return fmt.Errorf("stdin is not a terminal, so pk-metal cannot ask; " +
				"read the plan above and rerun with --yes to approve it")
		}
		fmt.Fprintf(a.Out, "\nApprove as %s? [y/N] ", a.User)
		line, _ := bufio.NewReader(a.In).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errDeclined
		}
	}

	// Marshalled, not formatted: Go's %q escapes are not JSON's, and a user
	// name comes from whatever the identity provider allows.
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"approval": map[string]any{"approvedBy": a.User}},
	})
	if err != nil {
		return err
	}
	if err := cl.Patch(ctx, op, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("approve %s: %w", ref, err)
	}
	fmt.Fprintf(a.Out, "approved %s as %s; the operator starts it now. Follow it with: pk-metal status\n", ref, a.User)
	return nil
}

// opPhase derives the operation's phase from its conditions. The stored
// phase is a summary for people to read, written after the conditions; a
// decision reads the conditions themselves, so a phase that lags behind them
// cannot make approve or retry act on the wrong operation.
func opPhase(op *metalv1alpha1.HostOperation) metalv1alpha1.OperationPhase {
	p, _ := phase.ForOperation(op.Status.Conditions)
	return p
}

// finished: the executor recorded an outcome, success or failure.
func finished(op *metalv1alpha1.HostOperation) bool {
	p := opPhase(op)
	return p == metalv1alpha1.OperationSucceeded || p == metalv1alpha1.OperationFailed
}

// waitsForApproval: the operation needs a person and nobody has approved it.
// It does not wait for the status: a just-proposed operation has no conditions yet.
func waitsForApproval(op *metalv1alpha1.HostOperation) bool {
	if finished(op) {
		return false
	}
	required := op.Spec.Approval.Required == nil || *op.Spec.Approval.Required
	return required && op.Spec.Approval.ApprovedBy == ""
}

// listPending prints one line for each operation that waits for approval.
func listPending(ctx context.Context, cl client.Client, w io.Writer, now time.Time) error {
	ops := &metalv1alpha1.HostOperationList{}
	if err := cl.List(ctx, ops); err != nil {
		return fmt.Errorf("list operations: %w", err)
	}
	pending := slices.DeleteFunc(ops.Items, func(op metalv1alpha1.HostOperation) bool { return !waitsForApproval(&op) })
	if len(pending) == 0 {
		fmt.Fprintln(w, "no operation waits for approval")
		return nil
	}
	slices.SortFunc(pending, func(a, b metalv1alpha1.HostOperation) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "OPERATION\tHOST\tWHY\tSTEPS\tPROPOSED BY\tAGE")
	for _, op := range pending {
		age := "-"
		if !op.CreationTimestamp.IsZero() {
			age = shortDuration(now.Sub(op.CreationTimestamp.Time))
		}
		fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%d\t%s\t%s\n", op.Namespace, op.Name, op.Spec.HostRef.Name,
			orDash(op.Spec.Reason), len(op.Spec.Actions), orDash(op.Spec.ProposedBy), age)
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "\nSee the plan of one and approve it: pk-metal approve <namespace>/<operation>")
	return nil
}

// planContext is what the plan names besides the operation itself. Any part
// may be missing (deleted, not yet created, no permission to read it); the
// plan then says so instead of failing.
type planContext struct {
	Host       *metalv1alpha1.Host
	Endpoints  []metalv1alpha1.ManagementEndpoint // the host's BMC paths, in path order
	BootAssets map[string]*osv1alpha1.BootAsset
	Nodes      map[string]*osv1alpha1.TalosNode
}

func loadPlanContext(ctx context.Context, cl client.Client, op *metalv1alpha1.HostOperation) (planContext, error) {
	pc := planContext{BootAssets: map[string]*osv1alpha1.BootAsset{}, Nodes: map[string]*osv1alpha1.TalosNode{}}
	// optional reads a cluster-scoped object; not found leaves it out.
	optional := func(name string, obj client.Object) (bool, error) {
		err := cl.Get(ctx, client.ObjectKey{Name: name}, obj)
		switch {
		case err == nil:
			return true, nil
		case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
			return false, nil
		}
		return false, fmt.Errorf("read %s for the plan: %w", name, err)
	}
	h := &metalv1alpha1.Host{}
	if ok, err := optional(op.Spec.HostRef.Name, h); err != nil {
		return pc, err
	} else if ok {
		pc.Host = h
		for _, p := range h.Status.ManagedBy {
			ep := &metalv1alpha1.ManagementEndpoint{}
			if ok, err := optional(p.EndpointRef.Name, ep); err != nil {
				return pc, err
			} else if ok {
				pc.Endpoints = append(pc.Endpoints, *ep)
			}
		}
	}
	for _, a := range op.Spec.Actions {
		if a.InsertMedia != nil && a.InsertMedia.BootAssetRef != nil {
			name := a.InsertMedia.BootAssetRef.Name
			ba := &osv1alpha1.BootAsset{}
			if ok, err := optional(name, ba); err != nil {
				return pc, err
			} else if ok {
				pc.BootAssets[name] = ba
			}
		}
		if a.AwaitCheckIn != nil {
			name := a.AwaitCheckIn.NodeRef.Name
			n := &osv1alpha1.TalosNode{}
			if ok, err := optional(name, n); err != nil {
				return pc, err
			} else if ok {
				pc.Nodes[name] = n
			}
		}
	}
	return pc, nil
}

// renderPlan says in plain words what the operation will do, in order.
func renderPlan(op *metalv1alpha1.HostOperation, pc planContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "operation %s/%s\n", op.Namespace, op.Name)
	if op.Spec.Reason != "" {
		fmt.Fprintf(&b, "  why:         %s\n", op.Spec.Reason)
	}
	fmt.Fprintf(&b, "  proposed by: %s\n", orDash(op.Spec.ProposedBy))

	fmt.Fprintf(&b, "host %s", op.Spec.HostRef.Name)
	if pc.Host == nil {
		b.WriteString(" (not found in the cluster)\n")
	} else {
		var facts []string
		if assets := assetsOf(pc.Endpoints); len(assets) > 0 {
			facts = append(facts, "asset "+strings.Join(assets, ", "))
		}
		if id := pc.Host.Status.Identity; id != nil && id.Serial != "" {
			facts = append(facts, "serial "+id.Serial)
		}
		if len(facts) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(facts, ", "))
		}
		b.WriteString("\n")
		if len(pc.Endpoints) == 0 {
			b.WriteString("  BMC: none known\n")
		}
		for _, ep := range pc.Endpoints {
			fmt.Fprintf(&b, "  BMC: %s (endpoint %s)\n", ep.Spec.Address, ep.Name)
		}
	}

	b.WriteString("steps, in this order:\n")
	for i, a := range op.Spec.Actions {
		fmt.Fprintf(&b, "  %2d. %s\n", i+1, describeAction(a, pc))
	}

	for _, a := range op.Spec.Actions {
		if a.AwaitCheckIn == nil {
			continue
		}
		name := a.AwaitCheckIn.NodeRef.Name
		n := pc.Nodes[name]
		if n == nil {
			fmt.Fprintf(&b, "boots TalosNode %s (not found in the cluster: the disk it installs onto is unknown)\n", name)
			continue
		}
		fmt.Fprintf(&b, "boots TalosNode %s: address %s, role %s\n", name, orDash(nodeAddress(n)), n.Spec.Role)
		fmt.Fprintf(&b, "  installs Talos onto the disk that matches %s; everything on that disk is overwritten\n",
			diskSelector(n.Spec.InstallDiskSelector))
	}
	return b.String()
}

func describeAction(a metalv1alpha1.HostOperationAction, pc planContext) string {
	switch {
	case a.EjectMedia != nil:
		return fmt.Sprintf("eject whatever is mounted on virtual media slot %s", a.EjectMedia.MediaID)
	case a.InsertMedia != nil:
		im := a.InsertMedia
		what := "the image " + im.ImageURL
		if im.BootAssetRef != nil {
			what = describeBootAsset(im.BootAssetRef.Name, pc.BootAssets[im.BootAssetRef.Name])
		}
		s := fmt.Sprintf("mount %s on virtual media slot %s", what, im.MediaID)
		if im.Digest != "" {
			s += " (content " + shortDigest(im.Digest) + ")"
		}
		return s
	case a.SetBootOverride != nil:
		o := a.SetBootOverride
		s := fmt.Sprintf("boot from %s at the next start only", o.Target)
		if o.Once != nil && !*o.Once {
			s = fmt.Sprintf("boot from %s at every start, until changed", o.Target)
		}
		if o.Mode != "" {
			s += ", in " + o.Mode + " mode"
		}
		return s
	case a.PowerCycle != nil:
		return "power cycle: if the machine is on, force it off (no clean shutdown) and on again; if it is off, power it on"
	case a.PowerOff != nil:
		return "power the machine off (forced; the OS is not asked to shut down)"
	case a.ApplyBios != nil:
		var kv []string
		for _, k := range slices.Sorted(maps.Keys(a.ApplyBios.Attributes)) {
			kv = append(kv, k+"="+string(a.ApplyBios.Attributes[k].Raw))
		}
		return fmt.Sprintf("change %d BIOS setting(s): %s (takes effect at the next start)", len(kv), strings.Join(kv, ", "))
	case a.FlashFirmware != nil:
		return fmt.Sprintf("flash firmware version %s from %s", a.FlashFirmware.Version, a.FlashFirmware.Image)
	case a.AwaitCheckIn != nil:
		node := a.AwaitCheckIn.NodeRef.Name
		timeout := 30 * time.Minute
		if a.AwaitCheckIn.TimeoutSeconds > 0 {
			timeout = time.Duration(a.AwaitCheckIn.TimeoutSeconds) * time.Second
		}
		where := ""
		if n := pc.Nodes[node]; n != nil && nodeAddress(n) != "" {
			where = " at " + nodeAddress(n)
		}
		return fmt.Sprintf("wait up to %s for TalosNode %s to report in%s", shortDuration(timeout), node, where)
	}
	return "unknown step (this pk-metal is older than the operator; update it)"
}

func describeBootAsset(name string, ba *osv1alpha1.BootAsset) string {
	if ba == nil {
		return fmt.Sprintf("boot asset %s (not found in the cluster)", name)
	}
	switch ba.Spec.Strategy {
	case osv1alpha1.StrategyFactory:
		return fmt.Sprintf("the Talos %s installer (boot asset %s)", orDash(ba.Spec.TalosVersion), name)
	case osv1alpha1.StrategyConfigISO:
		node := "-"
		if ba.Spec.NodeRef != nil {
			node = ba.Spec.NodeRef.Name
		}
		return fmt.Sprintf("the machine config of TalosNode %s (boot asset %s)", node, name)
	}
	return fmt.Sprintf("boot asset %s", name)
}

func nodeAddress(n *osv1alpha1.TalosNode) string {
	if n.Spec.Address != "" {
		return n.Spec.Address
	}
	return n.Status.Address
}

// diskSelector renders the install-disk selector as the admin would match it.
func diskSelector(d osv1alpha1.DiskSelector) string {
	var parts []string
	for _, f := range []struct{ k, v string }{
		{"serial", d.Serial}, {"model", d.Model}, {"wwid", d.WWID}, {"busPath", d.BusPath}, {"match", d.Match},
	} {
		if f.v != "" {
			parts = append(parts, f.k+"="+f.v)
		}
	}
	if len(parts) == 0 {
		return "no selector (Talos picks a disk)"
	}
	return strings.Join(parts, " and ")
}

func shortDigest(d string) string {
	if algo, hex, ok := strings.Cut(d, ":"); ok && len(hex) > 12 {
		return algo + ":" + hex[:12] + "…"
	}
	return d
}

// assetsOf lists the distinct assets the endpoints serve, in order.
func assetsOf(eps []metalv1alpha1.ManagementEndpoint) []string {
	var out []string
	for _, ep := range eps {
		if a := ep.Spec.AssetRef.Name; a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// shortDuration prints 45s, 30m, 1h30m, 2h, 3d: no zero units.
func shortDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		d = d.Round(time.Minute)
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}
