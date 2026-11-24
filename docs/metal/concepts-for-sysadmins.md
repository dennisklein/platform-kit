# Concepts for system administrators

This page is for people who install bare metal with PXE, kickstart or preseed,
run Foreman, Cobbler or xCAT, write Ansible or Chef, and use `ipmitool` and
BMC web consoles every day, and who are new to Kubernetes. It maps what you
know to what pk-metal does, says where each piece of state lives, and lists
every step that changes a machine.

The reference for every field is the generated [API reference](api.md). The
[virtual lab](../../lab/README.md) runs everything on this page on one
workstation.

## The short version

- pk-metal is a Kubernetes operator: a program that runs in a small
  management cluster, reads objects you write (YAML), and works to make the
  machines match them. It talks to BMCs over Redfish (HTTPS). It does not use
  PXE, TFTP or DHCP for the install: it mounts the installer and the node's
  configuration as virtual media and boots from them once.
- The operating system it installs is Talos Linux: an immutable OS with no
  shell, no ssh and no package manager, managed through an API (`talosctl`).
- Nothing disruptive happens to a machine before a person approves it. The
  operator proposes, a person approves, the operator executes.

## Translation table

| You know | Here |
| --- | --- |
| Inventory row: Ansible inventory, xCAT node table, CMDB entry | **Asset**: serial, BMC MAC, your asset tag (`cid`), BMC credentials |
| dhcpd or dnsmasq reservations for the BMCs | **The ledger**: BMC MAC to `https://<address>`, the ConfigMap `pk-metal-ledger` in namespace `pk-metal` (the manager's `--ledger` file) |
| BMC credentials in a vault or a password file | A Secret in namespace `pk-metal`, named by the Asset's `credentialsRef` |
| What you found when you ran `ipmitool fru` / `lshw` | **ManagementEndpoint** (one BMC) and **Host** (one machine), written by the operator |
| Kickstart or preseed file for one host | **TalosNode**: address, install disk, NICs, role |
| `%include`, group_vars, the shared part of the kickstart | **TalosCluster**: versions, cluster endpoint, kernel arguments, Talos config patches |
| Foreman or Cobbler "Build" flag, `nodeset <node> osimage=…` | **HostClaim** with `spec.os` naming the TalosNode |
| `ipmitool chassis bootdev cdrom` + `power cycle` + the PXE/TFTP server | One **HostOperation** (virtual media; there is no PXE) |
| Change ticket approval | `pk-metal approve` on the HostOperation |
| Foreman or xCAT node state | The Host's `phase` and its conditions (see below) |
| SOL, iKVM, serial console | The BMC's serial-over-LAN, as before; in the lab `.lab/state/<machine>/console.log` |
| `ssh root@node` | `talosctl -n <address> …` (there is no ssh) |
| `yum install` / `apt install` on the node | System extensions in the TalosCluster's `schematic`, baked into the image |
| Re-running the playbook | Edit the TalosNode or TalosCluster; the operator renders the configuration anew |

## Where the state lives

All state is in the management cluster, as API objects. There is no other
database and no file on a server that matters. Two kinds of facts never mix:

- **What you assert**, in objects you write and keep in git: Asset, the
  credentials Secret, TalosCluster, TalosNode, HostClaim, the profiles.
- **What the operator found or did**, in objects or fields it writes:
  ManagementEndpoint, Host, HostOperation, and the `status` of everything.

Where the two disagree (a serial on the Asset that is not the serial the BMC
reports), the operator stops and says so. It does not guess which one is
right.

Commands to look at it, with the management cluster's kubeconfig:

```sh
kubectl get metal                       # every pk-metal object, all kinds
pk-metal status                         # the rack: hosts, operations, endpoints, claims
pk-metal top                            # the same, live, one tile per machine (q quits)
kubectl describe host pklab0001         # conditions: why a machine waits
kubectl get hostoperations -n pk-metal  # what was proposed, approved, done
kubectl get events -n pk-metal --sort-by=.lastTimestamp
```

Read a machine's state from its **conditions**, not only from its phase. Each
condition has a status, a reason and a message in plain words. The phase is a
summary for the table: Enrolling and Inspecting (the operator is still finding
out what the machine is), Available (understood and free), Assigned (a claim
holds it, nothing runs), Provisioning (an operation works on it now),
Provisioned (the install finished and the node checked in), Releasing, and
Failed. `pk-metal status` shows the reason a machine waits in its BLOCKED
column.

## Which name is which

One machine has several names, because each object has its own job. For the
first lab machine:

| Object | Name | Where it comes from |
| --- | --- | --- |
| Asset | `node-1` | You chose it |
| ManagementEndpoint | `node-1` | Same as the Asset |
| Host | `pklab0001` | The machine's serial, lower-cased |
| TalosNode | `node-1` | You chose it; it is also the hostname |
| HostClaim | `default/node-1` | You chose it; claims live in a namespace (here `default`) |
| Boot operation | `pk-metal/provision-pklab0001-<hash>` | The operator; operations live in `pk-metal` |
| Power-off on release | `pk-metal/release-pklab0001-<id>` | The operator |

Assets, Hosts, ManagementEndpoints, TalosClusters and TalosNodes have no
namespace. HostClaims are in the namespace you create them in. HostOperations
are in the operator's namespace, `pk-metal`; give `-n pk-metal` to
`kubectl` and `pk-metal approve`.

## From rack to running node

1. **Rack and cable the machine.** Give the BMC an address (your DHCP
   reservation or a static one). Add one line to the ledger: the BMC's MAC
   and `https://<its address>`. The manager reads the ledger when it starts;
   applying a changed ConfigMap through kustomize rolls it.
2. **Write the Asset**: the serial, the BMC MAC (lower case, colon
   separated), your asset tag, and the name of the credentials Secret.
3. The operator finds the BMC through the ledger, logs in, and creates the
   **ManagementEndpoint** and the **Host**. It checks the serial the machine
   reports against the Asset. A mismatch stops everything for that machine.
4. **Write the TalosCluster and a TalosNode.** The operator renders the
   node's Talos configuration from the two (`kubectl get talosnodes` shows
   when it is rendered).
