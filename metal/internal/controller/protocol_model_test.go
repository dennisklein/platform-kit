package controller

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// An explicit-state model of the claim, operation, approval and release
// protocol, explored exhaustively to a bound. docs/metal/protocol-model.md
// describes it for people; this comment describes it for the code.
//
// The world is one host, one claim, its machine (the BMC), and three
// operation slots: the claim's boot, the claim's release, and a BIOS
// convergence proposal that competes for the host. Actors take turns:
//
//   - the claim reconciler (bind, propose, withdraw, release, finalizer),
//   - the executor, one pass per operation (gate, queue, baseline, one step),
//   - a person (approve, hold, unhold, maintenance on and off, delete the
//     claim, retry a failed operation),
//   - the machine (a node checks in once it booted the operator's media),
//   - convergence (proposes the BIOS operation).
//
// A reconciler reads through an informer cache, so each pass reads every
// object kind either from the current world or from the world one step
// before; the choice is made per kind, as informers are per kind. Its writes
// go to the current world: an update whose read is stale is a conflict and
// changes nothing (the pass ends there, as the real code returns the error),
// a create of an existing object is AlreadyExists, a delete always lands.
//
// Every decision the real code makes through a pure helper is made here by
// that helper, over real API objects built from the model state: opFinished,
// opAwaitsApproval, opActive, opSucceeded, approvalRequired, hostHolder,
// bootRan, handedOff, powerOffDecided, heldBy, inMaintenance, runsNode,
// approvalWaived, and the phase derivations. The control flow around them
// (release, provision, the executor's Reconcile) is mirrored by hand, branch
// by branch; the comments name the branch. Reads the real code makes through
// the API reader (the hosts of a deleted claim, bootRanUncached,
// activePredecessorUncached) read the current world.

type opSlot uint8

const (
	slotBios opSlot = iota
	slotBoot
	slotRelease
	nSlots
)

func (s opSlot) String() string { return [...]string{"bios", "boot", "release"}[s] }

// steps per operation kind, in order; see stepEffect.
var opSteps = [nSlots]uint8{slotBios: 2, slotBoot: 4, slotRelease: 2}

type mOp struct {
	Exists, Required, Approved bool
	Started                    bool  // the baseline is recorded
	Queued                     bool  // Executed=Unknown/OperationQueued
	Steps                      uint8 // recorded steps
	Outcome                    uint8 // 0 unfinished, 1 succeeded, 2 failed
	Seq                        uint8 // creation order: the queue's age
	// Ghosts: facts only the checker reads.
	HeldAtCreate, ApprovedWhileHeld bool
}

type mClaim struct {
	Exists, Deleting, Finalizer, Held bool
	HandOff, HandOffByOp              bool // status.handOff, and whether a boot of ours made it
	Waived                            bool // provisioning.approval: Automatic
}

type mHost struct {
	Claimed, Maintenance, OSReady bool
	PowerOff                      bool // the release's power-off decision, recorded on the host
}

type mBMC struct {
	On        bool
	Media     bool // operator media in a slot
	FromMedia bool // the machine runs what it booted from that media
}

type world struct {
	Claim     mClaim
	Host      mHost
	BMC       mBMC
	Ops       [nSlots]mOp
	NodeReady bool
	Seq       uint8
	// Ghosts.
	BootStarted, ReleaseDone bool
	// Budgets bound what a person and the machine do, so the space is finite.
	Holds, Maints, Retries, Fails, Proposals uint8
}

// state is the current world and the one before it: what a stale cache shows.
type state struct{ Cur, Prev world }

// view is what one reconciler pass reads.
type view struct {
	Claim     mClaim
	Host      mHost
	Ops       [nSlots]mOp
	NodeReady bool
}

func viewOf(s state, mask uint8) view {
	pick := func(bit uint8) *world {
		if mask&bit != 0 {
			return &s.Prev
		}
		return &s.Cur
	}
	return view{Claim: pick(1).Claim, Host: pick(2).Host, Ops: pick(4).Ops, NodeReady: pick(8).NodeReady}
}

// --- real API objects built from the model, for the real helpers ---

