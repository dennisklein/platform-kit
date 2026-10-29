# Seed state repo

The demo state repo, embedded into `pk` (package `seedfs`). `pk forge-init`
(internal/seed) builds it on the forge: the history of main with one merge
commit per change, and the open changes with branches and pull requests.

## Layout

```
seed/state/                     final tree of main (after the 8 merged changes)
seed/changes/NN-<id>/           one directory per change, in order
    change.yaml                 metadata (below)
    before/<path>               files the change touches, as they were before it
    after/<path>                the same files after it
```

A path in `before/` but not in `after/` is a deleted file; a path in `after/`
but not in `before/` is a new file. Paths in neither are untouched.

Changes 01 to 08 are merged (`merged: true`). Changes 09 to 12 are open
(`merged: false`); they branch off the final main (`state/`).

## Build

1. Initial tree: start from `state/`; for the merged changes from last to
   first, put back the `before/` files and delete the files that have no
   `before/` entry.
2. Commit the initial tree on main as `chore: initial state` (author dennis,
   2026-08-24T10:00:00Z). Main is not protected yet.
3. For each merged change in order:
   - branch `change/<id>-<slug>` from main;
   - commit `chore(change): open <id>` that adds `.pk/changes/<id>.yaml`;
   - make the commits of `commits:` (each puts the `after/` version of its
     files, or deletes them); without `commits:` one commit
     `<title>` with all files;
   - commit `chore(change): move metadata to the merge request` that deletes
     the metadata file;
   - open the pull request with the trailer description, approve it as each
     person in `approvedBy`, merge it as `mergedBy` with the merge message
     and trailers (`Pk-Change`, `Pk-Intent`, `Pk-Lab-Run`, `Pk-Author`,
     `Pk-Agent`, `Pk-Work-Item`, extra `trailers`, `Approved-by`,
     `Reviewed-in`), and delete the branch.
   - Dates: change commits at `date`, the merge at `mergedAt`.
4. Check: main equals `state/` (the seed test does the same replay).
5. Protect main (required check `pk/policy`, merge whitelist from the policy).
6. For each open change: branch from main, commit `chore(change): open
   <id>`, make the `commits:`, and push. `open:` says the rest:
   - `ready: false`: keep the metadata file (with `labRuns`), open a draft
     pull request (`WIP: ` title prefix).
   - `ready: true`: delete the metadata file in a commit, open a ready pull
     request with the trailer description.
   - `labRuns`: post the commit status `pk/lab` (success for pass, failure for
     fail) on the tested sha (the head before the `docs(lab)` report commit)
     and on the head.
   - `approvals`: approve as each person.
   - Let `pk ci` (or the builder) post `pk/policy` on each head.

## change.yaml

```yaml
id: c-2hd7vw
title: "Move cn-003 to data-a 37"
intent: "free data-b ports for rack 6Z"
author: jonas              # person, or a bot (sync-bot)
agent: anna-agent          # optional: commits by the agent, Pk-Agent trailer
date: "2026-09-11T15:05:00Z"
merged: true
mergedAt: "2026-09-12T10:15:00Z"   # merged changes only
mergedBy: jonas                    # a person; agents and bots never merge
approvedBy: [anna, dennis]
labRun: {run: c-2hd7vw/run-1, result: pass, report: kb/labs/c-2hd7vw-run-1.md}
workItems: []
trailers:                          # optional extra merge trailers
  - {key: Pk-Break-Glass, value: "openbao-cert 2026-10-02"}
commits:                           # optional; files must cover the overlays exactly
  - message: "fix(inventory): move cn-003 p2 from data-b 37 to data-a 37"
    files: [control/inventory/cn-003.yaml]
open:                              # open changes only
  ready: true
  approvals: [anna, jonas]
  labRuns:
    - {run: c-31d0aa/run-1, result: pass, report: kb/labs/c-31d0aa-run-1.md}
```

## Story facts the data keeps

- Rack GC5A-6Z: cn-025 to cn-034 at units 10 to 29 (2U each). Units 6 to 9
  are free for cn-035 (U6) and cn-036 (U8).
- Row switches mgmt, data-a, data-b (48 ports, uplinks on 47 and 48) are in
  GC5A-6Y. Nodes use ports 1 to 46.
- data-a 37 is used by cn-003 (moved there by c-2hd7vw). data-b 37 is free.
  Free for the demo: mgmt 37 and 39, data-b 37 and 39, data-a 38 and 40;
  cables MGT-0141, MGT-0142, DATA-0201 to DATA-0204.
- `bgp-rack-6z.yaml` selects cn-025 to cn-034; c-p4x8ma adds two neighbors at
  the bottom (no textual conflict with node selector edits). Its lab failed
  because the emulation has only the router 10.6.0.1.
- Node files are `inventory.pk.io/v1alpha1` until c-m1gr8t (2026-10-09).
- The Flux Kustomization `infra-metal` of each cluster applies
  `./control/inventory`; the Node render hook emits a `Machine` for the
  nodes of that cluster. Own kinds without a render hook emit nothing.
- `clusters/defiant/infra/cilium/helmrelease.yaml` has a SOPS value
  (`values.hubble.ui.auth.token`) and a `sops:` metadata block; the renderer
  drops the `sops` key.
