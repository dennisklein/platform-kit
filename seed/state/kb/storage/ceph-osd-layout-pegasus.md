---
title: Ceph OSD layout on pegasus
type: design
entities: [pegasus, sn-05, jb-05]
tags: [ceph, storage]
---
# Ceph OSD layout on pegasus

pegasus runs Ceph with Rook. One storage node, sn-05, holds the OSDs of the
jb-05 disk shelf. The CephCluster is in
`clusters/pegasus/infra/rook-ceph/cephcluster.yaml`.

## Layout

| Item | Value |
| --- | --- |
| Storage node | sn-05 (Huawei 2288H V5), GC5A-6Y U20 |
| Disk shelf | jb-05, 2 drawers with 24 slots each, SAS to sn-05 |
| OSDs | one OSD per disk, 48 OSDs |
| Failure domain | `osd` (single node; host failure domain comes with sn-06) |
| Pools | `rbd-tenants` (replica 3), `cephfs-data` (EC 4+2) |

## Decisions

- One OSD per disk. NVMe for WAL/DB is not in the shelf, so each OSD keeps
  its own WAL and DB on the spinning disk. Write latency is acceptable for
  the batch workloads on defiant.
- The failure domain stays `osd` until a second storage node exists. A host
  failure takes the pool down; the backups on tape cover this.
- Slot state lives in git (`jb-05.yaml`). A disk replacement is a normal
  change, see [Replace a failed disk](../runbooks/replace-failed-disk.md).
