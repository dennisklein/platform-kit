# platform-kit

[![ci](https://github.com/dennisklein/platform-kit/actions/workflows/ci.yml/badge.svg?branch=metal)](https://github.com/dennisklein/platform-kit/actions/workflows/ci.yml)

pk-metal is a Kubernetes operator that installs Talos Linux on bare-metal
servers through their BMCs (Redfish and virtual media). You write what must be
true. The operator proposes each change to a machine, and a person approves it.

Documentation: **https://dennisklein.github.io/platform-kit/**

## Status

pk-metal is experimental.

- The API groups (`metal.pk.io`, `os.pk.io`) are `v1alpha1`. They can change
  without a migration path.
- There is no release. No container image and no binaries are published. You
  build both from this repository.
- The automated tests use simulated BMCs, Talos containers and QEMU machines.
  They do not prove that a given server model works.
- The code is under the Apache License 2.0. See [LICENSE](LICENSE).

The [project status page](https://dennisklein.github.io/platform-kit/project/status/)
has the details.

## What it does

- **Day 1:** checks a BMC, enrolls the machine, boots the Talos installer from
  virtual media, installs Talos, and forms a cluster. It does not use PXE,
  TFTP or DHCP.
- **Day 2:** reboots, machine-configuration rollouts, Talos and Kubernetes
  upgrades, adding, removing and replacing nodes, etcd backup and restore, and
  BMC credential rotation.
- **Every change to a machine is a HostOperation.** A person reads the plan
  and approves it with `pk-metal approve`.

## Try it

The virtual lab builds a three-node cluster on one workstation. It needs Linux
with KVM, Docker, QEMU with OVMF, `sudo`, and about 12 GiB of free memory.
All other tools come from [mise](https://mise.jdx.dev/).

```sh
mise install
mise run lab:doctor    # check the prerequisites; changes nothing
mise run lab:up        # start the virtual lab
```

- Lab walkthrough: [lab/README.md](lab/README.md)
- One real server: [First real server](https://dennisklein.github.io/platform-kit/learn/first-real-server/)
- Your own cluster: [Install the operator](https://dennisklein.github.io/platform-kit/operate/day-1/install-operator/)

## Repository

| Path | Contents |
| --- | --- |
| `metal/` | The operator, its API, the `pk-metal` command and the manifests |
| `lab/` | The virtual lab: simulated BMCs, QEMU machines and a kind management cluster |
| `docs/` | The documentation site (Astro and Starlight); the pages are in `docs/src/content/docs` |

To change the code or the documentation, read [CONTRIBUTING.md](CONTRIBUTING.md).
To report a vulnerability, read [SECURITY.md](SECURITY.md).
