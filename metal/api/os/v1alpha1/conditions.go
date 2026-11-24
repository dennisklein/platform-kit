package v1alpha1

// Condition types. Same discipline as the metal group: conditions are the truth,
// each a fresh observation with observedGeneration, never a program counter.
const (
	// ConditionRendered: the TalosNode's machine config is rendered into its
	// config Secret at the current generation.
	ConditionRendered = "Rendered"

	// ConditionReady: the BootAsset's artifact exists and is fetchable (built
	// into its artifact Secret, or, factory, addressed by a computed URL).
	// On a TalosNode it is the check-in verdict instead: the machine's
	// aggregated status reports stage running and ready.
	ConditionReady = "Ready"

	// ConditionMachineReachable: the machine behind a hand-off answers its Talos
	// API at the node's address, over the authenticated machine API or, blank,
	// the maintenance service. Only observed while a Host hands this node a
	// machine.
	ConditionMachineReachable = "MachineReachable"

	// ConditionBootstrapped: a TalosCluster's etcd exists. Observed: True
	// when a control-plane member reports etcd members or the Kubernetes API
	// answers. The first True is also remembered, as status.bootstrappedAt,
	// so that a cluster whose etcd data is lost is never bootstrapped again.
	ConditionBootstrapped = "Bootstrapped"

	// ConditionAvailable: a TalosCluster's Kubernetes API answers and every
	// member TalosNode is Ready.
	ConditionAvailable = "Available"
)

// Reasons on the node's Rendered condition.
const (
	// ReasonConfigRendered on Rendered=True: the config Secret holds the render.
	ReasonConfigRendered = "ConfigRendered"
	// ReasonClusterNotFound on Rendered=False: clusterRef names no TalosCluster.
	ReasonClusterNotFound = "ClusterNotFound"
	// ReasonSecretsUnavailable on Rendered=False (and on a cluster's
	// Bootstrapped/Available): the secrets bundle Secret is missing,
	// unreadable or invalid. Nothing renders without it.
	ReasonSecretsUnavailable = "SecretsUnavailable"
	// ReasonPatchesUnresolvable on Rendered=False: a patch names a ConfigMap
	// or key that does not exist.
	ReasonPatchesUnresolvable = "PatchesUnresolvable"
	// ReasonRenderFailed on Rendered=False: the inputs do not form a valid
	// Talos configuration (the message carries the render or validation
	// error).
	ReasonRenderFailed = "RenderFailed"
	// ReasonSchematicUnavailable on Rendered=False (and on a BootAsset's
	// Ready=False): the image factory could not be told about the schematic, so
	// the image it addresses may not exist. Rendering a config that names an
	// installer nothing serves would move the failure onto the machine, where
	// the only witness is a serial console.
	ReasonSchematicUnavailable = "SchematicUnavailable"
)

// Reasons on the asset's Ready condition.
const (
	// ReasonArtifactBuilt on Ready=True: the artifact Secret holds the built
	// bytes; digest and size are stamped.
	ReasonArtifactBuilt = "ArtifactBuilt"
	// ReasonURLComputed on Ready=True (factory): the image factory knows the
	// schematic, and the content-addressed URL of its image is stamped. The
	// bytes behind that URL are not fetched (the machine does that) but they
	// are known to exist, which a computed URL alone never established.
	ReasonURLComputed = "URLComputed"
	// ReasonNodeNotFound on Ready=False: nodeRef names no TalosNode.
	ReasonNodeNotFound = "NodeNotFound"
	// ReasonConfigNotRendered on Ready=False: the node exists but has no rendered
	// config Secret yet; the build waits for the render.
	ReasonConfigNotRendered = "ConfigNotRendered"
	// ReasonBuildFailed on Ready=False: packing the artifact failed (the message
	// carries the builder's error).
	ReasonBuildFailed = "BuildFailed"
	// ReasonBuilding on Ready=False (embeddedInstaller): the imager Job is
	// building the artifact for the current inputs. A stored artifact of
	// earlier inputs stays in status until the new one replaces it.
	ReasonBuilding = "Building"
	// ReasonBuilderUnavailable on Ready=False (embeddedInstaller): this
	// operator cannot build the artifact: no media store, no imager image
	// pinned for the Talos version, or a Talos version without embedded
	// config support. The message names the flag or field to set.
	ReasonBuilderUnavailable = "BuilderUnavailable"
)

