package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bootstrap"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Credential lifetimes. The published admin credentials live a year, like the
// ones Talos mints itself, and are re-minted a month before they expire, or as
// soon as the bundle or the endpoints they name move.
const (
	adminCredentialTTL   = 365 * 24 * time.Hour
	adminCredentialRenew = 30 * 24 * time.Hour
	// KubeconfigKey and TalosconfigKey are the data keys of the published
	// credential Secrets.
	KubeconfigKey  = "kubeconfig"
	TalosconfigKey = "talosconfig"
)

// KubernetesProber asks a cluster's Kubernetes API for its version with the
// given kubeconfig. It is a seam: envtest has no cluster behind the endpoint.
type KubernetesProber func(ctx context.Context, kubeconfig []byte) (string, error)

// TalosClusterReconciler brings a cluster from "nodes booted" to
// "bootstrapped, healthy, kubeconfig available". It publishes the admin
// kubeconfig and talosconfig (both minted from the secrets bundle, so they
// are re-derivable), bootstraps etcd once through internal/bootstrap, and
// reports member readiness and whether the Kubernetes API answers.
type TalosClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace receives the credential Secrets (the operator's namespace).
	Namespace string
	// Dial opens machine-API connections; nil disables bootstrapping and the
	// etcd observation (credentials and counts are still maintained).
	Dial talos.Dialer
	// ProbeKubernetes defaults to a discovery client with a short timeout.
	ProbeKubernetes KubernetesProber
	// CheckInterval paces the poll until the cluster is Available;
	// ReadyInterval afterwards. Zero values default.
	CheckInterval time.Duration
	ReadyInterval time.Duration
	// BootstrapGrace overrides bootstrap.RequestGrace (tests).
	BootstrapGrace time.Duration

	// requests remembers, by cluster UID, when this process last had a
	// bootstrap request accepted. The status condition is the durable record,
	// but a pass that runs before the informer cache shows that status write
	// would read no request and ask again; this closes that gap.
	requests sync.Map
}

// outstandingRequest is the later of the recorded request times: the
// Bootstrapped=Unknown/BootstrapRequested transition time in status, and this
// process's own record.
func (r *TalosClusterReconciler) outstandingRequest(cluster *osv1alpha1.TalosCluster) time.Time {
	var at time.Time
	if c := meta.FindStatusCondition(cluster.Status.Conditions, osv1alpha1.ConditionBootstrapped); c != nil &&
		c.Status == metav1.ConditionUnknown && c.Reason == osv1alpha1.ReasonBootstrapRequested {
		at = c.LastTransitionTime.Time
	}
	if v, ok := r.requests.Load(cluster.UID); ok && v.(time.Time).After(at) {
		at = v.(time.Time)
	}
	return at
}

// rememberedBootstrap is when the cluster was first seen bootstrapped. A cluster
// that an operator without status.bootstrappedAt saw bootstrapped carries
// only Bootstrapped=True; its transition time stands in, and is written into
// the status here, so the upgrade to this operator does not forget what the
// old one knew.
func rememberedBootstrap(cluster *osv1alpha1.TalosCluster) time.Time {
	if at := cluster.Status.BootstrappedAt; at != nil {
		return at.Time
	}
	if c := meta.FindStatusCondition(cluster.Status.Conditions, osv1alpha1.ConditionBootstrapped); c != nil &&
		c.Status == metav1.ConditionTrue {
		cluster.Status.BootstrappedAt = &metav1.Time{Time: c.LastTransitionTime.Time}
		return c.LastTransitionTime.Time
	}
	return time.Time{}
}

// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update,namespace=pk-metal

