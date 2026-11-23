# The virtual lab

Three virtual machines with simulated management controllers, and the operator
taking them from an inventory entry to a three-node Kubernetes cluster. The
machines are real UEFI machines under QEMU, the install is a real Talos install,
and the operator runs from the same image and manifests a real site would use.
Only the hardware is simulated.

New to Kubernetes, or coming from PXE, kickstart, Foreman or xCAT? Read
[Concepts for system administrators](../docs/metal/concepts-for-sysadmins.md)
first. It maps the objects here to the tools you know, says where each piece
of state lives, and lists what is destructive.

## Before you start: what changes a machine

- **A HostClaim with `spec.os` is an install order.** The operator proposes
  to boot that machine into the Talos installer. Once a person approves, the
  install disk is repartitioned and overwritten. `lab/manifests/30-claims.yaml`
  has three such claims.
- **Nothing happens to a machine before somebody approves.** The approval is
  `pk-metal approve` (`mise run lab:approve` in the lab). To say no, remove
  `spec.os` from the claim, or hold it: `pk-metal hold default/node-1`.
- **Deleting a HostClaim powers its machine off** (a graceful shutdown first,
  then a hard power-off). For a machine that runs its node, that power-off
  waits for an approval too. `kubectl delete -f lab/manifests/` does this to
  every lab machine. It is not "remove from the inventory".

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
  the working tree and loaded into it; nothing is pulled from a registry.
- `lab/fleet.yaml` describes the machines. The operator's ledger and the tap
  list are derived from it.
- `lab/manifests/` is what a person would commit for a real site: the Assets
  and their BMC credentials, the TalosCluster with its nodes, and one HostClaim
  per machine.
- `.lab/` (ignored by git) holds everything the lab writes: its own kubeconfig,
  the generated deployment, the Talos secrets bundle, disks, caches and logs.
  Your `~/.kube/config` is never touched.

## Prerequisites

Linux with KVM, cgroup v2, Docker Engine, QEMU with OVMF, `sudo`, about 12 GiB
of free memory and 15 GiB of free disk. `mise install` brings kind, kubectl,
talosctl, yq and jq at the versions the repository pins.

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
| `mise run lab:doctor` | Checks the workstation. Changes nothing. |
| `mise run lab:up` | Network, kind cluster, operator, fleet, manifests. Safe to rerun. |
| `mise run lab:status` | The rack as the operator sees it, then every object. |
| `mise run lab:approve` | Approves every operation that waits for a person. |
| `mise run lab:apply [path...]` | `kubectl apply` as a member of the admin group. Default: `lab/manifests/`. |
| `mise run lab:kubeconfig` | Writes `.lab/workload.kubeconfig` and `.lab/talosconfig`. |
| `mise run lab:load` | Rebuilds the operator image and rolls the manager. |
| `mise run lab:wait` | Unattended: approves and waits for three Ready nodes. |
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

To look at the management cluster yourself, point kubectl at the lab's
kubeconfig: `export KUBECONFIG=$PWD/.lab/kubeconfig`. Then `kubectl get metal`
lists every object of the operator, and `kubectl describe host pklab0002`
shows why something waits.

**Changing the manifests.** Edit a file under `lab/manifests/` and run
`mise run lab:apply lab/manifests/30-claims.yaml`. A plain `kubectl apply`
of a claim with `spec.os` is refused, even as the cluster admin: the claim
policy (`metal/config/policy`) asks for membership of the group
`pk-metal:admins` by name. `lab:apply` acts as a member by impersonation; a
real site binds its admins to that group instead.

## Look at a machine (there is no ssh)

Talos has no shell, no ssh and no package manager. Its API replaces them, and
`talosctl` is the client. The lab's talosconfig (the client certificate) is
written by `mise run lab:kubeconfig` once the cluster is bootstrapped.

```sh
# The serial console, from the first firmware line on, during the install too:
tail -f .lab/state/node-2/console.log

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

The node addresses are in `lab/manifests/20-talos.yaml`. On real hardware the
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
patch in `lab/manifests/20-talos.yaml` (1.1.1.1 and 8.8.8.8, Talos' own
defaults) and the NTP server `time.cloudflare.com`. At a site that blocks
those, add patches to the TalosCluster's `patches:` list:

```yaml
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
`mise run lab:apply`, before the machines are installed.

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
`10.77.0.1:8441-8443` on the host. `lab/net.sh status` lists the firewall rules
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

**`sudo` asks for a password.** Only `lab/net.sh` calls it; see "What runs as
root".

**Start over.** `mise run lab:down && mise run lab:reset` wipes the machines.
Removing `.lab/` as well discards the secrets bundle and the image cache.
`lab:reset` removes the fleet's `stateDir` only if it is inside `.lab/`, or
carries the mark `lab:up` leaves in a directory it created.