// Reasons on the node's MachineReachable / Ready check-in conditions.
const (
	// ReasonMachineAPI on MachineReachable=True: the authenticated machine API
	// answered, so the node runs a config from our PKI.
	ReasonMachineAPI = "MachineAPI"
	// ReasonMaintenanceMode on MachineReachable=True (and Ready=Unknown): only
	// the unauthenticated maintenance service answers; the machine is up and
	// blank, awaiting its config.
	ReasonMaintenanceMode = "MaintenanceMode"
	// ReasonDialFailed on MachineReachable=False: neither service answered.
	ReasonDialFailed = "DialFailed"
	// ReasonNoAddress on MachineReachable=False: the node has neither
	// spec.address nor a static link address, so there is nothing to dial.
	ReasonNoAddress = "NoAddress"
	// ReasonAwaitingContact on Ready=Unknown: no observation yet; the machine
	// has never answered since the hand-off.
	ReasonAwaitingContact = "AwaitingContact"
	// ReasonConverging on Ready=Unknown: the machine answers but has not reached
	// stage running + ready (the message carries stage and unmet conditions).
	ReasonConverging = "Converging"
	// ReasonNodeReady on Ready=True: stage running, all conditions met; the
	// check-in signal that revokes grants and advances the Host to Provisioned.
	ReasonNodeReady = "NodeReady"
)

// Reasons on the cluster's Bootstrapped / Available conditions.
const (
	// ReasonEtcdRunning on Bootstrapped=True: a control-plane member reports
	// etcd members.
	ReasonEtcdRunning = "EtcdRunning"
	// ReasonKubernetesAnswers on Bootstrapped=True: no member could be asked
	// about etcd, but the Kubernetes API answers, which it cannot without etcd.
	ReasonKubernetesAnswers = "KubernetesAnswers"
	// ReasonBootstrapRequested on Bootstrapped=Unknown: the operator asked a
	// control-plane member to bootstrap etcd and waits to observe it running.
	ReasonBootstrapRequested = "BootstrapRequested"
	// ReasonAwaitingControlPlane on Bootstrapped=False: not every control-plane
	// member answers its authenticated API yet. Bootstrap waits for all of
	// them, so that a member that was already bootstrapped but is down cannot
	// be mistaken for a cluster that never was.
	ReasonAwaitingControlPlane = "AwaitingControlPlane"
	// ReasonNoControlPlane on Bootstrapped=False: no TalosNode with role
	// controlplane names this cluster.
	ReasonNoControlPlane = "NoControlPlane"
	// ReasonBootstrapRefused on Bootstrapped=False: the member refused the
	// bootstrap call (the message carries why; it is retried).
	ReasonBootstrapRefused = "BootstrapRefused"
	// ReasonEtcdDataLost on Bootstrapped=False: the cluster was bootstrapped
	// before (status.bootstrappedAt), and now every control plane answers
	// and none runs etcd. The operator never bootstraps such a cluster
	// again; a person restores etcd or starts over.
	ReasonEtcdDataLost = "EtcdDataLost"
	// ReasonEtcdUnavailable on Bootstrapped=False: the cluster was
	// bootstrapped before, and no member lists etcd members now, while some
	// do not answer or run etcd without a quorum. It may come back.
	ReasonEtcdUnavailable = "EtcdUnavailable"
	// ReasonEtcdDataPresent on Bootstrapped=False: a cluster not yet
	// bootstrapped has a member whose etcd runs but lists no members, data
	// from an etcd that lost its quorum. Bootstrap is refused until a person
	// restores or resets it.
	ReasonEtcdDataPresent = "EtcdDataPresent"
	// ReasonClusterAvailable on Available=True.
	ReasonClusterAvailable = "ClusterAvailable"
	// ReasonKubernetesUnreachable on Available=False: the Kubernetes API does
	// not answer at the endpoint.
	ReasonKubernetesUnreachable = "KubernetesUnreachable"
	// ReasonNodesNotReady on Available=False: the API answers, but some member
	// TalosNode is not Ready.
	ReasonNodesNotReady = "NodesNotReady"
	// ReasonNotBootstrapped on Available=False: etcd does not exist yet.
	ReasonNotBootstrapped = "NotBootstrapped"
)
