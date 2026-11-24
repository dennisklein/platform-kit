package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// TalosNodeReconciler renders each node's machine config into an owned Secret
// (the one place the rendered secrets rest, under RBAC and encryption: the
// BootAsset builds from it, the asset server serves it under a grant), and
// checks in with the machine a Host hands the node. Rendered=False is
// alert-and-hold: the last good Secret stays; a broken input never un-renders
// a machine.
type TalosNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace receives the rendered config Secrets: the operator's
	// namespace, since TalosNodes are cluster-scoped and Secrets are not.
	Namespace string
	// Factory serves the installer image a rendered config names, and is told
	// about the node's schematic before that name is written down.
	Factory *render.Factory
	// Dial opens machine-API connections for the check-in; nil disables it
	// (render-only operation). Production wires machineapi.Dial, envtest the
	// fake.
	Dial talos.Dialer
	// CheckInterval paces the check-in poll while the machine converges;
	// ReadyInterval once it reported Ready (liveness). Zero values default.
	CheckInterval time.Duration
	ReadyInterval time.Duration
	// DriftInterval paces the dry-run drift check of a Ready node; zero
	// defaults. A new render, or a rollout of the node's cluster, checks
	// sooner.
	DriftInterval time.Duration
}

// The check-in cadences: tight while a hand-off converges, relaxed once the
// node is Ready and the poll is mere liveness.
const (
	defaultCheckInterval = 10 * time.Second
	defaultReadyInterval = 2 * time.Minute
	// checkinCertTTL bounds the per-dial admin client cert, minted from the
	// bundle for one observation and never persisted.
	checkinCertTTL = 10 * time.Minute
)

// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=metal.pk.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update,namespace=pk-metal
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch,namespace=pk-metal

// Reconcile renders a node's machine configuration into a Secret and checks
// the machine in over the Talos API.
func (r *TalosNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The config Secret dies with its owner; nothing to do here.
	if !node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	orig := node.DeepCopy()
	cluster, bundle, rendered, err := r.render(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	res, err := r.checkin(ctx, node, cluster, bundle, rendered)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := updateStatusIfChanged(ctx, r.Client, node, &orig.Status, &node.Status); err != nil {
		return ctrl.Result{}, err
	}
	// Collected after the status is written: a config Secret goes only once
	// the status no longer points at it.
	if err := r.collectConfigSecrets(ctx, node); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

// render observes the inputs and either writes the config Secret and
// Rendered=True or parks the node on a False condition. Only transport errors
// return error: semantic failures are conditions, re-triggered by the watches
// on their inputs. It hands the cluster and bundle on to the check-in when it
// got that far, and the rendered bytes when this pass rendered.
func (r *TalosNodeReconciler) render(ctx context.Context, node *osv1alpha1.TalosNode) (
	cluster *osv1alpha1.TalosCluster, bundle *secrets.Bundle, rendered []byte, err error) {
	hold := func(reason, message string) {
		setCondition(&node.Status.Conditions, node.Generation, osv1alpha1.ConditionRendered, metav1.ConditionFalse, reason, message)
	}
	node.Status.Address = render.NodeAddress(node)

	cluster = &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, nil, nil, err
		}
		hold(osv1alpha1.ReasonClusterNotFound,
			fmt.Sprintf("clusterRef names no TalosCluster %q", node.Spec.ClusterRef.Name))
		return nil, nil, nil, nil
	}

	bundle, err = loadBundle(ctx, r.Client, r.Namespace, cluster)
	if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
		hold(osv1alpha1.ReasonSecretsUnavailable, unavailable.msg)
		return cluster, nil, nil, nil
	} else if err != nil {
		return nil, nil, nil, err
	}

	read := configMapReader(ctx, r.Client, r.Namespace)
	clusterPatches, err := render.ResolvePatches(cluster.Spec.Patches, read)
	if err == nil {
		var nodePatches [][]byte
		nodePatches, err = render.ResolvePatches(node.Spec.Patches, read)
		if err == nil {
			rendered, err = r.renderWith(ctx, node, cluster, bundle, clusterPatches, nodePatches, hold)
		}
	}
	switch {
	case errors.Is(err, render.ErrPatchUnresolvable):
		hold(osv1alpha1.ReasonPatchesUnresolvable, err.Error())
	case err != nil:
		return nil, nil, nil, err
	}
	return cluster, bundle, rendered, nil
}

