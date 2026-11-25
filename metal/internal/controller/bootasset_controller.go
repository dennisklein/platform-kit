package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

const (
	// artifactKey is the data key of built artifact Secrets.
	artifactKey = "asset.iso"
	// configDigestAnnotation records which config digest an artifact Secret was
	// built from. ISO recording metadata would make artifact bytes per-build, so
	// rebuilds key on this — never on the artifact digest.
	configDigestAnnotation = "os.pk.io/config-digest"
)

// BootAssetReconciler produces boot artifacts. configISO packs the node's
// rendered config Secret into the metal-iso medium and rests it in an owned
// artifact Secret for the asset server to serve under grants; factory registers
// the schematic with the image factory and downloads the generic secret-free
// install image into the media store, from where it is served under grants
// too.
type BootAssetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace receives the artifact Secrets (the operator's namespace).
	Namespace string
	// Factory serves the images the factory strategy addresses, and is told
	// about the schematics addressing them. The embeddedInstaller strategy
	// asks it which images the official extensions are.
	Factory *render.Factory
	// Store and Imager build and keep the embeddedInstaller artifacts. Nil
	// holds those assets with a message naming the flags.
	Store  *asset.Store
	Imager *Imager
	// Fetches downloads the factory installers into Store. Nil makes one.
	Fetches   *mediaFetcher
	fetchOnce sync.Once
}

// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets/finalizers,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update,namespace=pk-metal
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete,namespace=pk-metal

// Reconcile builds the boot artifact a BootAsset describes and stores it in a
// Secret next to the rendered configuration.
func (r *BootAssetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ba := &osv1alpha1.BootAsset{}
	if err := r.Get(ctx, req.NamespacedName, ba); err != nil {
		if apierrors.IsNotFound(err) {
			// A stored installer has no owner reference to die with: its
			// file goes here.
			return ctrl.Result{}, r.sweepStore(ctx)
		}
		return ctrl.Result{}, err
	}
	// The artifact Secret and the build Jobs die with their owner; grants
	// are revoked by the boot lane on eject/check-in.
	if !ba.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	orig := ba.DeepCopy()
	var err error
	var res ctrl.Result
	switch ba.Spec.Strategy {
	case osv1alpha1.StrategyFactory:
		res = r.fetchImage(ctx, ba)
	case osv1alpha1.StrategyConfigISO:
		err = r.build(ctx, ba)
	case osv1alpha1.StrategyEmbeddedInstaller:
		err = r.embed(ctx, ba)
	default:
		// The CRD enum refuses anything else; a new strategy landing in the API
		// before its builder would park here visibly.
		r.hold(ba, osv1alpha1.ReasonBuildFailed, fmt.Sprintf("no builder for strategy %q", ba.Spec.Strategy))
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := updateStatusIfChanged(ctx, r.Client, ba, &orig.Status, &ba.Status); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *BootAssetReconciler) hold(ba *osv1alpha1.BootAsset, reason, message string) {
	setCondition(&ba.Status.Conditions, ba.Generation, osv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
}

// fetchImage is the factory strategy: the operator downloads the install
// image from the factory into the media store, and the media service serves
// it to the BMC under a grant, pinned to the digest recorded here. A BMC
// never fetches it from the factory itself: a BMC does not verify the
// certificate of what it mounts, and a substituted installer would read the
// config medium in the other slot.
//
// The URL is arithmetic — a content hash of the schematic — and arithmetic
// always succeeds, including for a schematic no factory has ever heard of.
// Registering first is what puts a fetchable image behind the URL.
func (r *BootAssetReconciler) fetchImage(ctx context.Context, ba *osv1alpha1.BootAsset) ctrl.Result {
	if _, err := r.Factory.Register(ctx, ba.Spec.Schematic); err != nil {
		r.hold(ba, osv1alpha1.ReasonSchematicUnavailable, err.Error())
		return ctrl.Result{}
	}
	url, err := r.Factory.ISOURL(ba.Spec.Schematic, ba.Spec.TalosVersion)
	if err != nil {
		r.hold(ba, osv1alpha1.ReasonBuildFailed, err.Error())
		return ctrl.Result{}
	}
	ba.Status.URL = url
	if r.Store == nil {
		r.hold(ba, osv1alpha1.ReasonBuilderUnavailable,
			"this operator has no media store to keep the installer in: start the manager with --media-store")
		return ctrl.Result{}
	}
	// The key names the URL, so another factory or another image is
	// another file.
	key := imageKey(url)
	if ba.Status.BuildKey == key && ba.Status.Digest != "" && r.Store.Has(ba.Name, key) {
		setCondition(&ba.Status.Conditions, ba.Generation, osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonImageDownloaded, "")
		return ctrl.Result{}
	}
	st := r.fetcher().start(r.Store, r.Factory.ImageClient(), url, ba.Name, key, "")
	switch {
	case st.running:
		r.hold(ba, osv1alpha1.ReasonBuilding, fmt.Sprintf(
			"the operator downloads the installer from %s into its media store; the BMC fetches it from the media service", url))
		return ctrl.Result{RequeueAfter: fetchPoll}
	case st.err != nil:
		r.hold(ba, osv1alpha1.ReasonBuildFailed, fmt.Sprintf("download the installer: %v. The operator tries again in %s", st.err, fetchRetry))
		return ctrl.Result{RequeueAfter: fetchRetry}
	}
	ba.Status.Digest, ba.Status.Size, ba.Status.BuildKey = st.sum, st.size, key
	setCondition(&ba.Status.Conditions, ba.Generation, osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonImageDownloaded, "")
	if err := r.Store.Prune(ba.Name, key); err != nil {
		logf.FromContext(ctx).Error(err, "prune superseded installers", "bootAsset", ba.Name)
	}
	return ctrl.Result{}
}

// fetchPoll is how often a running download is looked at; fetchRetry is
// how long a failed one waits before the next try.
const (
	fetchPoll  = 5 * time.Second
	fetchRetry = time.Minute
)

// imageKey is the store key of a downloaded image: a hash of its URL.
func imageKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:16])
}

