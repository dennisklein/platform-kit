package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutKind is what a rollout changes on each node.
// +kubebuilder:validation:Enum=Config;TalosUpgrade;KubernetesUpgrade;Reboot
type RolloutKind string

const (
	// RolloutConfig applies each node's current rendered config: without a
	// reboot where the node says it can take it live, with a drain and a
	// reboot where it says it cannot.
	RolloutConfig RolloutKind = "Config"
	// RolloutTalosUpgrade installs another Talos release on each node, one
	// node at a time, each drained first.
	RolloutTalosUpgrade RolloutKind = "TalosUpgrade"
	// RolloutKubernetesUpgrade applies the configs rendered for a new
	// Kubernetes version without a reboot (control planes first: their
	// static pods and kubelet), moves the kube-proxy DaemonSet to the new
	// image, then does the workers' kubelets.
	RolloutKubernetesUpgrade RolloutKind = "KubernetesUpgrade"
	// RolloutReboot reboots each node through Talos, drained first.
	RolloutReboot RolloutKind = "Reboot"
)

// AnnotationRolloutResume on a TalosRollout lets it go on past the pause
// after its first node. Its value must be the rollout's planHash: a resume
// given for one plan does not carry over to another. `pk-metal rollout
// resume` sets it.
const AnnotationRolloutResume = "os.pk.io/resume"

// LabelRollout on a HostOperation names the TalosRollout it is one node's
// part of.
const LabelRollout = "os.pk.io/rollout"