// renderWith is the render proper, once every input is in hand. It renders
// for the Talos version the machine runs while that is older than the
// cluster's (render.RenderVersion), and writes the result into a
// content-addressed Secret that is never rewritten: an operation approved for
// one digest finds exactly those bytes under that name.
func (r *TalosNodeReconciler) renderWith(ctx context.Context, node *osv1alpha1.TalosNode, cluster *osv1alpha1.TalosCluster,
	bundle *secrets.Bundle, clusterPatches, nodePatches [][]byte, hold func(string, string)) ([]byte, error) {
	version := render.RenderVersion(cluster.Spec.TalosVersion, node.Status.Version, node.Status.RenderedFor)
	if err := render.SupportedPair(version, cluster.Spec.KubernetesVersion); err != nil {
		hold(osv1alpha1.ReasonUnsupportedVersion, err.Error())
		return nil, nil
	}
	target := cluster
	if version != cluster.Spec.TalosVersion {
		target = cluster.DeepCopy()
		target.Spec.TalosVersion = version
	}

	// The config names its own installer image by schematic id, and the
	// machine pulls it mid-install with only a serial console watching.
	// Register before rendering: an id no factory knows is a 404 there, and a
	// sentence here.
	schematic := render.EffectiveSchematic(cluster, node)
	if _, err := r.Factory.Register(ctx, schematic); err != nil {
		hold(osv1alpha1.ReasonSchematicUnavailable, err.Error())
		return nil, nil
	}
	installer, err := r.Factory.InstallerImage(schematic, version)
	if err != nil {
		hold(osv1alpha1.ReasonRenderFailed, err.Error())
		return nil, nil
	}

	machineConfig, err := render.Node(render.Input{
		Cluster: target, Node: node, Bundle: bundle, InstallerImage: installer,
		ClusterPatches: clusterPatches, NodePatches: nodePatches,
	})
	if err != nil {
		hold(osv1alpha1.ReasonRenderFailed, err.Error())
		return nil, nil
	}

	sum := digest.FromBytes(machineConfig)
	secret, err := r.writeConfigSecret(ctx, node, sum, machineConfig)
	if err != nil {
		return nil, err
	}

	node.Status.ConfigSecretRef = &osv1alpha1.SecretReference{Namespace: secret.Namespace, Name: secret.Name}
	node.Status.ConfigDigest = sum.String()
	node.Status.InstallerImage = installer
	node.Status.RenderedFor = version
	setCondition(&node.Status.Conditions, node.Generation, osv1alpha1.ConditionRendered, metav1.ConditionTrue, osv1alpha1.ReasonConfigRendered, "")
	return machineConfig, nil
}

// checkin observes the machine a Host hands this node (the authenticated
// machine API first, the maintenance service as the blank-machine fallback)
// and reports MachineReachable/Ready plus stage and version. Errors here are
// observations (conditions), never reconcile errors: an unreachable machine is
// a state, and the poll re-observes it.
func (r *TalosNodeReconciler) checkin(ctx context.Context, node *osv1alpha1.TalosNode,
	cluster *osv1alpha1.TalosCluster, bundle *secrets.Bundle, rendered []byte) (ctrl.Result, error) {
	if r.Dial == nil {
		return ctrl.Result{}, nil
	}
	handed, err := nodeHandedOff(ctx, r.Client, node.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !handed {
		// No machine is handed this node: nothing to observe. Stale
		// observations leave with the hand-off (the release path).
		meta.RemoveStatusCondition(&node.Status.Conditions, osv1alpha1.ConditionMachineReachable)
		meta.RemoveStatusCondition(&node.Status.Conditions, osv1alpha1.ConditionReady)
		meta.RemoveStatusCondition(&node.Status.Conditions, osv1alpha1.ConditionConfigInSync)
		meta.RemoveStatusCondition(&node.Status.Conditions, osv1alpha1.ConditionVersionCurrent)
		node.Status.Stage, node.Status.Version, node.Status.Drift = "", "", nil
		return ctrl.Result{}, nil
	}

	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		setCondition(&node.Status.Conditions, node.Generation, t, s, reason, msg)
	}
	retry := ctrl.Result{RequeueAfter: cmp.Or(r.CheckInterval, defaultCheckInterval)}

	endpoint := node.Status.Address
	if endpoint == "" {
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionFalse, osv1alpha1.ReasonNoAddress,
			"set spec.address or a static link address")
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown, osv1alpha1.ReasonAwaitingContact, "nothing to dial")
		node.Status.Stage, node.Status.Version = "", ""
		return retry, nil
	}

	var tcfg []byte
	if cluster != nil && bundle != nil {
		tcfg, _ = render.Talosconfig(bundle, cluster.Name, []string{endpoint}, checkinCertTTL)
	}
	st, version, mode, oerr := r.observe(ctx, tcfg, endpoint)
	switch mode {
	case "api":
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionTrue,
			osv1alpha1.ReasonMachineAPI, "machine API answering at "+endpoint)
		node.Status.Stage, node.Status.Version = st.Stage, version
		r.observeVersion(node, cluster)
		if st.Stage == "running" && st.Ready {
			set(osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonNodeReady, "")
			rolling, err := r.rolloutActive(ctx, node.Spec.ClusterRef.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			r.checkDrift(ctx, node, tcfg, endpoint, rendered, rolling)
			if rolling {
				// The health gates between the nodes of a rollout read
				// this: a Ready polled two minutes ago is no gate (D7).
				return retry, nil
			}
			return ctrl.Result{RequeueAfter: cmp.Or(r.ReadyInterval, defaultReadyInterval)}, nil
		}
		msg := "stage " + st.Stage
		if len(st.UnmetConditions) > 0 {
			msg += "; unmet: " + strings.Join(st.UnmetConditions, ", ")
		}
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown, osv1alpha1.ReasonConverging, msg)
	case "maintenance":
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionTrue,
			osv1alpha1.ReasonMaintenanceMode, "maintenance service answering at "+endpoint+"; awaiting config")
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown, osv1alpha1.ReasonMaintenanceMode, "machine is blank")
		node.Status.Stage, node.Status.Version = "maintenance", version
	default:
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionFalse,
			osv1alpha1.ReasonDialFailed, oerr.Error())
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown,
			osv1alpha1.ReasonAwaitingContact, "machine not answering at "+endpoint)
		node.Status.Stage, node.Status.Version = "", ""
	}
	return retry, nil
}

