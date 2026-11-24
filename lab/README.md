# The virtual lab

Three virtual machines with simulated management controllers, and the operator
taking them from an inventory entry to a three-node Kubernetes cluster. The
machines are real UEFI machines under QEMU, the install is a real Talos install,
and the operator runs from the same image and manifests a real site would use.
Only the hardware is simulated. Three machines is the default; see "Size the
lab" for more.

New to Kubernetes, or coming from PXE, kickstart, Foreman or xCAT? Read
[Concepts for system administrators](../docs/metal/concepts-for-sysadmins.md)
first. It maps the objects here to the tools you know, says where each piece
of state lives, and lists what is destructive.

## Before you start: what changes a machine

- **A HostClaim with `spec.os` is an install order.** The operator proposes
  to boot that machine into the Talos installer. Once a person approves, the
  install disk is repartitioned and overwritten. The lab's
  `.lab/generated/manifests/30-claims.yaml` has one such claim per machine.
- **Nothing happens to a machine before somebody approves.** The approval is
  `pk-metal approve` (`mise run lab:approve` in the lab). To say no, remove
  `spec.os` from the claim, or hold it: `pk-metal hold default/node-1`.
- **Deleting a HostClaim powers its machine off** (a graceful shutdown first,
  then a hard power-off). For a machine that runs its node, that power-off
  waits for an approval too. `kubectl delete -f .lab/generated/manifests/`
  does this to every lab machine. It is not "remove from the inventory".

## What runs where

```
 workstation or CI runner
+------------------------------------------------------------------------+
|                                                                        |
|  kind cluster "pk-metal"                 pk-metal sim fleet            |
|  +---------------------------+          +----------------------------+ |
|  | pk-metal-manager          | Redfish  | BMC node-1  10.77.0.1:8441 | |
|  |  reconciles the API kinds | -------> | BMC node-2  10.77.0.1:8442 | |
|  |  serves boot media, 8443  | <------- | BMC node-3  10.77.0.1:8443 | |
|  +---------------------------+  media   +-------------+--------------+ |
|   NodePort 30443 on 10.77.0.1            power, media | boot order     |
|                                         +-------------v--------------+ |
|   bridge pkbr0, 10.77.0.1/24            | QEMU node-1  10.77.0.11    | |
|   taps pktap1-3 ----------------------> | QEMU node-2  10.77.0.12    | |
|   NAT to the outside                    | QEMU node-3  10.77.0.13    | |
|                                         | cluster VIP  10.77.0.10    | |
|                                         +----------------------------+ |
+------------------------------------------------------------------------+
```

- `lab/net.sh` makes the bridge, one tap per machine and the firewall rules. It
  is the only part that needs root, and `down` removes exactly what `up` made.
- `lab/kind.yaml` is the management cluster. The operator's image is built from
  the working tree and loaded into it; nothing is pulled from a registry
  (except the Talos imager, for a one-slot BMC: see "A one-slot BMC").
- `lab/lab.yaml` is the one file that describes the lab: the cluster name,
  the Talos and Kubernetes versions, the VIP, how many control planes and
  workers, their memory and CPUs, and per-machine overrides. `lab:up` renders
  everything else from it into `.lab/generated/`:
  - `fleet.yaml`, the machines the simulated BMCs serve;
  - `manifests/`, what a person would commit for a real site: the Assets and
    their BMC credentials, the TalosCluster with its nodes, and one HostClaim
    per machine;
  - `taps` for `lab/net.sh`, and `ledger.yaml` for the operator.

  Machine n is `node-<n>`, and everything that tells it apart comes from n:
  address `10.77.0.<10+n>`, BMC `https://10.77.0.1:<8440+n>`, tap `pktap<n>`,
  serial (`PKLAB0001` for node-1), MACs and disk serial. The files cannot
  disagree.
