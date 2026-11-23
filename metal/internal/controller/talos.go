package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// Helpers the TalosNode and TalosCluster reconcilers share: reading the
// cluster's secrets bundle and patch ConfigMaps, and mapping watch events on
// those inputs back to the objects that consume them.

// bundleUnavailable is a semantic failure to read the secrets bundle: the
// Secret or its key is missing, or the bundle does not decode or validate.
// It becomes a SecretsUnavailable condition, never a reconcile error.
type bundleUnavailable struct{ msg string }

func (e *bundleUnavailable) Error() string { return e.msg }

// loadBundle reads and validates the cluster's secrets bundle. Transport
// errors return as is; everything else is a *bundleUnavailable.
func loadBundle(ctx context.Context, c client.Reader, cluster *osv1alpha1.TalosCluster) (*secrets.Bundle, error) {
	ref := cluster.Spec.SecretsRef
	s := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, s); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, err
		}
		return nil, &bundleUnavailable{fmt.Sprintf("secrets Secret %s/%s not found", ref.Namespace, ref.Name)}
	}
	data := s.Data[render.BundleKey]
	if len(data) == 0 {
		return nil, &bundleUnavailable{fmt.Sprintf("secrets Secret %s/%s carries no %s", ref.Namespace, ref.Name, render.BundleKey)}
	}
	b, err := render.LoadBundle(data, cluster.Spec.TalosVersion)
	if err != nil {
		return nil, &bundleUnavailable{fmt.Sprintf("secrets Secret %s/%s: %v", ref.Namespace, ref.Name, err)}
	}
	return b, nil
}

// configMapReader resolves patch references against the API server.
func configMapReader(ctx context.Context, c client.Reader) render.ConfigMapReader {
	return func(ref osv1alpha1.ConfigMapKeyReference) (string, error) {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, cm); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return "", err
			}
			return "", fmt.Errorf("%w: ConfigMap %s/%s not found", render.ErrPatchUnresolvable, ref.Namespace, ref.Name)
		}
		v, ok := cm.Data[ref.Key]
		if !ok {
			return "", fmt.Errorf("%w: ConfigMap %s/%s has no key %q", render.ErrPatchUnresolvable, ref.Namespace, ref.Name, ref.Key)
		}
		return v, nil
	}
}

// clusterMembers lists the TalosNodes naming the cluster, sorted by name.
func clusterMembers(ctx context.Context, c client.Reader, cluster string) ([]osv1alpha1.TalosNode, error) {
	nodes := &osv1alpha1.TalosNodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, err
	}
	var out []osv1alpha1.TalosNode
	for _, n := range nodes.Items {
		if n.Spec.ClusterRef.Name == cluster {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b osv1alpha1.TalosNode) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out, nil
}

// clustersUsingSecret lists the clusters whose secretsRef names the Secret.
func clustersUsingSecret(ctx context.Context, c client.Reader, obj client.Object) []osv1alpha1.TalosCluster {
	clusters := &osv1alpha1.TalosClusterList{}
	if err := c.List(ctx, clusters); err != nil {
		return nil
	}
	var out []osv1alpha1.TalosCluster
	for _, cl := range clusters.Items {
		if ref := cl.Spec.SecretsRef; ref.Namespace == obj.GetNamespace() && ref.Name == obj.GetName() {
			out = append(out, cl)
		}
	}
	return out
}

// patchesUse reports whether any patch reads the ConfigMap.
func patchesUse(patches []osv1alpha1.Patch, obj client.Object) bool {
	for _, p := range patches {
		if r := p.ConfigMapRef; r != nil && r.Namespace == obj.GetNamespace() && r.Name == obj.GetName() {
			return true
		}
	}
	return false
}

func requestFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}
