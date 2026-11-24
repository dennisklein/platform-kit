package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/machineapi"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

func init() {
	verbHelp["restore"] = `
restore looks at a cluster's etcd on every control plane and says what is
wrong and what would fix it. It changes nothing on a machine itself: with
--propose it creates the operations of the plan, and each waits for another
person's approval like any other.

subcommands:
  plan <cluster> [--snapshot name] [--node name] [--propose]
      One member lost (its etcd does not run or reports errors while the
      others keep their majority): remove that member, wipe the node's etcd
      data (EPHEMERAL) and let it join again.
      Majority or everything lost (no control plane lists etcd members):
      wipe the etcd data of the members that still run a stuck etcd, then
      restore one control plane from a snapshot (etcdRecover); the others
      join it. The newest verified snapshot is used unless --snapshot names
      one.
  upload <operation> --identity <file> [--from-file <file.age>]
      For an approved etcdRecover operation: decrypt the snapshot on this
      machine with your age identity, check it against the snapshot record,
      and stream it to the node over the Talos API with your talosconfig.
      The operator then starts etcd from it.

The talosconfig is read from the cluster's published Secret
(<cluster>-talosconfig in pk-metal) unless --talosconfig names a file.

examples:
  pk-metal restore plan lab
  pk-metal restore plan lab --snapshot lab-daily-20261124-060000 --propose
  pk-metal approve pk-metal/lab-etcd-recover-node-1          (another person)
  pk-metal restore upload pk-metal/lab-etcd-recover-node-1 --identity ~/.config/age/keys.txt

docs/metal/disaster-recovery.md walks through each case.`
}

// restoreReason is the spec.reason of the operations restore proposes.
const restoreReason = "EtcdRestore"

// labelRestoreCluster marks the operations restore proposes for a cluster.
const labelRestoreCluster = "os.pk.io/etcd-restore"

func restoreCmd(args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs := newFlags("restore")
		parseFlags(fs, args)
		badUsage(fs, "name a subcommand: plan or upload")
	}
	sub, rest := args[0], args[1:]
	fs := newFlags("restore " + sub)
	addGlobalFlags(fs)
	talosconfigFile := fs.String("talosconfig", "", "talosconfig file (default: the cluster's published Secret)")
	ctx := context.Background()
	switch sub {
	case "plan":
		snapshot := fs.String("snapshot", "", "EtcdSnapshot to restore (default: the cluster's newest verified one)")
		node := fs.String("node", "", "control plane to restore on (default: the first by name that answers)")
		propose := fs.Bool("propose", false, "create the plan's operations (each waits for approval)")
		pos := parseAnywhere(fs, rest)
		if len(pos) != 1 {
			badUsage(fs, "give exactly one TalosCluster")
		}
		cl := mustClient()
		tc, err := talosconfigFor(ctx, cl, argAt(pos, 0), *talosconfigFile)
		if err != nil {
			die(err)
		}
		user := ""
		if *propose {
			if user, err = whoAmI(ctx, cl); err != nil {
				die(err)
			}
		}
		p, err := planRestore(ctx, cl, machineapi.Dial, tc, restoreInput{
			Cluster: argAt(pos, 0), Snapshot: *snapshot, Node: *node, User: user, Now: time.Now()})
		if err != nil {
			die(err)
		}
		fmt.Print(p.render())
		if *propose {
			if err := p.propose(ctx, cl, os.Stdout); err != nil {
				die(err)
			}
		} else if len(p.Ops) > 0 {
			fmt.Printf("\nnothing was created; to propose these operations: pk-metal restore plan %s --propose%s\n",
				argAt(pos, 0), p.flags())
		}
	case "upload":
		ns := fs.String("n", operatorNamespace, "namespace of the operation")
		identity := fs.String("identity", "", "age identity file (required)")
		fromFile := fs.String("from-file", "", "the encrypted snapshot as a file (needed for a pvc sink)")
		pos := parseAnywhere(fs, rest)
		if len(pos) != 1 || *identity == "" {
			badUsage(fs, "give one operation and --identity")
		}
		namespace, name, err := splitRef(argAt(pos, 0), *ns, flagGiven(fs, "n"))
		if err != nil {
			badUsage(fs, err.Error())
		}
		cl := mustClient()
		if err := uploadSnapshot(ctx, cl, machineapi.Dial, uploadInput{
			Namespace: namespace, Name: name, Identity: *identity, FromFile: *fromFile,
			TalosconfigFile: *talosconfigFile,
		}, os.Stdout); err != nil {
			die(err)
		}
	default:
		badUsage(fs, fmt.Sprintf("unknown subcommand %q: plan or upload", sub))
	}
}

