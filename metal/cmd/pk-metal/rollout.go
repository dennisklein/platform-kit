package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func init() {
	verbHelp["rollout"] = `
A rollout changes the nodes of one Talos cluster one at a time: a config
change, a Talos upgrade, a Kubernetes upgrade or a rolling reboot. The
operator proposes rollouts on its own when the cluster spec and the machines
disagree; it never starts one. A person approves the plan.

  list                    every rollout, newest last, with its state
  show <rollout>          the plan: each node in order, what changes on it,
                          whether it reboots, and how far the rollout got
  approve [--yes] <r>     show the plan, ask, and approve exactly that plan
  resume <rollout>        go on after the pause (the first node is done, or
                          someone paused it)
  pause <rollout>         stop before the next node; the node that runs now
                          finishes
  abort [--yes] <r>       stop for good: no further node starts; the node that
                          runs now finishes. An abort cannot be taken back

The approval names the plan by its hash. If the plan changes before the
first node starts (a node was added, the spec changed again), the approval
no longer covers it: the rollout ends as PlanChanged and the operator
proposes a fresh one.

After the first node the rollout always pauses, so you can look at that node
before the rest follow. Control planes go first, one at a time, then workers.

examples:
  pk-metal rollout list
  pk-metal rollout show lab-talosupgrade-1a2b3c4d
  pk-metal rollout approve lab-talosupgrade-1a2b3c4d
  pk-metal rollout resume lab-talosupgrade-1a2b3c4d`
}

func rolloutCmd(args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs := newFlags("rollout")
		parseFlags(fs, args)
		badUsage(fs, "give a subcommand: list, show, approve, resume, pause or abort")
	}
	sub, rest := args[0], args[1:]
	fs := newFlags("rollout " + sub)
	// Only approve and abort ask, so only they take --yes: on the others it
	// would be accepted and mean nothing.
	yes := new(bool)
	switch sub {
	case "approve":
		yes = fs.Bool("yes", false, "do not ask (for scripts); the plan is still printed and still approved only as read")
	case "abort":
		yes = fs.Bool("yes", false, "do not ask (for scripts)")
	}
	addGlobalFlags(fs)
	parseFlags(fs, rest)
	ctx := context.Background()
	need := func(n int) {
		if fs.NArg() != n {
			badUsage(fs, fmt.Sprintf("rollout %s takes %d argument(s)", sub, n))
		}
	}
	var run func(cl client.Client) error
	switch sub {
	case "list":
		need(0)
		run = func(cl client.Client) error { return listRollouts(ctx, cl, os.Stdout, time.Now()) }
	case "show":
		need(1)
		run = func(cl client.Client) error {
			ro, err := getRollout(ctx, cl, fs.Arg(0))
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, renderRollout(ro))
			return nil
		}
	case "approve":
		need(1)
		run = func(cl client.Client) error {
			user, err := whoAmI(ctx, cl)
			if err != nil {
				return err
			}
			return approveRollout(ctx, cl, rolloutApproval{Name: fs.Arg(0), User: user, Yes: *yes,
				In: os.Stdin, Interactive: isTerminal(os.Stdin), Out: os.Stdout})
		}
	case "resume":
		need(1)
		run = func(cl client.Client) error { return resumeRollout(ctx, cl, fs.Arg(0), os.Stdout) }
	case "pause":
		need(1)
		run = func(cl client.Client) error { return pauseRollout(ctx, cl, fs.Arg(0), os.Stdout) }
	case "abort":
		need(1)
		run = func(cl client.Client) error {
			return abortRollout(ctx, cl, fs.Arg(0), *yes, os.Stdin, isTerminal(os.Stdin), os.Stdout)
		}
	default:
		badUsage(fs, "unknown subcommand "+sub+": give list, show, approve, resume, pause or abort")
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	if err := run(cl); err != nil {
		die(err)
	}
}

func getRollout(ctx context.Context, cl client.Client, name string) (*osv1alpha1.TalosRollout, error) {
	ro := &osv1alpha1.TalosRollout{}
	if err := cl.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("rollout %s does not exist; see: pk-metal rollout list", name)
		}
		return nil, err
	}
	return ro, nil
}

