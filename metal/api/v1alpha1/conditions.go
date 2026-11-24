package v1alpha1

// Condition types. Each is a fresh observation of something true right now,
// carrying the generation it was observed against — never a step counter marking
// how far a process got. Read these; the phase fields are a summary of them.
const (
	// ConditionReachable: the management endpoint answers Redfish over its address.
	ConditionReachable = "Reachable"
	// ConditionInventoried: discovery has populated the endpoint's status facts.
	ConditionInventoried = "Inventoried"
	// ConditionVerified: org assertion matches discovery (Asset serial vs Redfish —
	// two independent origins). False is alert-and-hold, never destructive.
	ConditionVerified = "Verified"
	// ConditionConfigured: the matched EndpointProfile is applied (creds/TLS/NTP).
	ConditionConfigured = "Configured"

	// ConditionInspected: the host's hardware inventory is gathered.
	ConditionInspected = "Inspected"
	// ConditionFirmwareConverged: firmware matches the matched FirmwareProfile.
	// Firmware converges before BIOS — the BIOS schema depends on firmware.
	ConditionFirmwareConverged = "FirmwareConverged"
	// ConditionBiosConverged: BIOS settings match the materialized BiosSettings.
	ConditionBiosConverged = "BiosConverged"
	// ConditionBooted: the OS payload has been handed off and the machine came up.
	ConditionBooted = "Booted"
	// ConditionOSReady: the node reports OS-level readiness (Talos machine status).
	ConditionOSReady = "OSReady"
	// ConditionDrifted: the world moved with no active HostOperation (e.g. a
	// LastResetTime advance) — information, not error.
	ConditionDrifted = "Drifted"
	// ConditionApprovalPending: a proposed HostOperation awaits its human gate.
	ConditionApprovalPending = "ApprovalPending"
	// ConditionMaintenance: a person put the host in maintenance (annotation
	// metal.pk.io/maintenance); the message is their reason. Absent otherwise.
	ConditionMaintenance = "Maintenance"

	// ConditionBound: the HostClaim holds its Host (the authoritative pointer is
	// Host.spec.claimRef; this condition is the claim's view of it).
	ConditionBound = "Bound"
	// ConditionProvisioned: on a HostClaim with OS intent — the bound machine
	// runs the named node's config. False carries what the operator waits on
	// or why it holds; True means the hand-off happened.
	ConditionProvisioned = "Provisioned"
	// ConditionDeletionHeld: on a Host or ManagementEndpoint. A person deleted
	// it, but a claim still holds the machine. The object stays (finalizer
	// metal.pk.io/claim-protection) until the claim releases the machine.
	ConditionDeletionHeld = "DeletionHeld"

	// ConditionConverged: on a BiosSettings — the machine's managed attributes
	// match the materialized desired state. The Host's BiosConverged mirrors it.
	ConditionConverged = "Converged"

	// ConditionApproved: the HostOperation's human gate. False is AwaitingApproval;
	// True carries who (or that the disruption policy waived it).
	ConditionApproved = "Approved"
	// ConditionExecuted: the HostOperation's outcome. Unknown while queued or
	// executing; a False is terminal — disruptive one-shots never auto-retry.
	ConditionExecuted = "Executed"
)

// Reasons on the operation's Approved / Executed conditions.
const (
	// ReasonAwaitingApproval on Approved=False: the gate is required and unstamped.
	ReasonAwaitingApproval = "AwaitingApproval"
	// ReasonApproved on Approved=True: a human stamped approvedBy, and admission
	// confirmed they stamped it as themselves.
	ReasonApproved = "Approved"
	// ReasonApprovalNotRequired on Approved=True: the disruption policy waived the gate.
	ReasonApprovalNotRequired = "ApprovalNotRequired"
	// ReasonOperationQueued on Executed=Unknown: another operation holds the host —
	// at most one runs per Host.
	ReasonOperationQueued = "OperationQueued"
	// ReasonExecuting on Executed=Unknown: the baseline is persisted and steps are
	// being issued.
	ReasonExecuting = "Executing"
	// ReasonStepsSucceeded on Executed=True: every action converged to its goal.
	ReasonStepsSucceeded = "StepsSucceeded"
	// ReasonStepFailed on Executed=False: a step failed — terminal, a human decides.
	ReasonStepFailed = "StepFailed"
	// ReasonHostNotFound on Executed=Unknown: hostRef names no Host (yet).
	ReasonHostNotFound = "HostNotFound"
	// ReasonHostNotClaimed on Executed=False: the boot of a claim was about to
	// start, but the claim is in deletion or no longer holds the host. The
	// operation did not touch the machine.
	ReasonHostNotClaimed = "HostNotClaimed"
	// ReasonNoReachablePath on Executed=Unknown: every management path is down.
	ReasonNoReachablePath = "NoReachablePath"
	// ReasonClusterDisruption on Executed=Unknown: the operation would take
	// down a control plane of a Talos cluster. But another control plane of
	// that cluster is going down, or etcd is not fully healthy without this
	// member. The operation starts when that is over.
	ReasonClusterDisruption = "ClusterDisruption"
	// ReasonAborted on Executed=False: a person asked the operation to stop
	// (annotation metal.pk.io/abort, pk-metal abort). It stopped between two
	// steps and undid what it could; the message says what.
	ReasonAborted = "Aborted"
)

