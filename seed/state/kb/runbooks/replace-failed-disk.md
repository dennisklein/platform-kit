---
title: Replace a failed disk in a storage node
type: runbook
entities: [sn-05, jb-05, pegasus]
tags: [ceph, storage, disk]
---
# Replace a failed disk in a storage node

Use this runbook when Ceph marks an OSD on sn-05 as down and the disk is in
jb-05. The desired state of each slot is in `control/inventory/jb-05.yaml`.

## Before you start

- Find the OSD and its device: `ceph osd tree down` and
  `ceph device ls-by-host sn-05`.
- Map the device to a drawer and a slot with
  [JBOD drawers: slot numbers](../hardware/jbod-slot-numbers.md).
- Check that the cluster is healthy apart from this OSD: `ceph health detail`.
  Do not start when placement groups are already degraded for another reason.

## Steps

1. Open a change:

   ```
   pk change new "Replace disk jb-05 slot 17" --intent "OSD down on sn-05, disk in drawer 1 slot 17"
   ```

2. Set the slot to `replace`:

   ```
   pk edit jb-05 --set drawers[1].slots[17].state=replace
   ```

3. Run the change lab, mark the change ready and get one approval.
4. Merge. The storage operator drains the OSD and marks it out. Wait until
   `ceph -s` shows no misplaced objects.
5. Replace the disk. The slot LED blinks amber while the operator holds it.
6. The asset DB sync opens a change with the new serial. Review it, then set
   the slot back to `ok` in the same change.

## After the replacement

- Ceph backfills the new OSD. This takes about 6 h for a 20 TB disk.
- Add a line to the incident log if more than one disk failed in a week.
