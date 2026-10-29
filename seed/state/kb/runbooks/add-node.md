---
title: Add a compute node to a cluster
type: runbook
entities: [GC5A-6Z, defiant, data-a, data-b, mgmt]
tags: [inventory, bgp, labs]
---
# Add a compute node to a cluster

A compute node is one file in `control/inventory/`. The bench renders it into
a `Machine` for its cluster, and the metal operator installs Talos on it.

## Steps

1. Open a change and link the work item:

   ```
   pk change new "Add cn-035 to defiant" --intent "compute node for tenants" --work-item w-114
   ```

2. Add the node from kind **Node** (Files, New file). Set the cluster, the
   role and the rack location. A 2U node at unit 6 fills units 6 and 7.
3. Cable the ports. The form shows the free ports of `mgmt`, `data-a` and
   `data-b`. Use one port on `data-a` and one on `data-b`, so the node keeps
   traffic when one data switch fails. The bench checks that each switch port
   is free and each cable id is unique.
4. Add the node to the BGP peering policy of its rack
   (`clusters/<cluster>/infra/cilium/bgp-<rack>.yaml`). A node that no policy
   selects gets no routes. The advisor "Check this commit" finds a missing node.
5. Run the change lab. The inventory path needs a green lab on the head of
   the branch. The default scenario installs Talos through the emulated BMC,
   joins the node, checks BGP and takes `data-b` down.
6. Mark ready. Two platform admins approve. Merge.

## Checks after merge

- The change shows `deployed` when Flux applied the merge commit on the
  cluster, and `verified` when the node is Ready and BGP is up.
- The work item moves to done.
