package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AnnotationRetry on a failed convergence proposal (spec.reason
// BiosConvergence or FirmwareConvergence) asks the operator for the same plan
// again: it deletes the failed operation and proposes a new one, which waits
// for approval like the first. pk-metal retry sets it. Deleting such an
// operation instead is not a retry: deletion is how a proposal is refused.
const AnnotationRetry = "metal.pk.io/retry"

// AnnotationAbort on a HostOperation asks the executor to stop it. The value
// says who asked and why (pk-metal abort writes "<user>: <reason>"). An
// operation that has not started ends at once. A running one stops before
// its next step: a step that already acted on the machine (a reboot asked
// for, a power cycle half done) is finished first, then the executor undoes
// what the operation left behind (it ejects the media it mounted, clears the
// boot override it set, and makes schedulable again the node it cordoned)
// and the operation ends as Aborted. Deleting the operation also stops it,
// but undoes nothing.
const AnnotationAbort = "metal.pk.io/abort"

// AnnotationOverrideClusterGuard on a HostOperation lets it take a control
// plane down although etcd is not fully healthy without it: the way out
// when etcd is already broken and the reboot is the repair, or for a cluster
// with a single control plane. The value says why. It does not lift the
// one-control-plane-at-a-time rule, and it does not lift self-protection.
const AnnotationOverrideClusterGuard = "metal.pk.io/override-cluster-guard"

// Spec reasons of the operations the convergence lane proposes.
const (
	OperationReasonBiosConvergence     = "BiosConvergence"
	OperationReasonFirmwareConvergence = "FirmwareConvergence"
)

// OperationPhase summarizes an operation's conditions. Proposed means it exists
// but has not been approved; AwaitingApproval that it is waiting on a human;
// Running that steps are executing; Succeeded, Failed and Aborted are
// terminal. A failed operation stays for inspection — nothing disruptive is
// retried automatically. Aborted means a person stopped it (metal.pk.io/abort).
// +kubebuilder:validation:Enum=Proposed;AwaitingApproval;Running;Succeeded;Failed;Aborted
type OperationPhase string

const (
	OperationProposed         OperationPhase = "Proposed"
	OperationAwaitingApproval OperationPhase = "AwaitingApproval"
	OperationRunning          OperationPhase = "Running"
	OperationSucceeded        OperationPhase = "Succeeded"
	OperationFailed           OperationPhase = "Failed"
	OperationAborted          OperationPhase = "Aborted"
)

// HostOperationSpec is what to do to a machine, and the approval that lets it
// happen.
//
// The operator proposes an operation by creating it; a person approves it by
// setting spec.approval. The machine and the actions cannot be changed after
// creation, so the right to edit an operation is only the right to approve
// it. The cluster checks who may approve, and refuses an approval by the
// person who proposed the operation.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.proposedBy) || (has(self.proposedBy) && self.proposedBy == oldSelf.proposedBy)",message="proposedBy is frozen after creation"
// +kubebuilder:validation:XValidation:rule="has(self.rolloutRef) == has(oldSelf.rolloutRef) && (!has(self.rolloutRef) || self.rolloutRef == oldSelf.rolloutRef)",message="rolloutRef is frozen after creation"
type HostOperationSpec struct {
	// HostRef names the Host this operation acts on.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hostRef is frozen after creation"
	HostRef Reference `json:"hostRef"`

	// Reason names the desired-state gate this operation serves (BiosConvergence,
	// FirmwareConvergence, …); empty for a human-requested op. It becomes the
	// Approval(<reason>) domain on the Host's blockedOn — hence the condition-reason
	// shape.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Reason string `json:"reason,omitempty"`

	// ProposedBy is the username that created the operation. Admission checks
	// it against the creating request, so it cannot name someone else, and
	// refuses an approval stamped by the same person: nobody approves their
	// own proposal.
	// +kubebuilder:validation:MinLength=1
	ProposedBy string `json:"proposedBy"`

	// PlanHash binds a proposed operation to the desired state it serves — an
	// unchanged hash never spawns a second op (the convergence reconcilers' guard).
	// +optional
	PlanHash string `json:"planHash,omitempty"`

	// Actions run in order; each entry is exactly one action. A failed step fails
	// the operation — disruptive one-shots never auto-retry. At most one power
	// action per operation: the power-cycle proof lives in status once.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:XValidation:rule="self.filter(a, has(a.powerCycle) || has(a.powerOff)).size() <= 1",message="at most one power action per operation"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="actions are frozen after creation"
	// +listType=atomic
	Actions []HostOperationAction `json:"actions"`

	// Approval is the human gate. Omitted means required — the safe default is
	// a closed gate.
	// +kubebuilder:default={required: true}
	// +optional
	Approval OperationApproval `json:"approval,omitempty"`

	// TTLSecondsAfterFinished garbage-collects a succeeded operation
	// Job-style. A failed one stays until a person retries or deletes it: it
	// is the record they decide on. Unset keeps it forever (human-requested
	// ops stay as their own record);
	// convergence proposals set it — the plan memory on the proposing object,
	// not the operation's existence, is what keeps an unchanged planHash from
	// spawning again.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// RolloutRef names the rollout (os.pk.io TalosRollout) this operation is
	// one node's part of, and the plan it was created for. The executor
	// refuses an operation whose rollout is gone, not approved, or approved
	// for another planHash: an operation the rollout's approval covers
	// carries no approval of its own, so the binding is what makes that
	// approval mean this operation. Empty for an operation approved on its
	// own.
	// +optional
	RolloutRef *RolloutReference `json:"rolloutRef,omitempty"`
}

// RolloutReference binds an operation to one approved rollout plan.
type RolloutReference struct {
	// Name is the TalosRollout's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// PlanHash is the rollout's planHash when this operation was created.
	// +kubebuilder:validation:MinLength=1
	PlanHash string `json:"planHash"`
}

// HostOperationAction is a one-of union: exactly one action per entry.
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=1
type HostOperationAction struct {
	// PowerCycle restarts the system: off, observed off, on, observed on — or
	// just on when it was off at the baseline. Each half converges on the
	// reported power state, which every vendor serves, so a resumed operation
	// never cycles a machine twice (the iBMC serves no reset time to tell).
	// +optional
	PowerCycle *PowerCycleAction `json:"powerCycle,omitempty"`

	// PowerOff turns the system off and waits until it reports off. With
	// gracefulTimeoutSeconds it first asks the OS to shut down, and forces
	// the machine off only if it is still on when that time has passed.
	// +optional
	PowerOff *PowerOffAction `json:"powerOff,omitempty"`

	// SetBootOverride sets the source the machine boots from next. The target
	// must be one the endpoint actually offers — machines disagree about what
	// they call a virtual CD, so the value is checked against what was
	// discovered rather than assumed.
	// +optional
	SetBootOverride *SetBootOverrideAction `json:"setBootOverride,omitempty"`

	// InsertMedia mounts an image URL on a virtual-media slot, dispatching the
	// dialect the endpoint advertises (standard InsertMedia vs OEM VmmControl).
	// +optional
	InsertMedia *InsertMediaAction `json:"insertMedia,omitempty"`

	// EjectMedia unmounts a virtual-media slot.
	// +optional
	EjectMedia *EjectMediaAction `json:"ejectMedia,omitempty"`

	// ApplyBios stages a BiosSettings object's desired attributes into the
	// machine's pending (@Redfish.Settings) object. Staging changes nothing —
	// the world moves at the next system reset, so a convergence operation
	// pairs this with a powerCycle.
	// +optional
	ApplyBios *ApplyBiosAction `json:"applyBios,omitempty"`

	// FlashFirmware pushes a firmware image to the management controller and
	// waits for proof that it took: the controller rebooting, and the expected
	// version reading back afterwards. A reported success is not believed on
	// its own.
	// +optional
	FlashFirmware *FlashFirmwareAction `json:"flashFirmware,omitempty"`

	// AwaitCheckIn waits, without touching the machine, until the named
	// TalosNode reports that the machine runs its config — the point after
	// which the boot media are no longer needed. It changes nothing; it keeps
	// the operation (and the grants it issued) alive until the hand-off.
	// +optional
	AwaitCheckIn *AwaitCheckInAction `json:"awaitCheckIn,omitempty"`

	// The actions below act through the node's Talos API or its cluster's
	// Kubernetes API, never through the BMC. Each names the TalosNode it acts
	// on (nodeRef), which must be the node the Host runs (Host
	// spec.os.nodeRef): the name is fixed at proposal, so the approver sees
	// which node, and an operation cannot be pointed at another node later.

	// Cordon marks the node's Kubernetes Node unschedulable. Already
	// cordoned is success.
	// +optional
	Cordon *CordonAction `json:"cordon,omitempty"`

	// Drain evicts the pods on the node through the Eviction API, so that
	// PodDisruptionBudgets hold: an eviction a budget refuses is waited out,
	// not forced, until the timeout fails the step. The node stays cordoned
	// either way. A node not yet cordoned is cordoned first.
	// +optional
	Drain *DrainAction `json:"drain,omitempty"`

	// Uncordon marks the node schedulable again. Already schedulable is
	// success.
	// +optional
	Uncordon *UncordonAction `json:"uncordon,omitempty"`

	// TalosReboot asks Talos to reboot the node: services stop and
	// filesystems unmount before the kernel restarts, unlike a BMC
	// powerCycle, which cuts the power.
	// +optional
	TalosReboot *TalosRebootAction `json:"talosReboot,omitempty"`

	// TalosShutdown asks Talos to shut the node down cleanly.
	// +optional
	TalosShutdown *TalosShutdownAction `json:"talosShutdown,omitempty"`

	// TalosUpgrade installs another Talos version from an installer image
	// and reboots into it.
	// +optional
	TalosUpgrade *TalosUpgradeAction `json:"talosUpgrade,omitempty"`

	// TalosApplyConfig applies a rendered machine config, the bytes pinned
	// by digest at proposal.
	// +optional
	TalosApplyConfig *TalosApplyConfigAction `json:"talosApplyConfig,omitempty"`

	// EtcdLeave makes the node's etcd member leave the cluster, asked on the
	// node itself: the orderly removal of a control plane that still
	// answers.
	// +optional
	EtcdLeave *EtcdLeaveAction `json:"etcdLeave,omitempty"`

	// EtcdRemoveMember removes one etcd member by its ID, asked through a
	// healthy control-plane peer: the path for a member whose node can no
	// longer leave by itself (wiped, dead, unreachable).
	// +optional
	EtcdRemoveMember *EtcdRemoveMemberAction `json:"etcdRemoveMember,omitempty"`

	// TalosReset wipes the node through Talos, back towards maintenance
	// mode.
	// +optional
	TalosReset *TalosResetAction `json:"talosReset,omitempty"`

	// WaitNodeReady waits, changing nothing, until the node runs and is
	// ready: Talos stage running with every readiness condition met, and its
	// Kubernetes Node Ready.
	// +optional
	WaitNodeReady *WaitNodeReadyAction `json:"waitNodeReady,omitempty"`

	// WaitEtcdHealthy waits, changing nothing, until the etcd cluster of the
	// node's TalosCluster is healthy: every member answers, none reports an
	// error or alarm, and none is a learner.
	// +optional
	WaitEtcdHealthy *WaitEtcdHealthyAction `json:"waitEtcdHealthy,omitempty"`

	// EtcdRecover restores the etcd of a cluster that lost all of it from
	// an EtcdSnapshot, on one control plane: a person uploads the decrypted
	// snapshot (pk-metal restore upload), then Talos starts etcd from it.
	// See EtcdRecoverAction and the docs site: operate/day-2/restore-etcd.
	// +optional
	EtcdRecover *EtcdRecoverAction `json:"etcdRecover,omitempty"`

	// EtcdForfeitLeadership hands etcd leadership to another member when
	// the node's member leads, asked on the node itself; a follower is left
	// as it is. A control plane that is about to leave or go down does it
	// first, so that its departure does not cost an election under load.
	// +optional
	EtcdForfeitLeadership *EtcdForfeitLeadershipAction `json:"etcdForfeitLeadership,omitempty"`

	// DeleteKubernetesNode deletes the node's Node object from its
	// cluster's Kubernetes API, the last trace of a removed node there.
	// Already gone is success.
	// +optional
	DeleteKubernetesNode *DeleteKubernetesNodeAction `json:"deleteKubernetesNode,omitempty"`

	// StageFirmware pushes a system firmware image (BIOS/UEFI, NIC, RAID)
	// to the BMC to apply at the machine's next reset. Staging changes
	// nothing that runs; a reboot later in the operation applies it, and
	// the version the inventory reports afterwards is the proof.
	// +optional
	StageFirmware *StageFirmwareAction `json:"stageFirmware,omitempty"`
}

// StageFirmwareAction stages one update package for some inventory entries.
type StageFirmwareAction struct {
	// Image is the digest-pinned OCI reference of the update package.
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Version is the version the entries report once the package applied:
	// an entry that already reports it is not staged again.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Targets are the FirmwareInventory member ids to update, fixed at
	// proposal: the approver sees which components change.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	Targets []string `json:"targets"`
}

// PowerCycleAction restarts the machine. It takes no fields: there is only one
// way to do it, forcibly, and a machine that is already off is turned on instead.
type PowerCycleAction struct{}

// PowerOffAction turns the machine off. Already off is success.
type PowerOffAction struct {
	// GracefulTimeoutSeconds, when set, makes the step ask the OS to shut
	// down first (Redfish GracefulShutdown, the same as a short press of the
	// power button or `ipmitool chassis power soft`), wait this long for the
	// machine to report off, and only then force it off. Unset or 0 forces
	// the machine off at once. A BMC that refuses a graceful shutdown is
	// forced at once too.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +optional
	GracefulTimeoutSeconds int32 `json:"gracefulTimeoutSeconds,omitempty"`
}

// AwaitCheckInAction holds the operation until a TalosNode checks in.
type AwaitCheckInAction struct {
	// NodeRef names the TalosNode (os.pk.io) whose check-in ends the wait:
	// MachineReachable=True over the authenticated machine API, or Ready=True.
	NodeRef Reference `json:"nodeRef"`

	// TimeoutSeconds bounds the wait, counted from the previous step's end.
	// Running out fails the step — the media are ejected and the grants
	// revoked either way.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=1800
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// SetBootOverrideAction points the next boot at a particular source.
type SetBootOverrideAction struct {
	// Target is the boot source (UsbCd, Cd, Pxe, …) — validated against the
	// endpoint's discovered AllowableValues at execution, not at admission.
	Target string `json:"target"`

	// Once limits the override to the next boot (the default); false is Continuous.
	// +kubebuilder:default=true
	// +optional
	Once *bool `json:"once,omitempty"`

	// Mode is the firmware boot mode for the override. Omitted leaves the
	// machine's own setting alone (some firmware rejects the property).
	// +kubebuilder:validation:Enum=UEFI;Legacy
	// +optional
	Mode string `json:"mode,omitempty"`
}

// InsertMediaAction mounts an image on one of the machine's virtual-media slots,
// the way you would put a disc in a drive attached to it. The image is either a
// plain URL or a BootAsset: a secret-free asset mounts from its stable URL, a
// secret-bearing one (configISO) is served by the operator's media service
// under a grant issued at execution time and revoked when the operation ends.
//
// +kubebuilder:validation:XValidation:rule="has(self.imageUrl) != has(self.bootAssetRef)",message="exactly one of imageUrl and bootAssetRef"
// +kubebuilder:validation:XValidation:rule="!has(self.digest) || has(self.bootAssetRef)",message="digest pins a bootAssetRef"
type InsertMediaAction struct {
	// MediaID is the virtual-media slot ("CD", "USBStick", …).
	MediaID string `json:"mediaId"`
	// ImageURL is what to mount. Already-mounted with the same URL is success —
	// steps converge to their goal, so a resumed operation never double-issues.
	// +optional
	ImageURL string `json:"imageUrl,omitempty"`
	// BootAssetRef names the BootAsset (os.pk.io) to mount.
	// +optional
	BootAssetRef *Reference `json:"bootAssetRef,omitempty"`
	// Digest pins the artifact bytes (sha256:<hex>) the approver saw: an
	// artifact rebuilt since the proposal fails the step instead of mounting
	// content nobody approved. With a digest the asset need not be Ready: the
	// artifact approved is mounted while a newer build runs, and an asset
	// that names no artifact yet makes the step wait. Secret-bearing assets
	// (configISO, embeddedInstaller) only.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
}

// EjectMediaAction unmounts a virtual-media slot. Ejecting an empty slot is
// success — the goal is an empty slot, not the act of emptying it.
type EjectMediaAction struct {
	// MediaID is the virtual-media slot to empty.
	MediaID string `json:"mediaId"`
}

// ApplyBiosAction stages a machine's wanted BIOS attributes onto the firmware's
// pending object. Staging on its own changes nothing that is running: BIOS
// settings take effect at the next reset, which is why a converging operation
// pairs this with a powerCycle. Where the BMC lists OnReset among the
// supported apply times (@Redfish.Settings.SupportedApplyTimes), the PATCH
// asks for it (@Redfish.SettingsApplyTime): iDRAC 9 applies staged values
// only then, through the BIOS configuration job that request creates.
type ApplyBiosAction struct {
	// SettingsRef names the BiosSettings the attributes came from — provenance
	// only: the executor never reads it, so an edit after approval cannot
	// change what runs.
	// +optional
	SettingsRef *Reference `json:"settingsRef,omitempty"`

	// Attributes are the values to stage, copied in at proposal time — what
	// the approver saw is exactly what is applied. Keys already current are
	// left out; keys already staged with the wanted value are PATCHed again
	// with the rest, so the staging carries the apply time this client asks
	// for, and a refusal to stage only such keys again counts as staged.
	// +kubebuilder:validation:MinProperties=1
	Attributes map[string]apiextensionsv1.JSON `json:"attributes"`
}

// FlashFirmwareAction replaces the firmware on a machine's management controller.
// The controller reboots as part of it; the machine it manages keeps running.
type FlashFirmwareAction struct {
	// Image is the digest-pinned OCI reference to fetch and push (the same
	// shape as FirmwareProfile.spec.manager.image).
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Version is the version expected to read back after the flash — the
	// convergence goal: a manager already serving it makes this step a no-op.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

// CordonAction marks a node unschedulable.
type CordonAction struct {
	// NodeRef names the TalosNode (os.pk.io); its hostname is the Kubernetes
	// Node's name.
	NodeRef Reference `json:"nodeRef"`
}

// DrainAction evicts a node's pods, respecting PodDisruptionBudgets.
type DrainAction struct {
	// NodeRef names the TalosNode (os.pk.io) to drain.
	NodeRef Reference `json:"nodeRef"`

	// TimeoutSeconds bounds the whole drain, budget waits included. Running
	// out fails the step and leaves the node cordoned.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=900
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// GracePeriodSeconds overrides each pod's termination grace period;
	// unset keeps the pod's own.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +optional
	GracePeriodSeconds *int32 `json:"gracePeriodSeconds,omitempty"`

	// DeleteEmptyDirData lets the drain evict pods with emptyDir volumes,
	// whose data is lost. Without it such a pod fails the step.
	// +optional
	DeleteEmptyDirData bool `json:"deleteEmptyDirData,omitempty"`
}

// UncordonAction marks a node schedulable.
type UncordonAction struct {
	// NodeRef names the TalosNode (os.pk.io).
	NodeRef Reference `json:"nodeRef"`
}

// TalosRebootAction reboots a node through Talos.
type TalosRebootAction struct {
	// NodeRef names the TalosNode (os.pk.io) to reboot.
	NodeRef Reference `json:"nodeRef"`

	// Mode is Default (kexec or a warm reboot) or PowerCycle (a cold start
	// at the end, for firmware that applies only then).
	// +kubebuilder:validation:Enum=Default;PowerCycle
	// +kubebuilder:default=Default
	// +optional
	Mode string `json:"mode,omitempty"`
}

// TalosShutdownAction shuts a node down through Talos.
type TalosShutdownAction struct {
	// NodeRef names the TalosNode (os.pk.io) to shut down.
	NodeRef Reference `json:"nodeRef"`

	// Force shuts down even when the node cannot cordon and drain itself
	// first (its Kubernetes API is down).
	// +optional
	Force bool `json:"force,omitempty"`
}

// TalosUpgradeAction upgrades a node's Talos.
type TalosUpgradeAction struct {
	// NodeRef names the TalosNode (os.pk.io) to upgrade.
	NodeRef Reference `json:"nodeRef"`

	// Image is the installer image, pinned by digest: what the approver saw
	// is what is installed.
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Version is the Talos version expected to read back afterwards, the
	// step's goal: a node that already reports it is not upgraded again.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`
	Version string `json:"version"`

	// Stage writes the new image and upgrades at the next reboot, for a node
	// whose open files would block an upgrade in place.
	// +optional
	Stage bool `json:"stage,omitempty"`

	// Preserve keeps the EPHEMERAL partition (pod data, etcd). Talos 1.8
	// and later always preserve it; the field is passed through for older
	// nodes.
	// +kubebuilder:default=true
	// +optional
	Preserve *bool `json:"preserve,omitempty"`

	// Force skips the etcd health check Talos runs before upgrading a
	// control plane.
	// +optional
	Force bool `json:"force,omitempty"`

	// RebootMode is how the reboot into the new image ends.
	// +kubebuilder:validation:Enum=Default;PowerCycle
	// +kubebuilder:default=Default
	// +optional
	RebootMode string `json:"rebootMode,omitempty"`
}

// TalosApplyConfigAction applies a pinned machine config.
type TalosApplyConfigAction struct {
	// NodeRef names the TalosNode (os.pk.io) whose config this is.
	NodeRef Reference `json:"nodeRef"`

	// ConfigSecretRef names the Secret, in the operator's namespace, that
	// holds the rendered config.
	ConfigSecretRef Reference `json:"configSecretRef"`

	// Digest pins the config bytes (sha256:<hex>) the approver saw: a Secret
	// whose content no longer hashes to it fails the step instead of
	// applying a config nobody approved.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// Mode is how the node takes the config: NoReboot fails when a change
	// needs a reboot, Reboot always reboots, Staged applies at the next
	// boot, Try applies it and the step confirms it once the node is
	// healthy (unconfirmed, the node rolls back after tryTimeoutSeconds),
	// Auto lets the node choose.
	// +kubebuilder:validation:Enum=Auto;NoReboot;Reboot;Staged;Try
	// +kubebuilder:default=NoReboot
	// +optional
	Mode string `json:"mode,omitempty"`

	// TryTimeoutSeconds bounds Try mode; unset is the node's default.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	// +optional
	TryTimeoutSeconds int32 `json:"tryTimeoutSeconds,omitempty"`
}

// EtcdLeaveAction makes a control plane's etcd member leave the cluster.
type EtcdLeaveAction struct {
	// NodeRef names the control-plane TalosNode (os.pk.io) that leaves.
	NodeRef Reference `json:"nodeRef"`
}

// EtcdRemoveMemberAction removes one etcd member through a peer.
type EtcdRemoveMemberAction struct {
	// NodeRef names the TalosNode (os.pk.io) whose member is removed:
	// provenance for the approver, and how the executor finds the cluster
	// and a peer to ask.
	NodeRef Reference `json:"nodeRef"`

	// MemberID is the etcd member ID in hex, as etcd lists it, fixed at
	// proposal: the executor removes exactly this member, never "whatever
	// carries that hostname now", which after a rejoin is the new member.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{1,16}$`
	MemberID string `json:"memberId"`

	// Hostname is the member's hostname as etcd listed it at proposal, for
	// the approver.
	// +optional
	Hostname string `json:"hostname,omitempty"`
}

// TalosResetAction wipes a node through Talos.
//
// +kubebuilder:validation:XValidation:rule="!has(self.systemPartitions) || !has(self.mode) || self.mode != 'UserDisks'",message="systemPartitions do not apply to mode UserDisks"
// +kubebuilder:validation:XValidation:rule="!has(self.userDisks) || !has(self.mode) || self.mode != 'SystemDisk'",message="userDisks do not apply to mode SystemDisk"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'UserDisks' || (has(self.userDisks) && size(self.userDisks) > 0)",message="mode UserDisks needs userDisks: Talos wipes only the user disks a reset names"
type TalosResetAction struct {
	// NodeRef names the TalosNode (os.pk.io) to reset.
	NodeRef Reference `json:"nodeRef"`

	// Graceful leaves etcd (on a control plane) and cordons and drains the
	// node before wiping. A node that cannot reach its peers needs false,
	// and its member is then removed by an etcdRemoveMember step.
	// +kubebuilder:default=true
	// +optional
	Graceful *bool `json:"graceful,omitempty"`

	// Reboot boots the node into maintenance mode afterwards; false leaves
	// it powered off.
	// +optional
	Reboot bool `json:"reboot,omitempty"`

	// Mode picks the disks: All (the system disk and the disks in
	// userDisks), SystemDisk, or UserDisks (only the disks in userDisks).
	// +kubebuilder:validation:Enum=All;SystemDisk;UserDisks
	// +kubebuilder:default=All
	// +optional
	Mode string `json:"mode,omitempty"`

	// SystemPartitions limits the wipe to these system partitions; empty
	// wipes what mode names. STATE holds the machine config and its
	// credentials, EPHEMERAL the pod data and etcd.
	// +kubebuilder:validation:MaxItems=3
	// +listType=set
	// +optional
	SystemPartitions []SystemPartition `json:"systemPartitions,omitempty"`

	// UserDisks names the data disks to wipe in mode All or UserDisks, by
	// device path (/dev/sdb). Talos finds no user disk by itself: a disk
	// that is not named keeps its data. Talos refuses a path it does not
	// know, a read-only disk and the system disk.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^/dev/[A-Za-z0-9._/:-]+$`
	// +listType=set
	// +optional
	UserDisks []string `json:"userDisks,omitempty"`
}

// SystemPartition is a Talos system partition label.
// +kubebuilder:validation:Enum=STATE;EPHEMERAL;META
type SystemPartition string

// WaitNodeReadyAction waits for a node to run and be ready.
type WaitNodeReadyAction struct {
	// NodeRef names the TalosNode (os.pk.io) to wait for.
	NodeRef Reference `json:"nodeRef"`

	// Version, when set, is also waited for: the Talos version the node must
	// report, the proof an upgrade took.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`
	// +optional
	Version string `json:"version,omitempty"`

	// TimeoutSeconds bounds the wait, counted from the previous step's end.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=1800
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// WaitEtcdHealthyAction waits for a cluster's etcd to be healthy.
type WaitEtcdHealthyAction struct {
	// NodeRef names a control-plane TalosNode (os.pk.io) of the cluster.
	// Its own member must be listed too, as a voting member: after a join
	// or a reboot, that is the member the wait is for.
	NodeRef Reference `json:"nodeRef"`

	// TimeoutSeconds bounds the wait, counted from the previous step's end.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=900
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// EtcdForfeitLeadershipAction moves etcd leadership off a control plane.
type EtcdForfeitLeadershipAction struct {
	// NodeRef names the control-plane TalosNode (os.pk.io) whose member
	// gives up leadership.
	NodeRef Reference `json:"nodeRef"`
}

// DeleteKubernetesNodeAction deletes a node's Kubernetes Node object.
type DeleteKubernetesNodeAction struct {
	// NodeRef names the TalosNode (os.pk.io); its name is the Kubernetes
	// Node's name, and its cluster says which Kubernetes API to ask.
	NodeRef Reference `json:"nodeRef"`
}

// OperationApproval is the CEL-guarded human gate (a field, not a subresource).
type OperationApproval struct {
	// Required: this operation must be approved before it runs (from the
	// disruption policy; explicit false only where the policy waives the gate,
	// and admission lets only the operator or an admin create such an
	// operation). A pointer so that EVERY client that says nothing — YAML
	// omitting the key, a Go zero value — gets the safe default: a closed gate.
	// Frozen: flipping it would be an approval without a name on it.
	// +kubebuilder:default=true
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="approval.required is frozen after creation"
	// +optional
	Required *bool `json:"required,omitempty"`

	// ApprovedBy is stamped by the approver — the ValidatingAdmissionPolicy
	// enforces that it equals the requesting username, is empty at creation, and
	// is write-once.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// OperationBaseline is the observation captured at execution start — the
// re-derivable "did I already reboot for this request" predicate: completion is
// the world moving past it, never a remembered step counter.
type OperationBaseline struct {
	// +optional
	PowerState string `json:"powerState,omitempty"`
	// LastResetTime as observed before any action ran. Micro precision: the sim
	// resets faster than a second, and second-precision would blind the advance
	// predicate.
	// +optional
	LastResetTime *metav1.MicroTime `json:"lastResetTime,omitempty"`
	// CapturedAt is when the baseline was taken — the start of execution,
	// which bounds how long a power transition may take to show.
	// +optional
	CapturedAt *metav1.Time `json:"capturedAt,omitempty"`
}

// TalosStepProgress records what one Talos step did before its goal shows.
type TalosStepProgress struct {
	// Step is the index in spec.actions this progress belongs to.
	// +kubebuilder:validation:Minimum=0
	Step int32 `json:"step"`
	// BootTime is the node's boot time before the step acted. The step's
	// restart shows as a later boot time.
	// +optional
	BootTime *metav1.Time `json:"bootTime,omitempty"`
	// RequestedAt is when the node accepted the request.
	// +optional
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`
	// Mode is how the node said it applies a config (talosApplyConfig):
	// reboot, no-reboot, staged or try.
	// +optional
	Mode string `json:"mode,omitempty"`
}

// OperationStep is one executed action's outcome.
type OperationStep struct {
	// Action is the union key that ran (powerCycle, insertMedia, …), or
	// "abort" for the record of an abort and what it undid.
	Action string `json:"action"`
	// +kubebuilder:validation:Enum=Succeeded;Failed;Aborted
	Phase string `json:"phase"`
	// +optional
	Message string `json:"message,omitempty"`
	// FinishedAt is when the step reached its goal (or failed).
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// FirmwareUploadProgress is the progress of a firmware step's upload.
type FirmwareUploadProgress struct {
	// Step is the index of the action in spec.actions that uploaded.
	// +kubebuilder:validation:Minimum=0
	Step int32 `json:"step"`
	// SentAt is when the executor started the upload.
	SentAt metav1.MicroTime `json:"sentAt"`
	// ManagerResetTime is the manager's last reset time before a flash, if
	// the manager serves one: the flash is done only after a later reset.
	// +optional
	ManagerResetTime *metav1.MicroTime `json:"managerResetTime,omitempty"`
	// Targets are the inventory entries a stageFirmware step sent its image
	// for, in order.
	// +listType=atomic
	// +optional
	Targets []string `json:"targets,omitempty"`
}

// HostOperationStatus is what happened: which steps ran and how they ended, plus
// the observations that prove the machine really did what was asked.
type HostOperationStatus struct {
	// Baseline is captured (and persisted) before the first mutating call.
	// +optional
	Baseline *OperationBaseline `json:"baseline,omitempty"`

	// ObservedResetTime is the reset-time observation after the last step — the
	// proof the reset happened, and what marks a Host reset-time advance as
	// explained rather than Drifted.
	// +optional
	ObservedResetTime *metav1.MicroTime `json:"observedResetTime,omitempty"`

	// ShutdownRequested is when the executor asked the OS to shut down for a
	// graceful powerOff. The step forces the machine off once the graceful
	// window, counted from here, has passed; recording it means a resumed
	// operation neither asks again nor restarts the window.
	// +optional
	ShutdownRequested *metav1.MicroTime `json:"shutdownRequested,omitempty"`

	// OffObserved is when the executor saw the machine report off during a
	// power cycle — the first half of the cycle, recorded so that a resumed
	// operation finishes the cycle instead of starting a second one.
	// +optional
	OffObserved *metav1.MicroTime `json:"offObserved,omitempty"`

	// UploadAccepted records that a firmware step (flashFirmware,
	// stageFirmware) sent its image to the BMC. It is written before the
	// upload, so a resumed operation never sends the image a second time:
	// it only waits for the version to read back. A second upload to a
	// manager that is still busy with the first can wedge it.
	// +optional
	UploadAccepted *FirmwareUploadProgress `json:"uploadAccepted,omitempty"`

	// TalosStep is the progress of the Talos step in flight that restarts or
	// wipes the node (talosReboot, talosUpgrade, talosApplyConfig,
	// talosReset, talosShutdown): the node's boot time before it acted, and
	// when the request was accepted. A resumed operation waits for the boot
	// time to move on instead of asking a second time.
	// +optional
	TalosStep *TalosStepProgress `json:"talosStep,omitempty"`

	// CordonedNode is the Kubernetes Node this operation made unschedulable
	// (a cordon or drain step that found it schedulable). An abort makes it
	// schedulable again; a node a person had cordoned before is left as it
	// was.
	// +optional
	CordonedNode string `json:"cordonedNode,omitempty"`

	// Steps records each action's outcome in execution order, written as each
	// step reaches its goal. A resumed operation continues after the last
	// recorded step: a goal that a later step consumed (a Once boot override
	// used up by the boot that followed) is never re-armed.
	// +listType=atomic
	// +optional
	Steps []OperationStep `json:"steps,omitempty"`

	// Conditions are the truth — the only thing reconcilers branch on.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the conditions below for people and for kubectl columns.
	// It is a summary, not the contract: automation should read conditions.
	// +optional
	Phase OperationPhase `json:"phase,omitempty"`

	// BlockedOn is derived — why the operation isn't advancing (AwaitingApproval,
	// OperationQueued).
	// +optional
	BlockedOn string `json:"blockedOn,omitempty"`
}

// HostOperation is one step that changes a machine — an install, a shutdown, a
// BIOS change, a firmware flash — and the record of what happened. The
// operator proposes it; spec.actions lists exactly what will be done, in
// order, so read it before you approve.
//
//   - Approve:   pk-metal approve <namespace>/<operation>
//     (someone other than the proposer must approve).
//   - Refuse:    delete the operation, or set metal.pk.io/hold on the
//     HostClaim so the operator stops proposing.
//   - Retry:     pk-metal retry <namespace>/<operation> once you have fixed
//     the cause. A failed operation is never retried on its own.
//
// kubectl get hostoperations shows what waits (Phase AwaitingApproval) and
// what failed; kubectl describe shows each step's message.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=metal
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.hostRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Proposed",type=string,JSONPath=`.spec.proposedBy`,priority=1
// +kubebuilder:printcolumn:name="Approved",type=string,JSONPath=`.spec.approval.approvedBy`
// +kubebuilder:printcolumn:name="Blocked",type=string,JSONPath=`.status.blockedOn`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HostOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HostOperationSpec `json:"spec"`
	// +optional
	Status HostOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type HostOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostOperation `json:"items"`
}
