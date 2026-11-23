---
title: Lab report c-19f4tb/run-1
type: lab-report
entities: [cp-01, cp-02, cp-03, phoenix]
changes: [c-19f4tb]
run: c-19f4tb/run-1
result: pass
---
# Lab report c-19f4tb/run-1

Result: **pass**. Driver sim, host local, provisioning metal, fresh machines.

## Steps

| Step | Detail | Result |
| --- | --- | --- |
| topology | phoenix control nodes | pass |
| BMC | Redfish sim | pass |
| install | Talos, kernel args | pass |
| join | phoenix | pass |
| checks | scenario | pass |
| report | written | pass |

## Checks

- pass: nodes Ready: 3 of 3
- pass: kernel args: intel_iommu=on iommu=pt on cp-01..cp-03
- pass: etcd: 3 of 3 members healthy after rolling reboot
