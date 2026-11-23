---
title: Lab report c-p4x8ma/run-1
type: lab-report
entities: [GC5A-6Z]
changes: [c-p4x8ma]
run: c-p4x8ma/run-1
result: fail
---
# Lab report c-p4x8ma/run-1

Result: **fail**. Driver sim, host local, provisioning metal, fresh machines.

## Steps

| Step | Detail | Result |
| --- | --- | --- |
| topology | generate from inventory and cabling | pass |
| BMC | Redfish sim | pass |
| install | Talos | pass |
| join | defiant | pass |
| BGP | peers | fail |
| checks | skipped | skipped |
| report | written | pass |

## Checks

- pass: bgp: session to 10.6.0.1 established
- FAIL: bgp: session to 10.6.0.3 not established
- FAIL: bgp: session to 10.6.0.4 not established

The lab emulates only the router 10.6.0.1 (see `.pk/emulation.yaml`, network data-a). The org-a and org-b routers need an emulated peer before this change can pass.
