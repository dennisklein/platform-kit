package pivot

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// The move writes no guard lift as the wrong person: it creates and updates
// objects without them, and sets each lift again as the person it names.
// A lift that names nobody stays behind, and the move says so.
func TestMoveSetsGuardLiftsAsTheirAuthors(t *testing.T) {
	ctx := context.Background()
	m := fakeMover(t, nil, nil)
	var log bytes.Buffer
	m.Options.Log = &log
	target := m.Target.Client

	// who records the user of every write; a write as anyone but the
	// person a lift names that carries the lift is what admission refuses.
	var refused []string
	as := func(user string) client.Client {
		check := func(obj client.Object) {
			for _, key := range metalv1alpha1.NamedGuardLifts {
				if v, ok := obj.GetAnnotations()[key]; ok {
					if who, _, named := metalv1alpha1.ParseGuardLift(v); !named || who != user {
						refused = append(refused, user+" wrote "+key+"="+v)
					}
				}
			}
		}
		return interceptor.NewClient(target.(client.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				check(obj)
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				// An update that leaves a lift as stored passes admission.
				stored := obj.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err == nil {
					for _, key := range metalv1alpha1.NamedGuardLifts {
						if v, ok := obj.GetAnnotations()[key]; ok && stored.GetAnnotations()[key] != v {
							check(obj)
						}
					}
				}
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				err := c.Patch(ctx, obj, p, opts...)
				check(obj)
				return err
			},
		})
	}
	m.Target.Client = as("admin")
	m.Target.impersonate = func(user string, _ []string) (client.Client, error) { return as(user), nil }

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(metalGV.WithKind("HostClaim"))
	claim.SetNamespace("default")
	claim.SetName("c-1")
	claim.SetUID("claim-src")
	claim.SetAnnotations(map[string]string{
		metalv1alpha1.AnnotationAckEtcdRisk: "carol: disk swap",
		"example.com/note":                  "kept",
	})
	require.NoError(t, unstructured.SetNestedField(claim.Object, "asset-1", "spec", "assetRef", "name"))
	op := &unstructured.Unstructured{}
	op.SetGroupVersionKind(metalGV.WithKind("HostOperation"))
	op.SetNamespace("pk-metal")
	op.SetName("reboot-1")
	op.SetUID("op-src")
	op.SetAnnotations(map[string]string{
		metalv1alpha1.AnnotationOverrideClusterGuard: "alice: single control plane",
		metalv1alpha1.AnnotationIgnoreWindow:         "urgent",
	})
	require.NoError(t, unstructured.SetNestedField(op.Object, "alice", "spec", "proposedBy"))

	rw := &rewriter{uids: map[types.UID]types.UID{}, claims: map[types.UID]bool{"claim-src": true}}
	for _, so := range []sourceObject{
		{kind: movedKind{gvk: metalGV.WithKind("HostClaim"), namespaced: true}, obj: claim},
		{kind: movedKind{gvk: metalGV.WithKind("HostOperation"), namespaced: true}, obj: op},
	} {
		_, err := m.ensure(ctx, so, rw)
		require.NoError(t, err)
		// A second pass over objects that exist changes nothing and
		// removes no lift.
		_, err = m.ensure(ctx, so, rw)
		require.NoError(t, err)
	}
	assert.Empty(t, refused, "no lift is written as anyone but its author")

	got := &metalv1alpha1.HostClaim{}
	require.NoError(t, target.Get(ctx, client.ObjectKey{Namespace: "default", Name: "c-1"}, got))
	assert.Equal(t, "carol: disk swap", got.Annotations[metalv1alpha1.AnnotationAckEtcdRisk])
	assert.Equal(t, "kept", got.Annotations["example.com/note"])
	gotOp := &metalv1alpha1.HostOperation{}
	require.NoError(t, target.Get(ctx, client.ObjectKey{Namespace: "pk-metal", Name: "reboot-1"}, gotOp))
	assert.Equal(t, "alice: single control plane", gotOp.Annotations[metalv1alpha1.AnnotationOverrideClusterGuard])
	assert.NotContains(t, gotOp.Annotations, metalv1alpha1.AnnotationIgnoreWindow, "a lift that names nobody stays behind")
	assert.Contains(t, log.String(), "left metal.pk.io/ignore-window off HostOperation pk-metal/reboot-1")

	// A lift removed on the source is removed on the target.
	claim.SetAnnotations(map[string]string{"example.com/note": "kept"})
	_, err := m.ensure(ctx, sourceObject{kind: movedKind{gvk: metalGV.WithKind("HostClaim"), namespaced: true}, obj: claim}, rw)
	require.NoError(t, err)
	require.NoError(t, target.Get(ctx, client.ObjectKey{Namespace: "default", Name: "c-1"}, got))
	assert.NotContains(t, got.Annotations, metalv1alpha1.AnnotationAckEtcdRisk)
}
