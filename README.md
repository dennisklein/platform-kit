# platform-kit

pk-metal is a Kubernetes operator that installs Talos Linux on bare-metal
servers through their BMCs (Redfish and virtual media). You write what must be
true. The operator proposes each change to a machine, and a person approves it.

| Path | Contents |
| --- | --- |
| `metal/` | The operator, its API (`metal.pk.io`, `os.pk.io`), the `pk-metal` command and the manifests |

All tools come from [mise](https://mise.jdx.dev/):

```sh
mise run test          # run the unit tests
mise run lint          # run the linters
```
