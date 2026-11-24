# Upgrades, config changes and rolling reboots

One of the [day-2 pages](day2.md).

Once a Talos cluster runs, most changes to it reach the machines one node
at a time. A Talos upgrade, a Kubernetes upgrade, a changed machine config
and a rolling reboot all work this way. This page explains how the operator
notices that a change is needed, what it proposes, what you approve, and
what happens on each node. It also covers the secrets bundle and the
credentials the operator publishes.

The rule is the same as for installs: **the operator proposes, a person
approves.** No node is drained, rebooted or reconfigured until a person has
approved the plan that names it.

## The short version

```sh
# Ask for a new Talos release. This changes only the TalosCluster spec,
# waits for the operator to propose a rollout, and prints its plan.
pk-metal upgrade talos lab v1.14.7

# Read the plan again and approve it (another person than the proposer).
pk-metal rollout approve lab-talosupgrade-1a2b3c4d

# The first node runs, then the rollout pauses. Check that node, then:
pk-metal rollout resume lab-talosupgrade-1a2b3c4d

# Follow it.
pk-metal rollout show lab-talosupgrade-1a2b3c4d
```

`pk-metal upgrade kubernetes lab v1.36.3` works the same way for Kubernetes.
A config change needs no command to start it. You edit the TalosCluster or
TalosNode, and the operator proposes a Config rollout once it sees that the
machines differ (see [Drift](#drift)).

## Rollouts

A **TalosRollout** is one change, rolled through the nodes of one cluster.
There are four kinds:

| Kind | What happens on each node | Reboots |
|---|---|---|
| `TalosUpgrade` | drain, install the new Talos release, reboot into it, wait until the node is Ready on the new version, uncordon | yes |
| `KubernetesUpgrade` | apply the config rendered for the new Kubernetes version (kubelet and control-plane images), wait until Ready | only if Talos says the change needs one |
| `Config` | apply the config the operator rendered | only if Talos says the change needs one |
| `Reboot` | drain, reboot, wait until Ready, uncordon | yes |

On a control-plane node the operator also waits for etcd to be healthy
before the node counts as done. A node that does not reboot is not drained.

### The order

- **Control planes first, one at a time.** The current etcd leader goes
  last, so etcd elects a new leader once, not once per node.
- For a `KubernetesUpgrade`, kube-proxy is updated after the control planes
  and before the workers. Kubernetes allows kubelets that are older than the
  API server, but not newer ones.
- **Then workers.** By default one at a time. `spec.maxUnavailableWorkers`
  allows more at once.
- **Pause after the first node.** The rollout always stops after its first
  node is done, so you can look at that node before the others follow. This
  pause cannot be switched off. `pk-metal rollout resume` ends it.

Between two nodes the operator checks the cluster afresh. It does not rely
on conditions it wrote some minutes ago. Before every node, it asks etcd on
the control planes for its status and asks the Kubernetes API whether every
node is Ready. The next node starts only when all of these hold:

- etcd has a leader,
- no etcd member reports an error,
- no member is still a learner,
- every control plane is an etcd member,
- every Kubernetes node that is not part of this step is Ready.

Until then the rollout's `Progressing` condition says `WaitingForHealth` and
names what it waits for.

Only one approved rollout per cluster runs at a time. A second one waits for
the first one to finish.

### Proposals

The operator proposes a rollout when the cluster spec and the machines
disagree. It checks in this order and proposes the first one that applies:

1. A node runs another Talos version than `spec.talosVersion`: a `TalosUpgrade`.
2. The API server reports another Kubernetes version than
   `spec.kubernetesVersion`: a `KubernetesUpgrade`.
3. A node's dry run reports drift: a `Config` rollout for the drifted nodes.

While any rollout for the cluster is unfinished, nothing new is proposed. A
proposal's name ends in a hash of what it proposes, so the same need is
proposed only once. When a rollout failed or was aborted, the operator does
not propose it again on its own. Deleting the failed rollout is how you ask
for a new proposal. A `Reboot` rollout is never proposed; you create it
yourself.

### The plan and its approval

The operator writes the plan into the rollout's status. The plan lists every
node in order, the machine (host) it runs on, what changes, whether it
reboots, and the steps of its operation. It also has a hash, `planHash`.
`pk-metal rollout show` prints the plan; `pk-metal rollout approve` prints it
and asks.

The approval names the plan hash you saw. Until the rollout is approved, the
operator keeps the plan current; for example, it adds a node that has joined
in the meantime. After the approval the plan is frozen. If what the operator
would plan now differs from the approved plan before the first node starts,
the rollout ends with `Complete=False`, reason `PlanChanged`. Nothing runs in
that case, and the operator proposes a fresh rollout for you to look at.

The same admission rules apply as for single operations:

- whoever creates a rollout must name themselves in `proposedBy`;
- the approver must be someone else, and not the operator;
- an approval cannot be changed afterwards.

For each node the operator creates a HostOperation that names the rollout
and the plan hash. These operations need no approval of their own. Before
one starts, the executor checks that it is still covered:

- the rollout exists,
- the rollout is approved for that plan hash and is not aborted,
- the plan has a step for this node on this host,
- the operation's actions are exactly the step's actions.

If any of this fails, the operation fails with reason `RolloutRefused` and
the machine is not touched.

### Pause, resume, abort

- `pk-metal rollout pause <rollout>` stops the rollout before its next node.
  A node that is running finishes.
- `pk-metal rollout resume <rollout>` goes on after a pause. It clears
  `spec.paused` and sets the annotation `os.pk.io/resume` to the plan hash.
- `pk-metal rollout abort <rollout>` stops the rollout for good. A node that
  is running finishes, because stopping between a drain and its uncordon
  would leave the node worse off. The remaining nodes are marked `Skipped`.
  An abort cannot be taken back.

When a node fails, the rollout stops there with `Complete=False`, reason
`RolloutFailed`. The failed HostOperation says what went wrong. No further
node starts.

## Drift

The operator renders a machine config for every node. It stores each config
in a Secret named after the node and a digest of the config's content, for
example `machineconfig-node-1-0123456789ab`. A Secret is never changed in
place. A new config gets a new Secret, so an operation that was approved
for one config can never apply another one. The operator deletes old
Secrets once nothing refers to them any more. It keeps these:

- the current config,
- the config the node last had applied,
- configs named by an unfinished operation or rollout.

Every ten minutes, and every 30 seconds while a rollout runs, the operator
asks each Ready node a **dry run**: "what would change if this config were
applied?" Talos answers without changing anything. The result is the
node's `ConfigInSync` condition:

- `True` (`InSync`): the machine runs what the operator renders.
- `False` (`ConfigDrift`): it does not. The node's `status.drift` says how
  many lines differ, which config keys they touch, and whether applying the
  config needs a reboot. Only the keys are kept, never the values, so that a
  secret in the config does not end up in the status.
- `Unknown` (`DriftUnknown`): the dry run failed. The message says why.

When Talos does not say clearly whether a change needs a reboot, the
operator assumes that it does. A rollout then drains the node, which takes
longer but is safe.

A drifted node causes a `Config` proposal (see [Proposals](#proposals)).
That is how an edit to a TalosCluster or TalosNode reaches running
machines. It is also how a change made by hand with `talosctl` is undone.

## Versions and skew

### Each node has its own contract

The Talos version decides what a machine config looks like. A config
written for a newer Talos release can make no sense to an older one. So the
operator renders each node's config **for the version that node runs**, not
for the version the cluster asks for. The node's `status.renderedFor` says
which version that was.

When you raise `spec.talosVersion`, nothing changes on any machine. Each
node keeps getting configs for its current version, until the `TalosUpgrade`
rollout has upgraded it.

### Conditions

- TalosNode `VersionCurrent`: `False` with reason `VersionSkew` while the node
  runs another Talos version than the cluster asks for.
- TalosNode `Rendered`: `False` with reason `UnsupportedVersionPair` if the
  Talos and Kubernetes versions cannot be combined. Talos keeps the table of
  supported combinations. The operator holds the last good config.
- TalosCluster `VersionsSupported`: whether the Talos and Kubernetes versions
  in the spec work together.
- TalosCluster `VersionsCurrent`: whether every node and the API server run
  what the spec asks for. Reason `VersionsBehind` names the nodes that do
  not.

`pk-metal upgrade` checks the same tables before it changes the spec. It
refuses:

- a Talos release that cannot be reached from the current one in one step,
- a Kubernetes version that the Talos release does not support,
- a downgrade.

A Talos upgrade installs from an installer image. The operator pins the
image by its digest when it plans, so the approved plan names exactly the
image that is installed.

## Kubernetes upgrades

A Kubernetes upgrade runs in three parts:

1. The control planes, one at a time. Each one gets a config with the new
   kube-apiserver, controller-manager, scheduler and kubelet images.
2. The kube-proxy DaemonSet gets the new image. The rollout waits until
   every kube-proxy pod runs it.
3. The workers. Each one gets a config with the new kubelet image.

The rollout plans only for `spec.kubernetesVersion`. If you change the spec
again while a Kubernetes rollout waits for approval, its plan no longer
matches and it ends as `PlanChanged`.

## Secrets bundle

The secrets bundle (`spec.secretsRef`) holds the cluster's certificate
authorities and its join tokens. The running machines trust these CAs. If
the bundle in the Secret changes after the cluster was bootstrapped, the
operator could no longer talk to its own machines, and the credentials it
publishes would stop working.

That is why the operator keeps its own copy of the bundle the machines
were installed with. The copy is the Secret `<cluster>-bundle-applied` in
the operator's namespace. When the Secret named in the spec differs from
that copy, the operator keeps using the copy for:

- rendering configs,
- connecting to the machines,
- publishing credentials.

The cluster's `SecretsBundleHeld` condition is then `True`. It names what
changed: the Talos API CA, the Kubernetes CA, the etcd CA, or tokens or
keys.

The operator does **not** rotate CAs for you. It has no rollout for that
yet. To rotate a CA:

1. Follow the Talos documentation for CA rotation, with `talosctl`, against
   every node of the cluster. The operator keeps working with the old bundle
   meanwhile.
2. When every machine trusts the new bundle, put it into the Secret named in
   the spec. This may already have happened; that is what made the operator
   hold the old one.
3. Delete `<cluster>-bundle-applied`. The operator then takes the bundle
   from the spec, and makes a new copy of it.

If the bundle was changed by mistake, put the old one back.
`SecretsBundleHeld` goes back to `False` once the two match.

## Credentials

The operator publishes an admin kubeconfig and an admin talosconfig for
each cluster. Certificates signed by the cluster's CAs cannot be revoked
short of rotating the CA, so what limits a leaked copy is a short lifetime.

- `spec.credentials.ttl` sets how long a published credential is valid. The
  default is 24 hours.
- The operator issues a new credential when less than a third of the
  lifetime is left. If you shorten the TTL, credentials that were issued
  under the longer one are replaced at once.
- The cluster's `status.credentialsExpireAt` says when the earliest published
  credential expires. Tools that copy a credential out of the cluster must
  copy it again before that time.
- `spec.credentials.readerTalosconfig: true` also publishes
  `<cluster>-talosconfig-reader`. This talosconfig has the `os:reader` role,
  so it can read a machine's state but not change it. Use it for monitoring.
  Setting the field back to `false` removes the Secret.

## Annotations and wake-ups

The TalosCluster controller runs again when the cluster's annotations
change, not only its spec. It also runs when one of the cluster's rollouts
changes. So an annotation-driven action, or a rollout that finishes, is
seen at once rather than at the next periodic check.

## Where to look

| Question | Where |
|---|---|
| What waits for my approval? | `pk-metal rollout list` (state `AwaitingApproval`) |
| What will this rollout do? | `pk-metal rollout show <rollout>` |
| Why does the rollout not go on? | its `Paused` and `Progressing` conditions, shown by `rollout show` |
| Does node X run what the operator renders? | TalosNode `ConfigInSync`, `status.drift` |
| Which version does node X run? | TalosNode `status.version`, `VersionCurrent` |
| Is the operator holding an old bundle? | TalosCluster `SecretsBundleHeld` |
