package controller

import (
	"context"
	"crypto/sha256"
	"fmt"

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
// the schematic with the image factory and stamps the content-addressed URL of
// the generic secret-free install image — nothing to build, nothing to grant.
type BootAssetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace receives the artifact Secrets (the operator's namespace).
	Namespace string
	// Factory serves the images the factory strategy addresses, and is told
	// about the schematics addressing them.
	Factory *render.Factory
}

// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=bootassets/finalizers,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update,namespace=pk-metal

// Reconcile builds the boot artifact a BootAsset describes and stores it in a
// Secret next to the rendered configuration.
func (r *BootAssetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ba := &osv1alpha1.BootAsset{}
	if err := r.Get(ctx, req.NamespacedName, ba); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The artifact Secret dies with its owner; grants are revoked by the boot
	// lane on eject/check-in.
	if !ba.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	orig := ba.DeepCopy()
	var err error
	switch ba.Spec.Strategy {
	case osv1alpha1.StrategyFactory:
		r.addressImage(ctx, ba)
	case osv1alpha1.StrategyConfigISO:
		err = r.build(ctx, ba)
	default:
		// The CRD enum refuses anything else; a new strategy landing in the API
		// before its builder would park here visibly.
		r.hold(ba, osv1alpha1.ReasonBuildFailed, fmt.Sprintf("no builder for strategy %q", ba.Spec.Strategy))
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if !equality.Semantic.DeepEqual(orig.Status, ba.Status) {
		if err := r.Status().Update(ctx, ba); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *BootAssetReconciler) hold(ba *osv1alpha1.BootAsset, reason, message string) {
	meta.SetStatusCondition(&ba.Status.Conditions, metav1.Condition{
		Type: osv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: ba.Generation,
	})
}

// addressImage is the factory strategy: there is nothing to build, so the work
// is making sure the image this names can actually be fetched.
//
// The URL is arithmetic — a content hash of the schematic — and arithmetic
// always succeeds, including for a schematic no factory has ever heard of.
// Registering first is what puts a fetchable image behind the URL before Ready
// claims there is one; the alternative is a BMC mounting a 404 on the far side
// of the rack, with the cause several hours behind it.
func (r *BootAssetReconciler) addressImage(ctx context.Context, ba *osv1alpha1.BootAsset) {
	if _, err := r.Factory.Register(ctx, ba.Spec.Schematic); err != nil {
		r.hold(ba, osv1alpha1.ReasonSchematicUnavailable, err.Error())
		return
	}
	url, err := r.Factory.ISOURL(ba.Spec.Schematic, ba.Spec.TalosVersion)
	if err != nil {
		r.hold(ba, osv1alpha1.ReasonBuildFailed, err.Error())
		return
	}
	ba.Status.URL = url
	meta.SetStatusCondition(&ba.Status.Conditions, metav1.Condition{
		Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: osv1alpha1.ReasonURLComputed, ObservedGeneration: ba.Generation,
	})
}

// build is the configISO strategy: rendered config Secret in, artifact Secret
// out. The pack is skipped when the existing artifact was built from the same
// config digest.
func (r *BootAssetReconciler) build(ctx context.Context, ba *osv1alpha1.BootAsset) error {
	node := &osv1alpha1.TalosNode{}
	if err := r.Get(ctx, types.NamespacedName{Name: ba.Spec.NodeRef.Name}, node); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		r.hold(ba, osv1alpha1.ReasonNodeNotFound,
			fmt.Sprintf("nodeRef names no TalosNode %q", ba.Spec.NodeRef.Name))
		return nil
	}
	ref := node.Status.ConfigSecretRef
	if ref == nil {
		r.hold(ba, osv1alpha1.ReasonConfigNotRendered,
			fmt.Sprintf("TalosNode %s has no rendered config yet", node.Name))
		return nil
	}
	configSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, configSecret); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		r.hold(ba, osv1alpha1.ReasonConfigNotRendered,
			fmt.Sprintf("config Secret %s/%s not found", ref.Namespace, ref.Name))
		return nil
	}
	machineConfig := configSecret.Data[asset.ConfigFilename]
	if len(machineConfig) == 0 {
		r.hold(ba, osv1alpha1.ReasonConfigNotRendered,
			fmt.Sprintf("config Secret %s/%s carries no %s", ref.Namespace, ref.Name, asset.ConfigFilename))
		return nil
	}
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(machineConfig))

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
	ba.Status.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(iso))
	ba.Status.Size = int64(len(iso))
	meta.SetStatusCondition(&ba.Status.Conditions, metav1.Condition{
		Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: osv1alpha1.ReasonArtifactBuilt, ObservedGeneration: ba.Generation,
	})
	return nil
}

func (r *BootAssetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.BootAsset{}).
		Owns(&corev1.Secret{}).
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