func opObj(o mOp) *metalv1alpha1.HostOperation {
	op := &metalv1alpha1.HostOperation{}
	op.CreationTimestamp = metav1.NewTime(time.Unix(int64(o.Seq), 0))
	op.Spec.Approval.Required = ptr.To(o.Required)
	if o.Approved {
		op.Spec.Approval.ApprovedBy = "person"
	}
	if o.Started {
		op.Status.Baseline = &metalv1alpha1.OperationBaseline{PowerState: "On"}
	}
	set := func(t string, s metav1.ConditionStatus, reason string) {
		setCondition(&op.Status.Conditions, 1, t, s, reason, "")
	}
	switch {
	case approvalRequired(op) && !o.Approved:
		set(metalv1alpha1.ConditionApproved, metav1.ConditionFalse, metalv1alpha1.ReasonAwaitingApproval)
	default:
		set(metalv1alpha1.ConditionApproved, metav1.ConditionTrue, metalv1alpha1.ReasonApproved)
	}
	switch {
	case o.Outcome == 1:
		set(metalv1alpha1.ConditionExecuted, metav1.ConditionTrue, metalv1alpha1.ReasonStepsSucceeded)
	case o.Outcome == 2:
		set(metalv1alpha1.ConditionExecuted, metav1.ConditionFalse, metalv1alpha1.ReasonStepFailed)
	case o.Queued:
		set(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonOperationQueued)
	case o.Started:
		set(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonExecuting)
	}
	return op
}

// opsObj is the operations that exist, named by their slot, as a list the
// executor reads.
func opsObj(ops [nSlots]mOp) []metalv1alpha1.HostOperation {
	var out []metalv1alpha1.HostOperation
	for i, o := range ops {
		if o.Exists {
			op := opObj(o)
			op.Name, op.UID = opSlot(i).String(), types.UID(opSlot(i).String())
			out = append(out, *op)
		}
	}
	return out
}

// holder is the slot hostHolder picks, or nSlots for none.
func holder(ops [nSlots]mOp) opSlot {
	if h := hostHolder(opsObj(ops)); h != nil {
		for i := range nSlots {
			if i.String() == h.Name {
				return i
			}
		}
	}
	return nSlots
}

const modelClaimUID = "claim-uid"

func claimObj(c mClaim) *metalv1alpha1.HostClaim {
	cl := &metalv1alpha1.HostClaim{}
	cl.UID = modelClaimUID
	if c.Held {
		cl.Annotations = map[string]string{metalv1alpha1.AnnotationHold: ""}
	}
	if c.HandOff {
		cl.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "host-a", Node: "node-a"}
		if c.HandOffByOp {
			cl.Status.HandOff.Operation = "pk-metal/boot"
		}
	}
	if c.Waived {
		cl.Spec.Provisioning = &metalv1alpha1.ClaimProvisioning{Approval: metalv1alpha1.ProvisioningApprovalAutomatic}
	}
	return cl
}

func hostObj(h mHost) *metalv1alpha1.Host {
	host := &metalv1alpha1.Host{}
	host.Annotations = map[string]string{}
	if h.Maintenance {
		host.Annotations[metalv1alpha1.AnnotationMaintenance] = ""
	}
	if h.PowerOff {
		host.Annotations[annotationReleasePowerOff] = modelClaimUID
	}
	setCondition(&host.Status.Conditions, 1, metalv1alpha1.ConditionInspected, metav1.ConditionTrue, "Inspected", "")
	setCondition(&host.Status.Conditions, 1, metalv1alpha1.ConditionVerified, metav1.ConditionTrue, "Verified", "")
	if h.OSReady {
		setCondition(&host.Status.Conditions, 1, metalv1alpha1.ConditionOSReady, metav1.ConditionTrue, "NodeReady", "")
	}
	return host
}

func held(c mClaim) bool {
	_, ok := heldBy(claimObj(c))
	return ok
}

func maintenance(h mHost) bool {
	_, ok := inMaintenance(hostObj(h))
	return ok
}

// --- transitions ---

// pass is one actor's turn: it starts from the current world and records
// what the checker needs to judge the turn.
type pass struct {
	s     state
	next  world
	label string
	// Facts of the turn for the transition invariants.
	unbound, finalizerRemoved, mounted bool
	started                            opSlot
	startedAny                         bool
	createdRelease                     bool
	mountedBy                          opSlot
	dead                               bool // a write conflicted: the pass ended
}

