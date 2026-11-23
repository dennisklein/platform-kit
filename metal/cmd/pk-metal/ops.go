package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func init() {
	verbHelp["retry"] = `
A failed operation stays in the cluster, so that you can read why it failed
(pk-metal status shows the reason). The operator does not try again while it
is there. retry deletes it; the operator then proposes the same plan again,
and the new operation waits for approval like the first one.

To stop the operator from proposing it again instead, hold the claim:
  pk-metal hold default/<claim>`
	verbHelp["hold"] = `
A hold tells the operator to propose no new operation for the claim (no
install, no reinstall, no BIOS or firmware change). It changes nothing on the
machine, and operations that already exist are not changed: approve or delete
those yourself. The reason is stored on the claim, so that others can see why.

Claims usually live in namespace default, so that is the default here.

examples:
  pk-metal hold --reason "disk swap, ticket 123" default/node-1
  pk-metal unhold default/node-1`
}

func retryCmd(args []string) {
	fs := newFlags("retry")
	ns := fs.String("n", operatorNamespace, "namespace of the operation")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one operation")
	}
	namespace, name, err := splitRef(fs.Arg(0), *ns, flagGiven(fs, "n"))
	if err != nil {
		badUsage(fs, err.Error())
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	if err := retry(context.Background(), cl, namespace, name, os.Stdout); err != nil {
		die(err)
	}
}

// retry deletes a failed operation after saying what happens next.
func retry(ctx context.Context, cl client.Client, namespace, name string, w io.Writer) error {
	op := &metalv1alpha1.HostOperation{}
	if err := getNamed(ctx, cl, "operation", op, &metalv1alpha1.HostOperationList{}, namespace, name); err != nil {
		return err
	}
	ref := op.Namespace + "/" + op.Name
	if op.Status.Phase != metalv1alpha1.OperationFailed {
		return fmt.Errorf("operation %s is %s, not Failed; retry deletes failed operations only",
			ref, orDash(string(op.Status.Phase)))
	}
	if why := explain(op.Status.Conditions, "", metalv1alpha1.ConditionExecuted); why != "" {
		fmt.Fprintf(w, "operation %s failed: %s\n", ref, why)
	}
	claim := ""
	if n := op.Labels[labelClaimName]; n != "" {
		claim = op.Labels[labelClaimNamespace] + "/" + n
	}
	if err := cl.Delete(ctx, op); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	fmt.Fprintf(w, "deleted %s.\n", ref)
	if claim == "" {
		fmt.Fprintln(w, "If the operator proposed it, it proposes it again in a few seconds; "+
			"if a person created it, create it again.")
		return nil
	}
	fmt.Fprintf(w, "The operator proposes it again for claim %s in a few seconds; "+
		"it then waits for approval: pk-metal approve\n", claim)
	c := &metalv1alpha1.HostClaim{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: op.Labels[labelClaimNamespace], Name: op.Labels[labelClaimName]}, c); err == nil {
		if _, held := c.Annotations[holdAnnotation]; held {
			fmt.Fprintf(w, "Claim %s is on hold, so nothing is proposed until: pk-metal unhold %s\n", claim, claim)
		}
	}
	return nil
}

func holdCmd(args []string) {
	fs := newFlags("hold")
	ns := fs.String("n", claimNamespace, "namespace of the claim")
	reason := fs.String("reason", "", "why, for the people who see the hold (stored on the claim)")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	setHold(fs, ns, *reason, true)
}

func unholdCmd(args []string) {
	fs := newFlags("unhold")
	ns := fs.String("n", claimNamespace, "namespace of the claim")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	setHold(fs, ns, "", false)
}

// setHold runs hold or unhold for the one claim the command line names.
func setHold(fs *flag.FlagSet, ns *string, reason string, on bool) {
	if fs.NArg() != 1 {
		badUsage(fs, "give exactly one claim")
	}
	namespace, name, err := splitRef(fs.Arg(0), *ns, flagGiven(fs, "n"))
	if err != nil {
		badUsage(fs, err.Error())
	}
	cl, err := newClient()
	if err != nil {
		die(err)
	}
	if err := hold(context.Background(), cl, namespace, name, reason, on, os.Stdout); err != nil {
		die(err)
	}
}

// hold sets (on) or removes the hold annotation on a claim.
func hold(ctx context.Context, cl client.Client, namespace, name, reason string, on bool, w io.Writer) error {
	c := &metalv1alpha1.HostClaim{}
	if err := getNamed(ctx, cl, "claim", c, &metalv1alpha1.HostClaimList{}, namespace, name); err != nil {
		return err
	}
	ref := c.Namespace + "/" + c.Name
	var value any // JSON null removes the annotation in a merge patch
	if on {
		value = reason
	} else if _, held := c.Annotations[holdAnnotation]; !held {
		fmt.Fprintf(w, "claim %s is not on hold; nothing changed\n", ref)
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{holdAnnotation: value}}})
	if err != nil {
		return err
	}
	if err := cl.Patch(ctx, c, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("update claim %s: %w", ref, err)
	}
	if on {
		fmt.Fprintf(w, "claim %s is on hold: the operator proposes no new operation for it.\n"+
			"Existing operations are not changed. Remove the hold with: pk-metal unhold %s\n", ref, ref)
	} else {
		fmt.Fprintf(w, "claim %s is no longer on hold: the operator proposes operations for it again\n", ref)
	}
	return nil
}
