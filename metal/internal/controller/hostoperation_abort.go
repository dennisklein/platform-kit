package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/drain"
	"github.com/dennisklein/platform-kit/metal/internal/redact"
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
			failed = append(failed, what+": "+redact.String(e.Error()))
		}
		return nil
	}

	for _, slot := range x.leftMounted(i, true) {
		cl, _, cerr := x.client(ctx)
		if cerr == nil {
			cerr = boot.EjectAndVerify(ctx, cl, slot, nil)
		}
		if e := note("ejected virtual media "+slot, cerr); e != nil {
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

// leftMounted lists the slots that the steps before step i mounted a medium
// in and no later step of them emptied or reused. With inFlight, step i
// counts too: a pass can die between the mount and its record.
func (x *exec) leftMounted(i int, inFlight bool) []string {
	actions := x.op.Spec.Actions
	mounts := actions[:min(recordedSteps(x.op), i)]
	if inFlight && i < len(actions) && actions[i].InsertMedia != nil {
		mounts = actions[:i+1]
	}
	var slots []string
	for j, a := range mounts {
		in := a.InsertMedia
		if in == nil || slices.Contains(slots, in.MediaID) || slotReusedLater(mounts[j+1:], in.MediaID) {
			continue
		}
		slots = append(slots, in.MediaID)
	}
	return slots
}

// ejectAfterFailure ejects, once and best effort, the media that the steps
// up to the failed step i left mounted. The trailing eject steps of the
// operation never run after a failure, and a BMC keeps a mounted URL (on the
// vmm dialect with the grant credentials in it) until something ejects it.
// The grant is revoked anyway, so a failed eject is reported, not retried.
// The failed step itself is left out: a mount that failed holds no grant
// any more, and a check that failed before it touched the slot must not
// take out what somebody else mounted there. It returns one phrase for the
// failure record, or "" when nothing was mounted.
func (x *exec) ejectAfterFailure(ctx context.Context, i int) string {
	slots := x.leftMounted(i, false)
	if len(slots) == 0 {
		return ""
	}
	var done, failed []string
	for _, slot := range slots {
		cl, _, err := x.client(ctx)
		if err == nil {
			err = boot.EjectAndVerify(ctx, cl, slot, nil)
		}
		if err != nil {
			failed = append(failed, slot+": "+redact.String(err.Error()))
			continue
		}
		done = append(done, slot)
	}
	var parts []string
	if len(done) > 0 {
		parts = append(parts, "ejected virtual media "+strings.Join(done, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "could NOT eject virtual media, eject it by hand: "+strings.Join(failed, "; "))
	}
	return strings.Join(parts, "; ")
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