// Reasons on the host's Booted / OSReady conditions. Both mirror the bound
// TalosNode's check-in observations (MachineAPI, MaintenanceMode, NodeReady,
// Converging pass through verbatim); these two originate on the Host side.
const (
	// ReasonNodeNotFound on OSReady=Unknown: the machine was told to run an OS
	// whose TalosNode does not exist — provisioning is engaged but there is
	// nothing to render. Someone named a node that was never authored.
	ReasonNodeNotFound = "NodeNotFound"
	// ReasonAwaitingContact on OSReady=Unknown: no check-in observation yet.
	ReasonAwaitingContact = "AwaitingContact"
)

// Reasons on the convergence conditions (BiosSettings' Converged, the Host's
// FirmwareConverged / BiosConverged). Each hold alerts and holds: the
// condition blocks, and nothing destructive follows. An unchanged planHash
// never makes a second operation. To continue, a person changes the desired
// state (a profile edit), which changes the hash.
const (
	// ReasonInSync on Converged=True: observed matches desired.
	ReasonInSync = "InSync"
	// ReasonAwaitingOperation on Converged=False: drift is real and the proposed
	// operation exists but has not finished (it may still await its human gate —
	// the Host's ApprovalPending says so).
	ReasonAwaitingOperation = "AwaitingOperation"
	// ReasonOperationFailed on Converged=False: the proposed operation failed —
	// terminal for this plan, one-shots never auto-retry.
	ReasonOperationFailed = "OperationFailed"
	// ReasonOperationGone on Converged=False: the proposed operation no longer
	// exists while drift persists — a human deleted the proposal (a veto).
	ReasonOperationGone = "OperationGone"
	// ReasonConvergenceFailed on Converged=False: the operation succeeded, but
	// the machine does not show the desired state. For example, a flash
	// reported success but changed nothing, or a BIOS apply lost the value.
	ReasonConvergenceFailed = "ConvergenceFailed"
	// ReasonProfileConflict on Converged=False: two same-layer profiles set the
	// same key to different values — the materialization holds whole.
	ReasonProfileConflict = "ProfileConflict"
	// ReasonSchemaViolation on Converged=False: a desired value is not in the
	// discovered attribute registry (an unknown attribute, or a value out of
	// the enum). The machine would refuse or ignore it, so the operator
	// proposes nothing.
	ReasonSchemaViolation = "SchemaViolation"
	// ReasonAwaitingFirmware on Converged=False: BIOS holds until firmware
	// converges — the BIOS schema depends on firmware.
	ReasonAwaitingFirmware = "AwaitingFirmware"
	// ReasonNoUpdateTransport on FirmwareConverged=False: no fronting endpoint
	// advertises the proven multipart-push dialect — drift is visible but no
	// flash is proposed over an unproven transport.
	ReasonNoUpdateTransport = "NoUpdateTransport"
)

