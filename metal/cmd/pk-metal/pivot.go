package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/pivot"
)

func init() {
	verbHelp["pivot"] = `
pivot moves the pk-metal operator from the cluster it runs in now (the
current kubeconfig context, or --kubeconfig and --context) to another
cluster (--to). It is meant for this case: the operator ran in a bootstrap
cluster (kind) and installed a Talos cluster; now the operator moves onto that
Talos cluster and manages the machines from there.

The steps:
  1. Check: no operation runs or waits for approval, and the other cluster
     runs no pk-metal operator.
  2. Stop the operator here (scale it to zero) and wait until it has stopped.
  3. Install the operator there: CRDs, admission policies, RBAC, namespace,
     and the media store's PersistentVolumeClaim, empty (see --media-store).
  4. Copy the operator's objects there, with their status: machines, BMCs,
     claims, operations, Talos clusters and nodes, and the Secrets in the
     pk-metal namespace.
  5. Start the operator there, and watch it: it must not propose a new
     operation (an install that was already done is never done again).

The operator here stays stopped, and its objects stay. Nothing on the
machines changes. pivot prints what the new cluster must be able to reach
(BMCs, Talos API, media service); check it before you approve.

The media store (a PersistentVolumeClaim the operator keeps the installers
for one-slot BMCs in) needs storage on the other cluster: the claim there
already, or a default StorageClass to make it from. Its files do not move;
the operator builds the installers it needs again. A cluster without storage
is refused, unless --media-store=ephemeral: the operator there then mounts an
emptyDir, which starts empty and is lost on every restart, and it builds no
installer for a one-slot BMC until the claim exists.

Run pivot again after a failure: it continues where it stopped. To move back,
run pivot from the new cluster with --to naming the old one.

examples:
  pk-metal pivot --to mgmt.kubeconfig
  pk-metal --context kind-pk-metal pivot --to mgmt.kubeconfig --to-context admin@mgmt --yes`
}

func pivotCmd(args []string) {
	fs := newFlags("pivot")
	to := fs.String("to", "", "kubeconfig file of the cluster to move to (required)")
	toContext := fs.String("to-context", "", "context in the --to kubeconfig (default: its current context)")
	image := fs.String("image", "", "manager image on the new cluster (default: the image the operator runs now)")
	mediaURL := fs.String("media-url", "", "address the BMCs fetch boot media from on the new cluster, for example https://<node address>:30443 (default: the current one)")
	mediaStore := fs.String("media-store", string(pivot.ClaimsCopy),
		"what the media store becomes on the new cluster: copy (a claim made there from its StorageClass, or the one already there) or ephemeral (an emptyDir: no installer for a one-slot BMC is built there)")
	forceWaiting := fs.Bool("force-waiting", false, "move operations that wait for approval; they wait on the new cluster instead")
	settle := fs.Duration("settle", 90*time.Second, "how long to watch the new operator for operations it must not propose")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for each step")
	yes := fs.Bool("yes", false, "move without asking (for scripts); the checks are still printed")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		badUsage(fs, "pivot takes no arguments")
	}
	if *to == "" {
		badUsage(fs, "--to is required: the kubeconfig of the cluster to move to")
	}
	claims := pivot.ClaimsMode(*mediaStore)
	if claims != pivot.ClaimsCopy && claims != pivot.ClaimsEphemeral {
		badUsage(fs, fmt.Sprintf("--media-store is copy or ephemeral, not %q", *mediaStore))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	scheme := newScheme()
	srcCfg, srcName, err := namedRestConfig(global.kubeconfig, global.context)
	if err != nil {
		die(err)
	}
	dstCfg, dstName, err := namedRestConfig(*to, *toContext)
	if err != nil {
		die(err)
	}
	source, err := pivot.NewCluster(srcName, srcCfg, scheme)
	if err != nil {
		die(err)
	}
	target, err := pivot.NewCluster(dstName, dstCfg, scheme)
	if err != nil {
		die(err)
	}
	// The move sets approvals and guard lifts again on the target and prunes
	// what an earlier run left there: its audit lines (replayed, deleted)
	// go to standard error and name the user who runs it. Keep them.
	mover, err := whoAmI(ctx, target.Client)
	if err != nil {
		die(fmt.Errorf("the move's audit lines must name you: %w", err))
	}
	m, err := pivot.New(ctx, source, target, pivot.Options{
		Image: *image, MediaURL: *mediaURL, Claims: claims, ForceWaiting: *forceWaiting,
		Settle: *settle, Timeout: *timeout, Log: prefixed{os.Stdout},
		Audit: audit.New(os.Stderr, mover),
	})
	if err != nil {
		die(err)
	}
	plan, err := m.Preflight(ctx)
	if err != nil {
		die(err)
	}
	if err := confirmPivot(os.Stdout, os.Stdin, plan, srcName, dstName, *yes, isTerminal(os.Stdin)); err != nil {
		die(err)
	}
	rep, err := m.Run(ctx)
	if err != nil {
		die(err)
	}
	fmt.Print(pivotDone(rep, srcName, dstName, *to))
}