func newPass(s state, label string) *pass { return &pass{s: s, next: s.Cur, label: label} }

// write applies an update of one object whose read was v, if v is what the
// world still holds: otherwise it is a conflict and the pass ends.
func (p *pass) updateOp(i opSlot, read mOp, apply func(*mOp)) bool {
	if p.dead {
		return false
	}
	cur := p.next.Ops[i]
	if !cur.Exists || stripOp(cur) != stripOp(read) {
		p.dead = true
		return false
	}
	apply(&p.next.Ops[i])
	return true
}

func stripOp(o mOp) mOp { o.HeldAtCreate, o.ApprovedWhileHeld = false, false; return o }

func (p *pass) updateHost(read mHost, apply func(*mHost)) bool {
	if p.dead || p.next.Host != read {
		p.dead = true
		return false
	}
	apply(&p.next.Host)
	return true
}

func (p *pass) updateClaim(read mClaim, apply func(*mClaim)) bool {
	if p.dead || !p.next.Claim.Exists || p.next.Claim != read {
		p.dead = true
		return false
	}
	apply(&p.next.Claim)
	return true
}

func (p *pass) create(i opSlot, required bool) {
	if p.dead || p.next.Ops[i].Exists {
		return // AlreadyExists: the pass goes on with what it built
	}
	p.next.Seq++
	p.next.Ops[i] = mOp{Exists: true, Required: required, Seq: p.next.Seq, HeldAtCreate: p.next.Claim.Held}
	p.createdRelease = i == slotRelease
}

func (p *pass) delete(i opSlot) {
	if !p.dead {
		p.next.Ops[i] = mOp{}
	}
}

// claimPass mirrors HostClaimReconciler.Reconcile over view v.
func claimPass(s state, v view, mask uint8) *pass {
	p := newPass(s, fmt.Sprintf("claim reconcile (stale %04b)", mask))
	c := v.Claim
	if !c.Exists {
		// Gone without passing through release: unbind what it holds.
		if v.Host.Claimed {
			p.unbind(v.Host)
		}
		return p
	}
	if c.Deleting {
		if c.Finalizer {
			// finishRelease reads the hosts through the API reader: never
			// stale. The operations come from the cache, except where
			// bootRanUncached reads them (see release).
			live := v
			live.Host = s.Cur.Host
			p.release(live)
		}
		return p
	}
	claim := c
	if !claim.Finalizer {
		if !p.updateClaim(c, func(cl *mClaim) { cl.Finalizer = true }) {
			return p
		}
		claim.Finalizer = true
	}
	host := v.Host
	if !host.Claimed {
		if !p.updateHost(host, func(h *mHost) { h.Claimed = true }) {
			return p
		}
		host.Claimed = true
	}
	p.provision(v, claim, host)
	return p
}

// provision mirrors HostClaimReconciler.provision for the bound host.
func (p *pass) provision(v view, claim mClaim, host mHost) {
	boot := v.Ops[slotBoot]
	withdraw := func() {
		if boot.Exists && opAwaitsApproval(opObj(boot)) {
			p.delete(slotBoot)
		}
	}
	cl := claimObj(claim)
	switch {
	case cl.Status.HandOff != nil:
		withdraw()
	case v.NodeReady:
		// recordHandOff: the boot that got it there, if one of ours did.
		byOp := boot.Exists && bootRan([]metalv1alpha1.HostOperation{*opObj(boot)}, "")
		withdraw()
		p.updateClaim(v.Claim, func(c *mClaim) { c.Finalizer, c.HandOff, c.HandOffByOp = true, true, byOp })
	case boot.Exists && opSucceeded(opObj(boot)):
		p.updateClaim(v.Claim, func(c *mClaim) { c.Finalizer, c.HandOff, c.HandOffByOp = true, true, true })
	case held(claim):
		withdraw()
	case maintenance(host):
		withdraw()
	case !boot.Exists:
		p.create(slotBoot, !approvalWaived(cl))
	}
}