- `.lab/` (ignored by git) holds everything the lab writes: the rendered
  files, its own kubeconfig, the generated deployment, the Talos secrets
  bundle, disks, caches and logs. Your `~/.kube/config` is never touched.

## Prerequisites

Linux with KVM, cgroup v2, Docker Engine, QEMU with OVMF, `sudo`, about 12 GiB
of free memory (for the default three machines) and 15 GiB of free disk.
`mise install` brings go, kind, kubectl, talosctl, yq and jq at the versions
the repository pins.

```sh
# Debian, Ubuntu
sudo apt-get install qemu-system-x86 qemu-utils ovmf
# Fedora
sudo dnf install qemu-system-x86-core qemu-img edk2-ovmf
# RHEL 9, Rocky, Alma: QEMU comes as /usr/libexec/qemu-kvm, which the fleet
# finds on its own (PK_METAL_QEMU names any other binary)
sudo dnf install qemu-kvm qemu-img edk2-ovmf iptables-nft

mise install
mise run lab:doctor   # checks every item and prints the fix for each miss
```

`lab:doctor` changes nothing, but its sudo check may ask for your password.

**Container engine.** The lab is built and tested on Docker Engine, run as
root by the system's `docker` service. It does not work with:

- podman, including the `podman-docker` package that makes `docker` run
  podman (the RHEL default). Remove that package and install Docker Engine
  from <https://docs.docker.com/engine/install/>.
- Docker Desktop. Its engine runs in a VM, so the kind node cannot reach the
  lab bridge.
- rootless Docker. The kind node is then not on a host bridge, and Docker's
  rules are not in the host firewall that `lab/net.sh` writes to.

Doctor detects all three. Being in the `docker` group is as good as being
root on that machine; on a shared workstation, decide that on purpose.

## Commands

| Command | What it does |
| --- | --- |
| `mise run lab:doctor` | Checks the workstation, and that `lab/lab.yaml` renders and fits in memory. Changes nothing. |
| `mise run lab:render` | Renders `lab/lab.yaml` into `.lab/generated/`, to read before `lab:up`. |
| `mise run lab:up` | Render, network, kind cluster, operator, fleet, manifests. Safe to rerun. |
| `mise run lab:status` | The rack as the operator sees it, then every object. |
| `mise run lab:top` | Live view of every machine. Keys approve, hold and retry. |
| `mise run lab:approve` | Approves every operation that waits for a person. |
| `mise run lab:apply [path...]` | `kubectl apply` as a member of the admin group. Default: `.lab/generated/manifests/`. |
| `mise run lab:kubeconfig` | Writes `.lab/workload.kubeconfig` and `.lab/talosconfig`. |
| `mise run lab:load` | Rebuilds the operator image and rolls the manager. |
| `mise run lab:wait` | Unattended: approves and waits until every machine is a Ready node. |
| `mise run lab:release <namespace/name>` | Deletes a claim, approves the shutdown it proposes, and waits until its Host is Available. |
| `mise run lab:observe` | On a converged lab: checks the CLUSTER and NODE tables of `pk-metal status`, the operator's metrics, and a maintenance window that holds an approved operation until it opens (about three minutes). |
| `mise run lab:pivot` | Moves the operator from kind onto the first Talos cluster and installs a second one from there (see below). |
| `mise run lab:collect` | Writes logs and object dumps to `.lab/diagnostics`. |
| `mise run lab:down` | Stops everything and removes the network. Keeps disks and caches. |
| `mise run lab:reset` | Wipes the machines' disks. The lab must be down. |

The usual round:

```sh
mise run lab:up
mise run lab:status       # three operations wait in AwaitingApproval
mise run lab:approve      # the one human step: this installs three machines
mise run lab:status       # repeat until the three hosts are Provisioned
mise run lab:kubeconfig
kubectl --kubeconfig .lab/workload.kubeconfig get nodes
mise run lab:down
```