// confirmPivot prints the plan and asks; a resumed move only verifies and
// does not ask.
func confirmPivot(out io.Writer, in io.Reader, plan *pivot.Plan, src, dst string, yes, interactive bool) error {
	if plan.Resume {
		fmt.Fprintf(out, "the operator already runs on %s after a move from %s; checking it again\n", dst, src)
		return nil
	}
	fmt.Fprintf(out, "pk-metal pivot: move the operator from %s to %s\n\n", src, dst)
	fmt.Fprintf(out, "From %s, the operator must reach (pivot cannot test this):\n", dst)
	for _, line := range plan.Reachable {
		fmt.Fprintf(out, "  - %s\n", line)
	}
	if len(plan.Claims) > 0 {
		fmt.Fprintf(out, "\nThe volumes the operator mounts, on %s:\n", dst)
		for _, c := range plan.Claims {
			fmt.Fprintf(out, "  - %s\n", c)
		}
	}
	if plan.Prune {
		fmt.Fprintf(out, "\n%s holds an older copy of the operator's objects; objects that %s no longer has are removed from it.\n", dst, src)
	}
	fmt.Fprintf(out, "\nThe operator on %s stops now. No machine is touched.\n", src)
	if yes {
		return nil
	}
	if !interactive {
		return fmt.Errorf("stdin is not a terminal, so pk-metal cannot ask; read the checks above and rerun with --yes")
	}
	fmt.Fprint(out, "Move? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errDeclined
}

// pivotDone is the closing text: what happened and what a person does next.
func pivotDone(rep *pivot.Report, src, dst, toFile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nThe operator runs on %s now.\n", dst)
	if rep != nil && rep.Rewritten > 0 {
		fmt.Fprintf(&b, "%d references to claims and owners were changed to the new cluster's objects.\n", rep.Rewritten)
	}
	fmt.Fprintf(&b, `
The operator on %[1]s is stopped (zero replicas). Its objects are still there.
Do not start it again while the operator on %[2]s runs: two operators would
act on the same machines.

Next:
  pk-metal --kubeconfig %[3]s status       check the machines from the new cluster
  kind delete cluster --name <name>          remove a kind bootstrap cluster when you no longer need it

To move back, run the move in the other direction:
  pk-metal --kubeconfig %[3]s pivot --to <kubeconfig of %[1]s>
`, src, dst, toFile)
	return b.String()
}

// prefixed indents the move's step lines.
type prefixed struct{ w io.Writer }

func (p prefixed) Write(b []byte) (int, error) {
	if _, err := fmt.Fprintf(p.w, "  %s", b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// namedRestConfig loads a kubeconfig the way restConfig does, and names the
// cluster for messages by its context, or by the file.
func namedRestConfig(path, context string) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = path
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: context})
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("cannot load the kubeconfig %s: %w", path, err)
	}
	name := context
	if name == "" {
		if raw, err := loader.RawConfig(); err == nil {
			name = raw.CurrentContext
		}
	}
	if name == "" {
		name = path
	}
	if name == "" {
		name = cfg.Host
	}
	return cfg, name, nil
}