// observe tries the authenticated machine API, then the maintenance service.
// mode is "api", "maintenance", or "" with the last dial error.
func (r *TalosNodeReconciler) observe(ctx context.Context, tcfg []byte, endpoint string) (st talos.Status, version, mode string, err error) {
	if tcfg != nil {
		// Any failure here falls through to the maintenance service.
		if talos.With(ctx, r.Dial, talos.Target{Endpoint: endpoint, Talosconfig: tcfg}, func(c talos.Client) error {
			if st, err = c.Status(ctx); err != nil {
				return err
			}
			version, _ = c.Version(ctx)
			return nil
		}) == nil {
			return st, version, "api", nil
		}
	}
	if err := talos.With(ctx, r.Dial, talos.Target{Endpoint: endpoint, Insecure: true}, func(c talos.Client) error {
		var verr error
		version, verr = c.Version(ctx)
		return verr
	}); err != nil {
		return talos.Status{}, "", "", err
	}
	return talos.Status{}, version, "maintenance", nil
}

// nodeHandedOff reports whether any Host's OS hand-off names this node. A plain
// list-and-filter: the rack is small and this avoids a cross-controller index.
func nodeHandedOff(ctx context.Context, cl client.Client, nodeName string) (bool, error) {
	hosts := &metalv1alpha1.HostList{}
	if err := cl.List(ctx, hosts); err != nil {
		return false, err
	}
	for i := range hosts.Items {
		if os := hosts.Items[i].Spec.OS; os != nil && os.NodeRef.Name == nodeName {
			return true, nil
		}
	}
	return false, nil
}

func (r *TalosNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.TalosNode{}).
		Owns(&corev1.Secret{}).
		Watches(&osv1alpha1.TalosCluster{}, handler.EnqueueRequestsFromMapFunc(r.nodesForCluster)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.nodesForBundle)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.nodesForConfigMap)).
		Watches(&osv1alpha1.TalosRollout{}, handler.EnqueueRequestsFromMapFunc(r.nodesForRollout)).
		Named("talosnode")
	if r.Dial != nil {
		// A hand-off appearing (or leaving) on a Host starts (or ends) the poll.
		b = b.Watches(&metalv1alpha1.Host{}, handler.EnqueueRequestsFromMapFunc(nodeForHost))
	}
	return b.Complete(r)
}

// nodeForHost maps a Host event to the TalosNode its hand-off names.
func nodeForHost(_ context.Context, obj client.Object) []ctrl.Request {
	if os := obj.(*metalv1alpha1.Host).Spec.OS; os != nil {
		return []ctrl.Request{requestFor(os.NodeRef.Name)}
	}
	return nil
}

// nodesForCluster re-renders every member when its cluster's spec moves.
func (r *TalosNodeReconciler) nodesForCluster(ctx context.Context, obj client.Object) []ctrl.Request {
	nodes, err := clusterMembers(ctx, r.Client, obj.GetName())
	if err != nil {
		return nil
	}
	reqs := make([]ctrl.Request, 0, len(nodes))
	for _, n := range nodes {
		reqs = append(reqs, requestFor(n.Name))
	}
	return reqs
}

// nodesForBundle re-renders a cluster's members when its secrets bundle
// appears or rotates.
func (r *TalosNodeReconciler) nodesForBundle(ctx context.Context, obj client.Object) []ctrl.Request {
	var reqs []ctrl.Request
	for _, c := range clustersUsingSecret(ctx, r.Client, obj) {
		reqs = append(reqs, r.nodesForCluster(ctx, &c)...)
	}
	return reqs
}

// nodesForConfigMap re-renders the nodes a patch ConfigMap reaches, through
// their own patches or their cluster's.
func (r *TalosNodeReconciler) nodesForConfigMap(ctx context.Context, obj client.Object) []ctrl.Request {
	clusters := &osv1alpha1.TalosClusterList{}
	nodes := &osv1alpha1.TalosNodeList{}
	if r.List(ctx, clusters) != nil || r.List(ctx, nodes) != nil {
		return nil
	}
	affected := map[string]bool{}
	for _, c := range clusters.Items {
		affected[c.Name] = patchesUse(c.Spec.Patches, obj)
	}
	var reqs []ctrl.Request
	for _, n := range nodes.Items {
		if affected[n.Spec.ClusterRef.Name] || patchesUse(n.Spec.Patches, obj) {
			reqs = append(reqs, requestFor(n.Name))
		}
	}
	return reqs
}
