# Back up and restore etcd (disaster recovery)

A Talos cluster keeps all of its Kubernetes state in etcd, which runs on the
control-plane nodes. Each control plane keeps its etcd data in its
EPHEMERAL partition. If one control plane loses that data, the others still
have it and etcd keeps working. If most or all of them lose it, the cluster
loses its state: every Deployment, Secret and custom object is gone, even
though the machines still boot.

pk-metal does two things about that:

- It takes etcd snapshots on a schedule (an `EtcdBackupPolicy`), encrypts
  them to your age keys, stores them off the node, and checks each one.
- It restores a cluster from one of them, but only through operations a
  second person approves (`pk-metal restore`).

This page says how to set up backups, how to read what you have, and the
exact commands for each kind of loss. The fields are in the
[API reference](api.md); the [concepts page](concepts-for-sysadmins.md)
explains operations and approval.

## The short version

```sh
# Once per cluster: a policy (see "Set up backups").
kubectl apply -f etcdbackup.yaml

pk-metal backup now lab          # take one now and wait until it is checked
pk-metal backup list lab         # what you have

# Something is wrong:
pk-metal restore plan lab        # says what is wrong and what would fix it
pk-metal restore plan lab --propose
pk-metal approve pk-metal/lab-etcd-recover-node-1          # a second person
pk-metal restore upload pk-metal/lab-etcd-recover-node-1 --identity ~/.config/age/keys.txt
```

## What the operator does and does not do

- Taking a snapshot needs no approval. It reads etcd and changes no
  machine. The operator takes it from a healthy member, a follower if
  there is one, so that the leader is not slowed down.
