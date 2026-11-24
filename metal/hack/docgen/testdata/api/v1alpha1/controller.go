package v1alpha1

// The markers and the finalizer stand in for a reconciler's.

// +kubebuilder:rbac:groups=metal.pk.io,resources=hostoperations,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,namespace=pk-metal

const releaseFinalizer = "metal.pk.io/release"