// talosconfigFor reads a file, or the talosconfig the operator published
// for the cluster.
func talosconfigFor(ctx context.Context, cl client.Client, cluster, file string) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	c := &osv1alpha1.TalosCluster{}
	if err := cl.Get(ctx, client.ObjectKey{Name: cluster}, c); err != nil {
		return nil, fmt.Errorf("TalosCluster %s: %w", cluster, err)
	}
	ref := c.Status.TalosconfigSecretRef
	if ref == nil {
		return nil, fmt.Errorf("TalosCluster %s has published no talosconfig; give --talosconfig", cluster)
	}
	s := &corev1.Secret{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
		return nil, fmt.Errorf("read the talosconfig Secret %s/%s (or give --talosconfig): %w", ref.Namespace, ref.Name, err)
	}
	return s.Data["talosconfig"], nil
}

// memberView is what one control plane said about etcd.
type memberView struct {
	Node     osv1alpha1.TalosNode
	Endpoint string
	Host     string // the Host that runs it, "" when none
	// State: "listening" (lists members), "stuck" (etcd runs without a
	// member list: no quorum), "stopped" (answers, no etcd), "silent".
	State   string
	Detail  string
	Members []talos.EtcdMember
	Errors  []string
}

func probeMember(ctx context.Context, dial talos.Dialer, tc []byte, v *memberView) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v.State = "silent"
	err := talos.With(pctx, dial, talos.Target{Endpoint: v.Endpoint, Talosconfig: tc}, func(c talos.Client) error {
		st, serr := c.EtcdStatus(pctx)
		ms, merr := c.EtcdMembers(pctx)
		switch {
		case merr == nil && len(ms) > 0:
			v.State, v.Members = "listening", ms
			if serr == nil {
				v.Errors = st.Errors
			}
		case serr == nil:
			v.State, v.Detail = "stuck", fmt.Sprintf("etcd runs but lists no members: %v", merr)
		default:
			if _, verr := c.Version(pctx); verr != nil {
				v.Detail = verr.Error()
				return nil
			}
			v.State, v.Detail = "stopped", "etcd does not run"
		}
		return nil
	})
	if err != nil {
		v.Detail = err.Error()
	}
}

type restoreInput struct {
	Cluster, Snapshot, Node string
	// User is who proposes; empty when nothing is proposed.
	User string
	Now  time.Time
}

// restorePlan is the diagnosis and the operations that fix it.
type restorePlan struct {
	Cluster  string
	Scenario string
	Views    []memberView
	Findings []string
	Ops      []*metalv1alpha1.HostOperation
	Notes    []string
	in       restoreInput
}

func planRestore(ctx context.Context, cl client.Client, dial talos.Dialer, tc []byte, in restoreInput) (*restorePlan, error) {
	cluster := &osv1alpha1.TalosCluster{}
	if err := cl.Get(ctx, client.ObjectKey{Name: in.Cluster}, cluster); err != nil {
		return nil, fmt.Errorf("TalosCluster %s: %w", in.Cluster, err)
	}
	nodes := &osv1alpha1.TalosNodeList{}
	if err := cl.List(ctx, nodes); err != nil {
		return nil, err
	}
	hosts := &metalv1alpha1.HostList{}
	if err := cl.List(ctx, hosts); err != nil {
		return nil, err
	}
	hostOf := map[string]string{}
	for _, h := range hosts.Items {
		if h.Spec.OS != nil {
			hostOf[h.Spec.OS.NodeRef.Name] = h.Name
		}
	}
	p := &restorePlan{Cluster: in.Cluster, in: in}
	for _, n := range nodes.Items {
		if n.Spec.ClusterRef.Name != in.Cluster || n.Spec.Role != osv1alpha1.RoleControlPlane {
			continue
		}
		v := memberView{Node: n, Endpoint: render.NodeAddress(&n), Host: hostOf[n.Name]}
		if v.Endpoint == "" {
			v.State, v.Detail = "silent", "no address"
		} else {
			probeMember(ctx, dial, tc, &v)
		}
		p.Views = append(p.Views, v)
	}
	slices.SortFunc(p.Views, func(a, b memberView) int { return strings.Compare(a.Node.Name, b.Node.Name) })
	if len(p.Views) == 0 {
		return nil, fmt.Errorf("cluster %s has no control-plane TalosNode", in.Cluster)
	}

	var members []talos.EtcdMember
	for _, v := range p.Views {
		if v.State == "listening" {
			members = v.Members
			break
		}
	}
	if members != nil {
		p.planMembers(members)
		return p, nil
	}
	return p, p.planRecovery(ctx, cl, cluster)
}

