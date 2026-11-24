# Move the operator to another cluster (pivot)

The pk-metal operator runs in a Kubernetes cluster. At a new site there is
no cluster yet, so the operator starts in a small bootstrap cluster, usually
kind on a workstation. From there it installs the first Talos cluster. Then
`pk-metal pivot` moves the operator onto that Talos cluster. From its new
place the operator manages the same machines and installs further clusters.
The workstation is no longer needed.

This is the same idea as `clusterctl move` in Cluster API. This page says
what moves, in which order, what the move checks first, how to go back, and
what the new cluster must be able to reach. The [virtual lab](../../lab/README.md)
runs the whole round (`mise run lab:pivot`).

## The short version

```sh
# On the workstation, with the kubeconfig of the bootstrap cluster active:
pk-metal pivot --to mgmt.kubeconfig
```

- The move changes no machine. No BMC is called, nothing is powered,
  nothing is installed again.
- The operator in the bootstrap cluster is stopped (zero replicas), not
  deleted. Its objects stay there.
- The new operator must not propose any operation after the move. If it
  does, the move fails and says which operation it was.
- Run the command again after a failure. It goes on from where it stopped.

## Before you move

The move refuses to start when one of these is true. Each message says what
to do.

- **An operation runs.** An operation that has started (it has a baseline)
  and has not finished would be cut off halfway. Wait until it has finished:
  `pk-metal status`.
- **An operation waits for approval.** Approve it and let it finish, or
  delete it. With `--force-waiting` it moves as it is and waits on the new
  cluster instead.
- **An object is being deleted.** A claim that is being deleted has a release
  to finish (power off, media out). Let the operator in the bootstrap cluster
  finish it first.
- **The target already runs a pk-metal operator.** Two operators would act on
  the same machines. Scale it to zero first, or move from that cluster.
- **The target holds pk-metal objects of another fleet.** A move does not
  merge two fleets. Objects that an earlier move from this cluster left
  there are allowed; see "Move back" below.

An operation that is approved and has not started yet does not stop the move.
It starts on the new cluster.

## What must be reachable from the new cluster

The move cannot test this: the BMC network is usually not reachable from the
workstation. `pk-metal pivot` prints the list for your site before it asks.
Check it with your network team before you answer yes.

| From | To | Why |
| --- | --- | --- |
| Every node of the new cluster | Every BMC, HTTPS (port 443, or the port in the endpoint address) | The operator reads and controls the machines over Redfish. |
| Every node of the new cluster | Every installed machine, port 50000 (Talos API) | The operator checks that a node came up and reads its state. |
| Every BMC | The new cluster's nodes, NodePort 30443 | The BMCs download the boot media (installer and node configuration) from the operator's media service. |
| Every node of the new cluster | The Talos image factory (`factory.talos.dev`, or your mirror) | The operator registers the installer images it needs. |
| Every node of the new cluster | The registry the operator image comes from | The new cluster pulls the operator image. Give it with `--image` if the bootstrap cluster used a local image. |

The BMCs fetch media from the address in the manager's `--media-url`. In the
bootstrap cluster this is usually the workstation's address. Give the new
address with `--media-url https://<node address>:30443`. Use a node address,
not the control-plane VIP: kube-proxy answers a NodePort on the nodes' own
addresses, and the BMCs are refused on the VIP. For a site that must survive
the loss of that node, put a load balancer address in front of the NodePort and
give that. If you do not give `--media-url`, the move keeps the old one and
warns you.

## What moves, in which order

First the move installs the operator on the target:

1. The CRDs of `metal.pk.io` and `os.pk.io`, and it waits until they are
   served.
2. The admission policies that guard these kinds, their bindings and their
   parameters (the ConfigMap `pk-metal/hostoperation-approval`, which names
   the operator and the admin group).
3. The cluster roles and bindings whose names start with `pk-metal`.
4. The namespace `pk-metal`, its service account, roles and role bindings,
   its Services, and the manager's Deployment at zero replicas.

Then it copies the operator's state. Each object keeps its name, labels,
annotations, finalizers and status:

1. ConfigMaps and Secrets in the namespace `pk-metal`: the ledger, the BMC
   credentials (rotated passwords too), the Talos secrets bundles, the
   rendered configurations, the published kubeconfigs and talosconfigs.
2. Assets, ManagementEndpoints, EndpointProfiles, BiosProfiles,
   FirmwareProfiles, BiosSchemas and BiosSettings.
3. Hosts, with their claim reference.
4. HostClaims, in their own namespaces, and HostOperations, finished ones too.
   The finished ones are the record of what was done to each machine.
5. TalosClusters, TalosNodes and BootAssets.

What does not move: Secrets and ConfigMaps outside the namespace `pk-metal`,
Events, the leader lease, and service account tokens. Nothing of the
workload clusters the operator installed moves either: they run on their own
machines and do not depend on the operator.

### How the copy keeps the objects right

