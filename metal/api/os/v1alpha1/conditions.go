package v1alpha1

// Condition types. Same discipline as the metal group: conditions are the truth,
// each a fresh observation with observedGeneration — never a program counter.
const (
	// ConditionRendered: the TalosNode's machine config is rendered into its
	// config Secret at the current generation.
	ConditionRendered = "Rendered"

	// ConditionReady: the BootAsset's artifact exists and is fetchable (built
	// into its artifact Secret, or — factory — addressed by a computed URL).
	// On a TalosNode it is the check-in verdict instead: the machine's
	// aggregated status reports stage running and ready.
	ConditionReady = "Ready"

	// ConditionMachineReachable: the machine behind a hand-off answers its Talos
	// API at spec.ipAddress — over the authenticated machine API or, blank, the
	// maintenance service. Only observed while a Host hands this node a machine.
	ConditionMachineReachable = "MachineReachable"
)

// Reasons on the node's Rendered condition.
const (
	// ReasonConfigRendered on Rendered=True: the config Secret holds the render.
	ReasonConfigRendered = "ConfigRendered"
	// ReasonClusterNotFound on Rendered=False: clusterRef names no TalosCluster.
	ReasonClusterNotFound = "ClusterNotFound"
	// ReasonSecretsUnavailable on Rendered=False: the cluster secrets bundle
	// Secret is missing or unreadable — nothing renders without it.
	ReasonSecretsUnavailable = "SecretsUnavailable"
	// ReasonPatchesUnresolvable on Rendered=False: the spec carries "@" patch
	// references but the operator has no patch root to resolve them against.
	ReasonPatchesUnresolvable = "PatchesUnresolvable"
	// ReasonRenderFailed on Rendered=False: the render pipeline rejected the
	// inputs (the message carries the engine's error).
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
	// bytes behind that URL are not fetched — the machine does that — but they
	// are known to exist, which a computed URL alone never established.
	ReasonURLComputed = "URLComputed"
	// ReasonNodeNotFound on Ready=False: nodeRef names no TalosNode.
	ReasonNodeNotFound = "NodeNotFound"
	// ReasonConfigNotRendered on Ready=False: the node exists but has no rendered
	// config Secret yet — the build waits for the render.
	ReasonConfigNotRendered = "ConfigNotRendered"
	// ReasonBuildFailed on Ready=False: packing the artifact failed (the message
	// carries the builder's error).
	ReasonBuildFailed = "BuildFailed"
)

// Reasons on the node's MachineReachable / Ready check-in conditions.
const (
	// ReasonMachineAPI on MachineReachable=True: the authenticated machine API
	// answered — the node runs a config from our PKI.
	ReasonMachineAPI = "MachineAPI"
	// ReasonMaintenanceMode on MachineReachable=True (and Ready=Unknown): only
	// the unauthenticated maintenance service answers — the machine is up and
	// blank, awaiting its config.
	ReasonMaintenanceMode = "MaintenanceMode"
	// ReasonDialFailed on MachineReachable=False: neither service answered.
	ReasonDialFailed = "DialFailed"
	// ReasonAwaitingContact on Ready=Unknown: no observation yet — the machine
	// has never answered since the hand-off.
	ReasonAwaitingContact = "AwaitingContact"
	// ReasonConverging on Ready=Unknown: the machine answers but has not reached
	// stage running + ready (the message carries stage and unmet conditions).
	ReasonConverging = "Converging"
	// ReasonNodeReady on Ready=True: stage running, all conditions met — the
	// check-in signal that revokes grants and advances the Host to Provisioned.
	ReasonNodeReady = "NodeReady"
)