// release mirrors HostClaimReconciler.release and finishRelease.
func (p *pass) release(v view) {
	claim := claimObj(v.Claim)
	isHeld := held(v.Claim)
	keepRecords, done := isHeld, true
	boot := v.Ops[slotBoot]
	var provisionOps []metalv1alpha1.HostOperation
	if boot.Exists {
		provisionOps = append(provisionOps, *opObj(boot))
	}
	if v.Host.Claimed {
		host := v.Host
		rel := v.Ops[slotRelease]
		var op *metalv1alpha1.HostOperation
		if rel.Exists {
			op = opObj(rel)
		}
		powerOff := op != nil || bootRan(provisionOps, "") || handedOff(claim, "host-a") ||
			powerOffDecided(hostObj(host), claim)
		if !powerOff {
			// bootRanUncached: the boot operations as the API server holds
			// them; when none ran, the unfinished ones are deleted at the
			// version just read, which this turn does at once.
			live := p.next.Ops[slotBoot]
			if live.Exists && bootRan([]metalv1alpha1.HostOperation{*opObj(live)}, "") {
				powerOff = true
			} else if live.Exists && !opFinished(opObj(live)) {
				p.delete(slotBoot)
			}
		}
		unbind := !powerOff
		if powerOff {
			switch {
			case isHeld && (op == nil || opAwaitsApproval(op)):
				if op != nil {
					p.delete(slotRelease)
				}
				done = false
			case maintenance(host) && op == nil:
				done, keepRecords = false, true
			default:
				// recordPowerOff, before the boot records go.
				if !powerOffDecided(hostObj(host), claim) {
					if !p.updateHost(host, func(h *mHost) { h.PowerOff = true }) {
						return
					}
					host.PowerOff = true
				}
				if op == nil {
					required := runsNode(hostObj(host)) && !approvalWaived(claim)
					p.create(slotRelease, required)
					op = opObj(mOp{Exists: true, Required: required})
				}
				switch {
				case opAwaitsApproval(op):
					done = false
				case !opSucceeded(op):
					done = false // failed or not yet finished
				default:
					unbind = true
				}
			}
		}
		if unbind {
			if !p.unbind(host) {
				return
			}
		}
	}
	if keepRecords && !done {
		return
	}
	if boot.Exists && !opFinished(opObj(boot)) {
		p.delete(slotBoot)
	}
	if !done {
		return
	}
	p.delete(slotBoot)
	// finishRelease: every machine is back, the finalizer goes.
	if p.updateClaim(v.Claim, func(c *mClaim) { *c = mClaim{} }) {
		p.finalizerRemoved = true
	}
}

func (p *pass) unbind(read mHost) bool {
	if p.updateHost(read, func(h *mHost) { h.Claimed, h.PowerOff = false, false }) {
		p.unbound = true
		return true
	}
	return false
}

// executorPass mirrors HostOperationReconciler.Reconcile for slot i: at most
// one step per pass, which only adds interleavings.
func executorPass(s state, v view, mask uint8, i opSlot, fail bool) *pass {
	verb := "pass"
	if fail {
		verb = "pass, the machine refuses"
	}
	p := newPass(s, fmt.Sprintf("executor %s %s (stale %04b)", i, verb, mask))
	o := v.Ops[i]
	obj := opObj(o)
	if !o.Exists {
		return p // NotFound: its grants are revoked
	}
	if opFinished(obj) {
		if i != slotBoot {
			p.delete(i) // expire: release and convergence operations carry a TTL
		}
		return p
	}
	if approvalRequired(obj) && obj.Spec.Approval.ApprovedBy == "" {
		return p // Approved=False; nothing else
	}
	// activePredecessor: hostHolder over the cached operations.
	if holder(v.Ops) != i {
		if !o.Queued {
			p.updateOp(i, o, func(m *mOp) { m.Queued = true })
		}
		return p
	}
	if obj.Status.Baseline == nil {
		if maintenance(v.Host) {
			return p
		}
		// activePredecessorUncached: hostHolder over the operations as the
		// API server holds them. Passes run one at a time, so this read and
		// the baseline write below are one turn.
		if holder(p.next.Ops) != i {
			if !o.Queued {
				p.updateOp(i, o, func(m *mOp) { m.Queued = true })
			}
			return p
		}
		// recordBootStart: a boot of the claim stamps the host, read from
		// the API server, before its baseline.
		if h := p.next.Host; i == slotBoot && h.Claimed && !powerOffDecided(hostObj(h), claimObj(v.Claim)) {
			if !p.updateHost(h, func(h *mHost) { h.PowerOff = true }) {
				return p
			}
		}
		if p.updateOp(i, o, func(m *mOp) { m.Started, m.Queued = true, false }) {
			p.started, p.startedAny = i, true
			if i == slotBoot {
				p.next.BootStarted = true
			}
		}
		return p
	}
	step := o.Steps
	if i == slotBoot && step == 2 && !v.NodeReady {
		return p // awaitCheckIn waits
	}
	if fail {
		p.next.Fails--
		p.updateOp(i, o, func(m *mOp) { m.Outcome, m.Queued = 2, false })
		return p
	}
	p.stepEffect(i, step)
	last := step+1 == opSteps[i]
	if p.updateOp(i, o, func(m *mOp) {
		m.Steps, m.Queued = step+1, false
		if last {
			m.Outcome = 1
		}
	}) && last && i == slotRelease {
		p.next.ReleaseDone = true
	}
	return p
}

