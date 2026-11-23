# platform state repo

The desired state of the platform: bare metal, Talos, Kubernetes and apps for
the clusters phoenix (control), defiant (tenants) and pegasus (storage).

- `clusters/<id>/` cluster file, Flux objects and the paths Flux applies.
- `control/inventory/` racks, switches, nodes and disk shelves.
- `control/cabling.yaml` uplinks of the row switches.
- `control/talos/` talhelper configs.
- `kb/` knowledge base, `work/` work items and milestones.
- `.pk/` policy, emulation profile and labs.

Change it only with platform-kit (`pk bench`). Each change is a branch and a
merge request; main is protected by the `pk/policy` check.
