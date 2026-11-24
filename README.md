# platform-kit

pk-metal is a Kubernetes operator that installs Talos Linux on bare-metal
servers through their BMCs (Redfish and virtual media). You write what must be
true. The operator proposes each change to a machine, and a person approves it.

| Path | Contents |
| --- | --- |
| `metal/` | The operator, its API (`metal.pk.io`, `os.pk.io`), the `pk-metal` command and the manifests |
| `lab/` | The virtual lab: simulated BMCs, QEMU machines and a kind management cluster on one workstation ([lab/README.md](lab/README.md)) |

All tools come from [mise](https://mise.jdx.dev/):

```sh
mise run lab:up        # start the virtual lab
```
