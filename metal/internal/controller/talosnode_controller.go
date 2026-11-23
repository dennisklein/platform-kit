package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// secretsBundleKey is the data key of the cluster secrets Secret — the talhelper-
// format bundle, named like its sops-encrypted source of truth in git.
const secretsBundleKey = "talsecret.yaml"

// TalosNodeReconciler renders each node's machine config into an owned Secret
// (design doc § "Boot assets": the Secret is the single place the rendered
// secrets exist at rest, under RBAC and encryption — BootAsset builds from it,
// the asset server serves it under a grant). Rendered=False is alert-and-hold:
// the last good Secret stays; a broken input never un-renders a machine.
type TalosNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// SecretsSecret names the Secret in Namespace holding the cluster secrets
	// bundle under key talsecret.yaml (day-0: created from the sops file by the
	// runbook, plaintext piped straight from `sops -d`).
	SecretsSecret string
	// Namespace holds the bundle and receives the rendered config Secrets — the
	// operator's namespace; TalosNodes are cluster-scoped, Secrets are not.
	Namespace string
	// PatchRoot is the directory "@./" patch references resolve against (the
	// ConfigMap-mounted mirror of control/talos in-cluster). Empty: nodes whose
	// specs carry "@" references hold at PatchesUnresolvable.
	PatchRoot string
	// Factory serves the installer image a rendered config names, and is told
	// about the node's schematic before that name is written down.
	Factory *render.Factory
	// Dial opens machine-API connections for the check-in; nil disables it
	// (render-only operation). Production wires machineapi.Dial, envtest the fake.
	Dial talos.Dialer
	// CheckInterval paces the check-in poll while the machine converges;
	// ReadyInterval once it reported Ready (liveness). Zero values default.
	CheckInterval time.Duration
	ReadyInterval time.Duration
}

// The check-in cadences: tight while a hand-off converges, relaxed once the
// node is Ready and the poll is mere liveness.
const (
	defaultCheckInterval = 10 * time.Second
	defaultReadyInterval = 2 * time.Minute
	// checkinCertTTL bounds the per-dial admin client cert — minted from the
	// bundle for one observation, never persisted.
	checkinCertTTL = 10 * time.Minute
)

func (r *TalosNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The config Secret dies with its owner — nothing to do here.
	if !node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	orig := node.DeepCopy()
	if err := r.render(ctx, node); err != nil {
		return ctrl.Result{}, err
	}
	res, err := r.checkin(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(orig.Status, node.Status) {
		if err := r.Status().Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	return res, nil
}

// checkin observes the machine a Host hands this node — the authenticated
// machine API first, the maintenance service as the blank-machine fallback —
// and reports MachineReachable/Ready plus stage and version. Errors here are
// observations (conditions), never reconcile errors: an unreachable machine is
// a state, and the poll re-observes it.
func (r *TalosNodeReconciler) checkin(ctx context.Context, node *osv1alpha1.TalosNode) (ctrl.Result, error) {
	if r.Dial == nil {
		return ctrl.Result{}, nil
	}
	handed, err := nodeHandedOff(ctx, r.Client, node.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !handed {
		// No machine is handed this node — nothing to observe. Stale
		// observations leave with the hand-off (the release path).
		meta.RemoveStatusCondition(&node.Status.Conditions, osv1alpha1.ConditionMachineReachable)
		meta.RemoveStatusCondition(&node.Status.Conditions, osv1alpha1.ConditionReady)
		node.Status.Stage, node.Status.MachineVersion = "", ""
		return ctrl.Result{}, nil
	}

	endpoint := node.Spec.IPAddress
	if i := strings.IndexByte(endpoint, ','); i >= 0 {
		endpoint = endpoint[:i]
	}

	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg,
			ObservedGeneration: node.Generation,
		})
	}

	st, version, mode, oerr := r.observe(ctx, node, endpoint)
	switch mode {
	case "api":
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionTrue,
			osv1alpha1.ReasonMachineAPI, "machine API answering at "+endpoint)
		node.Status.Stage, node.Status.MachineVersion = st.Stage, version
		if st.Stage == "running" && st.Ready {
			set(osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonNodeReady, "")
			return ctrl.Result{RequeueAfter: cmp.Or(r.ReadyInterval, defaultReadyInterval)}, nil
		}
		msg := "stage " + st.Stage
		if len(st.UnmetConditions) > 0 {
			msg += "; unmet: " + strings.Join(st.UnmetConditions, ", ")
		}
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown, osv1alpha1.ReasonConverging, msg)
	case "maintenance":
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionTrue,
			osv1alpha1.ReasonMaintenanceMode, "maintenance service answering at "+endpoint+" — awaiting config")
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown, osv1alpha1.ReasonMaintenanceMode, "machine is blank")
		node.Status.Stage, node.Status.MachineVersion = "maintenance", version
	default:
		set(osv1alpha1.ConditionMachineReachable, metav1.ConditionFalse,
			osv1alpha1.ReasonDialFailed, oerr.Error())
		set(osv1alpha1.ConditionReady, metav1.ConditionUnknown,
			osv1alpha1.ReasonAwaitingContact, "machine not answering at "+endpoint)
		node.Status.Stage, node.Status.MachineVersion = "", ""
	}
	return ctrl.Result{RequeueAfter: cmp.Or(r.CheckInterval, defaultCheckInterval)}, nil
}

