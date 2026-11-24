# Day-2 steps: what each one does to a running node

One of the [day-2 pages](day2.md).

A HostOperation is a list of steps that one host runs in order, after another
person has approved them. The [concepts page](concepts-for-sysadmins.md)
covers the install steps, which go through the BMC (power, virtual media,
the boot override). This page covers the steps for a node that already runs
Talos: they go through the node's Talos API and its cluster's Kubernetes
API, and **never through the BMC**. An operation made only of these steps
runs while the BMC is down or unreachable.

The field reference is the [API reference](api.md). `pk-metal approve` prints
each step in plain words before you say yes.

## Rules every step follows

- **It checks first, then acts.** Each step looks at the node before it
  acts. A step whose goal already holds is done without acting: an upgrade
  to the version the node already runs, an etcd member that already left.
- **A node that does not answer is a wait, not a failure.** That is what a
  reboot looks like. Each step has a time budget; a wait that runs past it
  fails the step, and the operation stops there.
- **A node that answers and refuses fails the step.** For example, Talos
  refuses an upgrade while etcd is unhealthy. A failed operation stays for
  you to read. Run it again with `pk-metal retry`.
- **A restart is proven by the boot time.** Before a step asks the node to
  restart, it records the node's boot time. The restart counts once the node
  reports a later boot time. That works for a kexec, a warm reboot or a cold
  power cycle, however short the outage. If the operator restarts between
  sending the request and saving it, the later boot time tells it the
  request was already sent, so it is not sent twice.
- **The node is fixed when the operation is proposed.** Every step names its
  TalosNode, and that node must be the one the host runs. An operation on
  one host never acts on another.

## The steps

| Step | What it does | Done when | Budget |
| --- | --- | --- | --- |
| `cordon` | Marks the Kubernetes Node unschedulable. Running pods stay. | The Node is unschedulable. | none |
| `drain` | Cordons the node, then evicts its pods through the Eviction API, so every PodDisruptionBudget holds. DaemonSet pods and static (mirror) pods stay. A pod with an emptyDir volume fails the step unless `deleteEmptyDirData` is set, because that data would be lost. Finished pods do not count. | No pod is left that must leave. | `timeoutSeconds`, default 15m. When it runs out, the step fails and **the node stays cordoned**. |
| `uncordon` | Makes the Node schedulable again. | The Node is schedulable. | none |
| `talosReboot` | Asks Talos to reboot. Services stop and filesystems unmount first. Mode `PowerCycle` makes it a cold restart. | The boot time moved on, and the node runs and is ready. | 20m |
| `talosShutdown` | Asks Talos to power off. `force` shuts down even if the node cannot cordon and drain itself. | The node no longer answers. It stays off until it is powered on (BMC, or a `powerCycle` step). | 20m |
| `talosUpgrade` | Installs the image pinned by digest, then the node reboots into it. `stage` installs at the next reboot instead. `force` skips Talos's own etcd health check. | The node restarted and runs the named version. If it restarted on another version, Talos fell back to the old image: the step fails with "did not take". | 45m |
| `talosApplyConfig` | Sends the machine config from the named Secret (key `config.yaml`). The Secret's content must hash to the digest pinned at proposal, or nothing is sent. Modes: `NoReboot` (default; fails if the change needs a reboot), `Reboot`, `Auto` (the node decides), `Staged` (applied at the next reboot), `Try` (rolled back after `tryTimeoutSeconds` unless confirmed). | The node accepted the config. In mode `Reboot`, or `Auto` when the node chose to reboot: the boot time moved on and the node is ready. | 20m |
| `etcdLeave` | Asks the node's own etcd member to leave. Leadership is handed to another member first. Refused for a worker, and for the only control plane of a cluster. | Another control plane no longer lists the member. | none |
| `etcdRemoveMember` | Removes the member with the ID the approver saw, through another control plane. This is the step for a node that cannot answer. If a peer is down, the next peer is asked; a peer whose etcd answers and refuses fails the step. | The member is no longer listed. | none |
| `talosReset` | Wipes the node through Talos. `graceful` (default) leaves etcd and drains first. `mode` and `systemPartitions` choose what is wiped. `reboot` boots into maintenance mode afterwards; otherwise the node stays off. A node that does not answer cannot be reset this way: install it again instead. | The node no longer opens with the cluster's credentials, or it restarted. | 20m |
| `waitNodeReady` | Waits only, changes nothing. | Talos reports stage `running` and ready, on `version` when one is named, **and** the Kubernetes Node is Ready. | `timeoutSeconds`, default 30m, counted from the end of the previous step |
| `waitEtcdHealthy` | Waits only, changes nothing. | Every etcd member answers and reports no error, none is a learner, every member knows a leader, no alarm is raised, and for a control plane its own member is listed. | `timeoutSeconds`, default 15m, counted from the end of the previous step |

