# Observability

Three ways to see what pk-metal and the machines are doing, from the quick
look to the pager:

| | For | Where |
| --- | --- | --- |
| `pk-metal status` | a look at the rack and the clusters, a cron check | your workstation, reads the management cluster |
| `pk-metal top` | watching the fleet live | your workstation |
| Prometheus metrics and alerts | dashboards, paging | the manager's `/metrics` endpoint |

## pk-metal status

`pk-metal status` prints one table per kind of thing, and under each table one
line per row that needs a person, saying why in the operator's own words:

- **HOST**: each machine, its phase, claim, node and BMC paths.
- **OPERATION**: every operation that has not succeeded (waiting for approval,
  waiting for a maintenance window, running, failed).
- **ENDPOINT**: each BMC.
- **CLAIM**: claims that have no machine yet.
- **CLUSTER**: each TalosCluster. `TALOS WANT/RUN` is the Talos version the
  cluster asks for and every version its nodes run now (more than one means a
  rollout is under way or stuck); the same for Kubernetes. `CP` and `WORKERS`
  are ready/total. `ETCD` is what the cluster controller last saw of etcd (the
  reason of the TalosCluster's `Bootstrapped` condition: `EtcdRunning`, or
  `EtcdDataLost` when every control plane lost its data). `BOOTSTRAPPED` is
  when etcd was created; pk-metal never bootstraps a cluster twice. `BACKUP`
  is the `BackedUp` condition of the cluster's EtcdBackupPolicies: the reason
  of one that is backed up, else of one that is not, `-` without a policy
  ([disaster recovery](disaster-recovery.md)).
- **NODE**: each TalosNode: cluster, role, the machine it runs on, address,
  Talos version, boot stage and readiness.

Exit status: `0` nothing needs a person, `1` the cluster could not be read,
`2` something needs a person. `2` includes an operation waiting for approval, a
failed operation, a bootstrapped cluster whose etcd or Kubernetes API is
unhealthy or whose backup is stale, and a node of a running cluster that stopped
being Ready. An operation that waits for its maintenance window or queues
behind another one on the same machine is not counted: that is the plan
working.

`pk-metal status -o json` prints the same facts for scripts; `pk-metal help
status` lists every key.

## Metrics

The manager serves Prometheus metrics on its metrics port (`--metrics-bind-address`),
next to the controller-runtime defaults. Every series below starts with
`pk_metal_`. Most are read from the cluster state at each scrape, so a value is
never older than the scrape.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `host_phase` | host, phase | 1 for the phase each Host is in |
| `host_blocked` | host, reason | 1 while a Host is blocked, with the reason |
| `operations` | phase, action | HostOperations by phase and action (an operation counts once per distinct action) |
| `operation_failed` | namespace, operation, host, reason | 1 per failed operation still waiting for a person |
| `operation_awaiting_approval_seconds` | namespace, operation, host, reason | how long each operation has waited for approval |
| `operation_blocked` | namespace, operation, host, reason | 1 per approved operation that has not started (OperationQueued, OutsideMaintenanceWindow, …) |
| `bmc_reachable` | endpoint | 1 when the BMC answers Redfish, 0 when not |
| `talosnode_ready` | node, cluster, role | 1 when the node reports Ready |
| `talosnode_info` | node, cluster, version | 1 per node, with the Talos version it runs |
| `taloscluster_available` | cluster | 1 when the Kubernetes API answers and every node is Ready |
| `taloscluster_nodes` | cluster, role, ready | node counts |
| `credentials_expiry_timestamp_seconds` | cluster, kind | when the published admin kubeconfig and talosconfig expire |
| `etcd_backup_last_success_timestamp_seconds` | cluster | time of the newest verified etcd snapshot (set by the backup controller) |
| `rollout_nodes_done`, `rollout_nodes_total` | rollout, cluster | progress of a rollout's approved plan (set by the rollout controller) |

## Alerts

`metal/config/prometheus/rules.yaml` is a PrometheusRule for the Prometheus
Operator. It is not part of the default install, because a cluster without the
Prometheus Operator has no such kind; apply it where it exists:

```
kubectl apply -f metal/config/prometheus/rules.yaml
```

| Alert | Fires when | What to do |
| --- | --- | --- |
| PkMetalOperationFailed | an operation failed (5 min) | `kubectl describe hostoperation`, fix the cause, `pk-metal retry` |
| PkMetalApprovalWaiting | an operation waited more than a day for approval | approve it with `pk-metal approve`, or delete it to refuse |
| PkMetalBMCUnreachable | a BMC does not answer for 15 min | check its network and power; nothing can reach the machines behind it |
| PkMetalNodeNotReady | a node is not Ready for 15 min | `pk-metal status`, `kubectl describe talosnode` |
| PkMetalClusterUnavailable | a cluster's Kubernetes API does not answer or a node is down for 15 min | `kubectl describe taloscluster` |
| PkMetalEtcdBackupStale | the newest verified etcd backup is older than 12 hours | see [disaster recovery](disaster-recovery.md) |
| PkMetalCredentialsExpiring | a published admin credential expires within 14 days | the operator renews them 30 days ahead, so renewal is failing: read the manager log |

The thresholds are starting points. Copy the file and change them to fit your
site; the metric names are stable.