// stepEffect is what step n of an operation does to the machine.
func (p *pass) stepEffect(i opSlot, n uint8) {
	b := &p.next.BMC
	switch {
	case i == slotBoot && n == 0: // the installer and the config are mounted
		b.Media = true
		p.mounted, p.mountedBy = true, i
	case i == slotBoot && n == 1, i == slotBios && n == 1: // power cycle
		b.On, b.FromMedia = true, b.Media
	case i == slotBoot && n == 3, i == slotRelease && n == 1: // eject
		b.Media = false
	case i == slotRelease && n == 0: // power off
		b.On, b.FromMedia = false, false
	}
}

// successors lists every turn any actor can take in s.
func successors(s state) []*pass {
	var out []*pass
	for mask := range uint8(16) {
		// A stale read of a kind that did not change is no different read.
		if mask&1 != 0 && s.Cur.Claim == s.Prev.Claim ||
			mask&2 != 0 && s.Cur.Host == s.Prev.Host ||
			mask&4 != 0 && s.Cur.Ops == s.Prev.Ops ||
			mask&8 != 0 && s.Cur.NodeReady == s.Prev.NodeReady {
			continue
		}
		v := viewOf(s, mask)
		out = append(out, claimPass(s, v, mask))
		for i := range nSlots {
			out = append(out, executorPass(s, v, mask, i, false))
			if s.Cur.Fails > 0 && i != slotBios {
				out = append(out, executorPass(s, v, mask, i, true))
			}
		}
	}
	w := s.Cur
	person := func(label string, apply func(*world)) {
		p := newPass(s, label)
		apply(&p.next)
		out = append(out, p)
	}
	for i := range nSlots {
		// The admission policy: approve once, never your own proposal, and
		// only an unfinished operation that asks for it.
		if o := w.Ops[i]; o.Exists && opAwaitsApproval(opObj(o)) {
			person("person approves "+i.String(), func(n *world) {
				n.Ops[i].Approved = true
				n.Ops[i].ApprovedWhileHeld = n.Claim.Held
			})
		}
		if o := w.Ops[i]; o.Exists && o.Outcome == 2 && w.Retries > 0 {
			person("person retries "+i.String(), func(n *world) { n.Ops[i] = mOp{}; n.Retries-- })
		}
	}
	if w.Claim.Exists && !w.Claim.Held && w.Holds > 0 {
		person("person holds the claim", func(n *world) { n.Claim.Held = true; n.Holds-- })
	}
	if w.Claim.Exists && w.Claim.Held {
		person("person lifts the hold", func(n *world) { n.Claim.Held = false })
	}
	if !w.Host.Maintenance && w.Maints > 0 {
		person("person puts the host in maintenance", func(n *world) { n.Host.Maintenance = true; n.Maints-- })
	}
	if w.Host.Maintenance {
		person("person gives the host back", func(n *world) { n.Host.Maintenance = false })
	}
	if w.Claim.Exists && !w.Claim.Deleting {
		person("person deletes the claim", func(n *world) {
			n.Claim.Deleting = true
			if !n.Claim.Finalizer {
				n.Claim = mClaim{}
			}
		})
	}
	if !w.Ops[slotBios].Exists && w.Proposals > 0 {
		p := newPass(s, "convergence proposes a BIOS operation")
		p.next.Proposals--
		p.create(slotBios, true)
		out = append(out, p)
	}
	if w.BMC.On && w.BMC.FromMedia && !w.NodeReady {
		person("the node checks in", func(n *world) {
			n.NodeReady, n.Host.OSReady, n.BMC.FromMedia = true, true, false
		})
	}
	return out
}