The Kubernetes steps (`cordon`, `drain`, `uncordon`, `waitNodeReady`) use the
admin kubeconfig that the TalosCluster publishes
(`status.kubeconfigSecretRef`). A cluster that has not published one yet
cannot be drained.

## One control plane at a time

etcd keeps running only while a majority of its members are up. When three
operations on three control planes are approved at once, the operator still
takes down only one at a time, in every cluster it built.

The steps that take a node down are a power cycle, a power off,
`talosReboot`, `talosShutdown`, `talosUpgrade`, `talosReset`, `etcdLeave`, and
`talosApplyConfig` in mode `Reboot` or `Auto`. Before such a step runs on a
control plane, the operator checks two things:

1. **No other control plane of the same cluster is in the middle of such an
   operation.** It counts operations that started and have not finished.
2. **etcd is fully healthy without this node.** Every other member must
   answer, report no error, know a leader and not be a learner. No alarm may
   be raised. The members that are left must still be a majority. The
   operator counts from etcd's own member list, not from the TalosNodes: a
   member left behind by a wiped node still counts against the majority.

While either check fails, the operation waits. `pk-metal status` shows it
blocked on `ClusterDisruption`, with the reason. Nothing has happened to the
machine. The check runs again before every step that takes the node down,
not only at the start. A step that has already acted is always finished
first.

The guard does not hold workers, control planes that are not etcd members
(still joining, or already removed), or a cluster that was never
bootstrapped. On day zero every control plane is installed at once.

**Override.** To repair an etcd that is already broken, or for a cluster
with a single control plane, set the annotation
`metal.pk.io/override-cluster-guard` on the operation, with the reason as the
value. It lifts check 2 only. Check 1 stays, and so does the protection of
the operator's own nodes.

## Abort

`pk-metal abort <operation>` sets the annotation `metal.pk.io/abort` with your
user name and reason. You can also set it with kubectl.

- An operation that has not started ends at once. Nothing was done to the
  machine.
- A running operation stops before its next step. A step that already acted
  is finished first: a requested reboot is waited out, and a power cycle that
  saw the machine off turns it on again.
- Then the operator undoes what the operation left behind. It ejects the
  virtual media the operation mounted. It clears the boot override the
  operation set. It makes the Kubernetes Node schedulable again if this
  operation cordoned it; a node someone had cordoned before stays cordoned.
- What cannot be undone stays undone: a reboot, an applied config, an
  upgrade, a member that left etcd.

The operation ends as **Aborted**. Its last step lists what was undone and
what could not be. To run it again: `pk-metal retry`. Deleting an operation
also stops it, but undoes nothing.

## Rebooting a node: `pk-metal reboot`

```sh
pk-metal reboot w-1
pk-metal reboot --drain-timeout 30m --grace-period 60 cp-2
pk-metal reboot --mode powercycle w-1     # a cold start, for firmware
```

This proposes one operation for the host that runs the node:

```text
cordon → drain → talosReboot → waitNodeReady → uncordon
```

For a control plane, `waitEtcdHealthy` comes before `uncordon`. A kubelet can
be Ready while its etcd member is still catching up, and the next control
plane must not go down before then.

Another person approves it with `pk-metal approve`. To reboot a whole
cluster, propose one reboot per node and approve them all. Workers reboot in
parallel; control planes go one at a time, as described above. A drain held
up by a disruption budget waits until `--drain-timeout`, then fails and
leaves the node cordoned. Fix the budget or move the pods, then
`pk-metal retry`, or give the node back with `kubectl uncordon`.