// observe tries the authenticated machine API, then the maintenance service.
// mode is "api", "maintenance", or "" with the last dial error.
func (r *TalosNodeReconciler) observe(ctx context.Context, node *osv1alpha1.TalosNode, endpoint string) (st talos.Status, version, mode string, err error) {
	if tcfg, terr := r.talosconfig(ctx, node); terr == nil {
		if c, derr := r.Dial(ctx, talos.Target{Endpoint: endpoint, Talosconfig: tcfg}); derr == nil {
			st, err = c.Status(ctx)
			if err == nil {
				version, _ = c.Version(ctx)
				_ = c.Close()
				return st, version, "api", nil
			}
			_ = c.Close()
		}
	}
	c, derr := r.Dial(ctx, talos.Target{Endpoint: endpoint, Insecure: true})
	if derr != nil {
		return talos.Status{}, "", "", derr
	}
	defer func() { _ = c.Close() }()
	version, verr := c.Version(ctx)
	if verr != nil {
		return talos.Status{}, "", "", verr
	}
	return talos.Status{}, version, "maintenance", nil
}

// talosconfig mints the per-dial client credentials from the cluster spec and
// the secrets bundle — in memory, TTL-bounded, never persisted.
func (r *TalosNodeReconciler) talosconfig(ctx context.Context, node *osv1alpha1.TalosNode) ([]byte, error) {
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		return nil, err
	}
	bundleSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.SecretsSecret}, bundleSecret); err != nil {
		return nil, err
	}
	cfg, err := render.Map(cluster, []osv1alpha1.TalosNode{*node}, r.PatchRoot)
	if err != nil {
		return nil, err
	}
	input, err := render.Input(cfg, bundleSecret.Data[secretsBundleKey])
	if err != nil {
		return nil, err
	}
	return render.Talosconfig(cfg, input, checkinCertTTL)
}

// nodeHandedOff reports whether any Host's OS hand-off names this node. A plain
// list-and-filter — the rack is small and this avoids a cross-controller index.
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