func (r *BootAssetReconciler) fetcher() *mediaFetcher {
	r.fetchOnce.Do(func() {
		if r.Fetches == nil {
			r.Fetches = &mediaFetcher{}
		}
	})
	return r.Fetches
}

// nodeConfig loads the rendered config of the asset's node. A missing
// piece holds the asset and returns nil without an error.
func (r *BootAssetReconciler) nodeConfig(ctx context.Context, ba *osv1alpha1.BootAsset) (*osv1alpha1.TalosNode, []byte, error) {
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, types.NamespacedName{Name: ba.Spec.NodeRef.Name}, node); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, nil, err
		}
		r.hold(ba, osv1alpha1.ReasonNodeNotFound,
			fmt.Sprintf("nodeRef names no TalosNode %q", ba.Spec.NodeRef.Name))
		return nil, nil, nil
	}
	ref := node.Status.ConfigSecretRef
	if ref == nil {
		r.hold(ba, osv1alpha1.ReasonConfigNotRendered,
			fmt.Sprintf("TalosNode %s has no rendered config yet", node.Name))
		return nil, nil, nil
	}
	configSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, configSecret); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, nil, err
		}
		r.hold(ba, osv1alpha1.ReasonConfigNotRendered,
			fmt.Sprintf("config Secret %s/%s not found", ref.Namespace, ref.Name))
		return nil, nil, nil
	}
	machineConfig := configSecret.Data[asset.ConfigFilename]
	if len(machineConfig) == 0 {
		r.hold(ba, osv1alpha1.ReasonConfigNotRendered,
			fmt.Sprintf("config Secret %s/%s carries no %s", ref.Namespace, ref.Name, asset.ConfigFilename))
		return nil, nil, nil
	}
	return node, machineConfig, nil
}

// build is the configISO strategy: rendered config Secret in, artifact Secret
// out. The pack is skipped when the existing artifact was built from the same
// config digest.
func (r *BootAssetReconciler) build(ctx context.Context, ba *osv1alpha1.BootAsset) error {
	node, machineConfig, err := r.nodeConfig(ctx, ba)
	if node == nil {
		return err
	}
	configDigest := digest.FromBytes(machineConfig).String()

	artifact := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: r.Namespace, Name: "bootasset-" + ba.Name,
	}}
	var iso []byte
	if err := r.Get(ctx, client.ObjectKeyFromObject(artifact), artifact); err == nil &&
		artifact.Annotations[configDigestAnnotation] == configDigest && len(artifact.Data[artifactKey]) > 0 {
		iso = artifact.Data[artifactKey]
	} else if client.IgnoreNotFound(err) != nil {
		return err
	} else {
		var buildErr error
		if iso, buildErr = asset.BuildConfigISO(machineConfig); buildErr != nil {
			r.hold(ba, osv1alpha1.ReasonBuildFailed, buildErr.Error())
			return nil
		}
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, artifact, func() error {
		if artifact.Annotations == nil {
			artifact.Annotations = map[string]string{}
		}
		artifact.Annotations[configDigestAnnotation] = configDigest
		artifact.Data = map[string][]byte{artifactKey: iso}
		return controllerutil.SetControllerReference(ba, artifact, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("write artifact secret for %s: %w", ba.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("built boot artifact", "bootAsset", ba.Name, "secret", artifact.Name,
			"bytes", len(iso), "op", op)
	}

	ba.Status.ArtifactSecretRef = &osv1alpha1.SecretReference{Namespace: artifact.Namespace, Name: artifact.Name}
	ba.Status.Digest = digest.FromBytes(iso).String()
	ba.Status.Size = int64(len(iso))
	setCondition(&ba.Status.Conditions, ba.Generation, osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonArtifactBuilt, "")
	return nil
}

func (r *BootAssetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.BootAsset{}).
		Owns(&corev1.Secret{}).
		Owns(&batchv1.Job{}).
		Watches(&osv1alpha1.TalosNode{}, handler.EnqueueRequestsFromMapFunc(r.assetsForNode)).
		Named("bootasset").
		Complete(r)
}

// assetsForNode rebuilds the artifacts riding a node when its render moves.
func (r *BootAssetReconciler) assetsForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range assets.Items {
		if ref := assets.Items[i].Spec.NodeRef; ref != nil && ref.Name == obj.GetName() {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{Name: assets.Items[i].Name}})
		}
	}
	return reqs
}
