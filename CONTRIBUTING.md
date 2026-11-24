# Contributing to platform-kit

Read the [project status](README.md#status) first. No license is chosen yet,
so open an issue before you start a large change.

The [documentation site](https://dennisklein.github.io/platform-kit/) explains
the design. Start with the architecture page in the Concepts section. This file
is the short version for a person who changes the repository.

## Set up

1. Install [mise](https://mise.jdx.dev/).
2. Run `mise trust` and then `mise install` in the repository root.
3. Run `mise tasks` to see every task.

mise pins Go, kubectl, kind, talosctl, golangci-lint, Node and Vale.
Docker, QEMU, OVMF and shellcheck are system packages.

Development happens on the `metal` branch. Start your branch from it.

## Layout

| Path | Contents |
| --- | --- |
| `metal/` | The Go module: API types (`api/`), commands (`cmd/`), controllers and libraries (`internal/`), manifests (`config/`) |
| `lab/` | The virtual lab scripts |
| `docs/` | The documentation site |

## Tests

Each tier needs more than the one before it. Run the lowest tier that covers
your change, and let CI run the rest.

| Tier | Task | What it needs | Where it runs |
| --- | --- | --- | --- |
| Unit | `mise run test` | Go | CI, every push and pull request |
| Controllers (envtest) | `mise run envtest` | Go; downloads kube-apiserver and etcd | CI, every push and pull request |
| Race detector | `mise run test:race` | cgo and a C compiler | CI, every push and pull request |
| e2e, kind tier | `mise run e2e:kind` | Docker and a kind cluster | CI, pushes to one branch only (see `.github/workflows/ci.yml`) |
| e2e, VM tier | `mise run e2e:qemu` | KVM, QEMU, OVMF | Your machine only |
| Fuzz | `mise run fuzz` | Go | Your machine only |
| Virtual lab | `mise run lab:up` and the other `lab:*` tasks | See [lab/README.md](lab/README.md) | The `lab-e2e` workflow, not on pull requests (see `.github/workflows/lab-e2e.yml`) |

A change to code comes with a test. BMC behavior goes in the simulator
(`metal/internal/bmc/sim`). Controller behavior goes in
`metal/internal/envtest`.

## Lint

```sh
mise run lint          # golangci-lint, the phase check and shellcheck
```

CI runs the three parts as separate steps: `lint:go`, `lint:phase` and
`lint:shell`.

## Generated files

Do not edit these by hand:

- `metal/api/**/zz_generated.deepcopy.go`
- `metal/config/crd/**`
- `metal/config/rbac/role.yaml`
- `docs/src/data/generated/**`

After a change to the API types, the RBAC markers, the `pk-metal` commands or
the glossary:

```sh
mise run generate
```

Commit the result with your change. CI runs `mise run generate:check` and
fails on a difference.

## Invariants

A change must keep these rules.

- Only `metal/internal/phase` writes `status.phase` and `status.blockedOn`.
  Reconcilers do not read them. `mise run lint:phase` checks this.
- Each writer of a status has its own server-side-apply field owner.
- Every change to a machine goes through a HostOperation that a person
  approved.
- The operator does not invent facts. It reports what it read from the
  machine, or it reports nothing.

## Documentation

The pages are in `docs/src/content/docs`. They use ASD-STE100 style: short
sentences, active voice, one instruction in each step.

```sh
mise run docs:dev      # live preview at http://localhost:4321/platform-kit/
mise run docs:check    # build, and check the links (CI runs this)
mise run docs:lint     # check the prose with Vale
```

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/):
`type(scope): subject`.

- Types in use: `feat`, `fix`, `docs`, `test`, `ci`.
- The scope is the part you changed: `controller`, `bmc`, `talos`, `pk-metal`,
  `api`, `lab`, `site` (for the documentation site), and so on.
- Write the subject in the imperative, in lower case, without a full stop.
- Write the body as short bullets that say what changed and why.
- Make one logical change in each commit. Include its tests and its generated
  files.

## Before you open a pull request

```sh
mise run build
mise run test
mise run envtest
mise run lint
mise run generate:check
```

If you changed the documentation, also run `mise run docs:check` and
`mise run docs:lint`.

## Security

Do not open a public issue for a vulnerability. Read [SECURITY.md](SECURITY.md).