// Reasons on the endpoint's Configured condition (the EndpointProfile lanes).
// InSync and ProfileConflict are shared with the convergence vocabulary above.
// There is no operation gate here — merging the profile is the approval — so
// every False is either in-flight work or an alert-and-hold the next pass
// retries (the factory account stays as break-glass, faults self-heal).
const (
	// ReasonAwaitingVerification on Configured=Unknown: a profile matches but
	// the endpoint is not Verified — nothing is written to an unproven box.
	ReasonAwaitingVerification = "AwaitingVerification"
	// ReasonRotationFailed on Configured=False: the operator could not set the
	// password of the managed Secret on the managed account, or the BMC did not
	// accept a new password. Neither the managed nor the factory session set a
	// password that logs in. The working password stays in the Secret.
	ReasonRotationFailed = "RotationFailed"
	// ReasonNTPFailed on Configured=False: the manager's time sources could
	// not be converged — the write failed or did not read back.
	ReasonNTPFailed = "NTPFailed"
	// ReasonNoIdentityTransport on Configured=False: an identity is desired
	// but the endpoint advertises no way to replace its certificate.
	ReasonNoIdentityTransport = "NoIdentityTransport"
	// ReasonAwaitingIssuance on Configured=False: the CertificateRequest is
	// created and not yet signed.
	ReasonAwaitingIssuance = "AwaitingIssuance"
	// ReasonIdentityPushed on Configured=False: the signed certificate went
	// to the manager and the manager was restarted — the next observation
	// decides whether it is being served.
	ReasonIdentityPushed = "IdentityPushed"
	// ReasonIdentityPushFailed on Configured=False: the push did not land or
	// the pushed certificate is not being served (success answered, nothing
	// changed — the lying-oracle shape). Held per desired state: a profile
	// edit re-arms the push.
	ReasonIdentityPushFailed = "IdentityPushFailed"
	// ReasonIdentityPushHeld on Configured=False: the signed certificate is
	// ready, but pushing it restarts the manager, and an operation is running
	// on a machine behind it. A restart drops the virtual media mounts and
	// the session of the step in flight, so the push waits for the operation
	// to finish.
	ReasonIdentityPushHeld = "IdentityPushHeld"
)

// Reasons on the host's Drifted condition (information, not error).
const (
	// ReasonUnexpectedReset on Drifted=True: the reset-time observation advanced
	// with no operation explaining it — someone or something reset the machine.
	ReasonUnexpectedReset = "UnexpectedReset"
	// ReasonResetExplained on Drifted=False: the latest advance is covered by an
	// operation's observation.
	ReasonResetExplained = "ResetExplained"
)

// Reasons on the claim's Provisioned condition. All but HandedOff are False:
// in-flight waits (AwaitingRender, AwaitingBootAsset, AwaitingOperation,
// Releasing), the install operation's gate (AwaitingApproval, shown as
// blockedOn Approval(Provisioning)), or alert-and-hold stops
// (SingleMediumUnsupported, NoBootableMedia, OperationFailed, ReleaseFailed)
// that a person resolves.
const (
	// ReasonHandedOff on Provisioned=True: the machine checked in with the
	// node's config.
	ReasonHandedOff = "HandedOff"
	// ReasonAwaitingRender: the TalosNode (or its cluster) is missing or has
	// no rendered config yet.
	ReasonAwaitingRender = "AwaitingRender"
	// ReasonAwaitingBootAsset: the installer image, the config medium or the
	// installer with the config built in is not built (Ready) for the current
	// render yet.
	ReasonAwaitingBootAsset = "AwaitingBootAsset"
	// ReasonSingleMediumUnsupported: the endpoint can hold one virtual medium
	// only, and the machine has no Talos to boot from disk. An installer with
	// the config inside cannot help either. The Talos release of the cluster
	// cannot read one (before 1.12), or the one slot does not boot an ISO.
	ReasonSingleMediumUnsupported = "SingleMediumUnsupported"
	// ReasonNoBootableMedia: the endpoint advertises no insertable media slot
	// or no removable-media boot target.
	ReasonNoBootableMedia = "NoBootableMedia"
	// ReasonReleasing: the claim is being deleted; the release operation
	// (power off, eject) runs before the machine is handed back.
	ReasonReleasing = "Releasing"
	// ReasonReleaseFailed: the release operation failed — the machine stays
	// bound until a person deletes the failed operation (which retries it).
	ReasonReleaseFailed = "ReleaseFailed"
	// ReasonHeldByUser: the claim carries the metal.pk.io/hold annotation —
	// nothing is proposed for it until the annotation is removed.
	ReasonHeldByUser = "HeldByUser"
	// ReasonHostInMaintenance: the bound host carries the
	// metal.pk.io/maintenance annotation, so the operator proposes nothing for
	// it. It is also the reason on Maintenance=True and on the blockedOn of a
	// host in maintenance. An operation that waits for the end of the
	// maintenance has it on Executed=Unknown.
	ReasonHostInMaintenance = "HostInMaintenance"
	// ReasonSelfDisruption: the step would reboot or power off a machine that
	// the operator needs. This is the node of its pod, or a control plane of
	// the cluster that it runs in. The operator proposes and starts nothing for
	// it, unless the Host has the metal.pk.io/allow-self-disruption annotation.
	// Even then, it takes down only one control plane at a time, and only
	// while etcd keeps a majority.
	// It is the reason on Provisioned=False of a claim, on BiosConverged=False
	// of a host, and on Executed=Unknown of an operation that waits.
	ReasonSelfDisruption = "SelfDisruption"
)

