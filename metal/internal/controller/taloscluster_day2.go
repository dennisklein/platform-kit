package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/role"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The TalosCluster's day-2 observations: credential lifetimes (D10), the
// version support matrix and skew (D24).

// credentialSlack is how much longer than the TTL a published credential may
// be valid before it counts as minted under another policy (a longer TTL
// before this one) and is minted again.
const credentialSlack = time.Hour

// credentialTTL is the lifetime of the published credentials.
func credentialTTL(cluster *osv1alpha1.TalosCluster) time.Duration {
	if c := cluster.Spec.Credentials; c != nil && c.TTL != nil && c.TTL.Duration > 0 {
		return c.TTL.Duration
	}
	return defaultCredentialTTL
}

// notTooLong reports whether a credential expiring at notAfter fits the
// current TTL: a credential minted under a longer one is minted again, so
// that shortening the TTL takes effect at once instead of in a year.
func notTooLong(notAfter time.Time, err error) func(*secrets.Bundle, time.Duration) bool {
	return func(bundle *secrets.Bundle, ttl time.Duration) bool {
		return err == nil && !notAfter.After(bundle.Clock.Now().Add(ttl+credentialSlack))
	}
}

func expiryOf(t time.Time, err error) time.Time {
	if err != nil {
		return time.Time{}
	}
	return t
}

// earliest is the earlier of two times; a zero time is no time.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	}
	return b
}

// publishReader keeps the os:reader talosconfig when the spec asks for it,
// and removes it when it no longer does. It returns its expiry.
func (r *TalosClusterReconciler) publishReader(ctx context.Context, cluster *osv1alpha1.TalosCluster,
	bundle *secrets.Bundle, endpoints []string, ttl time.Duration) (time.Time, error) {
	name := cluster.Name + "-talosconfig-reader"
	if c := cluster.Spec.Credentials; c == nil || !c.ReaderTalosconfig {
		cluster.Status.ReaderTalosconfigSecretRef = nil
		err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.Namespace, Name: name}})
		return time.Time{}, client.IgnoreNotFound(err)
	}
	data, ref, err := r.credential(ctx, cluster, name, TalosconfigKey,
		func(cur []byte) bool {
			roles, rerr := render.TalosconfigRoles(cur)
			return rerr == nil && slices.Equal(roles, []string{string(role.Reader)}) &&
				render.TalosconfigCurrent(cur, bundle, endpoints, ttl/3) &&
				notTooLong(render.TalosconfigNotAfter(cur))(bundle, ttl)
		},
		func() ([]byte, error) {
			return render.TalosconfigWithRole(bundle, cluster.Name, endpoints, ttl, role.Reader)
		})
	if err != nil {
		return time.Time{}, err
	}
	cluster.Status.ReaderTalosconfigSecretRef = ref
	return expiryOf(render.TalosconfigNotAfter(data)), nil
}

// observeVersions sets VersionsSupported (the spec's Kubernetes version
// against its Talos version) and VersionsCurrent (what the machines and the
// API report against the spec).
func (r *TalosClusterReconciler) observeVersions(cluster *osv1alpha1.TalosCluster, nodes []osv1alpha1.TalosNode) {
	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		setCondition(&cluster.Status.Conditions, cluster.Generation, t, s, reason, msg)
	}
	spec := cluster.Spec
	if err := render.SupportedPair(spec.TalosVersion, spec.KubernetesVersion); err != nil {
		set(osv1alpha1.ConditionVersionsSupported, metav1.ConditionFalse, osv1alpha1.ReasonUnsupportedVersion,
			err.Error()+"; nothing renders until the pair is supported")
	} else {
		set(osv1alpha1.ConditionVersionsSupported, metav1.ConditionTrue, osv1alpha1.ReasonVersionsSupported,
			fmt.Sprintf("Talos %s supports Kubernetes %s", spec.TalosVersion, spec.KubernetesVersion))
	}

	var behind []string
	for _, n := range nodes {
		if v := n.Status.Version; v != "" && v != spec.TalosVersion {
			behind = append(behind, n.Name+" runs Talos "+v)
		}
	}
	if v := cluster.Status.KubernetesVersion; v != "" && v != spec.KubernetesVersion {
		behind = append(behind, "the Kubernetes API runs "+v)
	}
	if len(behind) == 0 {
		set(osv1alpha1.ConditionVersionsCurrent, metav1.ConditionTrue, osv1alpha1.ReasonVersionCurrent,
			fmt.Sprintf("every machine that reports runs Talos %s; Kubernetes wants %s", spec.TalosVersion, spec.KubernetesVersion))
		return
	}
	set(osv1alpha1.ConditionVersionsCurrent, metav1.ConditionFalse, osv1alpha1.ReasonVersionsBehind,
		fmt.Sprintf("%s (the spec asks for Talos %s, Kubernetes %s); an upgrade rollout is proposed: pk-metal rollout list",
			strings.Join(behind, "; "), spec.TalosVersion, spec.KubernetesVersion))
}

// clusterForRollout maps a rollout's event to its cluster.
func clusterForRollout(_ context.Context, obj client.Object) []ctrl.Request {
	return []ctrl.Request{requestFor(obj.(*osv1alpha1.TalosRollout).Spec.ClusterRef.Name)}
}