On a hosted CI runner (4 cores, 16 GB) `lab:up` takes about four minutes from
a cold start, and the three installs after the approval about three more. The
`lab-e2e` workflow in `.github/workflows` runs exactly this round, with
`lab:wait` in place of the manual steps. `lab:wait` approves everything that
is proposed, so use it only where that is what you want. Its progress lines
carry the date and the workstation's local time with its zone
(`YYYY-MM-DD HH:MM:SS ZONE`; CI runners use UTC).

The operator proposes each installation as a HostOperation and does not touch
a machine before somebody else approves it. `lab:approve` is that somebody: it
runs `pk-metal approve` as the kind cluster's admin. The operator's own
ServiceAccount has no permission to approve, and the admission policy would
refuse it if it had.

### Watch the lab live

`mise run lab:top` runs `pk-metal top` against the lab: one tile per machine,
updated as the cluster changes. A tile follows the machine from left to right:
the simulated BMC (power, boot override, mounted media, its last Redfish
call), the VM behind it, the Host and its claim, the operation that runs now
(step 7 of 9, and who approved it), and the Talos node. The marks mean `●` as
it should be, `◐` on its way or waiting for you, `✗` failed or stuck, `○` off
or nothing yet. The last console lines of each VM are at the bottom.

```text
pk-metal top   TalosNodes Ready 1/2   Kubernetes nodes Ready 1/1   needs a person: 0
cluster lab: ● Bootstrapped  ✗ Available (NodesNotReady)  control planes 1/1, workers 0/1
fleet http://127.0.0.1:8440/state
┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓╭────────────────────────────────────────────────╮
┃ pklab0001 · node-1 generic                     ┃│ pklab0002 · node-2 generic                     │
┃ BMC ● ─▶ media ○ ─▶ VM ● ─▶ node ●             ┃│ BMC ● ─▶ media ● ─▶ VM ✗ ─▶ node ◐             │
┃ BMC   10.77.0.1:8441  power on  boot none      ┃│ BMC   10.77.0.1:8442  power on  boot Cd/Once   │
┃ media none                                     ┃│ media CD: metal-amd64.iso                      │
┃ last  GET /Systems/1 200                       ┃│       USBStick: node-2-config.iso              │
┃ VM    ● running OSRunning                      ┃│ last  POST /Systems/1/ComputerSystem.Reset 204 │
┃ host  Provisioned                              ┃│ VM    ◐ running OSBootStarted · hang-cycled 1× │
┃ claim default/node-1                           ┃│ host  Provisioning                             │
┃ op    -                                        ┃│ claim default/node-2                           │
┃ node  node-1 10.77.0.11                        ┃│ op    7/9 awaitCheckIn, approved by alice      │
┃       ● Talos ready  ● kubelet Ready           ┃│       pk-metal/provision-node2-1a2b3c4d        │
┃ console .lab/state/node-1/console.log          ┃│ node  node-2 10.77.0.12                        │
┃ │ [talos] task startAllServices (1/1): done    ┃│       ◐ Talos MaintenanceMode                  │
┃ │ [talos] service[kubelet](Running): Health c… ┃│ console .lab/state/node-2/console.log          │
┃ │                                              ┃│ │ BdsDxe: loading Boot0002 "UEFI QEMU DVD-ROM… │
┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛│ │ [    0.000000] Linux version 6.12.40-talos   │
                                                  │ │                                              │
                                                  ╰────────────────────────────────────────────────╯
tab select  a approve  h hold/unhold  r retry  q quit
```

Keys: `tab` (or the arrows) selects a machine. `a` shows the plan of its
operation that waits for approval, the same text `pk-metal approve` prints,
and approves it only when you answer `y`. `h` puts the machine's claim on hold
or removes the hold. `r` retries a failed operation. `q` quits.

`pk-metal top` works the same against real hardware: leave out `--fleet` and
the simulator lines (media, last call, VM, console) are not shown.

