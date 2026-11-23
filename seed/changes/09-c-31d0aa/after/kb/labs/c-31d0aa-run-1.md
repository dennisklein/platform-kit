---
title: Lab report c-31d0aa/run-1
type: lab-report
entities: [data-a, data-b, GC5A-6Z]
changes: [c-31d0aa]
run: c-31d0aa/run-1
result: pass
---
# Lab report c-31d0aa/run-1

Result: **pass**. Driver sim, host local, provisioning metal, fresh machines.

## Steps

| Step | Detail | Result |
| --- | --- | --- |
| topology | generate from inventory and cabling | pass |
| network | containerlab, spine-1 and spine-2 | pass |
| checks | scenario | pass |
| report | written | pass |

## Checks

- pass: uplinks: 5 of 5 links up
- pass: fault: UPL-0002 down, data-a traffic via UPL-0004
- pass: fault: UPL-0003 down, data-b traffic via UPL-0005