// rolloutState is one word for where a rollout stands, from its conditions.
func rolloutState(ro *osv1alpha1.TalosRollout) string {
	conds := ro.Status.Conditions
	if c := meta.FindStatusCondition(conds, osv1alpha1.ConditionComplete); c != nil && c.Status != "Unknown" {
		return c.Reason
	}
	if ro.Spec.Abort {
		return "Aborting"
	}
	if c := meta.FindStatusCondition(conds, osv1alpha1.ConditionPaused); c != nil && c.Status == "True" {
		return c.Reason
	}
	if ro.Spec.Approval.ApprovedBy != "" {
		if c := meta.FindStatusCondition(conds, osv1alpha1.ConditionProgressing); c != nil {
			return c.Reason
		}
		return "Approved"
	}
	if c := meta.FindStatusCondition(conds, osv1alpha1.ConditionPlanned); c != nil && c.Status != "True" {
		return c.Reason
	}
	if ro.Status.PlanHash == "" {
		return "Planning"
	}
	return osv1alpha1.ReasonAwaitingApproval
}

func listRollouts(ctx context.Context, cl client.Client, w io.Writer, now time.Time) error {
	list := &osv1alpha1.TalosRolloutList{}
	if err := cl.List(ctx, list); err != nil {
		return fmt.Errorf("list rollouts: %w", err)
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "no rollouts")
		return nil
	}
	slices.SortFunc(list.Items, func(a, b osv1alpha1.TalosRollout) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	tw := tabwriter.NewWriter(w, 2, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLLOUT\tCLUSTER\tKIND\tTARGET\tNODES\tSTATE\tAPPROVED BY\tAGE")
	for i := range list.Items {
		ro := &list.Items[i]
		done := 0
		for _, st := range ro.Status.Steps {
			if st.State == osv1alpha1.RolloutNodeSucceeded {
				done++
			}
		}
		age := "-"
		if !ro.CreationTimestamp.IsZero() {
			age = shortDuration(now.Sub(ro.CreationTimestamp.Time))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\n", ro.Name, ro.Spec.ClusterRef.Name, ro.Spec.Kind,
			orDash(rolloutTarget(ro)), done, len(ro.Status.Plan), rolloutState(ro), orDash(ro.Spec.Approval.ApprovedBy), age)
	}
	_ = tw.Flush()
	return nil
}

func rolloutTarget(ro *osv1alpha1.TalosRollout) string {
	if ro.Spec.Target == nil {
		return ""
	}
	var parts []string
	if v := ro.Spec.Target.TalosVersion; v != "" {
		parts = append(parts, "Talos "+v)
	}
	if v := ro.Spec.Target.KubernetesVersion; v != "" {
		parts = append(parts, "Kubernetes "+v)
	}
	return strings.Join(parts, ", ")
}

// renderRollout is what a person reads before approving: the order, what
// changes on each node and whether it reboots, and how far it got.
func renderRollout(ro *osv1alpha1.TalosRollout) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rollout %s: %s on cluster %s", ro.Name, ro.Spec.Kind, ro.Spec.ClusterRef.Name)
	if t := rolloutTarget(ro); t != "" {
		fmt.Fprintf(&b, " to %s", t)
	}
	fmt.Fprintf(&b, "\n  proposed by %s\n", orDash(ro.Spec.ProposedBy))
	if len(ro.Spec.RequestedBy) > 0 {
		fmt.Fprintf(&b, "  asked by %s\n", strings.Join(ro.Spec.RequestedBy, ", "))
	}
	if ro.Spec.Approval.ApprovedBy != "" {
		fmt.Fprintf(&b, "  approved by %s for plan %s\n", ro.Spec.Approval.ApprovedBy, shortDigest(ro.Spec.Approval.PlanHash))
	}
	fmt.Fprintf(&b, "  state %s\n", rolloutState(ro))
	for _, t := range []string{osv1alpha1.ConditionPlanned, osv1alpha1.ConditionProgressing,
		osv1alpha1.ConditionPaused, osv1alpha1.ConditionComplete} {
		if c := meta.FindStatusCondition(ro.Status.Conditions, t); c != nil && c.Message != "" {
			fmt.Fprintf(&b, "  %s: %s\n", t, c.Message)
		}
	}
	if len(ro.Status.Plan) == 0 {
		b.WriteString("\nno plan yet\n")
		return b.String()
	}
	maxW := ro.Spec.MaxUnavailableWorkers
	if maxW < 1 {
		maxW = 1
	}
	fmt.Fprintf(&b, "\nplan %s: control planes one at a time, then up to %d worker(s) at once; "+
		"it pauses after the first node\n", shortDigest(ro.Status.PlanHash), maxW)
	for i, s := range ro.Status.Plan {
		state := ""
		if i < len(ro.Status.Steps) {
			st := ro.Status.Steps[i]
			state = "  [" + string(st.State) + "]"
			if st.Message != "" {
				state += " " + st.Message
			}
		}
		if s.Kind == osv1alpha1.RolloutStepKubeProxy {
			fmt.Fprintf(&b, "%3d. kube-proxy: set the image to %s and wait until it ran on every node%s\n", i+1, s.Image, state)
			continue
		}
		change := ""
		switch {
		case s.From != "" || s.To != "":
			change = fmt.Sprintf("%s -> %s", orDash(s.From), orDash(s.To))
		case s.ConfigDigest != "":
			change = "config " + shortDigest(s.ConfigDigest)
		}
		reboot := "no reboot"
		if s.RebootRequired {
			reboot = "REBOOTS"
		}
		fmt.Fprintf(&b, "%3d. %s (%s, host %s): %s, %s%s\n", i+1, s.Node, s.Role, orDash(s.Host), orDash(change), reboot, state)
		if len(s.Actions) > 0 {
			fmt.Fprintf(&b, "     steps: %s\n", strings.Join(s.Actions, ", "))
		}
	}
	return b.String()
}