// planMembers: etcd has a majority; replace the members that are broken.
func (p *restorePlan) planMembers(members []talos.EtcdMember) {
	byHost := map[string]talos.EtcdMember{}
	for _, m := range members {
		byHost[m.Hostname] = m
	}
	known := map[string]bool{}
	for _, v := range p.Views {
		known[v.Node.Name] = true
		m, listed := byHost[v.Node.Name]
		switch {
		case v.State == "listening" && len(v.Errors) == 0:
			continue
		case v.State == "silent":
			p.Findings = append(p.Findings, fmt.Sprintf("%s does not answer (%s): power it on or look at its console; "+
				"if it is gone for good, release its claim so that it leaves the cluster", v.Node.Name, v.Detail))
			continue
		}
		why := v.Detail
		if len(v.Errors) > 0 {
			why = "etcd reports: " + strings.Join(v.Errors, "; ")
		}
		p.Findings = append(p.Findings, fmt.Sprintf("%s: %s", v.Node.Name, why))
		if v.Host == "" {
			p.Findings = append(p.Findings, fmt.Sprintf("no Host runs %s, so no operation can be proposed for it", v.Node.Name))
			continue
		}
		var actions []metalv1alpha1.HostOperationAction
		ref := metalv1alpha1.Reference{Name: v.Node.Name}
		if listed {
			actions = append(actions, metalv1alpha1.HostOperationAction{EtcdRemoveMember: &metalv1alpha1.EtcdRemoveMemberAction{
				NodeRef: ref, MemberID: strconv.FormatUint(m.ID, 16), Hostname: m.Hostname}})
		}
		actions = append(actions, resetEphemeral(ref),
			metalv1alpha1.HostOperationAction{WaitEtcdHealthy: &metalv1alpha1.WaitEtcdHealthyAction{NodeRef: ref}})
		p.Ops = append(p.Ops, p.op(v.Host, "etcd-replace-"+v.Node.Name, actions))
	}
	for _, m := range members {
		if !known[m.Hostname] {
			p.Findings = append(p.Findings, fmt.Sprintf("etcd lists member %s (%x), which no control-plane TalosNode of "+
				"the cluster is: a leftover of a removed node. It counts towards the majority without voting; "+
				"remove it through a healthy member", m.Hostname, m.ID))
		}
	}
	switch {
	case len(p.Ops) > 0:
		p.Scenario = "one or more members lost; the majority holds"
		p.Notes = append(p.Notes, "replace one member at a time: approve the next operation only after the one "+
			"before has succeeded, so that the majority is never at risk")
	case len(p.Findings) > 0:
		p.Scenario = "the majority holds; nothing to propose"
	default:
		p.Scenario = "healthy: every control plane runs etcd and lists the members"
	}
}

