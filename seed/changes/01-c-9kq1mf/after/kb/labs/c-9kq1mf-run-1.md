---
title: Lab report c-9kq1mf/run-1
type: lab-report
entities: [GC5A-6Z, cn-025, cn-026, cn-027, cn-028, cn-029, cn-030, cn-031, cn-032, cn-033, cn-034]
changes: [c-9kq1mf]
run: c-9kq1mf/run-1
result: pass
---
# Lab report c-9kq1mf/run-1

Result: **pass**. Driver sim, host local, provisioning metal, fresh machines.

## Steps

| Step | Detail | Result |
| --- | --- | --- |
| topology | generate from inventory and cabling | pass |
| BMC | Redfish sim | pass |
| install | Talos | pass |
| join | defiant | pass |
| BGP | peers up | pass |
| checks | scenario | pass |
| report | written | pass |

## Checks

- pass: nodes Ready: 11 of 11
- pass: bgp: established, 10 of 10 nodes of GC5A-6Z selected by rack-6z
- pass: bmc: power cycle cn-030, back in 3m 05s
- pass: fault: data-b link down, traffic via data-a
