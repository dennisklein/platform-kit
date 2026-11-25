package v1alpha1

// Node lifecycle on day 2: removing a node, wiping a machine that could not
// be reset, reinstalling or replacing a node, and the etcd membership rules
// that guard all three. The annotations below are how a person (or pk-metal)
// asks for these; the operator proposes the operation, and a person approves
// it like any other.

// AnnotationDiskNotWiped on a Host says that the machine can still hold an
// installed Talos node, with the credentials of its cluster. On a control
// plane, the disk can also hold etcd data. The operator writes it when it
// releases a machine that it could not reset through Talos. The node did not
// answer, or the executor could not run a reset. Only an approved wipe
// removes it. While it is set, the operator proposes no install of another
// node on the machine. It also proposes no BIOS or firmware change that
// would power the machine on. The value is <cluster>/<node>, the node that
// can still be on the disk.
//
//	pk-metal wipe <host>
const AnnotationDiskNotWiped = "metal.pk.io/disk-not-wiped"

// AnnotationWipeWaived on a Host records who removed AnnotationDiskNotWiped
// without a wipe, and why: "<username>: <reason>". Admission refuses the
// removal of the mark by a person unless the same request sets this
// annotation in that form, with the person's own username
// (config/policy/guard-lift.yaml). The operator removes it when it marks the
// machine again or wipes it, and the audit trail names it when an install
// starts on the machine.
//
//	kubectl annotate host <host> metal.pk.io/disk-not-wiped- \
//	  metal.pk.io/wipe-waived="<you>: <reason>"
const AnnotationWipeWaived = "metal.pk.io/wipe-waived"

// AnnotationWipe on a Host asks the operator to propose a wipe of a machine
// that has AnnotationDiskNotWiped. The wipe boots an image that erases the
// system disk, and stops there. The value is the Talos version of the image
// to boot. The version of the node on the disk is correct. pk-metal wipe sets
// it. The operator removes it and AnnotationDiskNotWiped when the wipe
// succeeds.
const AnnotationWipe = "metal.pk.io/wipe"

// AnnotationReinstall on a HostClaim asks the operator to wipe and reinstall
// the node of the claim on the same machine. Use it for a damaged install or
// a replaced disk. The value is the claim's status.handOff.at (RFC 3339). Thus
// it names the install that was there when a person asked, and it works one
// time. The reinstall writes a new hand-off record. The annotation then names
// an install that is gone, and asks for nothing. pk-metal reinstall sets it.
const AnnotationReinstall = "metal.pk.io/reinstall"

// AnnotationAckEtcdRisk on a HostClaim or a TalosNode lets the removal or
// reinstall of a control plane continue, although etcd then has an even
// number of voting members, no healthy majority, or no member. Without it,
// the operator refuses such a removal and says why. The value is
// "<username>: <reason>": who accepted the risk and why. Admission accepts
// no other form, and only the caller's own username
// (config/policy/guard-lift.yaml). pk-metal scale sets it on the removals of
// a plan that passes through an even count, and names its caller.
const AnnotationAckEtcdRisk = "metal.pk.io/ack-etcd-risk"

// ConditionDiskWiped on a Host: whether the machine's disks are known to be
// free of an installed node. False (reason InstalledNodeRemains) while
// AnnotationDiskNotWiped is set; absent otherwise.
const ConditionDiskWiped = "DiskWiped"

// Reasons of the node lifecycle.
const (
	// ReasonInstalledNodeRemains on a Host's DiskWiped=False: the machine
	// was released without a Talos reset, so its disk may still hold a
	// node of a cluster, credentials included.
	ReasonInstalledNodeRemains = "InstalledNodeRemains"
	// ReasonDiskNotWiped on a claim's Provisioned=False: the bound host
	// still holds another node's install, and nothing is installed on it
	// until it is wiped.
	ReasonDiskNotWiped = "DiskNotWiped"
	// ReasonEtcdJoinWaiting on a claim's Provisioned=False: the claim's node
	// is a control plane of a running cluster. Its install waits until each
	// etcd member is healthy and votes, and no other control plane joins.
	// Control planes join one at a time.
	ReasonEtcdJoinWaiting = "EtcdJoinWaiting"
	// ReasonEtcdRiskRefused on a claim's Provisioned=False: the removal or
	// reinstall of the claim's control plane would leave etcd with an even
	// member count, without a healthy majority, or empty. Nobody accepted that
	// risk (AnnotationAckEtcdRisk).
	ReasonEtcdRiskRefused = "EtcdRiskRefused"
	// ReasonRemovalWaiting on a claim's Provisioned=False: the removal of a
	// control plane waits for its turn. Another control plane is in removal, a
	// member is still a learner, or this one is the etcd leader and others
	// leave too. The leader goes last.
	ReasonRemovalWaiting = "RemovalWaiting"
	// ReasonRemovingNode on a claim's Provisioned=False: the claim's
	// TalosNode is being deleted; the removal operation (drain, etcd leave,
	// reset, Node deletion, power off) runs first.
	ReasonRemovingNode = "RemovingNode"
	// ReasonNodeRemoved on a claim's Provisioned=False: the claim's
	// TalosNode was removed from its cluster and the machine wiped; the
	// claim keeps the machine for whatever node it names next.
	ReasonNodeRemoved = "NodeRemoved"
	// ReasonReinstalling on a claim's Provisioned=False: a reinstall the
	// claim asked for (AnnotationReinstall) is proposed or running.
	ReasonReinstalling = "Reinstalling"
	// ReasonReplacing on a claim's Bound=False: the claim was pointed at
	// another machine, and the one it ran on is being removed first.
	ReasonReplacing = "Replacing"
	// ReasonWipeRequested on a Host's DiskWiped=False: a wipe was asked for
	// and its operation is proposed or running.
	ReasonWipeRequested = "WipeRequested"
)

// OperationReasonWipe is the spec reason of a wipe operation.
const OperationReasonWipe = "Wipe"
