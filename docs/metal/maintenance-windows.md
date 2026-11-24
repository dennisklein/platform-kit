# Maintenance windows

A maintenance window says when disruptive work may start on a set of
machines: "the compute racks on Saturday night", "the storage nodes on the
first Tuesday morning, one off date for the controller swap". pk-metal still
asks a person to approve every change; a window only decides **when** an
approved change starts.

This page is for the people who plan downtime. The field reference is the
generated [API reference](api.md#maintenancewindow).

## What a window does

- An approved HostOperation on a machine that a window covers waits until one
  of the windows covering that machine is open. While it waits, it shows
  `BLOCKED OutsideMaintenanceWindow` in `kubectl get hostoperations -o wide`
  and in `pk-metal status`, and its Executed condition says when the next
  window opens.
- When a window opens, the waiting operations start by themselves. Nobody has
  to be awake.
- **A step that already runs is never stopped** when a window closes. Stopping
  a machine between "BIOS settings staged" and "rebooted into them", or in the
  middle of a drain, leaves it worse off than finishing. Plan windows long
  enough for the work you approve into them.
- A machine that no window covers is not held: without any MaintenanceWindow
  objects, pk-metal behaves exactly as before.
- When several windows cover a machine, any one of them being open is enough.
- New installs (operations with reason `Provisioning`) are not held: a machine
  being installed runs no workload yet.

What counts as disruptive is everything that goes through a HostOperation:
BIOS and firmware changes, reboots, Talos upgrades and config changes rolled
out node by node, releases. A rollout that reaches a node outside its windows
waits there, between nodes, until the next window.

## Writing a window

```yaml
apiVersion: metal.pk.io/v1alpha1
kind: MaintenanceWindow
metadata:
  name: compute-weekend
spec:
  # Which machines: by Host label, by cluster, or both (a machine is covered
  # when either matches).
  hostSelector:
    matchLabels:
      role: compute            # a label from the HostClaim
  clusters: [prod]             # every machine that runs a node of TalosCluster prod
  # When: in this time zone, daylight saving time included.
  timeZone: Europe/Berlin
  weekly:
    - days: [Sat]
      start: "22:00"
      duration: 6h             # may run past midnight
    - days: [Tue, Thu]
      start: "06:00"
      duration: 90m
  once:                        # one-off windows, in absolute time
    - start: "2026-12-03T18:00:00+01:00"
      end: "2026-12-03T23:00:00+01:00"
```

- `hostSelector` uses the same labels as BIOS and firmware profiles: the
  discovered `metal.pk.io/vendor` and `metal.pk.io/model`, and the labels you
  give the HostClaim. `hostSelector: {}` covers every machine.
- `timeZone` is an IANA name (`Europe/Berlin`, `America/Chicago`); the default
  is `UTC`. Weekly windows keep their local start time across a daylight saving
  change, so "Saturday 22:00" stays 22:00 on the wall clock.
- A weekly window lasts between one minute and one week.

`kubectl get maintenancewindows` shows whether each window is open now and
when that changes next:

```
NAME              OPEN    NEXT CHANGE   ZONE            AGE
compute-weekend   false   2d            Europe/Berlin   5d
```

A window pk-metal cannot read (an unknown time zone, a bad selector) shows
`Valid=False` in `kubectl describe maintenancewindow`. Such a window is never
open: the machines it covers are held until it is fixed. A mistake in a
window holds work back instead of letting it run at the wrong time.

## Starting one operation now anyway

For the urgent case (a security fix, a machine that is down anyway), annotate
the operation; the value is your reason and stays in its record:

```
kubectl annotate hostoperation -n pk-metal <operation> metal.pk.io/ignore-window="CVE-2026-0001, agreed with on-call"
```

The operation starts at its next pass, within a few seconds. Annotate after
approving or before, the order does not matter; nothing starts without the
approval.

## Checking what waits

```
pk-metal status
kubectl get hostoperations -n pk-metal -o wide
kubectl describe hostoperation -n pk-metal <operation>
```

An operation waiting for its window is not counted as "needs attention" in
`pk-metal status` (exit code 0 if nothing else is wrong): it is the plan
working. The metric `pk_metal_operation_blocked{reason="OutsideMaintenanceWindow"}`
lists them for dashboards (see [observability](observability.md)).