// rolloutApproval is one approve call.
type rolloutApproval struct {
	Name        string
	User        string
	Yes         bool
	In          io.Reader
	Interactive bool
	Out         io.Writer
	Window      time.Duration
}

// approvableRollout refuses a rollout that nobody can approve now. Whether
// the proposer may approve it is the site's setting (selfApproval).
func approvableRollout(ro *osv1alpha1.TalosRollout) error {
	switch {
	case ro.Spec.Approval.ApprovedBy != "":
		return fmt.Errorf("rollout %s is already approved by %s (an approval cannot be changed)", ro.Name, ro.Spec.Approval.ApprovedBy)
	case ro.Spec.Abort:
		return fmt.Errorf("rollout %s was aborted; there is nothing to approve", ro.Name)
	case ro.Status.PlanHash == "" || len(ro.Status.Plan) == 0:
		return fmt.Errorf("rollout %s has no plan yet; look again in a minute: pk-metal rollout show %s", ro.Name, ro.Name)
	}
	if c := meta.FindStatusCondition(ro.Status.Conditions, osv1alpha1.ConditionPlanned); c == nil || c.Status != "True" {
		why := "the operator has not finished planning it"
		if c != nil && c.Message != "" {
			why = c.Message
		}
		return fmt.Errorf("rollout %s cannot be approved: %s", ro.Name, why)
	}
	return nil
}

func approveRollout(ctx context.Context, cl client.Client, a rolloutApproval) error {
	ro, err := getRollout(ctx, cl, a.Name)
	if err != nil {
		return err
	}
	if err := approvableRollout(ro); err != nil {
		return err
	}
	role := ownRole(a.User, ro.Spec.ProposedBy, ro.Spec.RequestedBy)
	var self selfApproval
	if role != "" {
		// Admission decides; refusing here, where the setting is readable,
		// spares the person a plan they cannot approve.
		self = readSelfApproval(ctx, cl)
		if err := self.ownProposal("rollout "+ro.Name, a.User, role); err != nil {
			return err
		}
	}
	shown := renderRollout(ro)
	fmt.Fprint(a.Out, shown)
	if role != "" {
		fmt.Fprint(a.Out, "\n"+self.note("rollout", role))
	}
	if !a.Yes {
		if !a.Interactive {
			return fmt.Errorf("stdin is not a terminal, so pk-metal cannot ask; " +
				"read the plan above and rerun with --yes to approve it")
		}
		window := a.Window
		if window <= 0 {
			window = approvalWindow
		}
		fmt.Fprintf(a.Out, "\nApprove plan %s as %s? [y/N] ", shortDigest(ro.Status.PlanHash), a.User)
		line, err := ask(ctx, a.In, window)
		if errors.Is(err, errNoAnswer) {
			fmt.Fprintln(a.Out)
			return fmt.Errorf("no answer within %s; nothing changed. Run approve again to see the plan as it is now",
				shortDuration(window))
		}
		if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
			return errDeclined
		}
	}
	// The approval lands on exactly the rollout whose plan was shown, with
	// or without the question: the uid and resourceVersion make the API
	// server refuse it (409 Conflict) when the rollout was made again under
	// the same name or changed in any way since it was read, and the plan
	// hash in the approval makes the operator refuse to run another plan
	// (PlanChanged). The operator writes the rollout's status often, so a
	// conflict reads it again and approves only if it is the same rollout,
	// still approvable, with the same plan. --yes skips the question only.
	changed := fmt.Errorf("rollout %s changed since its plan was shown; nothing changed. "+
		"Look at the plan as it is now and approve again", ro.Name)
	shownUID, shownPlan := ro.UID, ro.Status.PlanHash
	reread := !a.Yes // the answer took a while
	for attempt := 0; ; attempt++ {
		if reread {
			now, err := getRollout(ctx, cl, a.Name)
			if err != nil {
				return err
			}
			if now.UID != shownUID || now.Status.PlanHash != shownPlan || approvableRollout(now) != nil {
				return changed
			}
			ro = now
		}
		err := cl.Patch(ctx, ro, client.RawPatch(types.MergePatchType, rolloutApprovalPatch(ro, a.User)))
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) {
			return approvalRefused("rollout "+ro.Name, err)
		}
		if attempt+1 >= patchAttempts {
			return changed
		}
		reread = true
	}
	fmt.Fprintf(a.Out, "approved rollout %s (plan %s) as %s; the first node starts now and the rollout pauses after it. "+
		"Follow it with: pk-metal rollout show %s\n", ro.Name, shortDigest(ro.Status.PlanHash), a.User, ro.Name)
	return nil
}

