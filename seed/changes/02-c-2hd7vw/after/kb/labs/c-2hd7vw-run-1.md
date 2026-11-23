---
title: Lab report c-2hd7vw/run-1
type: lab-report
entities: [cn-003, data-a, data-b]
changes: [c-2hd7vw]
run: c-2hd7vw/run-1
result: pass
---
# Lab report c-2hd7vw/run-1

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
- pass: cn-003: p1 data-b 9, p2 data-a 37
- pass: fault: data-b link down, cn-003 traffic via data-a