// planRecovery: no control plane lists etcd members.
func (p *restorePlan) planRecovery(ctx context.Context, cl client.Client, cluster *osv1alpha1.TalosCluster) error {
	p.Scenario = "etcd lost: no control plane lists etcd members"
	if cluster.Status.BootstrappedAt == nil && cluster.Annotations[osv1alpha1.AnnotationRestorePending] == "" {
		p.Findings = append(p.Findings, fmt.Sprintf("cluster %s was never seen bootstrapped and is not marked for "+
			"restore: the operator may bootstrap an empty etcd at any moment. Mark it first: "+
			"kubectl annotate taloscluster %s %s=true", cluster.Name, cluster.Name, osv1alpha1.AnnotationRestorePending))
	}
	snap, err := p.chooseSnapshot(ctx, cl)
	if err != nil {
		return err
	}
	target := p.chooseTarget()
	if target == nil {
		return fmt.Errorf("no control plane answers; bring at least one back before restoring")
	}
	var silent []string
	for _, v := range p.Views {
		switch v.State {
		case "silent":
			silent = append(silent, v.Node.Name)
		case "stuck":
			if v.Host == "" {
				p.Findings = append(p.Findings, fmt.Sprintf("%s runs a stuck etcd, but no Host runs it: "+
					"no operation can wipe it", v.Node.Name))
				continue
			}
			ref := metalv1alpha1.Reference{Name: v.Node.Name}
			p.Ops = append(p.Ops, p.op(v.Host, "etcd-reset-"+v.Node.Name, []metalv1alpha1.HostOperationAction{resetEphemeral(ref)}))
		}
	}
	if len(silent) > 0 {
		p.Findings = append(p.Findings, fmt.Sprintf("%s do(es) not answer: the restore waits until every control "+
			"plane answers. Bring them back, or, if they are gone for good, remove their TalosNodes from the cluster first",
			strings.Join(silent, ", ")))
	}
	if target.Host == "" {
		return fmt.Errorf("no Host runs %s, so the restore cannot be proposed on it; pick another with --node", target.Node.Name)
	}
	p.Ops = append(p.Ops, p.op(target.Host, "etcd-recover-"+target.Node.Name, []metalv1alpha1.HostOperationAction{{
		EtcdRecover: &metalv1alpha1.EtcdRecoverAction{
			NodeRef:     metalv1alpha1.Reference{Name: target.Node.Name},
			SnapshotRef: metalv1alpha1.Reference{Name: snap.Name},
			SHA256:      snap.Status.SHA256,
		}}}))
	p.Notes = append(p.Notes,
		fmt.Sprintf("restores snapshot %s (taken %s from %s, %s); every change made to the cluster after it is lost",
			snap.Name, snapTime(snap).UTC().Format(time.RFC3339), orDash(snap.Status.TakenFrom), snap.Status.SHA256),
		fmt.Sprintf("after the restore operation is approved, upload the snapshot: pk-metal restore upload %s/%s "+
			"--identity <age identity file>", operatorNamespace, p.Ops[len(p.Ops)-1].Name),
		"the other control planes join the restored etcd by themselves once their etcd data is gone; "+
			"pk-metal restore plan "+p.Cluster+" shows any that do not")
	return nil
}

func (p *restorePlan) chooseSnapshot(ctx context.Context, cl client.Client) (*osv1alpha1.EtcdSnapshot, error) {
	if p.in.Snapshot != "" {
		s, err := getSnapshot(ctx, cl, p.in.Snapshot)
		if err != nil {
			return nil, err
		}
		switch {
		case s.Status.ClusterName != p.Cluster:
			return nil, fmt.Errorf("snapshot %s is of cluster %q, not %s", s.Name, s.Status.ClusterName, p.Cluster)
		case snapState(s) != "Verified":
			return nil, fmt.Errorf("snapshot %s is %s, not Verified", s.Name, snapState(s))
		}
		return s, nil
	}
	l := &osv1alpha1.EtcdSnapshotList{}
	if err := cl.List(ctx, l, client.MatchingLabels{osv1alpha1.LabelSnapshotCluster: p.Cluster}); err != nil {
		return nil, err
	}
	var best *osv1alpha1.EtcdSnapshot
	for i := range l.Items {
		s := &l.Items[i]
		if snapState(s) == "Verified" && (best == nil || snapTime(s).After(snapTime(best))) {
			best = s
		}
	}
	if best == nil {
		return nil, fmt.Errorf("cluster %s has no verified etcd snapshot in this cluster's catalogue (pk-metal backup "+
			"list %s); with a snapshot from elsewhere, see docs/metal/disaster-recovery.md", p.Cluster, p.Cluster)
	}
	return best, nil
}

func (p *restorePlan) chooseTarget() *memberView {
	for i := range p.Views {
		v := &p.Views[i]
		if (p.in.Node == "" && v.State != "silent") || v.Node.Name == p.in.Node {
			return v
		}
	}
	return nil
}

func resetEphemeral(ref metalv1alpha1.Reference) metalv1alpha1.HostOperationAction {
	return metalv1alpha1.HostOperationAction{TalosReset: &metalv1alpha1.TalosResetAction{
		NodeRef: ref, Graceful: ptr.To(false), Reboot: true, Mode: "SystemDisk",
		SystemPartitions: []metalv1alpha1.SystemPartition{"EPHEMERAL"},
	}}
}

