# Add, remove, replace and reinstall nodes

A Talos cluster's size is the set of its TalosNodes that hold a machine
through a HostClaim. This page says how to change that set without breaking
the cluster: which order is safe, what the operator refuses, and what it does
to a machine on the way out. Every change to a machine is still an operation
that waits for `pk-metal approve`.

New to the objects? Read [Concepts for system administrators](concepts-for-sysadmins.md)
first.

## The short version

```sh
pk-metal scale lab --control-planes 3 --workers 4          # print the plan
pk-metal scale lab --control-planes 3 --workers 4 --apply  # create and delete the claims
pk-metal approve                                           # one operation at a time
pk-metal replace node-2 --host spare-7                     # failed hardware
pk-metal reinstall node-2                                  # corrupted install, new disk
pk-metal wipe spare-7                                      # a machine that kept a node
```

## How the operator counts members

- A TalosNode is a **member** once a claim has given it a machine. A
  TalosNode written ahead, waiting for its machine, is not: it shows in the
  TalosCluster's `status.unassignedNodes` and does not hold the cluster's
  `Available` condition down. So writing the TalosNodes of a later scale-up
  in advance is fine.
- A TalosNode being deleted is not a member either.
- For etcd, the operator does not trust these counts: before a control plane
  joins or leaves it reads the etcd member list from the cluster itself
  (healthy members, learners, the leader).

## Adding a node

1. Write the TalosNode (role, install disk, network links), or have it
   written already.
2. Create a HostClaim with `spec.os.nodeRef` naming it, on a free machine:
   `Available`, unclaimed, and wiped (see "Machines that kept a node").
   `pk-metal scale` does this step for you and pairs each node with a free
   machine, preferring the machine whose Asset has the node's name.
3. Approve the boot. The machine installs and the node joins.

**Control planes join one at a time.** The operator holds a control plane's
boot (`blockedOn: EtcdJoinWaiting`) while another control plane of the same
cluster is still booting or leaving, while any etcd member is unhealthy or
still a learner, or while etcd still lists a member under the joining node's
name (a leftover of an earlier install: remove it first). A joining node
starts as a learner and is promoted once it has caught up; the next one
waits for that.

## Removing a node

Delete the node's HostClaim (or `pk-metal scale` to a smaller size). The
release is one operation; once approved it:

1. cordons and drains the node (pods are evicted; emptyDir data is lost;
   the drain gives up after 15 minutes),
2. for a control plane: hands etcd leadership to another member if it holds
   it, then leaves etcd,
3. resets the node with Talos, which wipes its system disk,
4. deletes the Kubernetes Node object,
5. powers the machine off and returns it to the pool.

To remove a node for good, delete its TalosNode too. Deleting a TalosNode
whose claim still holds a machine does the same steps on that machine
(the claim stays, without a node, until you delete it). The TalosNode itself
is kept until its machine is released and, for a control plane, its etcd
member is gone (condition `RemovalBlocked` says what it waits for).

### What the operator refuses

A control plane leaves etcd only when what remains is safe:

| After the removal | Without acknowledgement |
|---|---|
| no member left | refused |
| fewer healthy members than a majority of the remaining members | refused |
| an even number of members (2, 4) | refused: it tolerates no more failures than one fewer |
| the member is the etcd leader while other control planes leave too | waits: the leader leaves last |
| another control plane is joining or leaving | waits |
| the member list cannot be read | waits |

The claim shows `blockedOn: EtcdRiskRefused` or `RemovalWaiting` with the
reason. To accept the risk, annotate the claim (or the TalosNode, when you
delete the node) with your reason, and the operator goes ahead:

```sh
kubectl annotate hostclaim -n default node-3 metal.pk.io/ack-etcd-risk="scale down to one control plane"
```

`pk-metal scale`, `replace` and `reinstall` take `--ack-etcd-risk "<reason>"`
and set it for you.

### Scaling control planes down

Go 3 to 1, or 5 to 3, never through a removal that leaves etcd without a
majority. Removing two of three control planes passes through two members,
which needs the acknowledgement: for that moment a single failure stops the
cluster. The operator removes one control plane at a time, waits until etcd
is healthy again, and removes the leader last.

Prefer odd sizes: 1, 3 or 5. A cluster of 4 control planes survives one
failure, like one of 3, and needs one more machine for it.

## Machines that kept a node

A node that does not answer cannot be drained or reset. Its release then:

- removes a control plane's etcd member by its ID, through the members that
  remain,
- deletes the Kubernetes Node object,
- powers the machine off through its BMC,
- marks the Host: condition `DiskWiped=False`, reason `InstalledNodeRemains`,
  annotation `metal.pk.io/disk-not-wiped` naming the cluster and node.

Its disk still holds that node and the cluster's credentials. If it booted
from that disk it would come back as a node that no longer belongs. The
operator therefore installs nothing else on a marked Host and proposes no
other operation that powers it on. To clear the mark:

```sh
pk-metal wipe <host>
pk-metal approve
```

The wipe boots a Talos image of the cluster's version whose kernel command
line erases the system disk, and leaves the machine in maintenance mode
with nothing on its disk. When it succeeded the mark goes and the machine
is free.

If you know the disk holds nothing any more (it was replaced, or erased some
other way), remove the mark yourself; the operator takes your word for it:

```sh
kubectl annotate host <host> metal.pk.io/disk-not-wiped-
```

## Replacing a node

**Hardware that still works: add first, remove after.** Write a new
TalosNode for the new machine, add it, wait until it is Ready (and, for a
control plane, a voting etcd member), then remove the old one. etcd never
runs short, and no acknowledgement is needed.

**Hardware that failed: `pk-metal replace`.** The node keeps its name and
moves to another machine:

```sh
pk-metal replace node-2 --host spare-7 --ack-etcd-risk "node-2 PSU failed"
```

The claim is pointed at the new machine. The old one is released first, as
above: reset if it answers, otherwise powered off and marked for a wipe. Then
the node is installed on the new machine and joins again. A control plane of
a three-member cluster is at two members in between, hence the
acknowledgement.

A TalosNode that names its install disk by serial or its network link by MAC
address fits one machine only. Edit those to the new machine's values before
you run `replace`.

## Reinstalling a node

For a corrupted install, or after the install disk was swapped (edit the
TalosNode's `installDiskSelector` first):

```sh
pk-metal reinstall node-2
```

The operator records an install once, in the claim's hand-off record, and
never installs that machine again on its own: a node that is down is not
reinstalled. `reinstall` is the one way to ask for it. The request names the
install it replaces, so it is carried out once. The operation drains the
node, takes a control plane out of etcd, resets its system disk, boots the
installer and waits for the node to check in again. A check-in left over
from before the reinstall does not count.

## In the lab

On a lab installed at three control planes and two workers
(`PK_LAB_NODES=3cp+2w mise run lab:up && mise run lab:wait`),
`mise exec -- lab/lab.sh day2 scale` runs the whole round on its five
machines, approving as `lab:wait` does: control planes down to one and back
up to three, one at a time; a worker removed; a control plane replaced on
the freed machine; and back to one control plane. On every turn it checks
etcd (never two learners at once, never fewer voters than the step allows)
and that each removed machine came back reset.
