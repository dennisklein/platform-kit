package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The secrets bundle guard (D9). Every certificate the machines trust and
// every credential the operator mints derive from the bundle. Editing it on a
// running cluster (a new CA, a new token) would make the operator render
// configs the machines reject, dial them with certificates they refuse, and
// publish credentials nobody can use: every control plane would look down at
// once. Rotating a CA safely takes a sequence (accept both CAs everywhere,
// switch, drop the old one) this operator does not run yet.
//
// So the operator keeps a copy of the bundle the machines run, the applied
// bundle, in its own Secret. Once the cluster was bootstrapped, a spec bundle
// that differs from that copy is held: everything keeps using the copy, and
// the TalosCluster says so (SecretsBundleHeld=True). A person ends the hold by
// putting the bundle back, or, once the machines run the new bundle (after a
// rotation done by hand), by deleting the copy, which the operator then takes
// again from the spec. The docs site (operate/day-2/rotate-cluster-credentials) describes both.

// appliedBundleName is the Secret holding the copy of the bundle the
// machines run.
func appliedBundleName(cluster string) string { return cluster + "-bundle-applied" }

// chooseBundle returns the bundle bytes to use for a cluster: the spec's, or
// the applied copy when the two differ and the cluster was bootstrapped
// (held). A missing copy is no hold: the cluster reconciler takes one.
func chooseBundle(ctx context.Context, c client.Reader, namespace string, cluster *osv1alpha1.TalosCluster,
	specData []byte) (data []byte, held bool, err error) {
	applied := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: appliedBundleName(cluster.Name)}, applied)
	if err != nil {
		return specData, false, client.IgnoreNotFound(err)
	}
	old := applied.Data[render.BundleKey]
	if len(old) == 0 || bytes.Equal(old, specData) || cluster.Status.BootstrappedAt == nil {
		return specData, false, nil
	}
	return old, true, nil
}

// guardBundle keeps the applied copy and reports the hold on the cluster. It
// takes the copy when there is none, follows the spec while the cluster was
// never bootstrapped (no machine runs anything yet), and otherwise leaves the
// copy alone. A spec bundle that cannot be read is loadBundle's to report.
func (r *TalosClusterReconciler) guardBundle(ctx context.Context, cluster *osv1alpha1.TalosCluster) error {
	ref := cluster.Spec.SecretsRef
	spec := &corev1.Secret{}
	if ref.Namespace != r.Namespace {
		return nil
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, spec); err != nil {
		return client.IgnoreNotFound(err)
	}
	specData := spec.Data[render.BundleKey]
	if len(specData) == 0 {
		return nil
	}
	set := func(s metav1.ConditionStatus, reason, msg string) {
		setCondition(&cluster.Status.Conditions, cluster.Generation, osv1alpha1.ConditionSecretsBundleHeld, s, reason, msg)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Namespace, Name: appliedBundleName(cluster.Name)}}
	err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	old := secret.Data[render.BundleKey]
	if err == nil && len(old) > 0 && !bytes.Equal(old, specData) && cluster.Status.BootstrappedAt != nil {
		set(metav1.ConditionTrue, osv1alpha1.ReasonBundleChanged, bundleHeldMessage(cluster, old, specData, r.Namespace))
		return nil
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Data = map[string][]byte{render.BundleKey: specData}
		return controllerutil.SetControllerReference(cluster, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("keep applied bundle of %s: %w", cluster.Name, err)
	}
	if op != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("recorded the secrets bundle the machines run", "cluster", cluster.Name, "op", op)
	}
	set(metav1.ConditionFalse, osv1alpha1.ReasonBundleApplied, "the machines run the bundle the spec names")
	return nil
}

// bundleHeldMessage names what differs between the applied and the spec
// bundle, and the two ways out.
func bundleHeldMessage(cluster *osv1alpha1.TalosCluster, old, cur []byte, namespace string) string {
	what := "the bundle"
	if ob, err := render.LoadBundle(old, cluster.Spec.TalosVersion); err == nil {
		if nb, err := render.LoadBundle(cur, cluster.Spec.TalosVersion); err == nil {
			what = strings.Join(bundleChanges(ob, nb), ", ")
		}
	}
	return fmt.Sprintf("secrets bundle %s/%s changed (%s) after the cluster was bootstrapped; the machines still run the "+
		"previous one, so the operator keeps rendering, dialing and publishing credentials from %s/%s. Put the bundle back, "+
		"or, once every machine runs the new one, delete %s/%s (https://dennisklein.github.io/platform-kit/operate/day-2/rotate-cluster-credentials/#rotate-a-ca)",
		cluster.Spec.SecretsRef.Namespace, cluster.Spec.SecretsRef.Name, what,
		namespace, appliedBundleName(cluster.Name), namespace, appliedBundleName(cluster.Name))
}

// bundleChanges names the parts of a bundle that differ.
func bundleChanges(a, b *secrets.Bundle) []string {
	var out []string
	pemDiff := func(name string, x, y []byte) {
		if !bytes.Equal(x, y) {
			out = append(out, name)
		}
	}
	if a.Certs != nil && b.Certs != nil {
		if a.Certs.OS != nil && b.Certs.OS != nil {
			pemDiff("Talos API CA", a.Certs.OS.Crt, b.Certs.OS.Crt)
		}
		if a.Certs.K8s != nil && b.Certs.K8s != nil {
			pemDiff("Kubernetes CA", a.Certs.K8s.Crt, b.Certs.K8s.Crt)
		}
		if a.Certs.Etcd != nil && b.Certs.Etcd != nil {
			pemDiff("etcd CA", a.Certs.Etcd.Crt, b.Certs.Etcd.Crt)
		}
	}
	if len(out) == 0 {
		out = append(out, "tokens or keys, not the CAs")
	}
	return out
}