// Reconcile bootstraps a cluster once its first control plane answers, and
// publishes the admin kubeconfig and talosconfig as Secrets in the operator's
// namespace. Both derive from the secrets bundle it reads there.
func (r *TalosClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := cluster.DeepCopy()
	res, err := r.reconcile(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := updateStatusIfChanged(ctx, r.Client, cluster, &orig.Status, &cluster.Status); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *TalosClusterReconciler) reconcile(ctx context.Context, cluster *osv1alpha1.TalosCluster) (ctrl.Result, error) {
	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		setCondition(&cluster.Status.Conditions, cluster.Generation, t, s, reason, msg)
	}
	retry := ctrl.Result{RequeueAfter: cmp.Or(r.CheckInterval, defaultCheckInterval)}

	nodes, err := clusterMembers(ctx, r.Client, cluster.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	var members []bootstrap.Member
	var endpoints []string
	st := &cluster.Status
	st.ControlPlanes, st.ReadyControlPlanes, st.Workers, st.ReadyWorkers = 0, 0, 0, 0
	for _, n := range nodes {
		ready := meta.IsStatusConditionTrue(n.Status.Conditions, osv1alpha1.ConditionReady)
		if n.Spec.Role == osv1alpha1.RoleControlPlane {
			st.ControlPlanes++
			if ready {
				st.ReadyControlPlanes++
			}
			if addr := render.NodeAddress(&n); addr != "" {
				members = append(members, bootstrap.Member{Name: n.Name, Endpoint: addr})
				endpoints = append(endpoints, addr)
			}
			continue
		}
		st.Workers++
		if ready {
			st.ReadyWorkers++
		}
	}

	bundle, err := loadBundle(ctx, r.Client, r.Namespace, cluster)
	if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
		set(osv1alpha1.ConditionBootstrapped, metav1.ConditionUnknown, osv1alpha1.ReasonSecretsUnavailable, unavailable.msg)
		set(osv1alpha1.ConditionAvailable, metav1.ConditionUnknown, osv1alpha1.ReasonSecretsUnavailable, unavailable.msg)
		st.KubernetesVersion = ""
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	kubeconfig, err := r.publish(ctx, cluster, bundle, endpoints)
	if err != nil {
		return ctrl.Result{}, err
	}

	version, kerr := r.probe(ctx, kubeconfig)
	st.KubernetesVersion = version
	if r.Dial == nil {
		return retry, nil
	}

	talosconfig, err := render.Talosconfig(bundle, cluster.Name, endpoints, checkinCertTTL)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mint talosconfig for %s: %w", cluster.Name, err)
	}
	// The outstanding request, if any, is the transition time of
	// Bootstrapped=Unknown/BootstrapRequested: written when a call is
	// accepted and kept while the request waits, so it survives restarts.
	requestedAt := r.outstandingRequest(cluster)
	b := bootstrap.Ensure(ctx, r.Dial, talosconfig, members, bootstrap.Observation{
		KubernetesAnswers: kerr == nil, RequestedAt: requestedAt, Grace: r.BootstrapGrace,
		BootstrappedAt: rememberedBootstrap(cluster),
	})
	switch {
	case b.Bootstrapped:
		r.requests.Delete(cluster.UID)
		set(osv1alpha1.ConditionBootstrapped, metav1.ConditionTrue, b.Reason, b.Message)
		if st.BootstrappedAt == nil {
			// Remembered the first time it is seen, so that a later loss of
			// every member's etcd data reads as a loss, not as a new cluster.
			st.BootstrappedAt = ptrTime(metav1.Now())
			logf.FromContext(ctx).Info("cluster bootstrapped", "cluster", cluster.Name, "detail", b.Message)
		}
	case b.Reason == osv1alpha1.ReasonBootstrapRequested:
		if !b.RequestedAt.Equal(requestedAt) {
			// A new request: restart the condition so its transition time is
			// the moment the call was accepted, even when it was already
			// Unknown (a request repeated after the grace window).
			logf.FromContext(ctx).Info("requested etcd bootstrap", "cluster", cluster.Name, "detail", b.Message)
			r.requests.Store(cluster.UID, b.RequestedAt)
			meta.RemoveStatusCondition(&st.Conditions, osv1alpha1.ConditionBootstrapped)
			meta.SetStatusCondition(&st.Conditions, metav1.Condition{
				Type: osv1alpha1.ConditionBootstrapped, Status: metav1.ConditionUnknown,
				Reason: b.Reason, Message: b.Message, ObservedGeneration: cluster.Generation,
				LastTransitionTime: metav1.NewTime(b.RequestedAt),
			})
			break
		}
		set(osv1alpha1.ConditionBootstrapped, metav1.ConditionUnknown, b.Reason, b.Message)
	default:
		set(osv1alpha1.ConditionBootstrapped, metav1.ConditionFalse, b.Reason, b.Message)
	}

	switch {
	case !b.Bootstrapped && st.BootstrappedAt != nil:
		set(osv1alpha1.ConditionAvailable, metav1.ConditionFalse, b.Reason, b.Message)
	case !b.Bootstrapped:
		set(osv1alpha1.ConditionAvailable, metav1.ConditionFalse, osv1alpha1.ReasonNotBootstrapped, "etcd is not bootstrapped")
	case kerr != nil:
		set(osv1alpha1.ConditionAvailable, metav1.ConditionFalse, osv1alpha1.ReasonKubernetesUnreachable, kerr.Error())
	case st.ReadyControlPlanes < st.ControlPlanes || st.ReadyWorkers < st.Workers:
		set(osv1alpha1.ConditionAvailable, metav1.ConditionFalse, osv1alpha1.ReasonNodesNotReady,
			fmt.Sprintf("%d/%d control-plane and %d/%d worker nodes Ready",
				st.ReadyControlPlanes, st.ControlPlanes, st.ReadyWorkers, st.Workers))
	default:
		set(osv1alpha1.ConditionAvailable, metav1.ConditionTrue, osv1alpha1.ReasonClusterAvailable,
			"Kubernetes "+version+" answers and every node is Ready")
		return ctrl.Result{RequeueAfter: cmp.Or(r.ReadyInterval, defaultReadyInterval)}, nil
	}
	return retry, nil
}