// render observes the inputs and either writes the config Secret + Rendered=True
// or parks the node on a False condition. Only transport errors return error —
// semantic failures are conditions, re-triggered by the watches on their inputs.
func (r *TalosNodeReconciler) render(ctx context.Context, node *osv1alpha1.TalosNode) error {
	hold := func(reason, message string) {
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type: osv1alpha1.ConditionRendered, Status: metav1.ConditionFalse,
			Reason: reason, Message: message, ObservedGeneration: node.Generation,
		})
	}

	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Spec.ClusterRef.Name}, cluster); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		hold(osv1alpha1.ReasonClusterNotFound,
			fmt.Sprintf("clusterRef names no TalosCluster %q", node.Spec.ClusterRef.Name))
		return nil
	}

	bundleSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: r.SecretsSecret}, bundleSecret); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		hold(osv1alpha1.ReasonSecretsUnavailable,
			fmt.Sprintf("secrets Secret %s/%s not found", r.Namespace, r.SecretsSecret))
		return nil
	}
	bundle := bundleSecret.Data[secretsBundleKey]
	if len(bundle) == 0 {
		hold(osv1alpha1.ReasonSecretsUnavailable,
			fmt.Sprintf("secrets Secret %s/%s carries no %s", r.Namespace, r.SecretsSecret, secretsBundleKey))
		return nil
	}

	if r.PatchRoot == "" && hasFileRefs(cluster.Spec.Patches, node.Spec.Patches) {
		hold(osv1alpha1.ReasonPatchesUnresolvable,
			`spec carries "@" patch references but the operator runs without a patch root`)
		return nil
	}

	// The config names its own installer image by schematic id, and the machine
	// pulls it mid-install with only a serial console watching. Register before
	// rendering: an id no factory knows is a 404 there, and a sentence here.
	if _, err := r.Factory.Register(ctx, node.Spec.Schematic); err != nil {
		hold(osv1alpha1.ReasonSchematicUnavailable, err.Error())
		return nil
	}

	cfg, err := render.Map(cluster, []osv1alpha1.TalosNode{*node}, r.PatchRoot)
	if err != nil {
		hold(osv1alpha1.ReasonRenderFailed, err.Error())
		return nil
	}
	// Whichever factory we registered with is the one the installer URL must
	// name; talhelper's default would silently address the public one.
	cfg.ImageFactory.RegistryURL = r.Factory.RegistryURL
	input, err := render.Input(cfg, bundle)
	if err != nil {
		hold(osv1alpha1.ReasonRenderFailed, err.Error())
		return nil
	}
	machineConfig, err := render.RenderNode(cfg, node.Name, input, true)
	if err != nil {
		hold(osv1alpha1.ReasonRenderFailed, err.Error())
		return nil
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: r.Namespace, Name: "machineconfig-" + node.Name,
	}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Data = map[string][]byte{asset.ConfigFilename: machineConfig}
		return controllerutil.SetControllerReference(node, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("write config secret for %s: %w", node.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("rendered machine config", "node", node.Name, "secret", secret.Name, "op", op)
	}

	node.Status.ConfigSecretRef = &osv1alpha1.SecretReference{Namespace: secret.Namespace, Name: secret.Name}
	node.Status.ConfigDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(machineConfig))
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type: osv1alpha1.ConditionRendered, Status: metav1.ConditionTrue,
		Reason: osv1alpha1.ReasonConfigRendered, ObservedGeneration: node.Generation,
	})
	return nil
}

// hasFileRefs reports whether any patch list uses "@" file references —
// talhelper's rule: only "@"-prefixed strings are files.
func hasFileRefs(lists ...[]string) bool {
	for _, patches := range lists {
		for _, p := range patches {
			if strings.HasPrefix(p, "@") {
				return true
			}
		}
	}
	return false
}

func (r *TalosNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.TalosNode{}).
		Owns(&corev1.Secret{}).
		Watches(&osv1alpha1.TalosCluster{}, handler.EnqueueRequestsFromMapFunc(r.nodesForCluster)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.nodesForBundle)).
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
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: os.NodeRef.Name}}}
	}
	return nil
}

// nodesForCluster re-renders every member when its cluster's spec moves.
func (r *TalosNodeReconciler) nodesForCluster(ctx context.Context, obj client.Object) []ctrl.Request {
	nodes := &osv1alpha1.TalosNodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range nodes.Items {
		if nodes.Items[i].Spec.ClusterRef.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodes.Items[i].Name}})
		}
	}
	return reqs
}

// nodesForBundle re-renders everything when the cluster secrets bundle rotates.
func (r *TalosNodeReconciler) nodesForBundle(ctx context.Context, obj client.Object) []ctrl.Request {
	if obj.GetNamespace() != r.Namespace || obj.GetName() != r.SecretsSecret {
		return nil
	}
	nodes := &osv1alpha1.TalosNodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range nodes.Items {
		reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: nodes.Items[i].Name}})
	}
	return reqs
}
