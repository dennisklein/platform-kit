package v1alpha1

// Node lifecycle on day 2: removing a node, wiping a machine that could not
// be reset, reinstalling or replacing a node, and the etcd membership rules
// that guard all three. The annotations below are how a person (or pk-metal)
// asks for these; the operator proposes the operation, and a person approves
// it like any other.

// AnnotationDiskNotWiped on a Host says that the machine may still hold an
// installed Talos node, with that cluster's credentials and, on a control
// plane, its etcd data. The operator writes it when it releases a machine
// it could not reset through Talos (the node did not answer, or the
// operator's executor could not run a reset), and only an approved wipe
// clears it. While it is set the operator proposes no install of another
// node on the machine and no BIOS or firmware change that would power it on.
// The value is <cluster>/<node>, the node that may still be on the disk.
//
//	pk-metal wipe <host>
const AnnotationDiskNotWiped = "metal.pk.io/disk-not-wiped"

// AnnotationWipe on a Host asks the operator to propose a wipe of a machine
// that carries AnnotationDiskNotWiped: boot an image that erases the system
// disk, and stop there. The value is the Talos version of the image to boot
// (the version the node on the disk ran is fine). pk-metal wipe sets it; the
// operator removes it, together with AnnotationDiskNotWiped, when the wipe
// succeeded.
const AnnotationWipe = "metal.pk.io/wipe"

// AnnotationReinstall on a HostClaim asks the operator to wipe and reinstall
// the node the claim runs, on the same machine: for a corrupted install or a
// replaced disk. The value is the claim's status.handOff.at (RFC 3339), so
// that it means the install that was there when a person asked, and once:
// the reinstall writes a new hand-off record, and the annotation, now naming
// an install that is gone, asks for nothing. pk-metal reinstall sets it.
const AnnotationReinstall = "metal.pk.io/reinstall"

// AnnotationAckEtcdRisk on a HostClaim or a TalosNode lets the removal or
// reinstall of a control plane go ahead although it leaves etcd with an even
// number of voting members, without a healthy majority, or with no member at
// all. Without it the operator refuses such a removal and says why. The value
// is the person's reason. pk-metal scale sets it on the removals of a plan
// that passes through an even count.
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
	// is a control plane of a running cluster, and its install waits until
	// every etcd member is healthy and voting and no other control plane is
	// joining. Control planes join one at a time.
	ReasonEtcdJoinWaiting = "EtcdJoinWaiting"
	// ReasonEtcdRiskRefused on a claim's Provisioned=False: removing or
	// reinstalling the claim's control plane would leave etcd with an even
	// member count, without a healthy majority or empty, and nobody
	// acknowledged that (AnnotationAckEtcdRisk).
	ReasonEtcdRiskRefused = "EtcdRiskRefused"
	// ReasonRemovalWaiting on a claim's Provisioned=False: the removal of a
	// control plane waits for its turn: another control plane is being
	// removed, a member is still a learner, or this one is the etcd leader
	// and others are leaving too (the leader goes last).
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
