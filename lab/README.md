# The virtual lab

Three virtual machines with simulated management controllers, and the operator
taking them from an inventory entry to a three-node Kubernetes cluster. The
machines are real UEFI machines under QEMU, the install is a real Talos install,
and the operator runs from the same image and manifests a real site would use.
Only the hardware is simulated.

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
|   NodePort 30443 on the host             power, media | boot order     |
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

Linux with KVM, cgroup v2, Docker, QEMU with OVMF, `sudo`, about 12 GiB of free
memory and 15 GiB of free disk. `mise install` brings kind, kubectl, talosctl,
yq and jq at the versions the repository pins.

```sh
sudo apt-get install qemu-system-x86 qemu-utils ovmf
mise install
mise run lab:doctor   # checks every item and prints the fix for each miss
```

## Commands

| Command | What it does |
| --- | --- |
| `mise run lab:doctor` | Checks the workstation. Changes nothing. |
| `mise run lab:up` | Network, kind cluster, operator, fleet, manifests. Safe to rerun. |
| `mise run lab:status` | The rack as the operator sees it, then every object. |
| `mise run lab:approve` | Approves every operation that waits for a person. |
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
mise run lab:approve      # the one human step
mise run lab:status       # repeat until the three hosts are Provisioned
mise run lab:kubeconfig
kubectl --kubeconfig .lab/workload.kubeconfig get nodes
mise run lab:down
```

On a hosted CI runner (4 cores, 16 GB) `lab:up` takes about four minutes from
a cold start, and the three installs after the approval about three more. The
`lab-e2e` workflow in `.github/workflows` runs exactly this round, with
`lab:wait` in place of the manual steps.

The operator proposes each installation as a HostOperation and does not touch
a machine before somebody else approves it. `lab:approve` is that somebody: it
runs `pk-metal approve` as the kind cluster's admin. The operator's own
ServiceAccount has no permission to approve, and the admission policy would
refuse it if it had.

To look at the management cluster yourself, point kubectl at the lab's
kubeconfig: `export KUBECONFIG=$PWD/.lab/kubeconfig`.

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

**Endpoints stay unreachable.** The operator in kind has to reach
`10.77.0.1:8441-8443` on the host. `lab/net.sh status` lists the firewall rules
the lab needs and marks the missing ones. A host firewall that drops input
before those rules (ufw with a default deny, firewalld) needs the bridge
allowed, for example `sudo ufw allow in on pkbr0`.

**An operation fails at `insertMedia`.** The simulated BMC downloads the
installer image from `factory.talos.dev` and the node configuration from the
operator at `https://10.77.0.1:30443`. The first needs internet access from the
workstation, the second the NodePort mapping of `lab/kind.yaml`; check it with
`curl -k https://10.77.0.1:30443/`, where a 404 is the healthy answer.

**A machine boots but never checks in.** Its serial console is in
`.lab/state/<machine>/console.log`. The machines reach the outside through
NAT on the bridge; without it Talos cannot pull its installer or set its clock.

**`sudo` asks for a password.** Only `lab/net.sh` calls it, to make the bridge,
the taps and the firewall rules in `up` and to remove them in `down`. The
script is short; read it first if you want to know what runs as root.

**Start over.** `mise run lab:down && mise run lab:reset` wipes the machines.
Removing `.lab/` as well discards the secrets bundle and the image cache.