// op proposes one repair operation. Every one carries the cluster-guard
// override: the guard holds a control plane down while etcd is not fully
// healthy, which is exactly the state a repair starts from, so without it
// the repair would wait for itself. The override lifts only the health rule;
// control planes still go down one at a time, and the operator's own nodes
// stay protected. It also ignores maintenance windows: a cluster without
// etcd is already down, so waiting for a window only extends the outage.
func (p *restorePlan) op(host, suffix string, actions []metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	why := "pk-metal restore: etcd of cluster " + p.Cluster + " is broken and this operation is its repair"
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.Cluster + "-" + suffix, Namespace: operatorNamespace,
			Labels: map[string]string{labelRestoreCluster: p.Cluster},
			Annotations: map[string]string{
				metalv1alpha1.AnnotationOverrideClusterGuard: why,
				metalv1alpha1.AnnotationIgnoreWindow:         why,
			},
		},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef: metalv1alpha1.Reference{Name: host}, Reason: restoreReason,
			ProposedBy: p.in.User, Actions: actions,
		},
	}
}

func (p *restorePlan) flags() string {
	var f []string
	for _, op := range p.Ops {
		for _, a := range op.Spec.Actions {
			if a.EtcdRecover != nil {
				f = append(f, "--snapshot "+a.EtcdRecover.SnapshotRef.Name, "--node "+a.EtcdRecover.NodeRef.Name)
			}
		}
	}
	if len(f) == 0 {
		return ""
	}
	return " " + strings.Join(f, " ")
}