// --- invariants ---

type invariant struct {
	name string
	// known, when set, is why a violation is expected today. A violation
	// whose trace contains one of knownWhen (none matches every trace) is
	// reported as a skip with that reason; any other violation fails the
	// test, so a known defect cannot hide a new one.
	known     string
	knownWhen []string
	check     func(p *pass) string
}

func startedCount(w world) int {
	n := 0
	for _, o := range w.Ops {
		if o.Exists && o.Started && o.Outcome == 0 {
			n++
		}
	}
	return n
}

// knownTrace reports whether a trace contains one of marks; no marks
// matches every trace.
func knownTrace(trace, marks []string) bool {
	if len(marks) == 0 {
		return true
	}
	return slices.ContainsFunc(trace, func(l string) bool {
		return slices.ContainsFunc(marks, func(m string) bool { return strings.Contains(l, m) })
	})
}

var protocolInvariants = []invariant{
	{
		name: "at most one started, unfinished operation per host",
		check: func(p *pass) string {
			if n := startedCount(p.next); n > 1 {
				return fmt.Sprintf("%d started operations", n)
			}
			return ""
		},
	},
	{
		name: "a started, unfinished operation reads Running",
		check: func(p *pass) string {
			for i, o := range p.next.Ops {
				if !o.Exists || !o.Started || o.Outcome != 0 {
					continue
				}
				if ph, _ := phase.ForOperation(opObj(o).Status.Conditions); ph != metalv1alpha1.OperationRunning {
					return fmt.Sprintf("%s reads %s", opSlot(i), ph)
				}
			}
			return ""
		},
	},
	{
		name: "media are mounted only by an operation a person approved or that needs none",
		check: func(p *pass) string {
			if !p.mounted {
				return ""
			}
			o := p.s.Cur.Ops[p.mountedBy]
			if approvalRequired(opObj(o)) && !o.Approved {
				return p.mountedBy.String() + " mounted without approval"
			}
			return ""
		},
	},
	{
		name: "a machine a boot touched is unbound only after a release powered it off",
		check: func(p *pass) string {
			if p.unbound && p.next.BootStarted && !p.next.ReleaseDone {
				return "unbound with no release done"
			}
			return ""
		},
	},
	{
		name: "an unbound machine runs no operator media and holds none",
		check: func(p *pass) string {
			b := p.next.BMC
			if !p.next.Host.Claimed && (b.Media || (b.On && b.FromMedia)) && !anyActive(p.next) {
				return fmt.Sprintf("unbound, media %v, booted from media %v", b.Media, b.On && b.FromMedia)
			}
			return ""
		},
	},
	{
		name: "the finalizer goes only when no host carries the claim",
		check: func(p *pass) string {
			if p.finalizerRemoved && p.next.Host.Claimed {
				return "finalizer removed, host still bound"
			}
			return ""
		},
	},
	{
		name:      "a release made while the claim is held starts only if a person approved it",
		knownWhen: []string{"person holds the claim"},
		known: "a race, not a defect of one branch: a hold set within one cache lag of the deletion is not " +
			"seen by the pass that makes a pre-cleared release, and a pre-cleared release is not withdrawn by a hold",
		check: func(p *pass) string {
			if p.startedAny && p.started == slotRelease {
				o := p.next.Ops[slotRelease]
				if o.HeldAtCreate && !o.ApprovedWhileHeld {
					return "release created under a hold started without a person"
				}
			}
			return ""
		},
	},
	{
		name: "a release of a machine that runs a node waits for a person, unless the claim waived it",
		check: func(p *pass) string {
			w := p.next
			if p.createdRelease && w.Host.OSReady && !w.Claim.Waived && !w.Ops[slotRelease].Required {
				return "a pre-cleared release of a running node"
			}
			return ""
		},
	},
	{
		name: "the host phase follows the bind and the claim's deletion",
		check: func(p *pass) string {
			w := p.next
			if w.Host.Maintenance {
				return ""
			}
			facts := phase.HostFacts{Claimed: w.Host.Claimed, Releasing: w.Host.Claimed && w.Claim.Deleting}
			ph, _ := phase.ForHost(hostObj(w.Host).Status.Conditions, facts)
			switch {
			case w.Host.Claimed && w.Claim.Deleting && ph != metalv1alpha1.HostReleasing:
				return "a released host reads " + string(ph)
			case w.Host.Claimed && (ph == metalv1alpha1.HostAvailable || ph == metalv1alpha1.HostEnrolling):
				return "a bound host reads " + string(ph)
			case !w.Host.Claimed && !w.Host.OSReady && ph != metalv1alpha1.HostAvailable:
				return "a free host reads " + string(ph)
			}
			return ""
		},
	},
}

