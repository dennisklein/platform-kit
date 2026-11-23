---
title: Resources
abstract: |
  The generated field reference for every kind in the metal.pk.io and
  os.pk.io API groups — what each field means, its type, its default,
  and what it validates against. Generated from the Go types, so it can never
  disagree with the cluster you are talking to.
# This page is one long list of eighty-odd sections, so show the whole tree at
# once instead of revealing each group's resources only once scrolled into.
toc-expand: true
# Validation patterns are regexes and they end in `$`; doc comments mention
# shell-style ${VAR}. Pandoc reads a pair of those as inline math and swallows
# everything between them — including the pipes holding a table together. No page
# here has math, so the reader that would find it is turned off.
from: markdown-tex_math_dollars
---

::: {.callout-note appearance="simple"}
This page is generated from the API types by `mise run docs`. Edit the Go
doc comments, not this file. Behaviour — what the controllers *do* with these
fields, and what every condition reason means — is written by hand in
[Behaviour](behaviour.md).

The same prose reaches you offline through `kubectl explain`, which reads it out
of the CRDs installed in the cluster:

```bash
kubectl explain host.spec
kubectl explain hostoperation.spec.actions --recursive
```
:::