func (p *restorePlan) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster %s: %s\n", p.Cluster, p.Scenario)
	for _, v := range p.Views {
		line := v.State
		if v.Detail != "" {
			line += " (" + v.Detail + ")"
		}
		if len(v.Members) > 0 {
			var names []string
			for _, m := range v.Members {
				s := fmt.Sprintf("%s/%x", m.Hostname, m.ID)
				if m.IsLearner {
					s += " learner"
				}
				names = append(names, s)
			}
			line += ", members " + strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "  %-16s %-15s %s\n", v.Node.Name, v.Endpoint, line)
	}
	for _, f := range p.Findings {
		fmt.Fprintf(&b, "finding: %s\n", f)
	}
	if len(p.Ops) > 0 {
		b.WriteString("plan, one operation per machine, in this order:\n")
		for i, op := range p.Ops {
			fmt.Fprintf(&b, "  %d. %s/%s on host %s:\n", i+1, op.Namespace, op.Name, op.Spec.HostRef.Name)
			for _, a := range op.Spec.Actions {
				fmt.Fprintf(&b, "       - %s\n", describeAction(a, planContext{}))
			}
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}

// propose creates the operations; an existing one of the same name is
// left as it is and named.
func (p *restorePlan) propose(ctx context.Context, cl client.Client, w io.Writer) error {
	for _, op := range p.Ops {
		err := cl.Create(ctx, op)
		switch {
		case apierrors.IsAlreadyExists(err):
			fmt.Fprintf(w, "exists already: %s/%s (delete it to propose it again)\n", op.Namespace, op.Name)
		case err != nil:
			return fmt.Errorf("propose %s/%s: %w", op.Namespace, op.Name, err)
		default:
			fmt.Fprintf(w, "proposed %s/%s\n", op.Namespace, op.Name)
		}
	}
	if len(p.Ops) > 0 {
		fmt.Fprintln(w, "each waits for approval by another person: pk-metal approve")
	}
	return nil
}

// describeEtcdRecover is the approve line of an etcdRecover step.
func describeEtcdRecover(a *metalv1alpha1.EtcdRecoverAction) string {
	return fmt.Sprintf("RESTORE etcd of the node's cluster on TalosNode %s from snapshot %s (%s): after "+
		"approval a person uploads the snapshot (pk-metal restore upload), then Talos starts etcd from it; "+
		"everything written to the cluster after the snapshot is lost",
		a.NodeRef.Name, a.SnapshotRef.Name, shortDigest(a.SHA256))
}

type uploadInput struct {
	Namespace, Name string
	Identity        string
	FromFile        string
	TalosconfigFile string
}

// uploadSnapshot decrypts the approved snapshot here and streams it to the
// node. The plaintext is read twice, never stored: once to check it against
// the record, once into the upload, so that nothing unchecked reaches the
// node.
func uploadSnapshot(ctx context.Context, cl client.Client, dial talos.Dialer, in uploadInput, w io.Writer) error {
	op := &metalv1alpha1.HostOperation{}
	if err := getNamed(ctx, cl, "operation", op, &metalv1alpha1.HostOperationList{}, in.Namespace, in.Name); err != nil {
		return err
	}
	var a *metalv1alpha1.EtcdRecoverAction
	for _, act := range op.Spec.Actions {
		if act.EtcdRecover != nil {
			a = act.EtcdRecover
		}
	}
	if a == nil {
		return fmt.Errorf("operation %s/%s has no etcdRecover step", op.Namespace, op.Name)
	}
	if ptr.Deref(op.Spec.Approval.Required, true) && op.Spec.Approval.ApprovedBy == "" {
		return fmt.Errorf("operation %s/%s is not approved yet; nothing is uploaded before it is: pk-metal approve %s/%s",
			op.Namespace, op.Name, op.Namespace, op.Name)
	}
	if c := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted); c != nil && c.Status != metav1.ConditionUnknown {
		return fmt.Errorf("operation %s/%s has finished (%s); propose a new one", op.Namespace, op.Name, c.Reason)
	}
	if op.Annotations[metalv1alpha1.AnnotationEtcdSnapshotUploaded] == a.SHA256 {
		fmt.Fprintln(w, "the snapshot was uploaded already; the operator is starting etcd from it")
		return nil
	}
	snap, err := getSnapshot(ctx, cl, a.SnapshotRef.Name)
	if err != nil {
		return err
	}
	if snap.Status.SHA256 != a.SHA256 {
		return fmt.Errorf("snapshot %s records %s, but the operation was approved for %s", snap.Name, snap.Status.SHA256, a.SHA256)
	}
	node := &osv1alpha1.TalosNode{}
	if err := cl.Get(ctx, client.ObjectKey{Name: a.NodeRef.Name}, node); err != nil {
		return fmt.Errorf("TalosNode %s: %w", a.NodeRef.Name, err)
	}
	endpoint := render.NodeAddress(node)
	if endpoint == "" {
		return fmt.Errorf("TalosNode %s has no address", node.Name)
	}
	tc, err := talosconfigFor(ctx, cl, node.Spec.ClusterRef.Name, in.TalosconfigFile)
	if err != nil {
		return err
	}
	ids, err := loadIdentities(in.Identity)
	if err != nil {
		return err
	}
	open := func() (io.ReadCloser, error) {
		if in.FromFile != "" {
			return os.Open(in.FromFile)
		}
		return openSnapshot(ctx, cl, snap)
	}

	// First pass: decrypt and check, writing nothing.
	src, err := open()
	if err != nil {
		return err
	}
	rep, err := etcdbackup.Restore(src, ids, io.Discard)
	_ = src.Close()
	if err == nil {
		err = rep.Valid()
	}
	if err != nil {
		return fmt.Errorf("the snapshot does not decrypt to an intact etcd snapshot: %w", err)
	}
	if got := "sha256:" + rep.SHA256; got != a.SHA256 {
		return fmt.Errorf("the decrypted snapshot is %s, but %s was approved: wrong file?", got, a.SHA256)
	}
	fmt.Fprintf(w, "decrypted and checked: %s, %s\n", humanBytes(rep.Size), a.SHA256)

	// Second pass: decrypt into the upload.
	src, err = open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	pr, pw := io.Pipe()
	check := make(chan error, 1)
	go func() {
		rep, err := etcdbackup.Restore(src, ids, pw)
		if err == nil && "sha256:"+rep.SHA256 != a.SHA256 {
			err = errors.New("the snapshot changed between the check and the upload")
		}
		_ = pw.CloseWithError(err)
		check <- err
	}()
	uctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	err = talos.With(uctx, dial, talos.Target{Endpoint: endpoint, Talosconfig: tc}, func(c talos.Client) error {
		return c.EtcdRecover(uctx, pr)
	})
	_ = pr.CloseWithError(cmp.Or(err, io.ErrClosedPipe))
	if cerr := <-check; err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("upload to %s (%s): %w", node.Name, endpoint, err)
	}
	fmt.Fprintf(w, "uploaded to %s (%s)\n", node.Name, endpoint)

	patch := client.MergeFrom(op.DeepCopy())
	if op.Annotations == nil {
		op.Annotations = map[string]string{}
	}
	op.Annotations[metalv1alpha1.AnnotationEtcdSnapshotUploaded] = a.SHA256
	if err := cl.Patch(ctx, op, patch); err != nil {
		return fmt.Errorf("record the upload on %s/%s (the snapshot is on the node; set annotation %s=%s by hand): %w",
			op.Namespace, op.Name, metalv1alpha1.AnnotationEtcdSnapshotUploaded, a.SHA256, err)
	}
	fmt.Fprintf(w, "recorded on %s/%s; the operator now starts etcd from it. Follow it with: pk-metal status\n",
		op.Namespace, op.Name)
	return nil
}
