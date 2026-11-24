# Day 2: running the clusters pk-metal built

Day 0 and day 1 are the install: a machine goes from powered off to a Ready
node ([Concepts for system administrators](concepts-for-sysadmins.md)). Day 2
is everything after that, while workloads run on the nodes: reboots,
upgrades, config changes, backups, adding and removing machines, firmware,
and keeping an eye on all of it.

The rule does not change on day 2: **the operator proposes, a person
approves.** Every change to a machine is a HostOperation that waits for
`pk-metal approve`, or a TalosRollout that waits for `pk-metal rollout
approve`. Reading (a status check, an etcd snapshot, a dry run of a config)
needs no approval.

## The pages

| Task | Page | Commands |
| --- | --- | --- |
| Reboot one node; what each step of an operation does; the cluster guard; stopping an operation | [Day-2 steps](day2-actions.md) | `pk-metal reboot`, `pk-metal abort` |
| Upgrade Talos or Kubernetes, roll out a config change, reboot every node in turn; drift, version skew, the secrets bundle, credentials | [Upgrades, config changes and rolling reboots](upgrades.md) | `pk-metal upgrade`, `pk-metal rollout` |
| Back up etcd and restore it after a loss | [Back up and restore etcd](disaster-recovery.md) | `pk-metal backup`, `pk-metal restore` |
| Add, remove, replace and reinstall nodes; wipe a released machine | [Add, remove, replace and reinstall nodes](scaling.md) | `pk-metal scale`, `replace`, `reinstall`, `wipe` |
| Say when approved work may start | [Maintenance windows](maintenance-windows.md) | `kubectl apply` a MaintenanceWindow |
| See what runs, what waits and what is wrong; metrics and alerts | [Observability](observability.md) | `pk-metal status`, `pk-metal top` |
| Move the operator onto a cluster it built | [Move the operator (pivot)](pivot.md) | `pk-metal pivot` |

## Words these pages use

- **Operation** (HostOperation): the steps one host runs in order, after one
  approval. Its steps (also called actions in the API) are listed in
  [Day-2 steps](day2-actions.md).
- **Rollout** (TalosRollout): a change to a whole cluster, node by node. One
  approval covers its plan; it pauses after the first node until a person
  resumes it. Each node's part is an operation the rollout creates.
- **Release**: what happens to a machine whose HostClaim is deleted. A node
  that answers is drained, leaves etcd, is reset (its disk wiped) and is
  powered off. A node that does not answer is powered off and its Host is
  marked **not wiped** until an approved wipe.
- **Cluster guard**: holds a step that takes a control plane down while
  another control plane of the same cluster is down, or while etcd would
  lose its healthy majority. The annotation
  `metal.pk.io/override-cluster-guard` lifts the etcd part for a repair.
- **Self-protection**: the same idea for the machines the operator itself
  runs on.
- **Maintenance window**: when approved work may start on the machines it
  covers. It never approves anything.
- **Abort**: `pk-metal abort` stops an operation between steps and undoes
  what it left behind (media, boot override, its cordon).

## Trying it in the lab

Each day-2 round has a lab task ([virtual lab](../../lab/README.md)). In CI
each is an input of the `lab-e2e` workflow, off by default:

| Lab task | Workflow input | What it runs |
| --- | --- | --- |
| `mise run lab:day2` | `day2` | a worker and a control plane rebooted, then a config change that must not reboot |
| `mise run lab:upgrade` | `upgrade` | a lab one Talos release behind, upgraded through a rollout |
| `mise run lab:backup-restore` | `backup_restore` | an etcd snapshot, etcd wiped, and restored |
| `mise run lab:scale` | `scale` | control planes 3 -> 1 -> 3, a worker removed, a control plane replaced |
| `mise run lab:observe` | `observe` | the status tables, the metrics, a window holding an operation |