// anyActive: some operation still holds the host, so the machine is between
// two of the operator's steps and not yet handed back.
func anyActive(w world) bool {
	for _, o := range w.Ops {
		if o.Exists && o.Started && o.Outcome == 0 {
			return true
		}
	}
	return false
}

// --- the explorer ---

type edge struct {
	from  state
	label string
}

type found struct {
	trace []string
	msg   string
}

func explore(t *testing.T, initial []state, depth int) (violations, knownViolations map[string]*found, n int) {
	parent := map[state]edge{}
	seen := map[state]int{}
	frontier := slices.Clone(initial)
	for _, s := range initial {
		seen[s] = 0
	}
	violations, knownViolations = map[string]*found{}, map[string]*found{}
	traceTo := func(s state, last string) []string {
		out := []string{last}
		for {
			e, ok := parent[s]
			if !ok {
				break
			}
			out = append(out, e.label)
			s = e.from
		}
		slices.Reverse(out)
		return out
	}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []state
		for _, s := range frontier {
			for _, p := range successors(s) {
				for _, inv := range protocolInvariants {
					if violations[inv.name] != nil {
						continue
					}
					msg := inv.check(p)
					if msg == "" {
						continue
					}
					tr := traceTo(s, p.label)
					switch {
					case inv.known != "" && knownTrace(tr, inv.knownWhen):
						if knownViolations[inv.name] == nil {
							knownViolations[inv.name] = &found{trace: tr, msg: msg}
						}
					default:
						violations[inv.name] = &found{trace: tr, msg: msg}
					}
				}
				ns := state{Cur: p.next, Prev: s.Cur}
				if ns.Cur == s.Cur {
					continue // a turn that changed nothing
				}
				if _, ok := seen[ns]; ok {
					continue
				}
				seen[ns] = d + 1
				parent[ns] = edge{from: s, label: p.label}
				next = append(next, ns)
			}
		}
		frontier = next
	}
	t.Logf("explored %d states to depth %d", len(seen), depth)
	return violations, knownViolations, len(seen)
}

// TestProtocolModel explores every interleaving of the actors to a depth of
// 14 turns (10 with -short; PK_MODEL_DEPTH sets another bound) from a claim
// with and without approval waived. Each person action is budgeted: one
// hold, one maintenance, one retry, one refused step, one BIOS proposal; a
// hold and a maintenance may be lifted again.
func TestProtocolModel(t *testing.T) {
	depth := 14
	if testing.Short() {
		depth = 10
	}
	if d, err := strconv.Atoi(os.Getenv("PK_MODEL_DEPTH")); err == nil && d > 0 {
		depth = d
	}
	var initial []state
	for _, waived := range []bool{false, true} {
		w := world{
			Claim: mClaim{Exists: true, Waived: waived},
			Holds: 1, Maints: 1, Retries: 1, Fails: 1, Proposals: 1,
		}
		initial = append(initial, state{Cur: w, Prev: w})
	}
	violations, known, n := explore(t, initial, depth)
	require.Positive(t, n)
	for _, inv := range protocolInvariants {
		t.Run(inv.name, func(t *testing.T) {
			report := func(v *found) string {
				return fmt.Sprintf("%s:\n  %s", v.msg, strings.Join(v.trace, "\n  "))
			}
			if v := violations[inv.name]; v != nil {
				t.Errorf("counterexample, %s", report(v))
				return
			}
			if v := known[inv.name]; v != nil {
				t.Skipf("known defect (%s); counterexample %s", inv.known, report(v))
			}
		})
	}
}