// TalosRolloutSpec says what to change on which nodes of one cluster, and
// carries the one approval that covers every node of it.
//
// A rollout is proposed by the operator (or created by a person), then
// planned: the operator writes the ordered node list, each node's actions
// and the exact target (config digest, installer image digest) into
// status.plan, and its hash into status.planHash. A person approves that
// plan by stamping spec.approval with their name and the planHash they saw.
// From then on the plan is frozen: the operator runs exactly it, or nothing.
//
// +kubebuilder:validation:XValidation:rule="self.clusterRef == oldSelf.clusterRef && self.kind == oldSelf.kind",message="clusterRef and kind are frozen after creation"
// +kubebuilder:validation:XValidation:rule="(!has(self.target) && !has(oldSelf.target)) || (has(self.target) && has(oldSelf.target) && self.target == oldSelf.target)",message="target is frozen after creation"
// +kubebuilder:validation:XValidation:rule="(!has(self.nodes) && !has(oldSelf.nodes)) || (has(self.nodes) && has(oldSelf.nodes) && self.nodes == oldSelf.nodes)",message="nodes are frozen after creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.proposedBy) || (has(self.proposedBy) && self.proposedBy == oldSelf.proposedBy)",message="proposedBy is frozen after creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.abort) || !oldSelf.abort || (has(self.abort) && self.abort)",message="an abort cannot be taken back"
// +kubebuilder:validation:XValidation:rule="(has(self.maxUnavailableWorkers) ? self.maxUnavailableWorkers : 1) == (has(oldSelf.maxUnavailableWorkers) ? oldSelf.maxUnavailableWorkers : 1)",message="maxUnavailableWorkers is frozen after creation"
// +kubebuilder:validation:XValidation:rule="(has(self.drainTimeoutSeconds) ? self.drainTimeoutSeconds : 900) == (has(oldSelf.drainTimeoutSeconds) ? oldSelf.drainTimeoutSeconds : 900)",message="drainTimeoutSeconds is frozen after creation"
type TalosRolloutSpec struct {
	// ClusterRef names the TalosCluster whose nodes this rollout changes.
	ClusterRef Reference `json:"clusterRef"`

	// Kind is what changes on each node.
	Kind RolloutKind `json:"kind"`

	// Target is where the rollout takes the nodes: the Talos version of a
	// TalosUpgrade, the Kubernetes version of a KubernetesUpgrade. A Config
	// or Reboot rollout takes none; its target is each node's current
	// rendered config, frozen into the plan.
	// +optional
	Target *RolloutTarget `json:"target,omitempty"`

	// Nodes limits the rollout to these TalosNodes. Empty means every node
	// of the cluster that needs the change.
	// +optional
	// +listType=set
	Nodes []string `json:"nodes,omitempty"`

	// MaxUnavailableWorkers is how many workers may be in the middle of the
	// change at once. Control planes always go one at a time, and only
	// while etcd keeps every member healthy.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=1
	// +optional
	MaxUnavailableWorkers int32 `json:"maxUnavailableWorkers,omitempty"`

	// DrainTimeoutSeconds bounds the drain of each node that is drained
	// (an upgrade, a reboot, a config that needs a reboot). It is part of
	// what is approved, so it is frozen after creation.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=900
	// +optional
	DrainTimeoutSeconds int32 `json:"drainTimeoutSeconds,omitempty"`

	// ProposedBy is the username that created the rollout. Admission checks
	// it against the creating request. Whether the same person may also
	// approve it is a site setting (selfApproval in the ConfigMap
	// pk-metal/hostoperation-approval, default Allowed).
	// +kubebuilder:validation:MinLength=1
	ProposedBy string `json:"proposedBy"`

	// Approval is the one human gate of the whole rollout.
	// +optional
	Approval RolloutApproval `json:"approval,omitempty"`

	// Paused stops the rollout from starting another node. A node already
	// in the middle of its change finishes it. Set and clear it at will.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// Abort stops the rollout for good: no further node starts, a node in
	// the middle of its change finishes it, and nodes already changed stay
	// as they are. It cannot be taken back; a new rollout is how to go on.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// RolloutTarget is the version a rollout moves the nodes to.
type RolloutTarget struct {
	// TalosVersion is the Talos release of a TalosUpgrade.
	// +kubebuilder:validation:Pattern=`^v1\.(1[4-9]|[2-9][0-9])\.[0-9]+$`
	// +optional
	TalosVersion string `json:"talosVersion,omitempty"`

	// KubernetesVersion is the Kubernetes release of a KubernetesUpgrade.
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+$`
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
}

// RolloutApproval is the approval of one plan.
//
// +kubebuilder:validation:XValidation:rule="has(self.approvedBy) == has(self.planHash)",message="approvedBy and planHash are set together: an approval names the plan it approves"
type RolloutApproval struct {
	// ApprovedBy is the person who approved. Admission enforces that it
	// equals the requesting username, is empty at creation, is write-once,
	// and is not the operator; it is the proposer only where the site allows
	// self-approval.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`

	// PlanHash is the status.planHash the approver saw. The rollout runs
	// only while it equals status.planHash: a plan that changed after it was
	// shown is not the plan that was approved.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	PlanHash string `json:"planHash,omitempty"`
}

// RolloutNodeState is where one node of a plan stands.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Skipped
type RolloutNodeState string

const (
	RolloutNodePending   RolloutNodeState = "Pending"
	RolloutNodeRunning   RolloutNodeState = "Running"
	RolloutNodeSucceeded RolloutNodeState = "Succeeded"
	RolloutNodeFailed    RolloutNodeState = "Failed"
	// RolloutNodeSkipped: the rollout was aborted or failed before this
	// node started.
	RolloutNodeSkipped RolloutNodeState = "Skipped"
)

// RolloutStepKind tells a node's change from a cluster-wide step of the plan.
// +kubebuilder:validation:Enum=Node;KubeProxy
type RolloutStepKind string

const (
	// RolloutStepNode changes one node through a HostOperation.
	RolloutStepNode RolloutStepKind = "Node"
	// RolloutStepKubeProxy moves the kube-proxy DaemonSet to a new image
	// through the cluster's Kubernetes API, between the control planes and
	// the workers of a KubernetesUpgrade.
	RolloutStepKubeProxy RolloutStepKind = "KubeProxy"
)

// RolloutStep is one entry of a frozen plan. Everything that decides what is
// done to the node is in here, and so in the planHash.
type RolloutStep struct {
	// Kind is Node (one node through a HostOperation) or KubeProxy.
	Kind RolloutStepKind `json:"kind"`

	// Node is the TalosNode; Host the Host that runs it. Empty for a
	// KubeProxy step.
	// +optional
	Node string `json:"node,omitempty"`
	// +optional
	Host string `json:"host,omitempty"`
	// Role is the node's role; control planes go one at a time.
	// +optional
	Role NodeRole `json:"role,omitempty"`

	// From is what the node runs now, as far as the operator knows (a
	// version, or the digest of the config it last confirmed); To is what
	// it runs afterwards.
	// +optional
	From string `json:"from,omitempty"`
	// +optional
	To string `json:"to,omitempty"`

	// Image is the installer image of a TalosUpgrade, pinned by digest, or
	// the kube-proxy image of a KubeProxy step.
	// +optional
	Image string `json:"image,omitempty"`

	// ConfigSecret and ConfigDigest pin the rendered config a Config or
	// KubernetesUpgrade step applies. The Secret is content-addressed and
	// never rewritten, so the bytes approved are the bytes applied.
	// +optional
	ConfigSecret string `json:"configSecret,omitempty"`
	// +optional
	ConfigDigest string `json:"configDigest,omitempty"`

	// RebootRequired: the node said, in a dry run, that it cannot take
	// this config without a reboot. The node is then drained, the config
	// applied with a reboot, and the node uncordoned once it is back.
	// +optional
	RebootRequired bool `json:"rebootRequired,omitempty"`

	// Actions are the HostOperation action keys this step runs, in order,
	// for the approver to read.
	// +optional
	Actions []string `json:"actions,omitempty"`
}

// RolloutStepStatus is how one step of the plan went.
type RolloutStepStatus struct {
	// State is Pending, Running, Succeeded, Failed or Skipped.
	State RolloutNodeState `json:"state"`
	// Operation is the HostOperation (namespace/name) running a Node step.
	// +optional
	Operation string `json:"operation,omitempty"`
	// Message says why a step failed or what it waits for.
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// TalosRolloutStatus is the plan and how far it got.
type TalosRolloutStatus struct {
	// Conditions: Planned, Approved, Progressing, Paused, Complete.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Plan is the ordered list of steps: control planes first, one at a
	// time, then a KubeProxy step for a Kubernetes upgrade, then the
	// workers. Rewritten on every pass until approved, frozen afterwards.
	// +optional
	Plan []RolloutStep `json:"plan,omitempty"`

	// PlanHash is sha256 over the kind, cluster, target and plan: what an
	// approval names, and what every HostOperation of the rollout carries.
	// +optional
	PlanHash string `json:"planHash,omitempty"`

	// Steps is the progress of each plan entry, by index.
	// +optional
	Steps []RolloutStepStatus `json:"steps,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Approved",type=string,JSONPath=`.spec.approval.approvedBy`
// +kubebuilder:printcolumn:name="Progress",type=string,JSONPath=`.status.conditions[?(@.type=="Progressing")].reason`
// +kubebuilder:printcolumn:name="Complete",type=string,JSONPath=`.status.conditions[?(@.type=="Complete")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TalosRollout is one change rolled through the nodes of a cluster in a safe
// order — a Talos upgrade, a Kubernetes upgrade, a config change or a
// reboot — under one approval of a frozen plan, with a mandatory pause after
// the first node and health gates between nodes.
type TalosRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TalosRolloutSpec `json:"spec"`
	// +optional
	Status TalosRolloutStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TalosRolloutList contains a list of TalosRollout.
type TalosRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TalosRollout `json:"items"`
}