5. **Write a HostClaim** that names the Asset and, under `spec.os`, the
   TalosNode. This is the install order. The operator proposes a boot
   **HostOperation** and waits.
6. **A person approves** (next section). The operator ejects the virtual
   media, mounts the Talos installer and the node's configuration, sets a
   one-time boot from CD, power-cycles the machine and waits for the node to
   check in over the Talos API.
7. The node installs itself to disk, reboots from disk and joins the
   cluster. The Host becomes Provisioned.

## BMCs with one virtual media slot

Step 6 needs two virtual media: the installer and the configuration. Some
BMCs hold one medium only (a Huawei iBMC, for example). For a blank machine
behind such a BMC, the operator builds an installer for that one node, with
its configuration inside, and mounts it in the one slot. It is like a
kickstart file baked into a per-host boot ISO, instead of one served over
the network.

Why not boot the plain installer and send the configuration over the
network? A Talos machine without a configuration (maintenance mode) accepts a
configuration from anybody who reaches it, and the configuration holds the
cluster's keys and tokens. The operator does not do that. The embedded
installer travels the same way as the configuration ISO: the BMC fetches it
from the operator's media service with a one-time grant, and the machine
needs no network path to the operator.

How it works:

1. The operator creates a **BootAsset** `<node>-embedded` for the TalosNode
   (`kubectl get bootassets`).
2. It runs the **Talos imager** as a Job in `pk-metal`. The Job reads the
   node's configuration from its Secret as a file. The configuration is never
   in the Job's arguments or log. The Job runs as an ordinary user, with no
   privileges and no loop devices.
3. The ISO (about 370 MB) goes to the **media store**, a volume of the
   operator. The BootAsset records its digest, and the media service checks
   the digest each time it hands the file to a BMC.
4. The approval plan says that the installer carries the configuration of
   that node. After approval the operator mounts it, boots from CD,
   power-cycles the machine, waits for the node to check in and ejects the
   medium.