- Each snapshot is encrypted with [age](https://age-encryption.org) before
  it leaves the operator. The policy names only public keys
  (`age1...`). The operator holds no secret key, so it cannot read its own
  backups, and neither can anyone who steals the bucket or the volume.
  Decrypting happens only on your machine.
- After writing a snapshot, the operator reads it back and checks it. The
  ciphertext must hash to what was written. The plaintext was checked on
  the way in: a valid bbolt database (etcd's file format) and, when etcd
  appended one, a matching sha256 trailer. Only such a snapshot is marked
  Verified, and only a Verified snapshot is restored.
- Old snapshots are removed by the policy's retention (a number to keep
  and, if you like, a maximum age). The newest Verified snapshot is never
  removed, however old it is.
- The operator never restores on its own. A cluster that was bootstrapped
  once and lost its etcd is not bootstrapped again: that would start an
  empty cluster on top of your data. The TalosCluster says so in its
  `Bootstrapped` condition and names this page.
- A restore changes machines, so it runs as HostOperations that wait for a
  second person's approval, like every other machine change.

## Set up backups

### 1. Make age keys

Every person who may restore needs an age key pair. Use the `age-keygen`
from your distribution, or the one that comes with the repository:

```sh
cd metal && go run filippo.io/age/cmd/age-keygen -o ~/.config/age/keys.txt
age-keygen -y ~/.config/age/keys.txt        # prints the public key, age1...
```

Keep `keys.txt` like an SSH private key. Name at least two recipients in
each policy, for example two people, or one person and a key kept offline
in a safe. Otherwise a restore waits for one person.

### 2. Choose where snapshots go (the sink)

| Sink | Where | Use it for |
| --- | --- | --- |
| `s3` | A bucket of any S3-compatible store (MinIO, Ceph RGW, AWS) | Production, and always for the cluster the operator runs on |
| `pvc` | The operator's backup volume, a PersistentVolumeClaim | Clusters the operator manages from outside, when no store is at hand |

The operator refuses a `pvc` sink for the cluster it runs on. That volume
lives in the etcd it would back up and is lost with it. The policy then
shows `Ready=False` with reason `SinkRefused`.

For `s3`, create the bucket and a key that may write, read, list and delete
there and nothing else. Put the key in a Secret in the operator's
namespace:

```sh
kubectl -n pk-metal create secret generic etcd-backup-s3 \
  --from-literal=accessKeyID=... --from-literal=secretAccessKey=...
# Only if the store has a certificate from your own CA:
kubectl -n pk-metal create secret generic etcd-backup-s3-ca --from-file=ca.crt=ca.pem
```

For `pvc`, add the `config/etcd-backup` component to your overlay. It
creates the claim `pk-metal-etcd-backups` (5 GiB) and mounts it into the
manager:

```yaml
components:
  - ../etcd-backup
```

### 3. Create the policy

`metal/config/samples/etcdbackup.yaml` has one of each. A policy for one
cluster, every six hours, to S3:

```yaml
apiVersion: os.pk.io/v1alpha1
kind: EtcdBackupPolicy
metadata:
  name: lab-offsite
spec:
  clusterRef:
    name: lab
  interval: 6h           # at least 5m
  retention:
    count: 28            # default 14
    maxAge: 336h         # optional
  sink:
    s3:
      endpoint: https://s3.example.org
      bucket: etcd-backups
      prefix: metal/
      pathStyle: true
      credentialsSecretRef:
        name: etcd-backup-s3
  encryption:
    ageRecipients:
      - age1...          # person one
      - age1...          # the offline key
```

Then take the first snapshot by hand and check it:

```sh
pk-metal backup now lab-offsite      # or the cluster name, if it has one policy
pk-metal backup list lab
kubectl get etcdbackuppolicy lab-offsite
```

`backup now` waits until the snapshot is stored and verified (`--wait=false`
returns at once). Set `suspend: true` on a policy to pause it without
deleting it.

### What to watch

On the policy:

- `Ready` says whether the policy can work: the cluster exists, the sink and
  its credentials are usable, the recipients parse.
- `BackedUp` is False after a failed snapshot.
- `status.lastSuccessfulSnapshot` names the newest Verified snapshot and
  when it was taken.
- `status.consecutiveFailures` counts failed snapshots since the last good
  one. Alert when it is above zero for longer than one interval.

On each `EtcdSnapshot`: `Complete` (stored or failed), `Verified`, the
member it was taken from, its size, the sha256 of the plaintext and of the
ciphertext, and its location (`s3://bucket/key` or `pvc://claim/key`).

## Read a snapshot on your machine

```sh
# Decrypt and check, without keeping the plaintext:
pk-metal backup decrypt lab-offsite-20261124-060000 --identity ~/.config/age/keys.txt
# Keep the plaintext (mode 0600) for talosctl or etcdutl:
pk-metal backup decrypt lab-offsite-20261124-060000 --identity ~/.config/age/keys.txt -o lab.db
# Only download the ciphertext:
pk-metal backup get lab-offsite-20261124-060000 -o lab.db.age
```

`decrypt` and `get` read an `s3` sink directly with the policy's
credentials, so you need read access to that Secret. A plaintext snapshot
holds every Secret of the cluster: delete it when you are done.

### Getting a snapshot off a pvc sink

The manager image has no shell, so copy the file out with a small helper
pod that mounts the same claim. It runs as the manager's user, which owns
the files, and meets the namespace's `restricted` Pod Security level. On
storage that attaches a volume to one node only, the pod must run on the
manager's node; the commands below put it there.

```sh
node=$(kubectl -n pk-metal get pod -l app.kubernetes.io/name=pk-metal,app.kubernetes.io/component=manager \
  -o jsonpath='{.items[0].spec.nodeName}')
kubectl apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: etcd-backup-reader
  namespace: pk-metal
spec:
  nodeName: $node
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: reader
      image: busybox:1.37
      command: ["sleep", "3600"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: ["ALL"] }
      volumeMounts:
        - { name: backups, mountPath: /backups, readOnly: true }
  volumes:
    - name: backups
      persistentVolumeClaim: { claimName: pk-metal-etcd-backups, readOnly: true }
YAML
kubectl -n pk-metal wait --for=condition=Ready pod/etcd-backup-reader
kubectl -n pk-metal exec etcd-backup-reader -- ls -l /backups/lab
kubectl -n pk-metal exec etcd-backup-reader -- cat /backups/lab/lab-daily-20261124-060000.db.age > lab.db.age
kubectl -n pk-metal delete pod etcd-backup-reader

pk-metal backup decrypt --in lab.db.age --identity ~/.config/age/keys.txt
```

The key is `<cluster>/<snapshot>.db.age`, as the snapshot's `status.key` says.

## Which case is it?

Start every recovery with:

```sh
pk-metal restore plan lab
```

It asks every control plane about its etcd, with the cluster's talosconfig,
and prints what it found and what it would do. It changes nothing. Without
`--propose` it creates nothing either. Every operation it proposes carries
the cluster-guard override (`metal.pk.io/override-cluster-guard`, see
[Day-2 steps](day2-actions.md#one-control-plane-at-a-time)): the guard holds a control
plane while etcd is unhealthy, and a repair starts from exactly that. The
override does not lift the one-control-plane-at-a-time rule, so approve the
operations one after another. It sorts the cluster into one of
these cases:

| What `plan` sees | Case | What it proposes |
| --- | --- | --- |
| Every member healthy | nothing to do | nothing |
| One member's etcd stopped or reports errors; the others still have a majority | [One member lost](#one-member-lost) | replace that member |
| No control plane lists members, or most do not answer | [Majority or everything lost](#majority-or-everything-lost) | reset the stuck members, then restore one from a snapshot |

## One member lost

The cluster still works, because the other members keep the majority. The
broken member is removed from etcd, its EPHEMERAL partition is wiped, and
it joins again as a new member that copies the data from the others. No
snapshot is involved.

```sh
pk-metal restore plan lab
pk-metal restore plan lab --propose
```

`--propose` creates one operation, `lab-etcd-replace-<node>`, with three
steps:

1. `etcdRemoveMember`: a healthy member removes the broken one from the
   member list, so that the majority is counted without it.
2. `talosReset` of EPHEMERAL, not graceful: the node's etcd data and
   kubelet state are wiped and it reboots.
3. `waitEtcdHealthy`: the step waits until the node has joined again and
   every member is healthy.

A second person approves it, then it runs:

```sh
pk-metal approve pk-metal/lab-etcd-replace-node-2
kubectl -n pk-metal get hostoperation lab-etcd-replace-node-2 -w
```

Do this for one member at a time. With three control planes, a second lost
member is a lost majority.

## Majority or everything lost

Without a majority, etcd stops answering, and the cluster's state is only
in your snapshots (and possibly in a stuck member's data). The way back:
wipe every control plane that still runs a stuck etcd, start etcd on one
control plane from a snapshot, and let the others join it.

What is lost: everything written after the snapshot was taken. The plan
prints when the snapshot was taken so that you can decide.

```sh
pk-metal backup list lab
pk-metal restore plan lab                                  # newest verified snapshot
pk-metal restore plan lab --snapshot lab-daily-20261124-060000 --node node-1
pk-metal restore plan lab --snapshot lab-daily-20261124-060000 --node node-1 --propose
```

`--propose` creates:

- `lab-etcd-reset-<node>` for each control plane that still runs etcd: a
  `talosReset` of EPHEMERAL, not graceful.
- `lab-etcd-recover-<node>` with one `etcdRecover` step on the chosen
  control plane. It names the snapshot and its sha256. The approver sees
  both in `pk-metal approve`.

A second person approves them all. Then:

```sh
pk-metal approve pk-metal/lab-etcd-reset-node-2            # second person
pk-metal approve pk-metal/lab-etcd-recover-node-1          # second person

# Once the recover operation is approved, you upload the snapshot:
pk-metal restore upload pk-metal/lab-etcd-recover-node-1 --identity ~/.config/age/keys.txt
# For a pvc sink, copy the file out first (see above) and add:
#   --from-file lab.db.age
```

`restore upload` decrypts the snapshot on your machine. It checks the
plaintext against the snapshot record and against the sha256 that was
approved, and stops if either differs. Then it streams the plaintext to the
node over the Talos API with your talosconfig (the cluster's published
`<cluster>-talosconfig` Secret, or `--talosconfig <file>`), and records the
upload on the operation. The plaintext is never written to disk and never
passes through the operator.

The `etcdRecover` step then:

1. waits until every control plane answers and none runs etcd, so that the
   reset operations are done and the restored etcd will be the only one;
2. waits for your upload;
3. asks Talos on the chosen node to bootstrap etcd from it, once;
4. succeeds when etcd on that node lists its members.

The other control planes then join the restored member on their own. Watch:

```sh
kubectl -n pk-metal get hostoperation -l os.pk.io/etcd-restore=lab
kubectl get taloscluster lab -o jsonpath='{.status.conditions[?(@.type=="Bootstrapped")]}'
```

`Bootstrapped` turns True again when etcd answers. The step's budget is two
hours by default (`timeoutSeconds`); a reset that never finishes or a node
that never answers turns into a failed operation, not a restore beside a
running etcd.

A control plane that is gone for good (a broken disk, a machine you will
not repair) blocks step 1 because it never answers. Remove its TalosNode
from the cluster first, or replace the machine and let it install, then
propose the plan again.

### When the operator's own cluster is lost

If the cluster that runs the operator loses its etcd, the operator, its
EtcdSnapshot records and its view of the machines are gone with it. Only
the snapshots in the S3 bucket are left. That is why the operator refuses
a pvc sink for its own cluster.

Recover it the way it was built in the first place: from a bootstrap
cluster on a workstation (see [pivot](pivot.md)).

1. Start kind and install the operator in it, as for a new site.
2. Re-create the objects from git: the secrets bundle Secret, the
   TalosCluster, its TalosNodes, the Hosts. Add the restore-pending mark to
   the TalosCluster **before** you apply it, so that the operator in kind
   does not bootstrap an empty cluster on your machines:

   ```yaml
   metadata:
     name: mgmt
     annotations:
       os.pk.io/restore-pending: "true"
   ```

   While the mark is set, the TalosCluster's `Bootstrapped` condition says
   `RestorePending`, and the operator does not bootstrap.
3. Get the newest snapshot from the bucket (your S3 client) and check it:

   ```sh
   pk-metal backup decrypt --in mgmt.db.age --identity ~/.config/age/keys.txt -o mgmt.db
   ```

   The kind cluster has no EtcdSnapshot record of it, so `pk-metal restore
   plan` cannot propose an `etcdRecover` here. Restore with talosctl, as
   the Talos documentation describes, using the cluster's talosconfig from
   your records:

   ```sh
   # Every control plane that still runs etcd: wipe its etcd data.
   talosctl -n 192.0.2.12 reset --graceful=false --reboot --system-labels-to-wipe=EPHEMERAL
   # Then on one control plane:
   talosctl -n 192.0.2.11 bootstrap --recover-from=./mgmt.db
   rm mgmt.db
   ```

4. Once `kubectl get taloscluster mgmt` shows `Bootstrapped=True`, remove the
   mark (`kubectl annotate taloscluster mgmt os.pk.io/restore-pending-`).
   The restored cluster still holds the operator and its objects as they
   were when the snapshot was taken. Scale the operator in kind to zero and
   check that the one in the restored cluster runs before anything else.

## Starting over instead

If no snapshot is worth restoring, an empty cluster is the other way. The
operator does not do this on its own: wipe EPHEMERAL on every control plane
(a `talosReset` operation each), then delete and re-create the TalosCluster.
A new TalosCluster was never seen bootstrapped, so the operator bootstraps
it. Every workload must then be deployed again.

## Lab

`lab/lab.sh backup-restore` runs the majority-lost case in the virtual lab:
a pvc sink in kind, a snapshot, a wiped control plane, the plan, the
approval, the upload and the checks. The lab-e2e workflow runs it when its
`backup_restore` input is set. See [the lab README](../../lab/README.md).

## Reference

- `pk-metal backup -h` and `pk-metal restore -h` list every flag.
- [API reference](api.md): `EtcdBackupPolicy`, `EtcdSnapshot`, and the
  `etcdRecover` action of `HostOperation`.
- The single-member replacement uses the Talos steps `etcdRemoveMember`,
  `talosReset` and `waitEtcdHealthy` of `HostOperation`.