To look at the management cluster yourself, point kubectl at the lab's
kubeconfig: `export KUBECONFIG=$PWD/.lab/kubeconfig`. Then `kubectl get metal`
lists every object of the operator, and `kubectl describe host pklab0002`
shows why something waits.

**Changing the manifests.** Edit `lab/lab.yaml` and run `mise run lab:up`
again: it renders the manifests anew and applies them. The files under
`.lab/generated/` are overwritten on every `lab:up`; do not edit them. To
apply one of them by hand: `mise run lab:apply
.lab/generated/manifests/30-claims.yaml`. A plain `kubectl apply` of a claim
with `spec.os` is refused, even as the cluster admin: the claim policy
(`metal/config/policy`) asks for membership of the group `pk-metal:admins` by
name. `lab:apply` acts as a member by impersonation; a real site binds its
admins to that group instead.

## Size the lab

The default is one control plane and two workers. Set another size with
`PK_LAB_NODES=<control planes>cp+<workers>w`:

```sh
PK_LAB_NODES=3cp+2w mise run lab:doctor   # does it fit in memory?
PK_LAB_NODES=3cp+2w mise run lab:up
mise run lab:wait                         # waits for all five
```

- Use 1, 3 or 5 control planes. etcd needs a majority, so an even number is
  refused. With three or more, the Kubernetes API address (the VIP,
  `10.77.0.10`) moves to another control plane when its holder goes down.
- `3cp+0w` is allowed: the control planes then also run ordinary workloads.
- The machines are numbered control planes first. With `3cp+2w`, node-1 to
  node-3 are control planes and node-4 and node-5 are workers.
- Memory: each control plane gets 4 GiB and each worker 2 GiB, plus about
  3 GiB for kind. `lab:doctor` adds this up for the size you give it and
  compares it with the free memory. `3cp+2w` needs about 19 GiB.
- To change the defaults for good, edit `controlPlanes`, `workers`, the
  `controlPlane` and `worker` sizes, or one machine under `nodes:` in
  `lab/lab.yaml`. An entry under `nodes:` for a machine the size does not
  have is ignored.

The size is fixed while the lab runs. `lab:up` refuses a new size while the
fleet runs, and the other commands use what the last `lab:up` rendered, so
`PK_LAB_NODES` is needed on `lab:up` (and `lab:doctor`) only. To change it:

```sh
mise run lab:down && mise run lab:reset
PK_LAB_NODES=3cp+2w mise run lab:up
```

## Move the operator onto the cluster it built (pivot)

In a real site the operator starts in a small bootstrap cluster (kind on a
workstation), installs the first Talos cluster, and then moves onto that
cluster with `pk-metal pivot`. The lab runs this round too. It needs a
second cluster for the operator to install from its new place:

```sh
PK_LAB_NODES=1cp+0w PK_LAB_WORKLOAD=1cp+0w mise run lab:doctor
PK_LAB_NODES=1cp+0w PK_LAB_WORKLOAD=1cp+0w mise run lab:up
mise run lab:wait         # installs the first cluster, from kind
mise run lab:pivot        # moves the operator, deletes kind, installs the second
kubectl --kubeconfig .lab/second.kubeconfig get nodes
```

What `lab:pivot` does:

1. It starts an image registry on the workstation (`10.77.0.1:5000`) and
   pushes the operator image there. The first cluster pulls it through a
   registry mirror, which the lab renders only when there is a second
   cluster.
2. It runs `pk-metal pivot --to .lab/workload.kubeconfig`. The move stops the
   operator in kind, installs it on the first cluster, copies its objects,
   and fails if the operator there proposes any operation. The machine it
   installed must not be installed again. The first cluster has no
   StorageClass, so the round moves with `--media-store=ephemeral`: the
   operator there keeps its media store in an emptyDir and builds no
   installer for a one-slot BMC. Keep the second cluster's machines two-slot
   (the default); a one-slot machine there waits with `BuilderUnavailable`.
