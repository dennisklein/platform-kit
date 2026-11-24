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
- Whether the node checked in.

### Who takes turns

- The claim controller: finalizer, bind, propose or withdraw the boot,
  record the hand-off, release, remove the finalizer.
- The operation executor, one pass per operation: approval gate, queue,
  baseline, then one step.
- A person: approve, hold and lift the hold, put the host in maintenance and
  give it back, delete the claim, retry a failed operation.
- The machine: the node checks in once it booted the operator's media.
- Convergence: it proposes the BIOS operation.

### Stale reads and conflicts

The model keeps the current world and the world one turn before. A
controller pass reads each object kind (claim, host, operations, node) from
either one, chosen per kind, because Kubernetes informers are per kind. The
model explores every choice. Writes go to the current world. An update made
from a stale read is a conflict and changes nothing, as on a real API server.
Reads that the real code makes through the uncached API reader always see
the current world: the hosts a deleted claim holds, the check that no boot
ran before a release unbinds a machine as it is, and the queue check before
an operation starts.

### Tied to the real code

The model does not copy decision logic. It builds real API objects from the
model state and calls the same helpers the controllers call: `opFinished`,
`opAwaitsApproval`, `opActive`, `opSucceeded`, `approvalRequired`,
`hostHolder`, `bootRan`, `handedOff`, `powerOffDecided`, `approvalWaived`,
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
8. A release of a machine that runs a node waits for a person, unless the
   claim waived approval.
9. The host phase, derived by `internal/phase` from conditions, follows the
   bind and the claim's deletion.

### Bounds

| Bound | Value |
| --- | --- |
| Hosts, claims | 1 each |
| Operations | boot, release, one BIOS proposal |
| Turns | 14 by default, 10 with `go test -short` |
| Person actions | one hold, one maintenance, one retry, one refused step, one BIOS proposal; a hold and a maintenance can also be lifted |
| Claims at start | one with approval required, one with approval waived |
| Cache lag | one turn, per object kind |

At 14 turns the model visits about 430,000 states in about 10 seconds. Set
`PK_MODEL_DEPTH` to explore further, for example:

```sh
cd metal
PK_MODEL_DEPTH=16 go test -count=1 -run TestProtocolModel -v ./internal/controller
```

Depth 16 visits about 870,000 states in about 25 seconds. Memory and time
grow fast with each extra turn.

### Known violations

Some invariants fail today for reasons that are known and need a larger
change. The test reports these as skips with the reason and a
counterexample. A skip is used only when the counterexample contains the
known cause, for example a turn labelled `person retries boot`. Any other
violation of the same invariant fails the test, so a known defect cannot
hide a new one.

- `pk-metal retry` deletes a failed boot, and the claim then makes a new
  one. If the claim is deleted before the new boot starts, nothing records
  that the failed boot touched the machine: the power-off decision on the
  host is written only by a release. The release then unbinds the machine
  without powering it off, possibly with the failed boot's media still
  mounted. This breaks invariants 4 and 5. A fix needs a record that a boot
  started which outlives the operation.
- A hold set within one cache lag of the claim's deletion is not seen by the
  pass that creates a release that needs no approval. This breaks
  invariant 7.

When a fix lands, remove the matching `known` text from the invariant. The
test then fails if the defect comes back.

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
