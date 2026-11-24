# Model tests for the claim and operation protocol

pk-metal changes a machine only through a few cooperating controllers: the
claim controller binds a host and proposes a boot or a release, a person
approves, and the operation executor runs the steps against the BMC. Each
controller reads a cache that can lag behind the API server, and a process
can die at any point. Ordinary unit tests check one path at a time. The two
model tests on this page check many paths, so that an order of events nobody
wrote a test for is still covered.

Both run with `go test ./...` in `metal/`. They need no cluster and no
hardware.

## The protocol model

File: `metal/internal/controller/protocol_model_test.go`, test
`TestProtocolModel`.

This is a small explicit-state model checker. It holds the whole system as
one value, the *world*, and tries every turn every actor can take, breadth
first, up to a fixed number of turns. It checks the invariants after each
turn. When one fails, it prints the shortest sequence of turns that leads
there.

### What is in the world

- One host and one claim.
- The machine as the BMC sees it: power, operator media in a slot, and
  whether the machine runs what it booted from that media.
- Three operation slots: the claim's boot, the claim's release, and a BIOS
  operation that convergence proposes on the same host.
- Whether the node checked in, and whether an installed node has stopped
  answering for a while.
- Whether the host runs a control plane of the cluster the operator itself
  runs in, and whether it carries the `metal.pk.io/allow-self-disruption`
  annotation.

### Who takes turns

- The claim controller: finalizer, bind, propose or withdraw the boot,
  record the hand-off, release, remove the finalizer.
- The operation executor, one pass per operation: approval gate, queue,
  baseline, then one step.
- A person: approve, hold and lift the hold, put the host in maintenance and
  give it back, delete the claim, retry a failed or aborted operation, abort
  an unfinished operation (`metal.pk.io/abort`), allow the disruption of the
  operator's own control plane and take that back.
- The machine: the node checks in once it booted the operator's media. Once
  installed, it can stop answering for a while (a network flap, the Talos
  API restarting) and answer again.
- Convergence: it proposes the BIOS operation.

### Stale reads and conflicts

The model keeps the current world and the world one turn before. A
controller pass reads each object kind (claim, host, operations, node) from
either one, chosen per kind, because Kubernetes informers are per kind. The
model explores every choice. Writes go to the current world. An update made
from a stale read is a conflict and changes nothing, as on a real API server.
Reads that the real code makes through the uncached API reader always see
the current world: the hosts a deleted claim holds, the check that no boot
ran before a release unbinds a machine as it is, the queue check before an
operation starts, and the claim and host a boot reads before it starts.

### Tied to the real code

The model does not copy decision logic. It builds real API objects from the
model state and calls the same helpers the controllers call: `opFinished`,
`opAwaitsApproval`, `opActive`, `opSucceeded`, `approvalRequired`,
`hostHolder`, `bootRan`, `handedOff`, `powerOffDecided`, `approvalWaived`,
`releaseNeedsPerson`, `disruptionVerdict`,
and the phase functions in `internal/phase`. Only the control flow around these helpers is written
again in the test. Comments name the branch of the real code that each part
mirrors. If you change a branch in the controller, change its mirror too.

### Invariants

1. At most one started, unfinished operation runs on a host.
2. A started, unfinished operation reads `Running`.
3. Media are mounted only by an operation that a person approved, or that
   needs no approval.
4. A machine that a boot touched is unbound only after a release powered it
   off.
5. An unbound machine has no operator media in a slot and does not run from
   that media.
6. The claim finalizer is removed only when no host is bound to the claim.
7. A release made while the claim is held starts only after a person
   approved it.
8. A release of a machine that runs its node, or ran it on record, waits
   for a person, unless the claim waived approval. On record means the
   claim's hand-off, or the boot's recorded check-in step. The node may stop
   answering for a while, so readiness at the moment of the release is not
   enough.
9. The host phase, derived by `internal/phase` from conditions, follows the
   bind and the claim's deletion.