3. It deletes the kind cluster. `.lab/kubeconfig` names the first cluster
   from now on, so `lab:status`, `lab:approve` and `lab:collect` go there.
4. It applies `.lab/generated/workload/` (TalosCluster, TalosNodes and
   HostClaims of the second cluster), approves, and waits until the second
   cluster's nodes are Ready. It writes `.lab/second.kubeconfig` and
   `.lab/second.talosconfig`.

Its machines come after the first cluster's: with `1cp+0w` and `1cp+0w`,
node-1 is the first cluster and node-2 the second. The second cluster's VIP
is `10.77.0.9`. To change the defaults, add a `workload:` section to
`lab/lab.yaml` (there is a commented example at its end). Once the operator
has moved, `lab:up` refuses to start kind again; take the lab down first. The
whole procedure for a real site is in `docs/metal/pivot.md`.

## A one-slot BMC

Most BMCs in the lab hold two virtual media: the Talos installer in one slot
and a small ISO with the machine config in the other. A Huawei iBMC
(personality `huawei-2288h-v5`) holds one medium only. For such a machine the
operator builds an installer for that one node, with its machine config inside,
and the BMC fetches it over the same protected path as the config ISO. The
machine needs no network path to the operator, and no config is sent to it
over the network. [Concepts for system
administrators](../docs/metal/concepts-for-sysadmins.md) says why, and what it
means for secrets on disk.

The default lab does not do this. To try it, give one machine that
personality in a copy of the spec:

```sh
yq '.nodes."node-2" = {"personality": "huawei-2288h-v5"}' lab/lab.yaml >/tmp/lab-huawei.yaml
PK_LAB_SPEC=/tmp/lab-huawei.yaml mise run lab:up
mise run lab:wait
```

In CI, start the `lab-e2e` workflow by hand and tick `huawei`.

What happens, and what it costs:

- The operator starts a Job in the `pk-metal` namespace that runs the Talos
  imager. kind pulls the imager image from `ghcr.io` (about 270 MB), so the
  lab needs to reach that registry. The image is pinned by digest.
- The build takes one to a few minutes and writes an ISO of about 370 MB to
  the media store, a volume of the operator. `kubectl -n pk-metal get jobs`
  shows the build; `kubectl -n pk-metal get bootassets` shows the result.
- The Job runs as an ordinary user with the restricted Pod Security profile.
  It needs no loop devices and no privileges, so it runs in kind as it is.
  Give it up to 2 GiB of memory while it runs.
- The approval plan (`mise run lab:approve`) says that the installer carries
  the machine config of that node.
- The build log never holds the machine config. `lab:collect` saves it to
  `imager.log`.

## Look at a machine (there is no ssh)

Talos has no shell, no ssh and no package manager. Its API replaces them, and
`talosctl` is the client. The lab's talosconfig (the client certificate) is
written by `mise run lab:kubeconfig` once the cluster is bootstrapped.

```sh
# The serial console, from the first firmware line on, during the install too:
tail -f .lab/state/node-2/console.log

# What each simulated BMC holds and what its VM does: power, media slots,
# boot override, the last Redfish requests, whether QEMU runs. Read-only,
# on loopback (pk-metal sim fleet --state-listen changes the address):
curl -s http://127.0.0.1:8440/state | jq '.machines[] | {name, power, media, backend}'

export TALOSCONFIG=$PWD/.lab/talosconfig
talosctl -n 10.77.0.12 dashboard          # like top, plus the kernel log and service states
talosctl -n 10.77.0.12 dmesg              # the kernel log
talosctl -n 10.77.0.12 services           # what runs, and its state
talosctl -n 10.77.0.12 logs kubelet       # one service's log
talosctl -n 10.77.0.12 get addresses      # also: get links, get routes, get disks
talosctl -n 10.77.0.12 read /proc/meminfo

# A machine that booted but has no configuration yet (maintenance mode)
# answers without a client certificate:
talosctl -n 10.77.0.12 --insecure get disks
```

