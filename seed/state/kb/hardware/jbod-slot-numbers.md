---
title: "JBOD drawers: slot numbers"
type: reference
entities: [jb-05]
tags: [hardware, disk]
---
# JBOD drawers: slot numbers

jb-05 is a Western Digital Ultrastar Data60 shelf with two drawers. In
`jb-05.yaml` drawers and slots count from 0, as in the enclosure firmware.

| Drawer | Slots | Front label | Linux device order |
| --- | --- | --- | --- |
| 0 (top) | 0 to 23 | A01 to A24 | sda to sdx |
| 1 (bottom) | 0 to 23 | B01 to B24 | sdy to sdav |

- The front label counts from 1. Slot 17 in drawer 1 is label **B18**.
- Rows of 6 slots run from the front to the back of the drawer.
- `sg_ses --page=aes /dev/sg<N>` shows the slot of a device. Device names can
  change after a reboot; the slot and the serial do not.
- A blinking amber LED means the operator holds the slot for replacement.