10. An operation that reboots or powers off an installed control plane of
    the cluster the operator runs in starts only while the host carries the
    `metal.pk.io/allow-self-disruption` annotation. Installed means the node
    answers now, or the claim records the hand-off: a control plane that
    stops answering for a while is still protected. The boot, the release
    and the BIOS operation all reboot or power off the machine. In the model
    the operator runs on one of three control planes and the other two
    answer, so the annotation is enough; etcd's majority, counted from what
    the other control planes answer at the moment of the decision, is
    covered by the unit tests of `disruptionVerdict` and `SelfGuard.Check`.
11. An aborted boot leaves no operator media in a slot. The abort ejects the
    media even when no step is on record yet: a pass that read the boot
    before the abort can mount and then lose its status write. An aborted
    release leaves the boot's media as it found them; the claim's release
    then waits for a person. The one-control-plane-at-a-time rule of the
    cluster guard needs more than one host, so it is covered by the unit
    tests of `ClusterGuard` and by envtest, not here.

### Bounds

| Bound | Value |
| --- | --- |
| Hosts, claims | 1 each |
| Operations | boot, release, one BIOS proposal |
| Turns | 14 by default, 10 with `go test -short` |
| Person actions | one hold, one maintenance, one retry, one refused step, one BIOS proposal, one allowance of self-disruption; a hold, a maintenance and the allowance can also be lifted. A fourth start state allows one abort, one retry and one BIOS proposal, and nothing else |
| Machine actions | the node checks in; where approval is required it may also stop answering once and answer again |
| Claims at start | one with approval required, one with approval waived, one on a control plane of the operator's own cluster, one where a person may abort |
| Cache lag | one turn, per object kind |

At 14 turns the model visits about 1,200,000 states in about 40 seconds. Set
`PK_MODEL_DEPTH` to explore further, for example:

```sh
cd metal
PK_MODEL_DEPTH=16 go test -count=1 -run TestProtocolModel -v ./internal/controller
```

Depth 16 visits about two million states in about a minute. Memory and time
grow fast with each extra turn.

### Known limit: a hold set right after a deletion

One invariant fails by design, and the test reports it as a skip with the
reason and a counterexample. A skip is used only when the counterexample
contains the known cause, here a turn labelled `person holds the claim`.
Any other violation of the same invariant fails the test, so the known
limit cannot hide a new defect.

A hold stops a release that waits for approval. It does not stop a release
that needs no approval (the machine runs no node, or the claim waived
approval) once that release exists. The window is the time between the
claim's deletion and the moment the claim controller's cache shows the
hold, usually well under a second:

1. A person deletes the claim.
2. A person holds the claim.
3. The claim controller runs a pass that still reads the claim without the
   hold, and creates a release that needs no approval.
4. The executor runs the release: the machine is powered off and its media
   ejected.

This breaks invariant 7. To keep a machine for sure, hold the claim before
you delete it.

### What the model does not cover

- More than one host or claim, and more than one operation of each kind.
- Liveness: the model checks that nothing bad happens, not that the
  protocol always finishes.
- The step details of each operation. The executor model covers those.
- The wait for known media slots before a release is proposed: the model
  takes the slots as known.

## The executor model

File: `metal/internal/controller/hostoperation_model_test.go`, test
`TestExecutorModel`.

This test drives the real `HostOperationReconciler` against the Redfish
simulator. Each case is a generated operation with random steps, and a
random set of faults, from a fixed seed:

- The BMC drops a call before it acts, or acts and the answer is lost.
- The BMC refuses one call.
- The API server answers a status write with a conflict, or applies the
  write and the answer is lost.
- The process dies at any BMC or API call. A new reconciler starts from what
  the API server holds, with an empty media grant table.
- A person deletes the operation in the middle of a pass.

After every pass the test checks that the recorded steps match what the BMC
did, that a finished operation makes no more BMC calls, and that a finished
or deleted operation holds no media grant. At the end it checks that the
machine is in the state the steps asked for, with one boot per power cycle.

It runs 300 cases, or 40 with `go test -short`. A failure names the seed, so
the case replays exactly. Firmware flashes are not generated, because each
one polls for seconds.