// ConditionMaterialized: on an Asset — the operator found the BMC's address in
// the ledger and created the ManagementEndpoint for it. False says what is
// missing; nothing about the machine is known until it is True.
const ConditionMaterialized = "Materialized"

// Reasons on the Asset's Materialized condition.
const (
	// ReasonEndpointMaterialized on Materialized=True: the endpoint exists.
	ReasonEndpointMaterialized = "EndpointMaterialized"
	// ReasonNoBMCMac on Materialized=False: the Asset names no bmcMac.
	ReasonNoBMCMac = "NoBMCMac"
	// ReasonNoLedgerEntry on Materialized=False: the bmcMac is not in the ledger.
	ReasonNoLedgerEntry = "NoLedgerEntry"
	// ReasonLedgerEntryInvalid on Materialized=False: the ledger lists the
	// bmcMac on a line that could not be used (not an https URL, or the MAC
	// listed twice with two addresses).
	ReasonLedgerEntryInvalid = "LedgerEntryInvalid"
	// ReasonMaterializeConflict on Materialized=False: an endpoint of the same
	// name belongs to another Asset.
	ReasonMaterializeConflict = "MaterializeConflict"
)

// ReasonAssetNotMaterialized on a claim's Bound=False: the Asset has no
// endpoint, and its Materialized condition says why.
const ReasonAssetNotMaterialized = "AssetNotMaterialized"

// ReasonHostClaimed on DeletionHeld=True: a claim holds the machine.
const ReasonHostClaimed = "HostClaimed"

// Reasons on the claim's Bound condition. HostNotDiscovered is in flight (the
// machine hasn't been discovered yet), the rest are blocked states.
const (
	// ReasonHostBound on Bound=True: the bind pointer is ours.
	ReasonHostBound = "HostBound"
	// ReasonBindRetained on Bound=True: the assetRef no longer resolves but a live
	// bind exists — alert-and-hold, a resolution failure never releases a machine.
	ReasonBindRetained = "BindRetained"
	// ReasonAssetNotFound: the referenced Asset does not exist.
	ReasonAssetNotFound = "AssetNotFound"
	// ReasonHostNotDiscovered: no Host has been discovered for the asset yet.
	ReasonHostNotDiscovered = "HostNotDiscovered"
	// ReasonAmbiguousSystem: the endpoint fronts several systems and no systemRef
	// says which one is meant — a chassis with more than one node in it.
	ReasonAmbiguousSystem = "AmbiguousSystem"
	// ReasonSystemNotFound: systemRef names no system the endpoint fronts.
	ReasonSystemNotFound = "SystemNotFound"
	// ReasonHostAlreadyClaimed: another claim holds the host (first writer won).
	ReasonHostAlreadyClaimed = "HostAlreadyClaimed"
)

// ReasonSerialMismatch on Verified=False: the asserted serial is not the discovered
// one — the machine behind this endpoint is not the machine the org claims.
const ReasonSerialMismatch = "SerialMismatch"

// ReasonMACMismatch on Verified=False: the manager at this address reports a NIC MAC
// other than the asserted bmcMac — the ledger join resolved to the wrong box.
const ReasonMACMismatch = "MACMismatch"

// ReasonIdentityDiverged on Inspected=False: two paths to the same serial anchor
// give different identities. The anchor leads to different machine facts, so
// no single Host identity exists. The operator alerts and holds.
const ReasonIdentityDiverged = "IdentityDiverged"
