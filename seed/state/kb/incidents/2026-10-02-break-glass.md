---
title: "Incident 2026-10-02: break-glass"
type: incident
entities: [phoenix, defiant]
changes: [c-b7gl4s]
tags: [break-glass, openbao, sso]
---
# Incident 2026-10-02: break-glass

## Summary

On 2026-10-02 at 07:40 the SSO provider returned errors after a certificate
expired. Nobody could log in to defiant with kubelogin. A tenant job queue
was stuck and needed a node drain. jonas used the break-glass certificate
from OpenBao at 08:05.

## Timeline

| Time | Event |
| --- | --- |
| 07:40 | SSO token endpoint returns 500. kubelogin fails for all admins. |
| 07:55 | Tenant reports stuck jobs on cn-031. |
| 08:05 | jonas reads the break-glass certificate from OpenBao. Alert to all owners and platform admins. |
| 08:12 | cn-031 drained and rebooted. Jobs run again. |
| 09:30 | SSO certificate renewed. kubelogin works. |
| 10:10 | Break-glass certificate rotated in OpenBao. |

## Follow-up

- The SSO certificate now has a renewal alert 14 days before expiry.
- The break-glass use is recorded in the merge commit of c-b7gl4s with the
  trailer `Pk-Break-Glass: openbao-cert 2026-10-02`.
