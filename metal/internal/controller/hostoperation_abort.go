package controller

import (
	"context"
	"fmt"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/drain"
)

// undo takes back what the steps before step i left on the machine when an
// operation is aborted there:
//
//   - a medium a step mounted, and no later step took out, is ejected: the
//     machine must not boot an installer at its next start;
//   - a boot override a step set, if the machine still holds it, is
//     cleared, for the same reason;
//   - a Kubernetes Node the operation cordoned is made schedulable again (a
//     node someone else had cordoned is left alone).
//
// What it cannot undo, it does not: a reboot happened, a config applied, an
// etcd member gone stay so, and the abort record says which steps ran.
//
// It returns what it undid and what it could not, one phrase each. err is a
// failure to reach the BMC or the cluster this pass, which the caller tries
// again; any other failure lands in failed, and the abort goes on.
func (x *exec) undo(ctx context.Context, i int) (undone, failed []string, err error) {
	actions := x.op.Spec.Actions
	ran := actions[:min(recordedSteps(x.op), i)]
	note := func(what string, e error) error {
		switch {
		case e == nil:
			undone = append(undone, what)
		case transient(ctx, e):
			return e
		default:
			failed = append(failed, what+": "+e.Error())
		}
		return nil
	}

	// The step in flight may have mounted already: a pass can die between
	// the mount and its record.
	mounts := ran
	if i < len(actions) && actions[i].InsertMedia != nil {
		mounts = actions[:i+1]
	}
	ejected := map[string]bool{}
	for j, a := range mounts {
		in := a.InsertMedia
		if in == nil || ejected[in.MediaID] || slotReusedLater(mounts[j+1:], in.MediaID) {
			continue
		}
		ejected[in.MediaID] = true
		cl, _, cerr := x.client(ctx)
		if cerr == nil {
			cerr = boot.EjectAndVerify(ctx, cl, in.MediaID, nil)
		}
		if e := note("ejected virtual media "+in.MediaID, cerr); e != nil {
			return nil, nil, e
		}
	}

	for _, a := range ran {
		o := a.SetBootOverride
		if o == nil {
			continue
		}
		if e := note("cleared the boot override to "+o.Target, x.clearOverride(ctx, o.Target)); e != nil {
			return nil, nil, e
		}
		break // one clear clears every override
	}

	if node := x.op.Status.CordonedNode; node != "" {
		uerr := x.uncordonNode(ctx, node)
		if e := note("made Node "+node+" schedulable again", uerr); e != nil {
			return nil, nil, e
		}
		if uerr == nil {
			x.op.Status.CordonedNode = ""
		}
	}
	return undone, failed, nil
}

// clearOverride disables the boot override when the machine still holds one
// to target. One a boot used up already reads as disabled and is left alone.
func (x *exec) clearOverride(ctx context.Context, target string) error {
	cl, sysID, err := x.client(ctx)
	if err != nil {
		return err
	}
	sys, err := systemOf(ctx, cl, sysID)
	if err != nil {
		return err
	}
	cur := sys.BootOverride
	if cur.Enabled == bmc.OverrideDisabled || cur.Enabled == "" || cur.Target != target {
		return nil
	}
	return cl.SetBootOverride(ctx, sysID, bmc.BootOverride{Target: "None", Enabled: bmc.OverrideDisabled})
}

// uncordonNode makes the named Node schedulable through the cluster of the
// host's TalosNode.
func (x *exec) uncordonNode(ctx context.Context, node string) error {
	_, acc, err := x.talosNode(ctx, metalv1alpha1.Reference{Name: node})
	if err != nil {
		return err
	}
	kube, err := x.workload(ctx, acc)
	if err != nil {
		return err
	}
	if _, err := drain.Uncordon(ctx, kube, node); err != nil {
		return fmt.Errorf("uncordon %s: %w", node, err)
	}
	return nil
}