- **UIDs.** Every object gets a new UID on the target. A Host names its claim
  by UID (`spec.claimRef.uid`), and operations carry the claim's UID in the
  label `metal.pk.io/claim-uid`. The move rewrites each of these to the
  claim's new UID. It also rewrites owner references (a Host is owned by its
  ManagementEndpoint, a rendered configuration by its TalosNode). An owner
  that is copied later is filled in on a second pass. A reference to a UID
  the target does not know is never written: the garbage collector would
  delete the object.
- **Identities.** The admission policies check who writes a claim or an
  operation. The move creates claims as a member of the admin group, each
  operation as the user who proposed it, and replays each approval as the
  person who gave it. The records on the target name the same people.
- **Status.** The status of each object is copied too. The claim's hand-off
  record is what tells the operator that a machine was installed and must not
  be installed again. Host and endpoint status have several writers; the move
  writes each part under the name of the controller that owns it, so the
  controllers find the field ownership they expect.

## After the copy

The move starts the manager on the target with the replica count the source
had, and waits until it runs and holds its leader lease. Then it watches it
for the settle time (`--settle`, 90 seconds by default):

- Any HostOperation that the copy did not bring is a failure. The move never
  causes an install, a power action or a BIOS change.
- Every claim that was handed off must still read `HandedOff`.
- Every Host must show that the new manager has looked at it.

The last check is weak: it shows that the manager made a pass over each Host,
not that it agrees with everything. Watch `pk-metal status` for a while after
the move.

The move prints what to do next. The bootstrap cluster is stopped and keeps
its objects. When you no longer need it, delete it: for kind, `kind delete
cluster --name <name>`. Do not delete only the pk-metal objects in it: their
finalizers wait for an operator that no longer runs there, and the delete
hangs.

## Rerun and resume

Every step can run again. The markers that make this work are annotations on
the `pk-metal` namespace:

| Annotation | On | Says |
| --- | --- | --- |
| `metal.pk.io/pivoted-to` | the source | the operator moved away, to the cluster with this ID (kube-system UID) |
| `metal.pk.io/pivot-replicas` | the source | the replica count before the pause |
| `metal.pk.io/pivot-source` | the target | the operator was moved here from the cluster with this ID |

- Broken off before the new manager started: run the move again. The copy
  brings each object up to the source's version and does not create it twice.
- Broken off while it watched the new manager: run the move again. It sees
  the manager it started and only checks again.

## Move back (roll back)

**Before the new manager started.** Give the source its manager back:

```sh
kubectl -n pk-metal scale deployment pk-metal-manager --replicas=1
```

The objects on the target are a copy nobody acts on. Remove them, or leave
them for the next move: a move from the same source removes what the source
no longer has.

**After the new manager started.** Move in the other direction:

```sh
pk-metal --kubeconfig mgmt.kubeconfig pivot --to bootstrap.kubeconfig
```

The source of the first move is the target now. It holds the old copy, and
its `pivoted-to` marker names the cluster you move from, so the move accepts
it: objects that no longer exist on the new source are removed, the others
are updated. Never start both managers at the same time.

## Self-protection: the operator does not reboot its own machines

After the move, the operator runs on machines it manages. Rebooting the
machine it runs on, or a control plane of its own cluster, could stop it in
the middle of an operation. So the operator does not propose or run such a
step by itself:

- A power cycle or power off of the node the manager's pod runs on.
- A power cycle or power off of a running control plane of the operator's
  own cluster.

This covers the boot of an install, the power off of a release, a BIOS
change, and the executor itself. The claim or operation shows the reason
`SelfDisruption`, and the message says why and what to do.

The safe way is to move the operator to another cluster first. When you
accept the risk for one machine, annotate its Host:

```sh
kubectl annotate host <name> metal.pk.io/allow-self-disruption=true
```

For a control plane the annotation is not enough on its own. The operator
also waits until no other control plane of its cluster is being rebooted,
and until etcd keeps a majority without this one. Remove the annotation
afterwards: `kubectl annotate host <name> metal.pk.io/allow-self-disruption-`.

The operator finds its own node by the name in the environment variable
`NODE_NAME` (set from the pod's `spec.nodeName`) and matches it to a TalosNode
by name or by address. In kind, the manager's node is not a machine the
operator manages, so nothing is held there.

## Reference

```text
pk-metal pivot --to <kubeconfig> [--to-context name] [--image ref]
               [--media-url url] [--force-waiting] [--settle 90s]
               [--timeout 5m] [--yes]
```

- `--to`: the kubeconfig of the cluster to move to. The source is the current
  kubeconfig and context (`--kubeconfig`, `--context`).
- `--image`: the manager image on the target. Default: the source's image.
- `--media-url`: where the BMCs fetch media from on the target. Default: the
  source's value.
- `--force-waiting`: move operations that wait for approval.
- `--settle`: how long to watch the new manager.
- `--timeout`: how long to wait for each step.
- `--yes`: do not ask. Without a terminal, the move refuses to start
  without it.