Machine node-n has the address `10.77.0.<10+n>`; the head of
`.lab/generated/manifests/20-talos.yaml` lists them all. On real hardware the
console is the BMC's serial-over-LAN (`ipmitool -I lanplus -H <bmc> -U <user>
sol activate`) or its web console; the TalosCluster's `console=` kernel
argument must name the port the SOL redirects. Software is added to Talos as
system extensions in the TalosCluster's `schematic`, not installed on a
running machine.

**A boot operation fails with "gave up waiting for TalosNode … to check in".**
Watch the console log above. If the machine sits in maintenance mode, the
config medium did not reach it; `talosctl -n <address> --insecure get
addresses` shows whether it got the address you expect.

## What runs as root

Only `lab/net.sh`, through `sudo`, in `lab:up` (and `lab:down` to undo it). It
changes, on the workstation:

- a bridge `pkbr0` with the address `10.77.0.1/24`, and one tap per machine,
  owned by you so that QEMU runs without root;
- `net.ipv4.ip_forward=1`, if it was off; `down` turns it off again;
- iptables rules, each with the comment `pk-metal-lab`: accept forwarded
  traffic from and to the bridge (in `DOCKER-USER`, or `FORWARD` without
  Docker), accept input to `10.77.0.1` from the bridge and from Docker's
  bridges (where kind's node sits), and a `MASQUERADE` rule that NATs the
  lab subnet to the outside;
- with firewalld running: `pkbr0` in the zone `trusted` (runtime only).

Nothing of it is written to `/etc`, and nothing survives a reboot. The
simulated BMCs (user `admin`, password `admin-pass`) and the media port answer
on `10.77.0.1` only, not on your other interfaces. `lab/net.sh status` lists
every part; the script is short, read it first if you want to know exactly
what runs as root.

## Behind a proxy, and site DNS

**The workstation.** Set `HTTPS_PROXY`, and add the lab network to
`NO_PROXY`: the simulated BMCs fetch boot media from `https://10.77.0.1:30443`
and would otherwise ask the proxy for it.

```sh
export NO_PROXY="${NO_PROXY:+$NO_PROXY,}10.77.0.0/24,localhost,127.0.0.1,.local"
export no_proxy="$NO_PROXY"
```

Add the kind network as well (`docker network inspect kind --format '{{range
.IPAM.Config}}{{.Subnet}} {{end}}'`). `lab:doctor` checks `NO_PROXY` and
whether the image factory answers. `lab:up` passes the proxy on to the
operator's Deployment (`.lab/deploy/proxy.yaml`). Docker's own proxy, for
pulling the kind node image, is set in the Docker service, not here.

**The Talos machines** do not inherit any of it. They reach the outside
through the workstation's NAT, use the DNS servers of the `ResolverConfig`
patch in `lab/lab.yaml` (1.1.1.1 and 8.8.8.8, Talos' own defaults) and the
NTP server `time.cloudflare.com`. At a site that blocks those, change the
`patches:` list in `lab/lab.yaml`; it goes into the TalosCluster as it is:

```yaml
patches:
  - inline: |
      apiVersion: v1alpha1
      kind: ResolverConfig
      nameservers:
        - address: 192.0.2.53
  - inline: |
      apiVersion: v1alpha1
      kind: TimeSyncConfig
      bootTimeout: 2m0s
      ntp:
        servers:
          - ntp.example.org
  - inline: |
      apiVersion: v1alpha1
      kind: EnvironmentConfig
      variables:
        https_proxy: http://proxy.example.org:3128
        no_proxy: 10.77.0.0/24,10.96.0.0/12,10.244.0.0/16,.svc,.cluster.local,localhost,127.0.0.1
```

Replace the existing `ResolverConfig` and `TimeSyncConfig` entries rather
than adding a second one. The `no_proxy` above keeps the lab network and the
cluster's service and pod networks (Talos' defaults) off the proxy. Apply with
`mise run lab:up`, before the machines are installed.