A new configuration, Talos version or schematic gives a new build; the old
ISO is deleted when the new one is ready, and all of them go with the
TalosNode. A machine that already runs Talos from disk does not need this: its
configuration goes in the one slot as a small ISO, as before. Talos 1.12 or
later is needed to read an embedded configuration.

What it means for secrets on disk:

- **The ISO holds the node's configuration in plain text**, the same
  content as the node's Secret. It is a file on the media store volume, owned
  by the operator's user and readable by it only (mode 0400). It is not
  encrypted.
- Anybody who can read that volume, or the disk under it, can read the
  configuration. Treat the volume like the cluster's Secret storage: if you
  encrypt Secrets at rest, use an encrypted StorageClass for the media store.
- Backups of the volume carry the configuration too. You do not need to back
  it up: the operator rebuilds what is missing.

To turn it on at a site:

- Add the **media-store** kustomize component
  (`metal/config/media-store`) to your overlay. It adds a 10 GiB volume
  claim and the manager flags `--media-store` and `--media-store-claim`.
  Without it, a blank machine behind a one-slot BMC waits, and `pk-metal status`
  says why.
- The management cluster pulls the imager image from
  `ghcr.io/siderolabs/imager`, pinned by digest. The operator knows the digest
  of the Talos release it renders for. For another release, give the manager
  `--imager-digest=<version>=sha256:<digest>`; the BootAsset names the flag
  when a digest is missing.
- A failed build stops at the BootAsset (`kubectl describe bootasset
  <node>-embedded`). Its message gives the commands to read the build log and
  to try again.

## Approval, and how to say no

Every boot and every BIOS or firmware change is a HostOperation that waits
for a person. To see what is waiting and approve it:

```sh
pk-metal status                                          # OPERATION ... AwaitingApproval
kubectl get hostoperation -n pk-metal <name> -o yaml     # the steps it will run
pk-metal approve -n pk-metal <name>
```

`pk-metal approve` approves the operation whose plan it printed, and nothing
else. After you answer y it reads the operation again: if it was deleted and
created again under the same name, or its plan reads differently now, nothing
is approved and you are asked to look again. The approval itself names the
object it was read from (uid and resourceVersion), so the API server refuses
it if the operation changed in the moment between. The question waits five
minutes; with no answer by then nothing is approved. `pk-metal top` follows
the same rules when you press y on a plan.

The rules, enforced by admission policies in the API server
(`metal/config/policy`):

- You approve as yourself, and you cannot approve your own proposal.
- Permission to approve is permission to patch HostOperations: bind the
  ClusterRole `pk-metal-approver` to the people who may do it.
- Only members of the admin group (`pk-metal:admins` by default) may give a
  claim an OS (`spec.os`) or set its approval to Automatic. Being
  cluster-admin is not enough: the policy asks for the group by name. Bind
  your admins to that group, with a ClusterRoleBinding or a group from your
  identity provider.

To say **no** to a proposed install:

- **Remove `spec.os` from the claim.** The operator deletes the boot
  operations of that claim that have not finished (a running one stops
  between two steps), and proposes nothing until `spec.os` comes back. The claim keeps the machine reserved.
- **Or hold the claim**: `pk-metal hold default/node-1`. It sets the
  annotation `metal.pk.io/hold` on the claim; while it is there, the
  operator proposes nothing for that claim. Remove the annotation to go on.

Deleting a proposed operation is not "no": the operator proposes the same
plan again on its next pass. Deleting a **failed** operation is how you retry
it.

## What changes a machine, and when

| You do | What happens to the machine |
| --- | --- |
| Create an Asset | Nothing. The operator logs in to the BMC and reads. |
| Create a claim without `spec.os` | Nothing. The machine is reserved (Assigned). |
| Create a claim with `spec.os`, or add it | A boot is proposed. After approval: power cycle, install, **the install disk is overwritten**. |
| Approve an operation | It runs, step by step. |
| Delete a claim | The node is drained, leaves the cluster and its disk is reset; the machine is **powered off**. See below. |
| Edit a TalosNode or TalosCluster | The configuration is rendered again. A running node does not pick it up by itself. |
| Use `ipmitool` or the BMC web UI | Allowed. See "Using the BMC directly". |

