# pk-metal documentation

pk-metal takes a rack of machines from powered off to running Talos
clusters, through their BMCs, and keeps those clusters running afterwards.
Every change to a machine waits for a person's approval.

## Start here

- [Concepts for system administrators](concepts-for-sysadmins.md): the
  objects, where state lives, and every step that changes a machine.
- [The virtual lab](../../lab/README.md): all of it on one workstation, with
  simulated BMCs and QEMU machines.

## Day 2

[Day 2: running the clusters pk-metal built](day2.md) is the index of these
pages:

- [Day-2 steps](day2-actions.md): reboots, the cluster guard, abort.
- [Upgrades, config changes and rolling reboots](upgrades.md).
- [Back up and restore etcd](disaster-recovery.md).
- [Add, remove, replace and reinstall nodes](scaling.md).
- [Maintenance windows](maintenance-windows.md).
- [Observability](observability.md): status, metrics, alerts.

## More

- [Move the operator to another cluster (pivot)](pivot.md).
- [The relay, and its secret](relay.md).
- [Model tests for the claim and operation protocol](protocol-model.md).
- [API reference](api.md), generated from the types.