// rolloutApprovalPatch stamps the approval, with the plan hash that was
// shown, on exactly the rollout object that was read.
func rolloutApprovalPatch(ro *osv1alpha1.TalosRollout, user string) []byte {
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": ro.UID, "resourceVersion": ro.ResourceVersion},
		"spec":     map[string]any{"approval": map[string]any{"approvedBy": user, "planHash": ro.Status.PlanHash}},
	})
	return patch
}

// resumeRollout clears spec.paused and lets the rollout past the pause after
// its first node, for the plan it runs now.
func resumeRollout(ctx context.Context, cl client.Client, name string, w io.Writer) error {
	ro, err := getRollout(ctx, cl, name)
	if err != nil {
		return err
	}
	if ro.Spec.Approval.ApprovedBy == "" {
		return fmt.Errorf("rollout %s is not approved yet; approve it first: pk-metal rollout approve %s", name, name)
	}
	if ro.Status.PlanHash == "" {
		return fmt.Errorf("rollout %s has no plan", name)
	}
	user, err := requester(ctx, cl)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": ro.UID,
			"annotations": map[string]any{osv1alpha1.AnnotationRolloutResume: ro.Status.PlanHash,
				metalv1alpha1.AnnotationRequestedBy: user}},
		"spec": map[string]any{"paused": nil},
	})
	if err := cl.Patch(ctx, ro, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("resume rollout %s: %w", name, err)
	}
	fmt.Fprintf(w, "resumed rollout %s; the next node starts once the cluster is healthy\n", name)
	return nil
}

func pauseRollout(ctx context.Context, cl client.Client, name string, w io.Writer) error {
	ro, err := getRollout(ctx, cl, name)
	if err != nil {
		return err
	}
	user, err := requester(ctx, cl)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": ro.UID,
			"annotations": map[string]any{metalv1alpha1.AnnotationRequestedBy: user}},
		"spec": map[string]any{"paused": true},
	})
	if err := cl.Patch(ctx, ro, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("pause rollout %s: %w", name, err)
	}
	fmt.Fprintf(w, "paused rollout %s; a node that runs now finishes, no further node starts. Go on with: pk-metal rollout resume %s\n",
		name, name)
	return nil
}

func abortRollout(ctx context.Context, cl client.Client, name string, yes bool, in io.Reader, interactive bool, w io.Writer) error {
	ro, err := getRollout(ctx, cl, name)
	if err != nil {
		return err
	}
	if ro.Spec.Abort {
		fmt.Fprintf(w, "rollout %s is already aborted\n", name)
		return nil
	}
	if !yes {
		if !interactive {
			return fmt.Errorf("stdin is not a terminal, so pk-metal cannot ask; rerun with --yes to abort")
		}
		fmt.Fprintf(w, "Abort rollout %s for good? A node that runs now finishes; no further node starts. [y/N] ", name)
		line, err := ask(ctx, in, approvalWindow)
		if err != nil {
			return err
		}
		if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
			return errors.New("not aborted; nothing changed")
		}
	}
	user, err := requester(ctx, cl)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": ro.UID,
			"annotations": map[string]any{metalv1alpha1.AnnotationRequestedBy: user}},
		"spec": map[string]any{"abort": true},
	})
	if err := cl.Patch(ctx, ro, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("abort rollout %s: %w", name, err)
	}
	fmt.Fprintf(w, "aborted rollout %s. Delete it when you want the operator to propose the change again\n", name)
	return nil
}