### The install disk

The TalosNode's `installDiskSelector` picks the disk Talos installs onto.
Every matcher you set must hold. **The first disk that matches is
repartitioned and overwritten**; the operator does not wipe it first, but
everything on it is gone after the install. Think of it as kickstart's
`ignoredisk --only-use=` plus `clearpart --all` on that one disk.

- Prefer `serial` or `wwid`: they name exactly one disk and survive
  re-cabling and enumeration order.
- A `model` pattern or a `match` expression (for example
  `disk.size > 100u * GB`) can match a data or scratch disk on a machine
  with several disks, and the first match wins.
- A TalosNode without any disk matcher is refused.

### Deleting a HostClaim

Deleting a claim releases the machine, and a released machine never keeps
its node. When the node ran there, or might have, the release is one
operation that waits for approval, like an install:

1. The node is cordoned and drained (`kubectl drain`, with a time limit).
2. A control plane leaves etcd: it hands etcd leadership on first, then
   leaves itself. The operator refuses a removal that would leave etcd
   without a majority of healthy members, or with an even number of members
   (two members tolerate no more failures than one), unless the claim carries
   the annotation `metal.pk.io/ack-etcd-risk` with your reason. The etcd
   leader leaves last.
3. Talos resets the node: its system disk is wiped, its credentials with it.
4. The Kubernetes Node object is deleted.
5. The machine is powered off and its virtual media ejected; the Host goes
   back to the pool.

"Ran there" is read from the records, not from whether the node answers at
that moment: the claim's hand-off record names the machine, or a boot of the
claim reached its check-in step. A machine no boot ever got that far is
powered off without asking.

When the node does not answer, steps 1 and 3 cannot run. A control plane's
etcd member is then removed by its ID from the members that remain, the Node
object is deleted, and the machine is powered off through its BMC. Its disk
still holds the node and that cluster's credentials, so the Host is marked:
condition `DiskWiped=False`, reason `InstalledNodeRemains`. The operator
installs nothing else on a marked Host until it is wiped:

```sh
pk-metal wipe <host>      # proposes the wipe; approve it as usual
```

The wipe boots a Talos image that erases the system disk and leaves the
machine in maintenance mode, with nothing on its disk.

Deleting a claim is not "remove from the inventory". `kubectl delete -f
<dir>`, or a GitOps tool that prunes a deleted file, does it too. To stop
managing a machine without touching it, leave the claim in place. To add,
remove, replace or reinstall nodes, see [Scaling](scaling.md).

### The hand-off record is the record of an install

When a node first checks in after its install, the operator writes the
claim's hand-off record (`status.handOff`: machine, node, time, and the boot
operation). From then on the claim is "installed" because of that record,
not because of the operation: deleting succeeded operations when you tidy up
`kubectl get hostoperations -n pk-metal` changes nothing, and a node that is
down (powered off for maintenance, for example) is not installed again.

Only three things clear or move the record, and each drains the node and
resets its disk first: `pk-metal reinstall <node>` installs it again on the
same machine, `pk-metal replace <node> --host <other>` moves it to another
machine, and deleting the TalosNode (or the claim) removes it. See
[Scaling](scaling.md).

### Using the BMC directly

Using `ipmitool`, Redfish or the BMC web UI on a managed machine is allowed;
the operator does not fight you. It notices that the machine was reset with
no operation running and sets the Host condition `Drifted=True`, reason
`UnexpectedReset`. That is information, not an error: nothing is blocked by
it. The condition clears (to `Drifted=False`, `ResetExplained`) the next time
a reset is covered by an operation, for example the next boot or power
operation the operator runs.

## Getting onto a node

Talos has no ssh and no shell. The Talos API replaces them; `talosctl` is the
client, and the talosconfig file (a client certificate for the cluster) is
its key. The operator publishes it as a Secret in `pk-metal`, named in the
TalosCluster's `status.talosconfigSecretRef`.