// publish keeps the admin kubeconfig and talosconfig Secrets current and
// returns the kubeconfig. A credential is re-minted only when it no longer
// serves (another CA, other endpoints, close to expiry), so the Secrets do
// not churn on every pass.
func (r *TalosClusterReconciler) publish(ctx context.Context, cluster *osv1alpha1.TalosCluster,
	bundle *secrets.Bundle, endpoints []string) ([]byte, error) {
	kubeconfig, ref, err := r.credential(ctx, cluster, cluster.Name+"-kubeconfig", KubeconfigKey,
		func(cur []byte) bool {
			return render.KubeconfigCurrent(cur, bundle, cluster.Spec.Endpoint, adminCredentialRenew)
		},
		func() ([]byte, error) {
			return render.Kubeconfig(bundle, cluster.Name, cluster.Spec.Endpoint, adminCredentialTTL)
		})
	if err != nil {
		return nil, err
	}
	cluster.Status.KubeconfigSecretRef = ref

	_, ref, err = r.credential(ctx, cluster, cluster.Name+"-talosconfig", TalosconfigKey,
		func(cur []byte) bool {
			return render.TalosconfigCurrent(cur, bundle, endpoints, adminCredentialRenew)
		},
		func() ([]byte, error) {
			return render.Talosconfig(bundle, cluster.Name, endpoints, adminCredentialTTL)
		})
	if err != nil {
		return nil, err
	}
	cluster.Status.TalosconfigSecretRef = ref
	return kubeconfig, nil
}

func (r *TalosClusterReconciler) credential(ctx context.Context, cluster *osv1alpha1.TalosCluster, name, key string,
	current func([]byte) bool, mint func() ([]byte, error)) ([]byte, *osv1alpha1.SecretReference, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Namespace, Name: name}}
	var data []byte
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		data = secret.Data[key]
		if !current(data) {
			minted, err := mint()
			if err != nil {
				return err
			}
			data = minted
			secret.Data = map[string][]byte{key: minted}
		}
		return controllerutil.SetControllerReference(cluster, secret, r.Scheme)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("publish %s for %s: %w", key, cluster.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("published admin credential", "cluster", cluster.Name, "secret", name, "op", op)
	}
	return data, &osv1alpha1.SecretReference{Namespace: secret.Namespace, Name: secret.Name}, nil
}

func (r *TalosClusterReconciler) probe(ctx context.Context, kubeconfig []byte) (string, error) {
	if r.ProbeKubernetes != nil {
		return r.ProbeKubernetes(ctx, kubeconfig)
	}
	return ProbeKubernetes(ctx, kubeconfig)
}

// ProbeKubernetes is the production KubernetesProber: GET /version through
// the admin kubeconfig, bounded so a silent endpoint cannot hold a reconcile.
func ProbeKubernetes(ctx context.Context, kubeconfig []byte) (string, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return "", err
	}
	cfg.Timeout = 5 * time.Second
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return "", err
	}
	body, err := dc.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "", err
	}
	var info version.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("decode /version: %w", err)
	}
	return info.GitVersion, nil
}

func (r *TalosClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only spec changes of the cluster itself trigger a pass: its status is
	// this reconciler's own output, and reacting to that write would rerun
	// the machine probes (and formerly a bootstrap) within milliseconds. The
	// pace between passes is RequeueAfter; members and Secrets have watches.
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.TalosCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Secret{}).
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(clusterForNode)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.clustersForBundle)).
		Named("taloscluster").
		Complete(r)
}

// clusterForNode maps a member's event (its readiness moved, it joined or
// left) to its cluster.
func clusterForNode(_ context.Context, obj client.Object) []ctrl.Request {
	return []ctrl.Request{requestFor(obj.(*osv1alpha1.TalosNode).Spec.ClusterRef.Name)}
}

func (r *TalosClusterReconciler) clustersForBundle(ctx context.Context, obj client.Object) []ctrl.Request {
	var reqs []ctrl.Request
	for _, c := range clustersUsingSecret(ctx, r.Client, obj) {
		reqs = append(reqs, requestFor(c.Name))
	}
	return reqs
}