## Troubleshooting

**Start with `mise run lab:status`.** The BLOCKED column says what each host,
operation and endpoint is waiting for. `mise run lab:collect` gathers the
operator log, the fleet log and the machines' console logs in one directory.

**`lab:up` stops in doctor.** Every failed check prints its fix. Rerun
`lab:up` afterwards; it continues where it stopped.

**The kind cluster does not start, and the host is on cgroup v1.** Kubernetes
1.35 and later need cgroup v2. Either boot with
`systemd.unified_cgroup_hierarchy=1`, or copy `lab/kind.yaml`, set
`image: kindest/node:v1.34.0` in the copy, and run with
`PK_LAB_KIND_CONFIG=/path/to/copy.yaml`.

**kind fails with "cannot assign requested address".** `lab/kind.yaml`
publishes the media port on `10.77.0.1`, which exists only while the lab
network is up. `lab:up` brings the network up first; for a partial rerun, run
`lab/net.sh up` (or `mise run lab:up`) before `lab/lab.sh cluster`.

**Endpoints stay unreachable.** The operator in kind has to reach
`10.77.0.1:8441` and up (one port per machine) on the host. `lab/net.sh status` lists the firewall rules
the lab needs and marks the missing ones. Host firewalls:

- firewalld (RHEL, Fedora): `lab/net.sh up` puts `pkbr0` into the zone
  `trusted` and `status` checks it. A `firewall-cmd --reload` drops that
  runtime setting; rerun `mise run lab:up`, or do it by hand:
  `sudo firewall-cmd --zone=trusted --add-interface=pkbr0`. Docker puts its
  own bridges into its zone `docker`.
- nftables rulesets of your own (`nftables.service`): `lab/net.sh up` warns
  about a chain that drops input or forwarded traffic by policy and prints
  the `nft` commands that let the lab's traffic through. Reject rules at the
  end of a chain are not detected; check `sudo nft list ruleset`.
- ufw: the lab's rules sit before ufw's; if traffic is still dropped,
  `sudo ufw allow in on pkbr0`.

**An operation fails at `insertMedia`.** The simulated BMC downloads the
installer image from `factory.talos.dev` and the node configuration from the
operator at `https://10.77.0.1:30443`. The first needs internet access from the
workstation (or a proxy, see above), the second the NodePort mapping of
`lab/kind.yaml`; check it with `curl -k https://10.77.0.1:30443/`, where a 404
is the healthy answer. With a proxy set, `NO_PROXY` must contain
`10.77.0.0/24`.

**A machine boots but never checks in.** Its serial console is in
`.lab/state/<machine>/console.log`. The machines reach the outside through
NAT on the bridge; without it, or without DNS (see "Behind a proxy, and site
DNS"), Talos cannot pull its installer or set its clock.

**A machine sits in its firmware after power-on.** OVMF sometimes stops
before it tries a boot option: the console log then holds only screen-clear
sequences and no text. The fleet watches for this. When a VM shows nothing
readable for 90 seconds after a power-on, the fleet power-cycles it once and
logs `FIRMWARE HANG` in `.lab/fleet.log`; the state endpoint counts it as
`backend.hangCycles`. It does this once per power-on, and never after the
console showed text, so a running system is never reset. A second hang stays
for you to look at. `pk-metal sim fleet --hang-timeout` changes the time;
`0` turns the guard off.

**`sudo` asks for a password.** Only `lab/net.sh` calls it; see "What runs as
root".

**Start over.** `mise run lab:down && mise run lab:reset` wipes the machines.
Removing `.lab/` as well discards the secrets bundle and the image cache.
`lab:reset` removes the fleet's `stateDir` only if it is inside `.lab/`, or
carries the mark `lab:up` leaves in a directory it created.
