# API Reference

## Packages
- [metal.pk.io/v1alpha1](#metalpkiov1alpha1)
- [os.pk.io/v1alpha1](#ospkiov1alpha1)


## metal.pk.io/v1alpha1

Package v1alpha1 is the metal.pk.io API group: the machines in the
rack, and what the cluster has been asked to do with them.

Two kinds of truth live here and never mix. An Asset is what the organization
asserts about a machine — the serial it was bought under, the MAC of its
management controller, where it was installed. A ManagementEndpoint and a Host
are what the cluster found when it went and looked. Where the two disagree the
disagreement is reported rather than quietly resolved: a Host whose discovered
serial contradicts its Asset stops and says so, because one of the two facts is
wrong and only a human knows which.

A HostClaim asks for a machine the way a PersistentVolumeClaim asks for storage;
a HostOperation is one thing done to one machine — a power cycle, a BIOS apply,
a firmware flash — and the record that it happened. The profile kinds
(BiosProfile, FirmwareProfile, EndpointProfile) are the desired settings a
machine is held against.

Read status through conditions. Every kind reports what is true about it right
now as a set of conditions; the phase and blockedOn fields summarize those for
people and for kubectl's columns. Conditions are the contract — branch on them,
not on the phase string.


**Resources:** [Asset](#asset) · [BiosProfile](#biosprofile) · [BiosSchema](#biosschema) · [BiosSettings](#biossettings) · [EndpointProfile](#endpointprofile) · [FirmwareProfile](#firmwareprofile) · [Host](#host) · [HostClaim](#hostclaim) · [HostOperation](#hostoperation) · [ManagementEndpoint](#managementendpoint)



### Asset



Asset is one physical machine as your records know it: its serial number and
the MAC address of its BMC. You create one per machine, by hand or from your
CMDB or inventory export. It is the entry point: the operator does nothing
with a machine until an Asset names it.

From the bmcMac the operator finds the BMC's address in the ledger and
creates a ManagementEndpoint; through that BMC it finds the machine and
creates a Host. It checks that the serial and MAC it finds are the ones
written here, so a cabling or ledger mistake stops work instead of touching
the wrong machine.

Its status says one thing only: whether the operator could turn it into a
ManagementEndpoint (condition Materialized), and if not, why — most often a
bmcMac that is not in the ledger. Everything the cluster learns by looking at
the machine lands on the ManagementEndpoint and Host. Those kinds keep a copy
of the Asset spec they last resolved, so an outage of whatever authors Assets
degrades to the last known values instead of stopping work.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `Asset` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AssetSpec](#assetspec)_ |  |  |  |
| `status` _[AssetStatus](#assetstatus)_ |  |  | Optional: \{\} <br /> |


### BiosProfile



BiosProfile is a set of BIOS settings that some group of machines should share
— authored in git, applying to whichever Hosts match its selector. Several may
match one machine; they are layered, and the result of that layering is the
machine's BiosSettings, which records which profile each value came from.

Nothing reconciles a BiosProfile directly. It states intent; the settings it
produces are what a machine is held against.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `BiosProfile` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BiosProfileSpec](#biosprofilespec)_ |  |  |  |


### BiosSchema



BiosSchema is the BIOS attribute registry discovered on one machine: which
settings that machine's firmware actually has, what each one accepts, and which
are read-only. It is written by the cluster, not by hand — it is how you find
out what a BiosProfile is allowed to ask for.

Cluster-scoped, named after its Host and owned by it.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `BiosSchema` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BiosSchemaSpec](#biosschemaspec)_ |  |  |  |


### BiosSettings



BiosSettings is one machine's BIOS state: what is wanted, what is actually set,
and where each wanted value came from. It is the rendered result of the
BiosProfiles that match the Host — read it to see what will change and why,
before anything is applied.

Cluster-scoped like its Host, named after it, and owned by it: deleting the
machine takes its settings with it.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `BiosSettings` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BiosSettingsSpec](#biossettingsspec)_ |  |  | Optional: \{\} <br /> |
| `status` _[BiosSettingsStatus](#biossettingsstatus)_ |  |  | Optional: \{\} <br /> |


### EndpointProfile



EndpointProfile is how management controllers themselves should be configured —
their users, network settings, time sources — authored in git and applied to
whichever endpoints match its selector.

Unlike BIOS and firmware there is no approval step: nothing here disturbs the
machine the controller is attached to, so merging the profile is the approval.
When something cannot be applied the endpoint holds and says so on its
Configured condition rather than retrying into a broken state.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `EndpointProfile` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EndpointProfileSpec](#endpointprofilespec)_ |  |  |  |


### FirmwareProfile



FirmwareProfile is the firmware some group of machines should be running —
authored in git, applying to whichever Hosts match its selector. Like
BiosProfile it states intent and is never reconciled directly: a machine that
drifts from it gets a HostOperation proposed against it, and flashing firmware
is disruptive enough that the operation waits for approval.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `FirmwareProfile` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[FirmwareProfileSpec](#firmwareprofilespec)_ |  |  |  |


### Host



Host is one physical machine as the operator found it through its BMC. The
operator creates it and names it after the machine's serial number, in lower
case; the label metal.pk.io/asset names the Asset it was found by. You read
it to see the state of the machine: kubectl get hosts shows the asset,
serial, vendor and phase, and kubectl describe host <name> shows why it
waits.

A HostClaim binds a Host for use. To take a machine out of the operator's
hands for hardware work, set the metal.pk.io/maintenance annotation on it.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `Host` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[HostSpec](#hostspec)_ |  |  | Optional: \{\} <br /> |
| `status` _[HostStatus](#hoststatus)_ |  |  | Optional: \{\} <br /> |


### HostClaim



HostClaim takes one machine for your use. What it does to the machine:

  - Creating it with spec.os proposes installing that TalosNode. Once the
    install is approved, the disk picked by the TalosNode's
    installDiskSelector is erased and overwritten.
  - Deleting it releases the machine. If the operator installed the machine,
    it is shut down: a graceful shutdown first, then forced off after 5
    minutes (the manager's --release-shutdown-timeout). If the machine runs
    a node, the shutdown waits for approval unless
    spec.provisioning.approval is Automatic. A machine the claim never
    installed is left running.
  - Without spec.os the claim only reserves the machine; nothing is done to
    it.

Each proposed step is a HostOperation; with approval Required (the
default) it waits until you run pk-metal approve <namespace>/<operation>.
To stop all proposals for a claim, set the metal.pk.io/hold annotation on
it. After a finished install, status.handOff records which machine runs
which node; the operator does not install it again.

A claim binds exactly one Host, and only one claim can hold a Host. Keep
claims in git, so that taking a machine is a reviewed change.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `HostClaim` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[HostClaimSpec](#hostclaimspec)_ |  |  |  |
| `status` _[HostClaimStatus](#hostclaimstatus)_ |  |  | Optional: \{\} <br /> |


### HostOperation



HostOperation is one step that changes a machine — an install, a shutdown, a
BIOS change, a firmware flash — and the record of what happened. The
operator proposes it; spec.actions lists exactly what will be done, in
order, so read it before you approve.

  - Approve:   pk-metal approve <namespace>/<operation>
    (someone other than the proposer must approve).
  - Refuse:    delete the operation, or set metal.pk.io/hold on the
    HostClaim so the operator stops proposing.
  - Retry:     pk-metal retry <namespace>/<operation> once you have fixed
    the cause. A failed operation is never retried on its own.

kubectl get hostoperations shows what waits (Phase AwaitingApproval) and
what failed; kubectl describe shows each step's message.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `HostOperation` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[HostOperationSpec](#hostoperationspec)_ |  |  |  |
| `status` _[HostOperationStatus](#hostoperationstatus)_ |  |  | Optional: \{\} <br /> |


### ManagementEndpoint



ManagementEndpoint is one BMC — iDRAC, iLO, XCC, iBMC or any other Redfish
controller — that the operator talks to. The operator creates it from an
Asset; you read it to see whether the BMC answers and what it reports
(kubectl get mep).

Its status holds what the BMC reports: vendor, model, serial numbers, the
machines behind it, virtual media slots and firmware. The Verified
condition compares that with the Asset. Phase Mismatch means the BMC at
this address is not the machine the Asset describes, and the operator does
nothing to it; Unreachable means nothing answered at the address.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `metal.pk.io/v1alpha1` | | |
| `kind` _string_ | `ManagementEndpoint` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ManagementEndpointSpec](#managementendpointspec)_ |  |  |  |
| `status` _[ManagementEndpointStatus](#managementendpointstatus)_ |  |  | Optional: \{\} <br /> |



### Supporting types

The shapes the resources above are made of. These are fields of a resource, not
resources themselves — nothing here is created or reconciled on its own.


#### ApplyBiosAction



ApplyBiosAction stages a machine's wanted BIOS attributes onto the firmware's
pending object. Staging on its own changes nothing that is running: BIOS
settings take effect at the next reset, which is why a converging operation
pairs this with a powerCycle. Where the BMC lists OnReset among the
supported apply times (@Redfish.Settings.SupportedApplyTimes), the PATCH
asks for it (@Redfish.SettingsApplyTime): iDRAC 9 applies staged values
only then, through the BIOS configuration job that request creates.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `settingsRef` _[Reference](#reference)_ | SettingsRef names the BiosSettings the attributes came from — provenance<br />only: the executor never reads it, so an edit after approval cannot<br />change what runs. |  | Optional: \{\} <br /> |
| `attributes` _object (keys:string, values:[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#json-v1-apiextensions-k8s-io))_ | Attributes are the values to stage, copied in at proposal time — what<br />the approver saw is exactly what is applied. Keys already current are<br />left out; keys already staged with the wanted value are PATCHed again<br />with the rest, so the staging carries the apply time this client asks<br />for, and a refusal to stage only such keys again counts as staged. |  | MinProperties: 1 <br /> |



#### AssetSnapshot



AssetSnapshot is the copy of the Asset spec that the endpoint last used. It
keeps work going on the last known values if the Asset is briefly gone.



_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cid` _string_ |  |  |  |
| `serial` _string_ |  |  |  |
| `bmcMac` _string_ |  |  |  |
| `vlan` _integer_ |  |  |  |
| `observedGeneration` _integer_ | ObservedGeneration is the Asset generation this snapshot was resolved at. |  |  |


#### AssetSpec



AssetSpec is what your records say about the machine — never what the
operator found by looking. Keeping the two apart is what lets the operator
catch a mistake: it compares what you wrote here with what the BMC reports.



_Appears in:_
- [Asset](#asset)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cid` _string_ | CID is your asset tag or CMDB id for the machine; any unique string. |  | MinLength: 1 <br /> |
| `serial` _string_ | Serial is the machine's serial number as your records have it: the<br />chassis or board serial on the label. The operator compares it with<br />the serial the BMC reports, and does nothing to the machine if they<br />differ. |  | MinLength: 1 <br /> |
| `bmcMac` _string_ | BMCMac is the MAC address of the BMC's own network port, as printed on<br />the label or shown by `ipmitool lan print`. Either case, with ':' or '-'<br />between the bytes. The operator looks it up in the ledger to find the<br />BMC's address, and checks that the BMC at that address reports it. |  | MaxLength: 17 <br />Optional: \{\} <br /> |
| `vlan` _integer_ | VLAN is the management network the BMC is on. It is used only to pick<br />matching EndpointProfiles; the operator does not configure any network. |  | Maximum: 4094 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `credentialsRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | CredentialsRef names a Secret in the operator's namespace with the<br />BMC's login (keys username and password), usually the factory default.<br />An EndpointProfile can later move the BMC to a managed account; this<br />login then stays as the way back in. |  |  |


#### AssetStatus



AssetStatus says whether the Asset became a ManagementEndpoint.



_Appears in:_
- [Asset](#asset)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions: Materialized. True names the endpoint; False says what is<br />missing (NoBMCMac, NoLedgerEntry, LedgerEntryInvalid,<br />MaterializeConflict) and how to fix it. |  | Optional: \{\} <br /> |


#### AwaitCheckInAction



AwaitCheckInAction holds the operation until a TalosNode checks in.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) whose check-in ends the wait:<br />MachineReachable=True over the authenticated machine API, or Ready=True. |  |  |
| `timeoutSeconds` _integer_ | TimeoutSeconds bounds the wait, counted from the previous step's end.<br />Running out fails the step — the media are ejected and the grants<br />revoked either way. | 1800 | Maximum: 86400 <br />Minimum: 60 <br />Optional: \{\} <br /> |



#### BiosProfileSpec



BiosProfileSpec is one layer of desired BIOS configuration for the Hosts it
matches. Profiles are git-authored data — no controller reconciles a profile;
the convergence reconciler materializes the merged result per host into a
BiosSettings object.



_Appears in:_
- [BiosProfile](#biosprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `match` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#labelselector-v1-meta)_ | Match selects Hosts by label. Two label axes exist by construction: intent<br />labels stamped from the bound claim (role: worker — absent on unclaimed<br />machines) and discovered facts (metal.pk.io/vendor, …/model —<br />present regardless of claim state, the axis for hardware baselines). |  |  |
| `layer` _integer_ | Layer orders overlapping profiles: materialization merges matched profiles<br />by (layer, name) and a later layer deliberately overrides an earlier one —<br />a role profile (layer 10) over a hardware baseline (layer 0). Two profiles<br />on the SAME layer setting the same attribute to different values are a<br />conflict, and conflict holds the whole materialization. | 0 | Optional: \{\} <br /> |
| `attributes` _object (keys:string, values:[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#json-v1-apiextensions-k8s-io))_ | Attributes are the BIOS attributes this profile manages, values as the<br />vendor's attribute registry types them (enum string, boolean, integer).<br />An attribute omitted here is unmanaged — a higher layer can override a<br />value but not retract management. |  | MinProperties: 1 <br /> |



#### BiosSchemaAttribute



BiosSchemaAttribute is one registry entry, mirroring the seam's shape.



_Appears in:_
- [BiosSchemaSpec](#biosschemaspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `type` _string_ | Type is the registry's attribute type: Enumeration, Boolean, Integer or<br />String — kept as served. |  |  |
| `allowedValues` _string array_ | AllowedValues enumerates the legal values; empty for non-enumerations. |  | Optional: \{\} <br /> |


#### BiosSchemaSpec



BiosSchemaSpec is the discovered attribute registry of one host's BIOS. Like
Asset it is spec-only — the object IS the data; nothing about it is derived
further. Written by the convergence reconciler from the registry the Bios
resource advertises (registry names are normalized on the client: the spelled
identity differs between the Bios resource, the Registries member, and the
file on both vendors).



_Appears in:_
- [BiosSchema](#biosschema)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `registry` _string_ | Registry is the registry identity as the Bios resource advertises it,<br />kept verbatim (e.g. BiosAttributeRegistry.v1_0_0). |  | Optional: \{\} <br /> |
| `attributes` _[BiosSchemaAttribute](#biosschemaattribute) array_ | Attributes lists the registry's entries, sorted by name. Desired values<br />in a materialized BiosSettings are validated against this before any<br />operation is proposed — the machine would otherwise reject an unknown<br />attribute at PATCH and silently drop an out-of-range value at apply. |  |  |



#### BiosSettingsSpec



BiosSettingsSpec is the rendered desired BIOS state for ONE host — the merge
of every matched BiosProfile, written by the convergence reconciler (its sole
author). Named after its Host and owned by it.



_Appears in:_
- [BiosSettings](#biossettings)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `attributes` _object (keys:string, values:[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#json-v1-apiextensions-k8s-io))_ | Attributes is the merged desired attribute map. Under a profile conflict<br />the previous merge is retained unchanged (alert-and-hold — a conflicted<br />desired state is never partially applied); which profile won each key is<br />status.provenance. |  | Optional: \{\} <br /> |


#### BiosSettingsStatus



BiosSettingsStatus carries the observation side: the managed keys as the
machine last served them, and the plan memory.



_Appears in:_
- [BiosSettings](#biossettings)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observed` _object (keys:string, values:[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#json-v1-apiextensions-k8s-io))_ | Observed caches the managed attributes as last read from the machine —<br />status-as-cache: an unreachable pass keeps the last view. |  | Optional: \{\} <br /> |
| `pending` _object (keys:string, values:[JSON](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#json-v1-apiextensions-k8s-io))_ | Pending caches the managed attributes staged in the @Redfish.Settings<br />object — applied at the next system reset. |  | Optional: \{\} <br /> |
| `provenance` _object (keys:string, values:string)_ | Provenance names the profile that won each attribute — the rendered<br />merge's derivation, so the object answers "what does this host get and<br />why" without re-running the layering in your head. |  | Optional: \{\} <br /> |
| `proposed` _[ProposedPlan](#proposedplan)_ | Proposed is the plan memory for the BIOS domain. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions are the truth — the only thing reconcilers branch on. |  | Optional: \{\} <br /> |


#### CertificateSubject



CertificateSubject is the organizational part of a certificate subject.



_Appears in:_
- [IdentityPolicy](#identitypolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `country` _string_ |  |  | Optional: \{\} <br /> |
| `state` _string_ |  |  | Optional: \{\} <br /> |
| `locality` _string_ |  |  | Optional: \{\} <br /> |
| `organization` _string_ |  |  | Optional: \{\} <br /> |
| `organizationalUnit` _string_ |  |  | Optional: \{\} <br /> |


#### ClaimHandOff



ClaimHandOff is the record of one finished install.



_Appears in:_
- [HostClaimStatus](#hostclaimstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `host` _string_ | Host is the Host the node was installed on. |  |  |
| `node` _string_ | Node is the TalosNode the machine runs. |  |  |
| `configDigest` _string_ | ConfigDigest is sha256:<hex> of the rendered config the machine booted<br />with; empty when the record was rebuilt from an operation that did not<br />carry it. |  | Optional: \{\} <br /> |
| `operation` _string_ | Operation is the boot operation (namespace/name) that installed the<br />machine. Empty when the machine already ran the node when the claim<br />found it: the operator never touched it, so a release leaves its<br />power alone. |  | Optional: \{\} <br /> |
| `at` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | At is when the operator saw the hand-off. |  |  |


#### ClaimOS



ClaimOS names what the machine should run. The node cannot be changed in
place: an install belongs to one node. To switch, remove spec.os (which
stops an install still running) and set it again.



_Appears in:_
- [HostClaimSpec](#hostclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) whose config the machine boots. |  |  |


#### ClaimPhase

_Underlying type:_ _string_

ClaimPhase says how many machines the claim's selection resolved to: none yet
(Pending), exactly one (Bound), or more than one with nothing to choose between
them (Ambiguous). Ambiguous is a stop — the cluster will not guess.

_Validation:_
- Enum: [Pending Bound Ambiguous]

_Appears in:_
- [HostClaimStatus](#hostclaimstatus)

| Field | Description |
| --- | --- |
| `Pending` |  |
| `Bound` |  |
| `Ambiguous` |  |


#### ClaimProvisioning



ClaimProvisioning is the claim's say in how its machine is provisioned.



_Appears in:_
- [HostClaimSpec](#hostclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `approval` _[ProvisioningApproval](#provisioningapproval)_ | Approval: Required (default) or Automatic. | Required | Enum: [Required Automatic] <br />Optional: \{\} <br /> |


#### ClaimReference



ClaimReference points a Host at the namespaced HostClaim holding it.



_Appears in:_
- [HostSpec](#hostspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ |  |  |  |
| `name` _string_ |  |  |  |
| `uid` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#uid-types-pkg)_ |  |  | Optional: \{\} <br /> |


#### CordonAction



CordonAction marks a node unschedulable.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io); its hostname is the Kubernetes<br />Node's name. |  |  |


#### CredentialsPolicy



CredentialsPolicy is the managed-account stanza of an EndpointProfile.



_Appears in:_
- [EndpointProfileSpec](#endpointprofilespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `username` _string_ | Username of the operator's managed account (role Administrator — the<br />executor flashes firmware and resets the manager over it). |  | Pattern: `^[a-z][a-z0-9-]\{0,15\}$` <br /> |
| `maxAge` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | MaxAge rotates the managed password when the Secret grows older than<br />this. Unset rotates only when the Secret is missing or names another<br />account. A rotation sets the new password through the managed<br />account's own session first, so it works when the factory login no<br />longer does. |  | Optional: \{\} <br /> |


#### DiscoveredMedia



DiscoveredMedia is one virtual media slot as the endpoint lists it.



_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `id` _string_ | ID is the slot's Redfish member id ("CD", "VirtualMedia1", …). |  |  |
| `mediaTypes` _string array_ | MediaTypes the slot advertises ("CD", "DVD", "USBStick", …). Empty says<br />nothing — the iBMC's CD slot serves none. |  | Optional: \{\} <br /> |


#### DiscoveredSystem



DiscoveredSystem is one ComputerSystem as seen through this endpoint — the
projection the HostReconciler joins into identity-keyed Hosts. ID is a path
coordinate (an opaque Redfish member id), never an identity; identity is
normalized in internal/identity over the serial anchors.



_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `id` _string_ | ID is the Redfish ComputerSystem member id behind this endpoint. |  |  |
| `uuid` _string_ |  |  | Optional: \{\} <br /> |
| `serialNumber` _string_ | SerialNumber as served by the ComputerSystem — the Huawei anchor; empty on<br />Supermicro X12 (there the chassis board serial anchors). |  | Optional: \{\} <br /> |
| `manufacturer` _string_ | Manufacturer as served by the ComputerSystem — the machine's vendor, which<br />on an aggregator path differs from the endpoint's vendor. |  | Optional: \{\} <br /> |
| `model` _string_ |  |  | Optional: \{\} <br /> |
| `lastResetTime` _[MicroTime](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#microtime-v1-meta)_ | LastResetTime as served by the ComputerSystem — the baseline-and-advance<br />observable (X12 serves it, iBMC doesn't). Micro precision so back-to-back<br />resets stay distinguishable. |  | Optional: \{\} <br /> |
| `bootTargets` _string array_ | BootTargets are the boot override targets the system allows<br />(BootSourceOverrideTarget@Redfish.AllowableValues) — what a proposed<br />boot override picks from, never assumed. |  | Optional: \{\} <br /> |
| `bootOverrideMode` _string_ | BootOverrideMode is the BootSourceOverrideMode the system serves<br />("UEFI" \| "Legacy"); empty when it serves none. A boot override asks for<br />UEFI only where this says otherwise — the X12 reports Legacy on a<br />machine that boots UEFI, and a system that serves no mode may refuse one. |  | Optional: \{\} <br /> |


#### DrainAction



DrainAction evicts a node's pods, respecting PodDisruptionBudgets.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) to drain. |  |  |
| `timeoutSeconds` _integer_ | TimeoutSeconds bounds the whole drain, budget waits included. Running<br />out fails the step and leaves the node cordoned. | 900 | Maximum: 86400 <br />Minimum: 30 <br />Optional: \{\} <br /> |
| `gracePeriodSeconds` _integer_ | GracePeriodSeconds overrides each pod's termination grace period;<br />unset keeps the pod's own. |  | Maximum: 3600 <br />Minimum: 0 <br />Optional: \{\} <br /> |
| `deleteEmptyDirData` _boolean_ | DeleteEmptyDirData lets the drain evict pods with emptyDir volumes,<br />whose data is lost. Without it such a pod fails the step. |  | Optional: \{\} <br /> |


#### EjectMediaAction



EjectMediaAction unmounts a virtual-media slot. Ejecting an empty slot is
success — the goal is an empty slot, not the act of emptying it.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mediaId` _string_ | MediaID is the virtual-media slot to empty. |  |  |


#### EndpointCapabilities



EndpointCapabilities is what this BMC can do, as discovered. Vendors differ
here, and the operator records the difference instead of assuming it.



_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `dualMedia` _boolean_ | DualMedia: two virtual media slots can hold an image at the same time<br />(Supermicro X12 yes, Huawei iBMC no) — needed to mount the installer<br />and the node's config together. |  | Optional: \{\} <br /> |
| `mediaDialects` _object (keys:string, values:string)_ | MediaDialects maps media slot id → insert dialect<br />("insert-media" \| "vmm-control" \| "none"). |  | Optional: \{\} <br /> |
| `updateDialect` _string_ | UpdateDialect is the firmware-update method the BMC offers<br />("multipart-push" \| "simple-update" \| "none"). The operator proposes a<br />firmware flash only over multipart-push, the one it has been tested<br />with. |  | Optional: \{\} <br /> |
| `identityDialect` _string_ | IdentityDialect is how the manager's HTTPS certificate is replaced<br />("csr-native" \| "key-upload" \| "none"): the iBMC generates its CSR on<br />the manager — the key never leaves it — while the X12 takes a<br />locally-generated leaf and key over an OEM upload. |  | Optional: \{\} <br /> |


#### EndpointPhase

_Underlying type:_ _string_

EndpointPhase summarizes an endpoint's conditions, in the order an endpoint
normally moves through them: Pending until it is reachable, then Connecting,
Inventorying and Verifying as the cluster learns what is there, Configuring
while settings are applied, and Ready. Unreachable and Mismatch are the two
stops — the first means nobody answered, the second that what answered was not
the machine the Asset described.

_Validation:_
- Enum: [Pending Connecting Inventorying Verifying Configuring Ready Unreachable Mismatch]

_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description |
| --- | --- |
| `Pending` |  |
| `Connecting` |  |
| `Inventorying` |  |
| `Verifying` |  |
| `Configuring` |  |
| `Ready` |  |
| `Unreachable` |  |
| `Mismatch` |  |



#### EndpointProfileSpec



EndpointProfileSpec is a shared endpoint-configuration class: it selects
ManagementEndpoints by label and declares how the manager itself is run —
the operator's own account, the manager's time sources. Overlap follows
the profile merge rules: profiles order by (layer, name), a later layer
overrides a whole stanza, same-layer disagreement on a stanza holds the
endpoint.



_Appears in:_
- [EndpointProfile](#endpointprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `match` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#labelselector-v1-meta)_ | Match selects ManagementEndpoints by label. Endpoints carry only<br />discovered facts (vendor, model, vlan) — there is no claim, hence no<br />intent axis. An empty selector matches every endpoint. |  |  |
| `layer` _integer_ | Layer orders overlapping profiles: higher layers override lower ones,<br />stanza by stanza. Profiles on the same layer must agree. | 0 | Optional: \{\} <br /> |
| `credentials` _[CredentialsPolicy](#credentialspolicy)_ | Credentials makes the operator run its own account on the manager:<br />created through the asset's factory credentials, password kept in a<br />managed Secret, rotated on cadence. A new password is kept beside the<br />working one (keys next-username, next-password) and replaces it only<br />after a login with it succeeds, so a manager that refuses it leaves the<br />working password in place. The factory account is left untouched: it is<br />the way back in after the managed password was changed behind the<br />operator's back, and the way in at adoption. |  | Optional: \{\} <br /> |
| `ntp` _[NTPPolicy](#ntppolicy)_ | NTP converges the manager's own clock sources. A skewed BMC clock is<br />not cosmetic: it rejects freshly-issued certificates (NotBefore in its<br />future) and mistimes every log line. |  | Optional: \{\} <br /> |
| `tls` _[IdentityPolicy](#identitypolicy)_ | TLS runs the manager's HTTPS identity: a cert-manager-signed<br />certificate for bmc-<endpoint>.<domain>, pushed over the vendor's<br />dialect and renewed on cadence. The factory certificate is replaced<br />and cannot be restored — a CA-signed identity is strictly better and<br />the intended end state. A push restarts the manager, which drops its<br />virtual media, so it waits while an approved operation on a machine<br />behind the endpoint has not finished (reason IdentityPushHeld). |  | Optional: \{\} <br /> |


#### EndpointTLS



EndpointTLS says how a BMC's HTTPS certificate is checked.



_Appears in:_
- [ManagementEndpointSpec](#managementendpointspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `caSecretRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | CASecretRef names a Secret (operator namespace, key ca.crt) with the<br />CA that signed the BMC's certificate. Wins over insecureSkipVerify. |  | Optional: \{\} <br /> |
| `insecureSkipVerify` _boolean_ | InsecureSkipVerify accepts any certificate. For simulated BMCs in the<br />lab only. |  | Optional: \{\} <br /> |


#### EtcdLeaveAction



EtcdLeaveAction makes a control plane's etcd member leave the cluster.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the control-plane TalosNode (os.pk.io) that leaves. |  |  |


#### EtcdRecoverAction



EtcdRecoverAction restores a cluster's etcd from a snapshot onto one
control plane, after the cluster lost all of it.

The snapshot is encrypted to people, not to the operator, so the operator
cannot upload it: after approval a person runs pk-metal restore upload,
which decrypts it on their machine and streams it to the node with their
own talosconfig. The step:

 1. refuses unless the cluster is known to have lost its etcd: it was
    bootstrapped before (status.bootstrappedAt) or is marked
    os.pk.io/restore-pending, and no control plane runs etcd now;
 2. waits for the upload (annotation metal.pk.io/etcd-snapshot-uploaded
    naming the approved sha256);
 3. asks Talos to bootstrap etcd from the upload (Bootstrap with
    RecoverEtcd) and waits until the node lists etcd members.

The other control planes then join the restored etcd by themselves, once
their own etcd data is wiped (a talosReset of EPHEMERAL each).



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the control-plane TalosNode (os.pk.io) etcd is restored<br />on. |  |  |
| `snapshotRef` _[Reference](#reference)_ | SnapshotRef names the EtcdSnapshot (os.pk.io) to restore. |  |  |
| `sha256` _string_ | SHA256 pins the plaintext snapshot (sha256:<hex>) the approver<br />approved: the EtcdSnapshot's record must still say so, and the upload<br />must name it. |  | Pattern: `^sha256:[a-f0-9]\{64\}$` <br /> |
| `timeoutSeconds` _integer_ | TimeoutSeconds bounds the step, the wait for the upload included,<br />counted from the previous step's end (or the approval). | 7200 | Maximum: 86400 <br />Minimum: 300 <br />Optional: \{\} <br /> |


#### EtcdRemoveMemberAction



EtcdRemoveMemberAction removes one etcd member through a peer.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) whose member is removed:<br />provenance for the approver, and how the executor finds the cluster<br />and a peer to ask. |  |  |
| `memberId` _string_ | MemberID is the etcd member ID in hex, as etcd lists it, fixed at<br />proposal: the executor removes exactly this member, never "whatever<br />carries that hostname now", which after a rejoin is the new member. |  | Pattern: `^[0-9a-f]\{1,16\}$` <br /> |
| `hostname` _string_ | Hostname is the member's hostname as etcd listed it at proposal, for<br />the approver. |  | Optional: \{\} <br /> |


#### FirmwareInventoryItem



FirmwareInventoryItem is one UpdateService SoftwareInventory member as
discovered. IDs are vendor-shaped (X12: BMC/Backup_BMC/…, iBMC:
ActiveBMC/…) and kept opaque.



_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `id` _string_ |  |  |  |
| `version` _string_ |  |  | Optional: \{\} <br /> |
| `updateable` _boolean_ |  |  | Optional: \{\} <br /> |



#### FirmwareProfileSpec



FirmwareProfileSpec is one layer of desired firmware for the Hosts it matches.
Like BiosProfile it is controller-less data; the convergence reconciler
compares the desired version against what the fronting endpoints serve and
proposes a flash operation on drift. Firmware converges before BIOS — the
BIOS schema depends on firmware.



_Appears in:_
- [FirmwareProfile](#firmwareprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `match` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#labelselector-v1-meta)_ | Match selects Hosts by label (same two axes as BiosProfile.spec.match). |  |  |
| `layer` _integer_ | Layer orders overlapping profiles exactly as on BiosProfile: (layer, name)<br />merge order, later overrides, same-layer disagreement is a conflict. | 0 | Optional: \{\} <br /> |
| `manager` _[FirmwareTarget](#firmwaretarget)_ | Manager is the desired management-controller firmware. The only firmware<br />target so far — the flash transport is proven for the manager alone<br />(multipart push; endpoints advertising only SimpleUpdate hold at<br />NoUpdateTransport rather than flashing over an unproven path). |  |  |


#### FirmwareTarget



FirmwareTarget pairs a desired version with the image that realizes it.



_Appears in:_
- [FirmwareProfileSpec](#firmwareprofilespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `version` _string_ | Version is the desired firmware version, compared verbatim against the<br />version the endpoint serves — the convergence observable. |  | MinLength: 1 <br /> |
| `image` _string_ | Image is a digest-pinned OCI reference to the firmware image (a single-<br />layer artifact in the internal registry, e.g.<br />registry.pk.internal:5000/firmware/x12-bmc@sha256:…). The digest is<br />required: the executor refuses to flash bytes it cannot verify. |  | Pattern: `@sha256:[a-f0-9]\{64\}$` <br /> |


#### FlashFirmwareAction



FlashFirmwareAction replaces the firmware on a machine's management controller.
The controller reboots as part of it; the machine it manages keeps running.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `image` _string_ | Image is the digest-pinned OCI reference to fetch and push (the same<br />shape as FirmwareProfile.spec.manager.image). |  | Pattern: `@sha256:[a-f0-9]\{64\}$` <br /> |
| `version` _string_ | Version is the version expected to read back after the flash — the<br />convergence goal: a manager already serving it makes this step a no-op. |  | MinLength: 1 <br /> |




#### HostClaimSpec



HostClaimSpec says which machine you want and what it should run. You pick
the machine by its Asset, because you know the Asset name before the
operator has found the machine and named its Host.

The machine a claim names cannot be changed: pointing it elsewhere would
leave the first machine running without a release. To use another machine,
delete the claim and create a new one.



_Appears in:_
- [HostClaim](#hostclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `assetRef` _[Reference](#reference)_ | AssetRef names the Asset whose machine this claim binds. |  |  |
| `systemRef` _[Reference](#reference)_ | SystemRef picks one machine by its Redfish system id when the BMC<br />manages more than one (a multi-node chassis). Leave it out when the BMC<br />manages one machine. |  | Optional: \{\} <br /> |
| `labels` _object (keys:string, values:[LabelValue](#labelvalue))_ | Labels are copied onto the bound Host and removed when the claim goes<br />away, for example role: worker. EndpointProfiles and firmware plans<br />select machines by them. Keys have no prefix (no "/"), so they never<br />clash with the labels the operator sets under metal.pk.io/. |  | MaxProperties: 16 <br />Optional: \{\} <br /> |
| `os` _[ClaimOS](#claimos)_ | OS names the TalosNode the machine should run. Setting it proposes an<br />install that erases the disk chosen by the TalosNode's<br />installDiskSelector. Without it the claim only reserves the machine and<br />nothing is done to it. |  | Optional: \{\} <br /> |
| `provisioning` _[ClaimProvisioning](#claimprovisioning)_ | Provisioning tunes how the operator installs the OS onto the machine. |  | Optional: \{\} <br /> |


#### HostClaimStatus



HostClaimStatus says which machine the claim holds and how far its install
got. The Host's spec.claimRef is what actually holds the machine; the first
claim to set it wins.



_Appears in:_
- [HostClaim](#hostclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hostRef` _[Reference](#reference)_ | HostRef names the bound Host. |  | Optional: \{\} <br /> |
| `handOff` _[ClaimHandOff](#claimhandoff)_ | HandOff records that the machine was installed: which host runs which<br />TalosNode, from which config, since when. It is written once, when the<br />boot finishes, and is the record the operator reads to know the work is<br />done — a node that is down later is not installed again, and deleting<br />old HostOperations changes nothing. It goes away with the claim. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions are the truth — the only thing reconcilers branch on. |  | Optional: \{\} <br /> |
| `phase` _[ClaimPhase](#claimphase)_ | Phase summarizes the conditions below for people and for kubectl columns.<br />It is a summary, not the contract: automation should read conditions. |  | Enum: [Pending Bound Ambiguous] <br />Optional: \{\} <br /> |
| `blockedOn` _string_ | BlockedOn says why the claim is not moving on: why it cannot bind<br />(AssetNotFound, AssetNotMaterialized, HostAlreadyClaimed) or, once<br />bound, why the install waits (Approval(Provisioning), HeldByUser,<br />HostInMaintenance, OperationFailed). kubectl describe shows the message<br />with the command to run. |  | Optional: \{\} <br /> |


#### HostIdentity



HostIdentity identifies the machine, whichever BMC it is reached through:
the same machine seen through two BMCs gets the same identity, and so one
Host. Vendors put the serial number in different places (Huawei on the
system, Supermicro on the board); the operator reads the right one for each.



_Appears in:_
- [HostStatus](#hoststatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `uuid` _string_ |  |  | Optional: \{\} <br /> |
| `serial` _string_ | Serial is the machine's serial number: the board serial on Supermicro,<br />the system serial on Huawei and other vendors. |  | Optional: \{\} <br /> |
| `vendor` _string_ |  |  | Optional: \{\} <br /> |


#### HostOS



HostOS names what the machine runs.



_Appears in:_
- [HostSpec](#hostspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) whose config this machine<br />boots. |  |  |



#### HostOperationAction



HostOperationAction is a one-of union: exactly one action per entry.

_Validation:_
- MaxProperties: 1
- MinProperties: 1

_Appears in:_
- [HostOperationSpec](#hostoperationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `powerCycle` _[PowerCycleAction](#powercycleaction)_ | PowerCycle restarts the system: off, observed off, on, observed on — or<br />just on when it was off at the baseline. Each half converges on the<br />reported power state, which every vendor serves, so a resumed operation<br />never cycles a machine twice (the iBMC serves no reset time to tell). |  | Optional: \{\} <br /> |
| `powerOff` _[PowerOffAction](#poweroffaction)_ | PowerOff turns the system off and waits until it reports off. With<br />gracefulTimeoutSeconds it first asks the OS to shut down, and forces<br />the machine off only if it is still on when that time has passed. |  | Optional: \{\} <br /> |
| `setBootOverride` _[SetBootOverrideAction](#setbootoverrideaction)_ | SetBootOverride sets the source the machine boots from next. The target<br />must be one the endpoint actually offers — machines disagree about what<br />they call a virtual CD, so the value is checked against what was<br />discovered rather than assumed. |  | Optional: \{\} <br /> |
| `insertMedia` _[InsertMediaAction](#insertmediaaction)_ | InsertMedia mounts an image URL on a virtual-media slot, dispatching the<br />dialect the endpoint advertises (standard InsertMedia vs OEM VmmControl). |  | Optional: \{\} <br /> |
| `ejectMedia` _[EjectMediaAction](#ejectmediaaction)_ | EjectMedia unmounts a virtual-media slot. |  | Optional: \{\} <br /> |
| `applyBios` _[ApplyBiosAction](#applybiosaction)_ | ApplyBios stages a BiosSettings object's desired attributes into the<br />machine's pending (@Redfish.Settings) object. Staging changes nothing —<br />the world moves at the next system reset, so a convergence operation<br />pairs this with a powerCycle. |  | Optional: \{\} <br /> |
| `flashFirmware` _[FlashFirmwareAction](#flashfirmwareaction)_ | FlashFirmware pushes a firmware image to the management controller and<br />waits for proof that it took: the controller rebooting, and the expected<br />version reading back afterwards. A reported success is not believed on<br />its own. |  | Optional: \{\} <br /> |
| `awaitCheckIn` _[AwaitCheckInAction](#awaitcheckinaction)_ | AwaitCheckIn waits, without touching the machine, until the named<br />TalosNode reports that the machine runs its config — the point after<br />which the boot media are no longer needed. It changes nothing; it keeps<br />the operation (and the grants it issued) alive until the hand-off. |  | Optional: \{\} <br /> |
| `cordon` _[CordonAction](#cordonaction)_ | Cordon marks the node's Kubernetes Node unschedulable. Already<br />cordoned is success. |  | Optional: \{\} <br /> |
| `drain` _[DrainAction](#drainaction)_ | Drain evicts the pods on the node through the Eviction API, so that<br />PodDisruptionBudgets hold: an eviction a budget refuses is waited out,<br />not forced, until the timeout fails the step. The node stays cordoned<br />either way. A node not yet cordoned is cordoned first. |  | Optional: \{\} <br /> |
| `uncordon` _[UncordonAction](#uncordonaction)_ | Uncordon marks the node schedulable again. Already schedulable is<br />success. |  | Optional: \{\} <br /> |
| `talosReboot` _[TalosRebootAction](#talosrebootaction)_ | TalosReboot asks Talos to reboot the node: services stop and<br />filesystems unmount before the kernel restarts, unlike a BMC<br />powerCycle, which cuts the power. |  | Optional: \{\} <br /> |
| `talosShutdown` _[TalosShutdownAction](#talosshutdownaction)_ | TalosShutdown asks Talos to shut the node down cleanly. |  | Optional: \{\} <br /> |
| `talosUpgrade` _[TalosUpgradeAction](#talosupgradeaction)_ | TalosUpgrade installs another Talos version from an installer image<br />and reboots into it. |  | Optional: \{\} <br /> |
| `talosApplyConfig` _[TalosApplyConfigAction](#talosapplyconfigaction)_ | TalosApplyConfig applies a rendered machine config, the bytes pinned<br />by digest at proposal. |  | Optional: \{\} <br /> |
| `etcdLeave` _[EtcdLeaveAction](#etcdleaveaction)_ | EtcdLeave makes the node's etcd member leave the cluster, asked on the<br />node itself: the orderly removal of a control plane that still<br />answers. |  | Optional: \{\} <br /> |
| `etcdRemoveMember` _[EtcdRemoveMemberAction](#etcdremovememberaction)_ | EtcdRemoveMember removes one etcd member by its ID, asked through a<br />healthy control-plane peer: the path for a member whose node can no<br />longer leave by itself (wiped, dead, unreachable). |  | Optional: \{\} <br /> |
| `talosReset` _[TalosResetAction](#talosresetaction)_ | TalosReset wipes the node through Talos, back towards maintenance<br />mode. |  | Optional: \{\} <br /> |
| `waitNodeReady` _[WaitNodeReadyAction](#waitnodereadyaction)_ | WaitNodeReady waits, changing nothing, until the node runs and is<br />ready: Talos stage running with every readiness condition met, and its<br />Kubernetes Node Ready. |  | Optional: \{\} <br /> |
| `waitEtcdHealthy` _[WaitEtcdHealthyAction](#waitetcdhealthyaction)_ | WaitEtcdHealthy waits, changing nothing, until the etcd cluster of the<br />node's TalosCluster is healthy: every member answers, none reports an<br />error or alarm, and none is a learner. |  | Optional: \{\} <br /> |
| `etcdRecover` _[EtcdRecoverAction](#etcdrecoveraction)_ | EtcdRecover restores the etcd of a cluster that lost all of it from<br />an EtcdSnapshot, on one control plane: a person uploads the decrypted<br />snapshot (pk-metal restore upload), then Talos starts etcd from it.<br />See EtcdRecoverAction and docs/metal/disaster-recovery.md. |  | Optional: \{\} <br /> |


#### HostOperationSpec



HostOperationSpec is what to do to a machine, and the approval that lets it
happen.

The operator proposes an operation by creating it; a person approves it by
setting spec.approval. The machine and the actions cannot be changed after
creation, so the right to edit an operation is only the right to approve
it. The cluster checks who may approve, and refuses an approval by the
person who proposed the operation.



_Appears in:_
- [HostOperation](#hostoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `hostRef` _[Reference](#reference)_ | HostRef names the Host this operation acts on. |  |  |
| `reason` _string_ | Reason names the desired-state gate this operation serves (BiosConvergence,<br />FirmwareConvergence, …); empty for a human-requested op. It becomes the<br />Approval(<reason>) domain on the Host's blockedOn — hence the condition-reason<br />shape. |  | MaxLength: 128 <br />Pattern: `^[A-Za-z][A-Za-z0-9]*$` <br />Optional: \{\} <br /> |
| `proposedBy` _string_ | ProposedBy is the username that created the operation. Admission checks<br />it against the creating request, so it cannot name someone else, and<br />refuses an approval stamped by the same person: nobody approves their<br />own proposal. |  | MinLength: 1 <br /> |
| `planHash` _string_ | PlanHash binds a proposed operation to the desired state it serves — an<br />unchanged hash never spawns a second op (the convergence reconcilers' guard). |  | Optional: \{\} <br /> |
| `actions` _[HostOperationAction](#hostoperationaction) array_ | Actions run in order; each entry is exactly one action. A failed step fails<br />the operation — disruptive one-shots never auto-retry. At most one power<br />action per operation: the power-cycle proof lives in status once. |  | MaxItems: 32 <br />MaxProperties: 1 <br />MinItems: 1 <br />MinProperties: 1 <br /> |
| `approval` _[OperationApproval](#operationapproval)_ | Approval is the human gate. Omitted means required — the safe default is<br />a closed gate. | \{ required:true \} | Optional: \{\} <br /> |
| `ttlSecondsAfterFinished` _integer_ | TTLSecondsAfterFinished garbage-collects a succeeded operation<br />Job-style. A failed one stays until a person retries or deletes it: it<br />is the record they decide on. Unset keeps it forever (human-requested<br />ops stay as their own record);<br />convergence proposals set it — the plan memory on the proposing object,<br />not the operation's existence, is what keeps an unchanged planHash from<br />spawning again. |  | Minimum: 0 <br />Optional: \{\} <br /> |
| `rolloutRef` _[RolloutReference](#rolloutreference)_ | RolloutRef names the rollout (os.pk.io TalosRollout) this operation is<br />one node's part of, and the plan it was created for. The executor<br />refuses an operation whose rollout is gone, not approved, or approved<br />for another planHash: an operation the rollout's approval covers<br />carries no approval of its own, so the binding is what makes that<br />approval mean this operation. Empty for an operation approved on its<br />own. |  | Optional: \{\} <br /> |


#### HostOperationStatus



HostOperationStatus is what happened: which steps ran and how they ended, plus
the observations that prove the machine really did what was asked.



_Appears in:_
- [HostOperation](#hostoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `baseline` _[OperationBaseline](#operationbaseline)_ | Baseline is captured (and persisted) before the first mutating call. |  | Optional: \{\} <br /> |
| `observedResetTime` _[MicroTime](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#microtime-v1-meta)_ | ObservedResetTime is the reset-time observation after the last step — the<br />proof the reset happened, and what marks a Host reset-time advance as<br />explained rather than Drifted. |  | Optional: \{\} <br /> |
| `shutdownRequested` _[MicroTime](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#microtime-v1-meta)_ | ShutdownRequested is when the executor asked the OS to shut down for a<br />graceful powerOff. The step forces the machine off once the graceful<br />window, counted from here, has passed; recording it means a resumed<br />operation neither asks again nor restarts the window. |  | Optional: \{\} <br /> |
| `offObserved` _[MicroTime](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#microtime-v1-meta)_ | OffObserved is when the executor saw the machine report off during a<br />power cycle — the first half of the cycle, recorded so that a resumed<br />operation finishes the cycle instead of starting a second one. |  | Optional: \{\} <br /> |
| `talosStep` _[TalosStepProgress](#talosstepprogress)_ | TalosStep is the progress of the Talos step in flight that restarts or<br />wipes the node (talosReboot, talosUpgrade, talosApplyConfig,<br />talosReset, talosShutdown): the node's boot time before it acted, and<br />when the request was accepted. A resumed operation waits for the boot<br />time to move on instead of asking a second time. |  | Optional: \{\} <br /> |
| `cordonedNode` _string_ | CordonedNode is the Kubernetes Node this operation made unschedulable<br />(a cordon or drain step that found it schedulable). An abort makes it<br />schedulable again; a node a person had cordoned before is left as it<br />was. |  | Optional: \{\} <br /> |
| `steps` _[OperationStep](#operationstep) array_ | Steps records each action's outcome in execution order, written as each<br />step reaches its goal. A resumed operation continues after the last<br />recorded step: a goal that a later step consumed (a Once boot override<br />used up by the boot that followed) is never re-armed. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions are the truth — the only thing reconcilers branch on. |  | Optional: \{\} <br /> |
| `phase` _[OperationPhase](#operationphase)_ | Phase summarizes the conditions below for people and for kubectl columns.<br />It is a summary, not the contract: automation should read conditions. |  | Enum: [Proposed AwaitingApproval Running Succeeded Failed Aborted] <br />Optional: \{\} <br /> |
| `blockedOn` _string_ | BlockedOn is derived — why the operation isn't advancing (AwaitingApproval,<br />OperationQueued). |  | Optional: \{\} <br /> |


#### HostPhase

_Underlying type:_ _string_

HostPhase summarizes a machine's conditions together with whether it is
claimed. Enrolling and Inspecting mean the cluster is still finding out what
the machine is; Available means it is understood and free; Assigned means
something claimed it; Provisioning means work is happening to it right now and
Provisioned that the work finished; Releasing means it is being handed back.
Failed and Maintenance are the two stops: Failed when an install did not
work, Maintenance while a person has set the metal.pk.io/maintenance
annotation.

Assigned and Provisioning are deliberately separate. A machine can be spoken
for without anything disruptive happening to it, and conflating the two leaves
no way to say so.

_Validation:_
- Enum: [Enrolling Inspecting Available Assigned Provisioning Provisioned Releasing Failed Maintenance]

_Appears in:_
- [HostStatus](#hoststatus)

| Field | Description |
| --- | --- |
| `Enrolling` |  |
| `Inspecting` |  |
| `Available` |  |
| `Assigned` |  |
| `Provisioning` |  |
| `Provisioned` |  |
| `Releasing` |  |
| `Failed` |  |
| `Maintenance` |  |


#### HostSpec



HostSpec is written by the operator from the HostClaim that binds the
machine; you do not edit it. Everything else about a Host is what the
operator found, not what anyone wrote.



_Appears in:_
- [Host](#host)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `claimRef` _[ClaimReference](#claimreference)_ | ClaimRef names the HostClaim that holds this machine. The first claim<br />to set it wins; any other claim on the same machine waits<br />(HostAlreadyClaimed). The UID pins the exact claim: a claim deleted and<br />created again under the same name is a new claim. |  | Optional: \{\} <br /> |
| `os` _[HostOS](#hostos)_ | OS is the TalosNode this machine should run, copied from the claim and<br />cleared when the claim is released. Until it is set, a bound machine is<br />reserved and nothing is done to it. |  | Optional: \{\} <br /> |


#### HostStatus



HostStatus is what the operator found out about the machine itself.



_Appears in:_
- [Host](#host)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `identity` _[HostIdentity](#hostidentity)_ |  |  | Optional: \{\} <br /> |
| `managedBy` _[ManagementPath](#managementpath) array_ | ManagedBy lists the ways to reach the machine — usually one BMC. |  | Optional: \{\} <br /> |
| `lastResetTime` _[MicroTime](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#microtime-v1-meta)_ | LastResetTime is when the BMC last saw the machine reset. When it moves<br />and no operation explains it, the Drifted condition says somebody reset<br />the machine another way (ipmitool, the BMC web page, the power button).<br />Not every BMC reports it (Huawei iBMC does not); drift is then not<br />detected. |  | Optional: \{\} <br /> |
| `firmwareProposed` _[ProposedPlan](#proposedplan)_ | FirmwareProposed remembers the last firmware plan proposed for this<br />machine, so the same plan is not proposed twice. Written by the<br />firmware lane only. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions are the truth — the only thing reconcilers branch on. |  | Optional: \{\} <br /> |
| `phase` _[HostPhase](#hostphase)_ | Phase summarizes the conditions below for people and for kubectl columns.<br />It is a summary, not the contract: automation should read conditions. |  | Enum: [Enrolling Inspecting Available Assigned Provisioning Provisioned Releasing Failed Maintenance] <br />Optional: \{\} <br /> |
| `blockedOn` _string_ | BlockedOn says why the host is not moving on (Approval(Provisioning),<br />SerialMismatch, HostInMaintenance); a reason, not a phase, because it<br />can happen in any phase. |  | Optional: \{\} <br /> |


#### IdentityPolicy



IdentityPolicy is the HTTPS-identity stanza of an EndpointProfile.



_Appears in:_
- [EndpointProfileSpec](#endpointprofilespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `issuerRef` _[IssuerRef](#issuerref)_ | IssuerRef names the cert-manager issuer (group cert-manager.io) that<br />signs the identity. |  |  |
| `domain` _string_ | Domain completes the identity: CN and DNS SAN are<br />bmc-<endpoint>.<domain>; the endpoint address, when an IP literal,<br />is added as an IP SAN (clients often reach BMCs by IP address). | metal.internal | MinLength: 1 <br />Optional: \{\} <br /> |
| `duration` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | Duration of issued certificates. | 2160h | Optional: \{\} <br /> |
| `renewBefore` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | RenewBefore re-issues when less than this remains on the served<br />certificate. Must stay below duration. | 720h | Optional: \{\} <br /> |
| `subject` _[CertificateSubject](#certificatesubject)_ | Subject carries the organizational attributes of the identity. The<br />iBMC's on-manager CSR generation requires country; everything else is<br />cosmetic. |  | Optional: \{\} <br /> |


#### IdentityPush



IdentityPush records what was last pushed to a management controller, keyed by
the hash of what was wanted at the time. Everything else about identity is
re-derived from the certificate the controller actually serves; this exists for
the one thing that cannot be observed — a push the controller reported success
for but never began serving — so that it is not pushed again forever.



_Appears in:_
- [ManagementEndpointStatus](#managementendpointstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `planHash` _string_ | PlanHash of the desired identity the push belonged to. |  |  |
| `fingerprint` _string_ | Fingerprint (sha256, hex) of the pushed leaf certificate. |  |  |


#### InsertMediaAction



InsertMediaAction mounts an image on one of the machine's virtual-media slots,
the way you would put a disc in a drive attached to it. The image is either a
plain URL or a BootAsset: a secret-free asset mounts from its stable URL, a
secret-bearing one (configISO) is served by the operator's media service
under a grant issued at execution time and revoked when the operation ends.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mediaId` _string_ | MediaID is the virtual-media slot ("CD", "USBStick", …). |  |  |
| `imageUrl` _string_ | ImageURL is what to mount. Already-mounted with the same URL is success —<br />steps converge to their goal, so a resumed operation never double-issues. |  | Optional: \{\} <br /> |
| `bootAssetRef` _[Reference](#reference)_ | BootAssetRef names the BootAsset (os.pk.io) to mount. |  | Optional: \{\} <br /> |
| `digest` _string_ | Digest pins the artifact bytes (sha256:<hex>) the approver saw: an<br />artifact rebuilt since the proposal fails the step instead of mounting<br />content nobody approved. With a digest the asset need not be Ready: the<br />artifact approved is mounted while a newer build runs, and an asset<br />that names no artifact yet makes the step wait. Secret-bearing assets<br />(configISO, embeddedInstaller) only. |  | Pattern: `^sha256:[a-f0-9]\{64\}$` <br />Optional: \{\} <br /> |


#### IssuerRef



IssuerRef names a cert-manager issuer.



_Appears in:_
- [IdentityPolicy](#identitypolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  | MinLength: 1 <br /> |
| `kind` _string_ |  | ClusterIssuer | Enum: [Issuer ClusterIssuer] <br />Optional: \{\} <br /> |


#### LabelValue

_Underlying type:_ _string_

LabelValue is one label value. It must be a valid Kubernetes label value,
so that a value the Host would reject is refused when you apply the claim.

_Validation:_
- MaxLength: 63
- Pattern: `^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`

_Appears in:_
- [HostClaimSpec](#hostclaimspec)




#### ManagementEndpointSpec



ManagementEndpointSpec is written by the operator from the Asset and the
ledger; you do not edit it.



_Appears in:_
- [ManagementEndpoint](#managementendpoint)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `assetRef` _[Reference](#reference)_ | AssetRef names the Asset this endpoint was made from, and whose serial<br />and MAC it is checked against. |  |  |
| `address` _string_ | Address is the BMC's https URL, from the ledger. It is how the operator<br />reaches the machine, not what identifies it: a Host is identified by<br />the serial number the BMC reports. |  | Pattern: `^https://` <br /> |
| `credentialsRef` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#localobjectreference-v1-core)_ | CredentialsRef names the Secret (operator namespace) used to connect,<br />defaulted from the Asset at materialization — the factory credentials,<br />kept as standing break-glass. Once an EndpointProfile rotates the<br />endpoint onto a managed account, every connect prefers the managed<br />Secret and this ref is the repair fallback. |  | Optional: \{\} <br /> |
| `tls` _[EndpointTLS](#endpointtls)_ | TLS says how the BMC's HTTPS certificate is checked, from the manager's<br />--bmc-ca-secret or --bmc-insecure. Unset uses the system's trusted CAs. |  | Optional: \{\} <br /> |


#### ManagementEndpointStatus



ManagementEndpointStatus is what the BMC reports; what your records say is
on the Asset.



_Appears in:_
- [ManagementEndpoint](#managementendpoint)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `managerType` _string_ | ManagerType is the discovered Redfish Manager type, kept opaque<br />(BMC \| EnclosureManager \| RackManager \| …). |  | Optional: \{\} <br /> |
| `vendor` _string_ | Vendor is the discovered manufacturer ("Huawei" \| "Supermicro" \| …). |  | Optional: \{\} <br /> |
| `model` _string_ |  |  | Optional: \{\} <br /> |
| `firmwareVersion` _string_ | FirmwareVersion is the manager's firmware as served. |  | Optional: \{\} <br /> |
| `boardSerial` _string_ | BoardSerial is the chassis board serial — the Supermicro identity anchor<br />(its ComputerSystem serves none; Huawei's matches the system serial). |  | Optional: \{\} <br /> |
| `firmware` _[FirmwareInventoryItem](#firmwareinventoryitem) array_ | Firmware lists the UpdateService inventory — the firmware-convergence<br />observable (desired version vs the entries here), small and bounded<br />(~5 members per vendor). Sorted by id. |  | Optional: \{\} <br /> |
| `systems` _[DiscoveredSystem](#discoveredsystem) array_ | Systems lists the machines (Redfish ComputerSystems) behind this BMC —<br />usually one, several for a chassis with more than one node. It is<br />kept like the rest of status: an unreachable endpoint keeps its<br />last view. |  | Optional: \{\} <br /> |
| `capabilities` _[EndpointCapabilities](#endpointcapabilities)_ |  |  | Optional: \{\} <br /> |
| `virtualMedia` _[DiscoveredMedia](#discoveredmedia) array_ | VirtualMedia lists the media slots in the endpoint's collection order,<br />with the media types each advertises — what a boot picks its slots by<br />(optical first, in this order), next to capabilities.mediaDialects,<br />which says how each is inserted. |  | Optional: \{\} <br /> |
| `asset` _[AssetSnapshot](#assetsnapshot)_ | Asset caches the last-resolved assertion, so an asset-source outage<br />degrades to the snapshot rather than failing resolution. |  | Optional: \{\} <br /> |
| `identityPushed` _[IdentityPush](#identitypush)_ | IdentityPushed is written by the endpoint convergence reconciler<br />alone; the observing endpoint reconciler leaves it untouched — the<br />two share this status with disjoint fields. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions are the truth — the only thing reconcilers branch on. |  | Optional: \{\} <br /> |
| `phase` _[EndpointPhase](#endpointphase)_ | Phase summarizes the conditions below for people and for kubectl columns.<br />It is a summary, not the contract: automation should read conditions. |  | Enum: [Pending Connecting Inventorying Verifying Configuring Ready Unreachable Mismatch] <br />Optional: \{\} <br /> |
| `blockedOn` _string_ | BlockedOn is derived — why the endpoint isn't advancing (e.g. SerialMismatch). |  | Optional: \{\} <br /> |


#### ManagementPath



ManagementPath is one way to reach this host: a BMC and the system behind
it. A machine can have more than one, each reachable or not on its own.



_Appears in:_
- [HostStatus](#hoststatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpointRef` _[Reference](#reference)_ | EndpointRef names the ManagementEndpoint (the BMC). |  |  |
| `systemId` _string_ | SystemID is the Redfish ComputerSystem id behind that BMC. |  |  |
| `reachable` _boolean_ | Reachable says whether this BMC answers. |  |  |


#### NTPPolicy



NTPPolicy is the time-source stanza of an EndpointProfile. Declaring it
enables NTP on the manager against exactly these servers.



_Appears in:_
- [EndpointProfileSpec](#endpointprofilespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `servers` _string array_ | Servers, in preference order. At most three — the iBMC carries exactly<br />three server slots. |  | MaxItems: 3 <br />MinItems: 1 <br />items:MinLength: 1 <br /> |


#### OperationApproval



OperationApproval is the CEL-guarded human gate (a field, not a subresource).



_Appears in:_
- [HostOperationSpec](#hostoperationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `required` _boolean_ | Required: this operation must be approved before it runs (from the<br />disruption policy; explicit false only where the policy waives the gate,<br />and admission lets only the operator or an admin create such an<br />operation). A pointer so that EVERY client that says nothing — YAML<br />omitting the key, a Go zero value — gets the safe default: a closed gate.<br />Frozen: flipping it would be an approval without a name on it. | true | Optional: \{\} <br /> |
| `approvedBy` _string_ | ApprovedBy is stamped by the approver — the ValidatingAdmissionPolicy<br />enforces that it equals the requesting username, is empty at creation, and<br />is write-once. |  | Optional: \{\} <br /> |


#### OperationBaseline



OperationBaseline is the observation captured at execution start — the
re-derivable "did I already reboot for this request" predicate: completion is
the world moving past it, never a remembered step counter.



_Appears in:_
- [HostOperationStatus](#hostoperationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `powerState` _string_ |  |  | Optional: \{\} <br /> |
| `lastResetTime` _[MicroTime](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#microtime-v1-meta)_ | LastResetTime as observed before any action ran. Micro precision: the sim<br />resets faster than a second, and second-precision would blind the advance<br />predicate. |  | Optional: \{\} <br /> |
| `capturedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | CapturedAt is when the baseline was taken — the start of execution,<br />which bounds how long a power transition may take to show. |  | Optional: \{\} <br /> |


#### OperationPhase

_Underlying type:_ _string_

OperationPhase summarizes an operation's conditions. Proposed means it exists
but has not been approved; AwaitingApproval that it is waiting on a human;
Running that steps are executing; Succeeded, Failed and Aborted are
terminal. A failed operation stays for inspection — nothing disruptive is
retried automatically. Aborted means a person stopped it (metal.pk.io/abort).

_Validation:_
- Enum: [Proposed AwaitingApproval Running Succeeded Failed Aborted]

_Appears in:_
- [HostOperationStatus](#hostoperationstatus)

| Field | Description |
| --- | --- |
| `Proposed` |  |
| `AwaitingApproval` |  |
| `Running` |  |
| `Succeeded` |  |
| `Failed` |  |
| `Aborted` |  |


#### OperationStep



OperationStep is one executed action's outcome.



_Appears in:_
- [HostOperationStatus](#hostoperationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `action` _string_ | Action is the union key that ran (powerCycle, insertMedia, …), or<br />"abort" for the record of an abort and what it undid. |  |  |
| `phase` _string_ |  |  | Enum: [Succeeded Failed Aborted] <br /> |
| `message` _string_ |  |  | Optional: \{\} <br /> |
| `finishedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | FinishedAt is when the step reached its goal (or failed). |  | Optional: \{\} <br /> |


#### PowerCycleAction



PowerCycleAction restarts the machine. It takes no fields: there is only one
way to do it, forcibly, and a machine that is already off is turned on instead.



_Appears in:_
- [HostOperationAction](#hostoperationaction)



#### PowerOffAction



PowerOffAction turns the machine off. Already off is success.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `gracefulTimeoutSeconds` _integer_ | GracefulTimeoutSeconds, when set, makes the step ask the OS to shut<br />down first (Redfish GracefulShutdown, the same as a short press of the<br />power button or `ipmitool chassis power soft`), wait this long for the<br />machine to report off, and only then force it off. Unset or 0 forces<br />the machine off at once. A BMC that refuses a graceful shutdown is<br />forced at once too. |  | Maximum: 3600 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### ProposedPlan



ProposedPlan remembers that an operation was proposed for one exact desired
state — the one fact that is not re-derivable from the world (the operation
object may be gone: TTL-GC'd, or deleted by a human as a veto). An unchanged
planHash never spawns a second operation; everything else about the outcome
is read live from the operation or its absence.



_Appears in:_
- [BiosSettingsStatus](#biossettingsstatus)
- [HostStatus](#hoststatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `planHash` _string_ | PlanHash is the sha256 of the desired state the operation serves. |  |  |
| `operation` _string_ | Operation is the proposed HostOperation as namespace/name. |  |  |


#### ProvisioningApproval

_Underlying type:_ _string_

ProvisioningApproval says whether the operations the operator proposes for
this claim (install, and shutting down a running node on release) wait for a
person.

_Validation:_
- Enum: [Required Automatic]

_Appears in:_
- [ClaimProvisioning](#claimprovisioning)

| Field | Description |
| --- | --- |
| `Required` | ProvisioningApprovalRequired: each operation waits for a person to<br />approve it (pk-metal approve). The default, because these operations<br />erase disks and power machines off.<br /> |
| `Automatic` | ProvisioningApprovalAutomatic: the operations run without approval.<br />Use it for machines you can reinstall at any time, where merging the<br />claim is the decision.<br /> |


#### Reference



Reference names another cluster-scoped object of this group.



_Appears in:_
- [ApplyBiosAction](#applybiosaction)
- [AwaitCheckInAction](#awaitcheckinaction)
- [ClaimOS](#claimos)
- [CordonAction](#cordonaction)
- [DrainAction](#drainaction)
- [EtcdLeaveAction](#etcdleaveaction)
- [EtcdRecoverAction](#etcdrecoveraction)
- [EtcdRemoveMemberAction](#etcdremovememberaction)
- [HostClaimSpec](#hostclaimspec)
- [HostClaimStatus](#hostclaimstatus)
- [HostOS](#hostos)
- [HostOperationSpec](#hostoperationspec)
- [InsertMediaAction](#insertmediaaction)
- [ManagementEndpointSpec](#managementendpointspec)
- [ManagementPath](#managementpath)
- [TalosApplyConfigAction](#talosapplyconfigaction)
- [TalosRebootAction](#talosrebootaction)
- [TalosResetAction](#talosresetaction)
- [TalosShutdownAction](#talosshutdownaction)
- [TalosUpgradeAction](#talosupgradeaction)
- [UncordonAction](#uncordonaction)
- [WaitEtcdHealthyAction](#waitetcdhealthyaction)
- [WaitNodeReadyAction](#waitnodereadyaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |


#### RolloutReference



RolloutReference binds an operation to one approved rollout plan.



_Appears in:_
- [HostOperationSpec](#hostoperationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the TalosRollout's name. |  | MinLength: 1 <br /> |
| `planHash` _string_ | PlanHash is the rollout's planHash when this operation was created. |  | MinLength: 1 <br /> |


#### SetBootOverrideAction



SetBootOverrideAction points the next boot at a particular source.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _string_ | Target is the boot source (UsbCd, Cd, Pxe, …) — validated against the<br />endpoint's discovered AllowableValues at execution, not at admission. |  |  |
| `once` _boolean_ | Once limits the override to the next boot (the default); false is Continuous. | true | Optional: \{\} <br /> |
| `mode` _string_ | Mode is the firmware boot mode for the override. Omitted leaves the<br />machine's own setting alone (some firmware rejects the property). |  | Enum: [UEFI Legacy] <br />Optional: \{\} <br /> |


#### SystemPartition

_Underlying type:_ _string_

SystemPartition is a Talos system partition label.

_Validation:_
- Enum: [STATE EPHEMERAL META]

_Appears in:_
- [TalosResetAction](#talosresetaction)



#### TalosApplyConfigAction



TalosApplyConfigAction applies a pinned machine config.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) whose config this is. |  |  |
| `configSecretRef` _[Reference](#reference)_ | ConfigSecretRef names the Secret, in the operator's namespace, that<br />holds the rendered config. |  |  |
| `digest` _string_ | Digest pins the config bytes (sha256:<hex>) the approver saw: a Secret<br />whose content no longer hashes to it fails the step instead of<br />applying a config nobody approved. |  | Pattern: `^sha256:[a-f0-9]\{64\}$` <br /> |
| `mode` _string_ | Mode is how the node takes the config: NoReboot fails when a change<br />needs a reboot, Reboot always reboots, Staged applies at the next<br />boot, Try rolls back after tryTimeoutSeconds unless confirmed, Auto<br />lets the node choose. | NoReboot | Enum: [Auto NoReboot Reboot Staged Try] <br />Optional: \{\} <br /> |
| `tryTimeoutSeconds` _integer_ | TryTimeoutSeconds bounds Try mode; unset is the node's default. |  | Maximum: 3600 <br />Minimum: 10 <br />Optional: \{\} <br /> |


#### TalosRebootAction



TalosRebootAction reboots a node through Talos.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) to reboot. |  |  |
| `mode` _string_ | Mode is Default (kexec or a warm reboot) or PowerCycle (a cold start<br />at the end, for firmware that applies only then). | Default | Enum: [Default PowerCycle] <br />Optional: \{\} <br /> |


#### TalosResetAction



TalosResetAction wipes a node through Talos.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) to reset. |  |  |
| `graceful` _boolean_ | Graceful leaves etcd (on a control plane) and cordons and drains the<br />node before wiping. A node that cannot reach its peers needs false,<br />and its member is then removed by an etcdRemoveMember step. | true | Optional: \{\} <br /> |
| `reboot` _boolean_ | Reboot boots the node into maintenance mode afterwards; false leaves<br />it powered off. |  | Optional: \{\} <br /> |
| `mode` _string_ | Mode picks the disks: All (system and user disks), SystemDisk or<br />UserDisks. | All | Enum: [All SystemDisk UserDisks] <br />Optional: \{\} <br /> |
| `systemPartitions` _[SystemPartition](#systempartition) array_ | SystemPartitions limits the wipe to these system partitions; empty<br />wipes what mode names. STATE holds the machine config and its<br />credentials, EPHEMERAL the pod data and etcd. |  | Enum: [STATE EPHEMERAL META] <br />MaxItems: 3 <br />Optional: \{\} <br /> |


#### TalosShutdownAction



TalosShutdownAction shuts a node down through Talos.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) to shut down. |  |  |
| `force` _boolean_ | Force shuts down even when the node cannot cordon and drain itself<br />first (its Kubernetes API is down). |  | Optional: \{\} <br /> |


#### TalosStepProgress



TalosStepProgress records what one Talos step did before its goal shows.



_Appears in:_
- [HostOperationStatus](#hostoperationstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `step` _integer_ | Step is the index in spec.actions this progress belongs to. |  | Minimum: 0 <br /> |
| `bootTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | BootTime is the node's boot time before the step acted. The step's<br />restart shows as a later boot time. |  | Optional: \{\} <br /> |
| `requestedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | RequestedAt is when the node accepted the request. |  | Optional: \{\} <br /> |
| `mode` _string_ | Mode is how the node said it applies a config (talosApplyConfig):<br />reboot, no-reboot, staged or try. |  | Optional: \{\} <br /> |


#### TalosUpgradeAction



TalosUpgradeAction upgrades a node's Talos.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) to upgrade. |  |  |
| `image` _string_ | Image is the installer image, pinned by digest: what the approver saw<br />is what is installed. |  | Pattern: `@sha256:[a-f0-9]\{64\}$` <br /> |
| `version` _string_ | Version is the Talos version expected to read back afterwards, the<br />step's goal: a node that already reports it is not upgraded again. |  | Pattern: `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` <br /> |
| `stage` _boolean_ | Stage writes the new image and upgrades at the next reboot, for a node<br />whose open files would block an upgrade in place. |  | Optional: \{\} <br /> |
| `preserve` _boolean_ | Preserve keeps the EPHEMERAL partition (pod data, etcd). Talos 1.8<br />and later always preserve it; the field is passed through for older<br />nodes. | true | Optional: \{\} <br /> |
| `force` _boolean_ | Force skips the etcd health check Talos runs before upgrading a<br />control plane. |  | Optional: \{\} <br /> |
| `rebootMode` _string_ | RebootMode is how the reboot into the new image ends. | Default | Enum: [Default PowerCycle] <br />Optional: \{\} <br /> |


#### UncordonAction



UncordonAction marks a node schedulable.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io). |  |  |


#### WaitEtcdHealthyAction



WaitEtcdHealthyAction waits for a cluster's etcd to be healthy.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names a control-plane TalosNode (os.pk.io) of the cluster.<br />Its own member must be listed too, as a voting member: after a join<br />or a reboot, that is the member the wait is for. |  |  |
| `timeoutSeconds` _integer_ | TimeoutSeconds bounds the wait, counted from the previous step's end. | 900 | Maximum: 86400 <br />Minimum: 60 <br />Optional: \{\} <br /> |


#### WaitNodeReadyAction



WaitNodeReadyAction waits for a node to run and be ready.



_Appears in:_
- [HostOperationAction](#hostoperationaction)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode (os.pk.io) to wait for. |  |  |
| `version` _string_ | Version, when set, is also waited for: the Talos version the node must<br />report, the proof an upgrade took. |  | Pattern: `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` <br />Optional: \{\} <br /> |
| `timeoutSeconds` _integer_ | TimeoutSeconds bounds the wait, counted from the previous step's end. | 1800 | Maximum: 86400 <br />Minimum: 60 <br />Optional: \{\} <br /> |



## os.pk.io/v1alpha1

Package v1alpha1 is the os.pk.io API group: the operating system payload
rendered onto the machines the metal group manages.

TalosCluster carries what every member of a cluster shares (the Talos and
Kubernetes versions, the API endpoint and VIP, pod and service networking,
registry mirrors, the reference to the secrets bundle) and reports what the
members form together: whether etcd was bootstrapped, whether the Kubernetes
API answers, and where the admin credentials were published. TalosNode
carries what belongs to one machine (its role, install disk, links, labels
and taints, image schematic) and reports its rendered config and check-in. A
BootAsset is the artifact a machine actually boots: either the generic
install image, shared by every node and free of secrets, or the small
per-node medium carrying that one node's rendered configuration.

The kinds describe intent in this operator's terms, not Talos' document
layout: internal/talos/render turns them into a Talos 1.14 multi-document
machine configuration, and patches reach anything the kinds do not model.


**Resources:** [BootAsset](#bootasset) · [EtcdBackupPolicy](#etcdbackuppolicy) · [EtcdSnapshot](#etcdsnapshot) · [TalosCluster](#taloscluster) · [TalosNode](#talosnode)



### BootAsset



BootAsset is a built boot artifact and how to fetch it: the controller produces
the artifact and reports readiness, digest, and — for grant-free strategies —
a URL.

One-slot BMCs: a BMC with one virtual media slot cannot mount the installer
and a config ISO side by side. For a blank machine behind one, the operator
creates an embeddedInstaller BootAsset named `<node>-embedded`: an installer
built for that node with its machine config inside, mounted in the one slot.
The config is not sent to a machine in maintenance mode, which would take it
from anybody who reaches it; the installer reaches the BMC only through the
grant-protected media service, like a config ISO.

Secrets at rest: the embedded installer holds the node's machine config in
plain text, keys and tokens included. It rests on the media store volume
(the manager's --media-store), readable by the operator's user only, and is
deleted with its BootAsset or when a newer build replaces it. It is not
encrypted: use an encrypted StorageClass for the media store where Secrets
are encrypted at rest.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `os.pk.io/v1alpha1` | | |
| `kind` _string_ | `BootAsset` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BootAssetSpec](#bootassetspec)_ |  |  |  |
| `status` _[BootAssetStatus](#bootassetstatus)_ |  |  | Optional: \{\} <br /> |


### EtcdBackupPolicy



EtcdBackupPolicy takes encrypted etcd snapshots of one TalosCluster on a
schedule and keeps a bounded number of them. Each snapshot is recorded as
an EtcdSnapshot. Taking a snapshot reads etcd and changes no machine, so
it needs no approval; restoring one does (docs/metal/disaster-recovery.md).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `os.pk.io/v1alpha1` | | |
| `kind` _string_ | `EtcdBackupPolicy` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EtcdBackupPolicySpec](#etcdbackuppolicyspec)_ |  |  |  |
| `status` _[EtcdBackupPolicyStatus](#etcdbackuppolicystatus)_ |  |  | Optional: \{\} <br /> |


### EtcdSnapshot



EtcdSnapshot is one encrypted etcd snapshot of a TalosCluster. The backup
policy creates one per schedule; pk-metal backup now creates one at once.
Deleting the object does not delete the stored bytes; retention deletes
both.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `os.pk.io/v1alpha1` | | |
| `kind` _string_ | `EtcdSnapshot` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EtcdSnapshotSpec](#etcdsnapshotspec)_ |  |  |  |
| `status` _[EtcdSnapshotStatus](#etcdsnapshotstatus)_ |  |  | Optional: \{\} <br /> |


### TalosCluster



TalosCluster is one Talos cluster: what its members share, and the
observed state of the cluster they form.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `os.pk.io/v1alpha1` | | |
| `kind` _string_ | `TalosCluster` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[TalosClusterSpec](#talosclusterspec)_ |  |  |  |
| `status` _[TalosClusterStatus](#talosclusterstatus)_ |  |  | Optional: \{\} <br /> |


### TalosNode



TalosNode is one member of a TalosCluster: the machine-specific half of its
configuration, and what the machine reports once it runs.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `os.pk.io/v1alpha1` | | |
| `kind` _string_ | `TalosNode` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[TalosNodeSpec](#talosnodespec)_ |  |  |  |
| `status` _[TalosNodeStatus](#talosnodestatus)_ |  |  | Optional: \{\} <br /> |



### Supporting types

The shapes the resources above are made of. These are fields of a resource, not
resources themselves — nothing here is created or reconciled on its own.



#### BootAssetSpec



BootAssetSpec declares one boot artifact. The strategy decides which of the
remaining fields apply; the CEL rules on the kind enforce the pairing.



_Appears in:_
- [BootAsset](#bootasset)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `strategy` _[BootAssetStrategy](#bootassetstrategy)_ | Strategy selects how the artifact is produced. |  | Enum: [configISO factory embeddedInstaller] <br /> |
| `nodeRef` _[Reference](#reference)_ | NodeRef names the TalosNode whose rendered config the artifact carries.<br />configISO and embeddedInstaller. |  | Optional: \{\} <br /> |
| `talosVersion` _string_ | TalosVersion pins the release of the install image. factory and<br />embeddedInstaller. |  | Pattern: `^v[0-9]+\.[0-9]+\.[0-9]+$` <br />Optional: \{\} <br /> |
| `schematic` _[Schematic](#schematic)_ | Schematic customizes the install image (system extensions, kernel<br />arguments). factory and embeddedInstaller; omitted means the vanilla<br />image. |  | Optional: \{\} <br /> |


#### BootAssetStatus



BootAssetStatus reports the built artifact. Secret-bearing artifacts never get
a stable URL — their fetch URLs are minted per mount as grants and revoked on
node check-in, eject, or expiry.



_Appears in:_
- [BootAsset](#bootasset)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions: Ready. |  | Optional: \{\} <br /> |
| `url` _string_ | URL is where the artifact is fetched without a grant. factory only. |  | Optional: \{\} <br /> |
| `digest` _string_ | Digest is sha256:<hex> of the artifact bytes. configISO and<br />embeddedInstaller. |  | Optional: \{\} <br /> |
| `size` _integer_ | Size is the artifact size in bytes. configISO and embeddedInstaller. |  | Optional: \{\} <br /> |
| `configDigest` _string_ | ConfigDigest is the rendered config (the TalosNode's<br />status.configDigest) the stored artifact carries. embeddedInstaller<br />only. |  | Optional: \{\} <br /> |
| `buildKey` _string_ | BuildKey names the inputs of the stored artifact: config, Talos<br />version, schematic and imager image. A change of any input is a new<br />key and a new build. embeddedInstaller only. |  | Optional: \{\} <br /> |
| `buildJob` _string_ | BuildJob is the imager Job of the latest build, in the operator's<br />namespace. A failed Job is kept so its log can be read.<br />embeddedInstaller only. |  | Optional: \{\} <br /> |
| `artifactSecretRef` _[SecretReference](#secretreference)_ | ArtifactSecretRef is where the built artifact rests (key asset.iso), owned<br />by this BootAsset and GC'd with it. configISO only. |  | Optional: \{\} <br /> |


#### BootAssetStrategy

_Underlying type:_ _string_

BootAssetStrategy selects how the artifact is produced.

_Validation:_
- Enum: [configISO factory embeddedInstaller]

_Appears in:_
- [BootAssetSpec](#bootassetspec)

| Field | Description |
| --- | --- |
| `configISO` | StrategyConfigISO builds the ~10 KB per-node config medium Talos reads when<br />booted with talos.config=metal-iso: an ISO9660 volume labeled metal-iso<br />holding one config.yaml. Mounted as the second virtual medium beside a<br />generic install image — the `secondary` delivery path. Secret-bearing:<br />served only under a grant.<br /> |
| `factory` | StrategyFactory points at the generic secret-free install ISO on the image<br />factory — content-addressed by schematic, shared across nodes, fetched<br />without a grant.<br /> |
| `embeddedInstaller` | StrategyEmbeddedInstaller builds a per-node install ISO with the node's<br />rendered config built in (Talos reads it from the image, no kernel<br />argument and no second medium needed). It is how a machine whose BMC<br />holds one virtual medium gets its config out of band. The Talos imager<br />builds it in a Job in the operator's namespace; the ISO rests in the<br />operator's media store. Secret-bearing: served only under a grant.<br /> |


#### CNI



CNI selects the pod network plugin.



_Appears in:_
- [ClusterNetwork](#clusternetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _[CNIName](#cniname)_ |  |  | Enum: [flannel none custom] <br /> |
| `urls` _string array_ | URLs are the manifests a custom CNI is installed from. |  | Optional: \{\} <br /> |


#### CNIName

_Underlying type:_ _string_

CNIName selects the pod network plugin Talos deploys.

_Validation:_
- Enum: [flannel none custom]

_Appears in:_
- [CNI](#cni)

| Field | Description |
| --- | --- |
| `flannel` | CNIFlannel deploys Talos' built-in Flannel.<br /> |
| `none` | CNINone deploys nothing: the cluster's own tooling installs a CNI after<br />bootstrap, and nodes stay NotReady until it does.<br /> |
| `custom` | CNICustom applies the manifests at CNI.URLs instead of Flannel.<br /> |


#### ClusterNetwork



ClusterNetwork is the cluster-wide Kubernetes networking.



_Appears in:_
- [TalosClusterSpec](#talosclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podSubnets` _string array_ | PodSubnets are the pod CIDRs, primary family first. Empty selects<br />Talos' default for the endpoint's address family. |  | Optional: \{\} <br /> |
| `serviceSubnets` _string array_ | ServiceSubnets are the service CIDRs, primary family first. Empty<br />selects Talos' default for the endpoint's address family. |  | Optional: \{\} <br /> |
| `cni` _[CNI](#cni)_ | CNI selects the pod network plugin. Empty means flannel. |  | Optional: \{\} <br /> |


#### ConfigMapKeyReference



ConfigMapKeyReference points at one key of a namespaced ConfigMap.



_Appears in:_
- [Patch](#patch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ |  |  |  |
| `name` _string_ |  |  |  |
| `key` _string_ |  |  |  |


#### ControlPlaneSpec



ControlPlaneSpec holds control-plane-only rendering.



_Appears in:_
- [TalosClusterSpec](#talosclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `vip` _string_ | VIP is a shared address the control-plane members announce over layer 2<br />(Talos' Layer2VIPConfig). Each control-plane node carries it on the link<br />whose static address shares a subnet with it. |  | Optional: \{\} <br /> |
| `allowScheduling` _boolean_ | AllowScheduling lets ordinary workloads run on control-plane nodes (the<br />control-plane NoSchedule taint is not rendered). |  | Optional: \{\} <br /> |


#### DiskSelector



DiskSelector says which disk to install onto, and so which disk to erase.
String matchers take glob patterns (*, ?); Match is a Talos disk expression
for anything else. Set at least one; serial or wwid is safest.



_Appears in:_
- [TalosNodeSpec](#talosnodespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serial` _string_ | Serial matches the disk serial number. It names one disk and does not<br />change when disks are re-cabled or found in another order. |  | Optional: \{\} <br /> |
| `model` _string_ | Model matches the disk model. Machines often have several disks of<br />the same model; combine it with another matcher. |  | Optional: \{\} <br /> |
| `wwid` _string_ | WWID matches the disk's world wide identifier. |  | Optional: \{\} <br /> |
| `busPath` _string_ | BusPath matches the disk's bus path. It changes when a disk moves to<br />another slot. |  | Optional: \{\} <br /> |
| `match` _string_ | Match is a CEL expression over `disk` (Talos' disk locator<br />environment), for example `disk.size > 100u * GB && !disk.rotational`. |  | Optional: \{\} <br /> |


#### EtcdBackupEncryption



EtcdBackupEncryption lists the age public keys snapshots are encrypted to.



_Appears in:_
- [EtcdBackupPolicySpec](#etcdbackuppolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ageRecipients` _string array_ | AgeRecipients are age public keys (age1..., from age-keygen -y). Any<br />one of their identities decrypts a snapshot; the operator holds none,<br />so it cannot. Name at least two people or a person and a key kept<br />offline, so that a restore does not hang on one key. Post-quantum<br />keys (age1pq1...) cannot be mixed with classic ones. |  | MaxItems: 16 <br />MinItems: 1 <br />items:Pattern: `^age1[0-9a-z]+$` <br /> |



#### EtcdBackupPolicySpec



EtcdBackupPolicySpec says how often to snapshot one cluster's etcd, how
long to keep the snapshots, where to keep them and whom to encrypt them for.



_Appears in:_
- [EtcdBackupPolicy](#etcdbackuppolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRef` _[Reference](#reference)_ | ClusterRef names the TalosCluster whose etcd is backed up. |  |  |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | Interval is the time between scheduled snapshots (6h, 30m). At least<br />5m: a snapshot reads the whole database from one member. |  |  |
| `retention` _[EtcdBackupRetention](#etcdbackupretention)_ | Retention bounds how many snapshots are kept, and for how long. |  | Optional: \{\} <br /> |
| `sink` _[EtcdBackupSink](#etcdbackupsink)_ | Sink is where the encrypted snapshots are stored. |  |  |
| `encryption` _[EtcdBackupEncryption](#etcdbackupencryption)_ | Encryption names who can read the snapshots. |  |  |
| `suspend` _boolean_ | Suspend stops scheduled snapshots; pk-metal backup now still takes<br />one, and retention still prunes. |  | Optional: \{\} <br /> |


#### EtcdBackupPolicyStatus



EtcdBackupPolicyStatus is the backup state of the cluster.



_Appears in:_
- [EtcdBackupPolicy](#etcdbackuppolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions: Ready (the sink and recipients can be used) and BackedUp<br />(a verified snapshot newer than twice the interval exists). |  | Optional: \{\} <br /> |
| `lastScheduleTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | LastScheduleTime is when the last scheduled snapshot was started. |  | Optional: \{\} <br /> |
| `lastSuccessfulSnapshot` _[SnapshotReference](#snapshotreference)_ | LastSuccessfulSnapshot is the newest verified snapshot. |  | Optional: \{\} <br /> |
| `consecutiveFailures` _integer_ | ConsecutiveFailures counts the snapshots that failed since the newest<br />verified one. |  | Optional: \{\} <br /> |
| `verifiedSnapshots` _integer_ | VerifiedSnapshots counts the verified snapshots kept. |  | Optional: \{\} <br /> |


#### EtcdBackupRetention



EtcdBackupRetention bounds what is kept. Both limits apply. The newest
verified snapshot is never deleted, whatever its age.



_Appears in:_
- [EtcdBackupPolicySpec](#etcdbackuppolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `count` _integer_ | Count is how many verified snapshots to keep, the newest. | 14 | Maximum: 1000 <br />Minimum: 1 <br />Optional: \{\} <br /> |
| `maxAge` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#duration-v1-meta)_ | MaxAge deletes verified snapshots older than this (168h for a week).<br />Unset keeps any age, up to Count. |  | Optional: \{\} <br /> |


#### EtcdBackupSink



EtcdBackupSink is where snapshots go: exactly one of s3 or pvc.



_Appears in:_
- [EtcdBackupPolicySpec](#etcdbackuppolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `s3` _[S3Sink](#s3sink)_ | S3 stores snapshots in a bucket of an S3-compatible object store,<br />off the cluster: the sink for production. |  | Optional: \{\} <br /> |
| `pvc` _[PVCSink](#pvcsink)_ | PVC stores snapshots on the operator's backup volume (the<br />config/etcd-backup component mounts a PersistentVolumeClaim into the<br />manager). Refused for the cluster the operator itself runs on: that<br />volume lives on the etcd it would back up, and is lost with it. |  | Optional: \{\} <br /> |



#### EtcdSnapshotSpec



EtcdSnapshotSpec names the policy a snapshot is taken under: its sink and
recipients.



_Appears in:_
- [EtcdSnapshot](#etcdsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `policyRef` _[Reference](#reference)_ | PolicyRef names the EtcdBackupPolicy. |  |  |


#### EtcdSnapshotStatus



EtcdSnapshotStatus records one snapshot. The object is the catalogue entry;
the bytes live in the sink, at Location.



_Appears in:_
- [EtcdSnapshot](#etcdsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions: Complete (True once stored; False when taking it failed,<br />which is final: the next schedule takes a new one) and Verified (the<br />plaintext is an intact etcd snapshot and the sink holds exactly the<br />ciphertext sent). |  | Optional: \{\} <br /> |
| `clusterName` _string_ | ClusterName is the TalosCluster, copied from the policy when taken. |  | Optional: \{\} <br /> |
| `takenAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | TakenAt is when the snapshot was started. |  | Optional: \{\} <br /> |
| `takenFrom` _string_ | TakenFrom is the control-plane TalosNode whose etcd member streamed<br />it, and MemberID that member's ID (hex). |  | Optional: \{\} <br /> |
| `memberID` _string_ |  |  | Optional: \{\} <br /> |
| `raftIndex` _integer_ | RaftIndex is the member's raft index just before the snapshot: the<br />snapshot holds at least everything up to it. |  | Optional: \{\} <br /> |
| `location` _string_ | Location is where the ciphertext is (s3://bucket/key,<br />pvc://claim/key), and Key its key in the sink. |  | Optional: \{\} <br /> |
| `key` _string_ |  |  | Optional: \{\} <br /> |
| `size` _integer_ | Size and SHA256 describe the plaintext snapshot (the bbolt database<br />and etcd's 32-byte integrity hash after it): what a restore uploads.<br />pk-metal restore checks the decrypted bytes against SHA256. |  | Optional: \{\} <br /> |
| `sha256` _string_ |  |  | Optional: \{\} <br /> |
| `encryptedSize` _integer_ | EncryptedSize and EncryptedSHA256 describe the object in the sink. |  | Optional: \{\} <br /> |
| `encryptedSHA256` _string_ |  |  | Optional: \{\} <br /> |
| `databaseTxID` _integer_ | DatabaseTxID is the bbolt transaction id of the database's newest<br />meta page, and PageSize its page size: read off the snapshot as it<br />passed, as evidence that it is a bbolt database. |  | Optional: \{\} <br /> |
| `pageSize` _integer_ |  |  | Optional: \{\} <br /> |
| `recipients` _string array_ | Recipients are the age public keys it was encrypted to. |  | Optional: \{\} <br /> |


#### Link



Link is one physical link: Talos aliases the link with the MAC address to
Name (LinkAliasConfig), then configures it under that name (LinkConfig, and
DHCPv4Config when DHCP is on). Naming links by alias keeps the rest of the
configuration, and every patch, independent of what the kernel calls the
interface on a given board.



_Appears in:_
- [NodeNetwork](#nodenetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the alias the link gets. Avoid kernel names (eth0, enp0s2):<br />an alias must not collide with a real interface. |  | Pattern: `^[a-z][a-z0-9-]\{0,14\}$` <br /> |
| `hardwareAddr` _string_ | HardwareAddr is the link's permanent MAC address. A physical link<br />whose driver reports no permanent address is matched on its current<br />one instead. Lower case and colon-separated (0c:c4:7a:12:34:56): Talos<br />compares it as text. |  | Pattern: `^([0-9a-f]\{2\}:)\{5\}[0-9a-f]\{2\}$` <br /> |
| `addresses` _string array_ | Addresses are static addresses in CIDR form, primary family first. |  | Optional: \{\} <br /> |
| `routes` _[Route](#route) array_ | Routes are static routes over this link. |  | Optional: \{\} <br /> |
| `dhcp` _boolean_ | DHCP runs a DHCPv4 client on the link, alongside any static addresses. |  | Optional: \{\} <br /> |
| `mtu` _integer_ | MTU overrides the link MTU. |  | Minimum: 576 <br />Optional: \{\} <br /> |


#### NodeNetwork



NodeNetwork is the node's link configuration.



_Appears in:_
- [TalosNodeSpec](#talosnodespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `links` _[Link](#link) array_ | Links configure physical links, each selected by its MAC address. |  | Optional: \{\} <br /> |


#### NodeRole

_Underlying type:_ _string_

NodeRole is the part a machine plays in its cluster.

_Validation:_
- Enum: [controlplane worker]

_Appears in:_
- [TalosNodeSpec](#talosnodespec)

| Field | Description |
| --- | --- |
| `controlplane` |  |
| `worker` |  |


#### PVCSink



PVCSink stores snapshots on the operator's backup volume, under
<cluster>/<snapshot>.db.age. It has no settings: the volume is the one
mounted into the manager.



_Appears in:_
- [EtcdBackupSink](#etcdbackupsink)



#### Patch



Patch is one Talos configuration patch: a multi-document YAML strategic
merge patch (documents merge by apiVersion, kind and name; `$patch: delete`
removes one), given inline or read from a ConfigMap key.



_Appears in:_
- [TalosClusterSpec](#talosclusterspec)
- [TalosNodeSpec](#talosnodespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `inline` _string_ | Inline is the patch text. |  | Optional: \{\} <br /> |
| `configMapRef` _[ConfigMapKeyReference](#configmapkeyreference)_ | ConfigMapRef reads the patch from a ConfigMap key. |  | Optional: \{\} <br /> |


#### Reference



Reference names another cluster-scoped object of this group. Declared here
rather than shared with the metal group, so the two groups can version apart.



_Appears in:_
- [BootAssetSpec](#bootassetspec)
- [EtcdBackupPolicySpec](#etcdbackuppolicyspec)
- [EtcdSnapshotSpec](#etcdsnapshotspec)
- [S3Sink](#s3sink)
- [TalosNodeSpec](#talosnodespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |


#### RegistryMirror



RegistryMirror redirects pulls from one registry to mirror endpoints.



_Appears in:_
- [TalosClusterSpec](#talosclusterspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `registry` _string_ | Registry is the upstream registry host the mirror stands in for<br />(docker.io, ghcr.io, factory.talos.dev), or "*" for every registry. |  | MinLength: 1 <br /> |
| `endpoints` _string array_ | Endpoints are the mirror URLs, tried in order. |  | MinItems: 1 <br /> |


#### Route



Route is a static route.



_Appears in:_
- [Link](#link)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `network` _string_ | Network is the destination CIDR; empty is the default route. |  | Optional: \{\} <br /> |
| `gateway` _string_ | Gateway is the next hop. |  | Optional: \{\} <br /> |
| `metric` _integer_ | Metric orders competing routes. |  | Optional: \{\} <br /> |


#### S3Sink



S3Sink is one bucket of an S3-compatible store.



_Appears in:_
- [EtcdBackupSink](#etcdbackupsink)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | Endpoint is the store's URL, https://host[:port]<br />(https://s3.eu-central-1.amazonaws.com, https://minio.example:9000).<br />http:// works for a store on a trusted network; the snapshots are<br />encrypted either way, the store's credentials are not. |  | Pattern: `^https?://[^/]+/?$` <br /> |
| `bucket` _string_ | Bucket must exist; the operator does not create it. |  | MaxLength: 63 <br />MinLength: 3 <br /> |
| `prefix` _string_ | Prefix goes in front of every key; the key is<br /><prefix><cluster>/<snapshot>.db.age. |  | Pattern: `^([^/].*/)?$` <br />Optional: \{\} <br /> |
| `region` _string_ | Region of the bucket; empty lets the client ask the store. |  | Optional: \{\} <br /> |
| `pathStyle` _boolean_ | PathStyle sends requests as https://host/bucket/key, which most<br />stores other than AWS need. |  | Optional: \{\} <br /> |
| `credentialsSecretRef` _[Reference](#reference)_ | CredentialsSecretRef names a Secret in the operator's namespace with<br />keys accessKeyID and secretAccessKey. Give the key write, read, list<br />and delete on the bucket (or the prefix) and nothing else. |  |  |
| `caSecretRef` _[Reference](#reference)_ | CASecretRef names a Secret in the operator's namespace whose key<br />ca.crt (PEM) replaces the system roots for the endpoint. |  | Optional: \{\} <br /> |


#### Schematic



Schematic is the Image Factory schematic subset this operator renders: the
content its id is a hash of, so every field here changes the image.



_Appears in:_
- [BootAssetSpec](#bootassetspec)
- [TalosClusterSpec](#talosclusterspec)
- [TalosNodeSpec](#talosnodespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `customization` _[SchematicCustomization](#schematiccustomization)_ |  |  | Optional: \{\} <br /> |


#### SchematicCustomization



SchematicCustomization holds the image customization knobs.



_Appears in:_
- [Schematic](#schematic)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `systemExtensions` _[SystemExtensions](#systemextensions)_ |  |  | Optional: \{\} <br /> |
| `extraKernelArgs` _string array_ | ExtraKernelArgs append to the image's kernel command line, for example<br />where to put the console (console=ttyS0, what the BMC's serial-over-LAN<br />shows). Do not add talos.config=metal-iso: the operator adds it to the<br />boot medium itself, and the installed system must not carry it. |  | Optional: \{\} <br /> |


#### SecretReference



SecretReference points at a namespaced Secret from a cluster-scoped object.



_Appears in:_
- [BootAssetStatus](#bootassetstatus)
- [TalosClusterSpec](#talosclusterspec)
- [TalosClusterStatus](#talosclusterstatus)
- [TalosNodeStatus](#talosnodestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ |  |  |  |
| `name` _string_ |  |  |  |


#### SnapshotReference



SnapshotReference names a snapshot and when it was taken.



_Appears in:_
- [EtcdBackupPolicyStatus](#etcdbackuppolicystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `takenAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ |  |  |  |


#### SystemExtensions



SystemExtensions bakes extensions into the image.



_Appears in:_
- [SchematicCustomization](#schematiccustomization)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `officialExtensions` _string array_ | OfficialExtensions are siderolabs/* extension names (siderolabs/drbd). |  | Optional: \{\} <br /> |


#### Taint



Taint is a Kubernetes node taint.



_Appears in:_
- [TalosNodeSpec](#talosnodespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ |  |  |  |
| `value` _string_ |  |  | Optional: \{\} <br /> |
| `effect` _[TaintEffect](#tainteffect)_ |  |  | Enum: [NoSchedule PreferNoSchedule NoExecute] <br /> |


#### TaintEffect

_Underlying type:_ _string_

TaintEffect is a Kubernetes taint effect.

_Validation:_
- Enum: [NoSchedule PreferNoSchedule NoExecute]

_Appears in:_
- [Taint](#taint)




#### TalosClusterSpec



TalosClusterSpec is what every member of one Talos cluster shares. The cluster
name is the object's metadata.name.



_Appears in:_
- [TalosCluster](#taloscluster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `talosVersion` _string_ | TalosVersion is the Talos release the cluster runs. It selects the<br />configuration contract the render targets and the installer image tag, so<br />it must match what the machines boot. Talos v1.14 or later. |  | Pattern: `^v1\.(1[4-9]\|[2-9][0-9])\.[0-9]+$` <br /> |
| `kubernetesVersion` _string_ | KubernetesVersion is the Kubernetes release the control plane runs. |  | Pattern: `^v[0-9]+\.[0-9]+\.[0-9]+$` <br /> |
| `endpoint` _string_ | Endpoint is the Kubernetes API URL every member enrolls against and the<br />admin kubeconfig names: the control-plane VIP or a DNS name for it, with<br />the port (https://10.77.0.10:6443). |  | Pattern: `^https://[^/]+$` <br /> |
| `controlPlane` _[ControlPlaneSpec](#controlplanespec)_ | ControlPlane carries the settings only control-plane members render. |  | Optional: \{\} <br /> |
| `network` _[ClusterNetwork](#clusternetwork)_ | Network is the cluster's pod and service networking. |  | Optional: \{\} <br /> |
| `certSANs` _string array_ | CertSANs are extra names the Kubernetes API server and the Talos API<br />certificates must be valid for, beyond the endpoint, the VIP and the<br />node addresses Talos adds on its own. |  | Optional: \{\} <br /> |
| `registryMirrors` _[RegistryMirror](#registrymirror) array_ | RegistryMirrors redirect image pulls, one entry per upstream registry. A<br />site without internet access needs them before the first node installs,<br />which is why they live here rather than in a patch someone has to<br />remember. |  | Optional: \{\} <br /> |
| `secretsRef` _[SecretReference](#secretreference)_ | SecretsRef names the Secret holding the cluster's Talos secrets bundle<br />under key secrets.yaml, as `talosctl gen secrets` writes it. A person<br />generates the bundle once and commits it sealed; the operator only reads<br />it. Every certificate and key in a rendered config, and the<br />kubeconfig and talosconfig the operator publishes, derive from it. |  |  |
| `schematic` _[Schematic](#schematic)_ | Schematic is the installer image customization every node uses unless it<br />sets its own (a node's schematic replaces this one, it does not merge<br />with it). |  | Optional: \{\} <br /> |
| `patches` _[Patch](#patch) array_ | Patches apply to every node, in order, after the operator's own<br />rendering and before the node's patches. |  | Optional: \{\} <br /> |


#### TalosClusterStatus



TalosClusterStatus is observed: whether etcd was bootstrapped, whether the
Kubernetes API answers, how many members are Ready, and where the admin
credentials derived from the secrets bundle were published.



_Appears in:_
- [TalosCluster](#taloscluster)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `bootstrappedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#time-v1-meta)_ | BootstrappedAt is when the operator first observed this cluster's etcd<br />running (or its Kubernetes API answering). It is the one remembered<br />fact in this status: once set the operator never bootstraps the<br />cluster again, so that a cluster whose control planes all lost their<br />etcd data is restored by a person instead of silently started empty<br />(Bootstrapped=False, reason EtcdDataLost). Write-once; starting over<br />means deleting and re-creating the TalosCluster. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions: Bootstrapped and Available. |  | Optional: \{\} <br /> |
| `controlPlanes` _integer_ | ControlPlanes and Workers count the TalosNodes that name this cluster;<br />the Ready counts are the ones whose Ready condition is True. |  | Optional: \{\} <br /> |
| `readyControlPlanes` _integer_ |  |  | Optional: \{\} <br /> |
| `workers` _integer_ |  |  | Optional: \{\} <br /> |
| `readyWorkers` _integer_ |  |  | Optional: \{\} <br /> |
| `kubernetesVersion` _string_ | KubernetesVersion is the version the Kubernetes API reports, empty<br />while it does not answer. |  | Optional: \{\} <br /> |
| `kubeconfigSecretRef` _[SecretReference](#secretreference)_ | KubeconfigSecretRef points at the Secret (key kubeconfig) holding an<br />admin kubeconfig minted from the bundle's Kubernetes CA. |  | Optional: \{\} <br /> |
| `talosconfigSecretRef` _[SecretReference](#secretreference)_ | TalosconfigSecretRef points at the Secret (key talosconfig) holding an<br />admin talosconfig minted from the bundle's Talos CA, with every<br />control-plane address as an endpoint. |  | Optional: \{\} <br /> |



#### TalosNodeSpec



TalosNodeSpec is what belongs to one machine. Its hostname is the object's
metadata.name.



_Appears in:_
- [TalosNode](#talosnode)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterRef` _[Reference](#reference)_ | ClusterRef names the TalosCluster this node is a member of. |  |  |
| `role` _[NodeRole](#noderole)_ | Role is controlplane or worker. |  | Enum: [controlplane worker] <br /> |
| `address` _string_ | Address is where the operator and talosctl reach the node's Talos API.<br />Empty means the first static address of the first link. A node that only<br />takes DHCP addresses needs it set, or its check-in has nothing to dial. |  | Optional: \{\} <br /> |
| `installDiskSelector` _[DiskSelector](#diskselector)_ | InstallDiskSelector picks the disk Talos installs onto. That disk is<br />repartitioned and everything on it is erased. A disk matches when every<br />matcher you set holds; if several disks match, Talos takes the first one<br />it finds, which can change between boots. Prefer serial or wwid, which<br />name exactly one disk. |  |  |
| `network` _[NodeNetwork](#nodenetwork)_ | Network configures the node's links. |  | Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | Labels are Kubernetes node labels the kubelet registers with. Worker<br />nodes may only set labels the NodeRestriction admission plugin allows. |  | Optional: \{\} <br /> |
| `taints` _[Taint](#taint) array_ | Taints are Kubernetes node taints the kubelet registers with. |  | Optional: \{\} <br /> |
| `schematic` _[Schematic](#schematic)_ | Schematic customizes this node's installer image and replaces the<br />cluster's schematic when set. Extensions and kernel arguments both live<br />here: Talos takes the installed kernel command line from the image, so an<br />argument that is not part of the schematic does not survive the install. |  | Optional: \{\} <br /> |
| `patches` _[Patch](#patch) array_ | Patches apply to this node only, in order, after the cluster's patches. |  | Optional: \{\} <br /> |


#### TalosNodeStatus



TalosNodeStatus reports the render and the machine's check-in. Rendered=False
is alert-and-hold: the last rendered Secret stays in place, so a broken input
never un-renders a machine's config.



_Appears in:_
- [TalosNode](#talosnode)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.36/#condition-v1-meta) array_ | Conditions: Rendered, plus the check-in observations MachineReachable<br />and Ready while a Host hands this node a machine. |  | Optional: \{\} <br /> |
| `configSecretRef` _[SecretReference](#secretreference)_ | ConfigSecretRef points at the Secret holding this node's rendered<br />machine config (key config.yaml). Owned by the TalosNode and collected<br />with it; it is the one place the rendered secrets rest. |  | Optional: \{\} <br /> |
| `configDigest` _string_ | ConfigDigest is sha256:<hex> of the rendered config, what BootAsset<br />builds key their rebuilds on. |  | Optional: \{\} <br /> |
| `installerImage` _string_ | InstallerImage is the installer the rendered config installs from. |  | Optional: \{\} <br /> |
| `address` _string_ | Address is where the check-in dials (spec.address, or the first static<br />link address). |  | Optional: \{\} <br /> |
| `stage` _string_ | Stage is the machine's observed boot stage (maintenance, booting,<br />installing, running, ...), empty while no machine answers. |  | Optional: \{\} <br /> |
| `version` _string_ | Version is the Talos version the machine reports. |  | Optional: \{\} <br /> |


