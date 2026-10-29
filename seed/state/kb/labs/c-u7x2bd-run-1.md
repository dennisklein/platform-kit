---
title: Lab report c-u7x2bd/run-1
type: lab-report
entities: [cn-003, cn-025, cn-026, cn-027, cn-028, cn-029, cn-030, cn-031, cn-032, cn-033, cn-034]
changes: [c-u7x2bd]
run: c-u7x2bd/run-1
result: pass
---
# Lab report c-u7x2bd/run-1

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
- pass: bmc: firmware 2.4 reported by Redfish sim on 11 of 11
- pass: bmc: power cycle cn-025, back in 3m 12s