```sh
export TALOSCONFIG=$PWD/talosconfig
talosctl -n 10.77.0.12 dashboard        # like top, with the kernel log and service states
talosctl -n 10.77.0.12 dmesg
talosctl -n 10.77.0.12 services
talosctl -n 10.77.0.12 logs kubelet
talosctl -n 10.77.0.12 get addresses    # also: links, routes, disks
talosctl -n 10.77.0.12 read /proc/cpuinfo
talosctl -n 10.77.0.12 reboot

# A machine in maintenance mode (booted, no configuration yet):
talosctl -n 10.77.0.12 --insecure get disks
```

The console is still the serial console. Use the BMC's serial-over-LAN
(`ipmitool -I lanplus -H <bmc> -U <user> sol activate`) or its web console.
The kernel argument `console=` in the TalosCluster's schematic must name the
port your BMC's SOL redirects: `ttyS0` under QEMU, often `ttyS1,115200n8` on
servers (many Supermicro boards, for example). With the wrong port, the
install runs but the console stays dark.

## Network checklist for a real site

- The **operator** (the management cluster) reaches every BMC on TCP 443.
- Every **BMC** reaches the operator's media service at the manager's
  `--media-url` (a NodePort or a load balancer, HTTPS). BMCs pull the
  installer and the node configuration from there. They do not verify its
  certificate. Still, the certificate stays within its validity period, for
  firmware that checks it: one named with `--media-cert-secret` (for example
  issued by cert-manager) is read again every minute and a renewed one is
  served without a restart; without it the manager makes its own self-signed
  certificate and replaces it ten days before it expires.
- Every BMC also reaches the **Talos image factory** (`factory.talos.dev`, or
  your own, see "Offline") to fetch the installer ISO, unless you serve it
  from inside.
- The **operator** reaches the image factory (it registers image builds) and
  every node's Talos API on TCP 50000.
- With one-slot BMCs, the **management cluster** pulls the Talos imager from
  `ghcr.io`, and the imager fetches the system extensions of the schematic
  from there too.
- The **nodes** reach DNS, NTP, the image factory's registry (the installer
  image) and the container registries Talos and Kubernetes pull from:
  `ghcr.io`, `registry.k8s.io`, `gcr.io`, `docker.io`. `talosctl images
  default` lists the exact images. Set DNS and NTP with `ResolverConfig` and
  `TimeSyncConfig` patches on the TalosCluster; a proxy with an
  `EnvironmentConfig` patch (`https_proxy`, `no_proxy`).
- The nodes' **static addresses, gateway and VIP** are in the TalosNodes and
  the TalosCluster. There is no DHCP for the nodes unless a link sets
  `dhcp: true`.
- Ledger entries: one per BMC MAC. A BMC that is not in the ledger is never
  found, and its claim waits.

## Offline and air-gapped sites

Everything the lab and the defaults download comes from the internet. To run
without it, mirror:

- **The Talos image factory**: run your own (it is open source, same API) and
  point the manager at it with `--image-factory=<host>`. The installer ISOs
  and installer images are then named on your host.
- **The container images** Talos and Kubernetes pull: `talosctl images
  default` lists them (from `ghcr.io/siderolabs`, `registry.k8s.io`,
  `gcr.io` and others). Copy them into your registry and tell the nodes with
  one `RegistryMirrorConfig` patch per upstream registry:

  ```yaml
      - inline: |
          apiVersion: v1alpha1
          kind: RegistryMirrorConfig
          name: ghcr.io
          endpoints:
            - url: https://registry.example.org/v2/ghcr.io
              overridePath: true
  ```

- **DNS and NTP**: point the nodes at your own servers (`ResolverConfig`,
  `TimeSyncConfig` patches).
- **Firmware images** for FirmwareProfiles come from your OCI registry; give
  the manager its CA with `--registry-ca` if it uses an internal one.
- **The operator image** itself: build it (`mise run image`) and push it to
  your registry.
- **The Talos imager**, for one-slot BMCs: copy `ghcr.io/siderolabs/imager`
  at the digest you pin and give the manager `--imager-repository=<your
  copy>`.
