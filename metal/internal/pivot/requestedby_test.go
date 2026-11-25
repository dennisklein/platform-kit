package pivot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
)

func TestStampRequester(t *testing.T) {
	obj := func(kind string, labels, annotations map[string]string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetKind(kind)
		u.SetLabels(labels)
		u.SetAnnotations(annotations)
		return u
	}
	key := metalv1alpha1.AnnotationRequestedBy
	for _, c := range []struct {
		name string
		obj  *unstructured.Unstructured
		want string
	}{
		{"a cluster that names nobody names the move", obj("TalosCluster", nil, nil), impersonatedMover},
		{"a claim keeps the person it names", obj("HostClaim", nil, map[string]string{key: "alice"}), "alice"},
		{"a Host asking for no wipe is not gated", obj("Host", nil, nil), ""},
		{"a Host asking for a wipe is", obj("Host", nil, map[string]string{metalv1alpha1.AnnotationWipe: "v1.14.6"}), impersonatedMover},
		{"a ConfigMap that is no patch is not gated", obj("ConfigMap", nil, nil), ""},
		{"a patch ConfigMap is", obj("ConfigMap", map[string]string{osv1alpha1.LabelPatch: "true"}, nil), impersonatedMover},
		{"an operation is not gated", obj("HostOperation", nil, nil), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stampRequester(c.obj)
			assert.Equal(t, c.want, c.obj.GetAnnotations()[key])
		})
	}
}

// Prune names itself as the requester of a gated object before it deletes
// it, and writes a deleted line for each object it removes.
func TestPruneStampsAndAudits(t *testing.T) {
	ctx := context.Background()
	stale := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "stale", UID: "h-1"}}
	old := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "old", UID: "c-1",
		Finalizers: []string{"example.com/hold"}}}
	m := fakeMover(t, nil, []client.Object{stale, old})
	var trail bytes.Buffer
	m.Options.Audit = audit.New(&trail, "dana")
	var deletedAs []string
	m.Target.impersonate = func(user string, _ []string) (client.Client, error) {
		return interceptor.NewClient(m.Target.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				assert.Equal(t, user, obj.GetAnnotations()[metalv1alpha1.AnnotationRequestedBy],
					"the stamp names the user of the request")
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletedAs = append(deletedAs, user+" "+obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
		}), nil
	}

	pruned, err := m.prune(ctx, nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Host stale", "TalosCluster old"}, pruned)
	assert.Equal(t, []string{impersonatedMover + " old"}, deletedAs, "only the gated cluster is deleted as the move")

	var events []string
	for _, raw := range strings.Split(strings.TrimSpace(trail.String()), "\n") {
		var l audit.Line
		require.NoError(t, json.Unmarshal([]byte(raw), &l), raw)
		events = append(events, fmt.Sprintf("%s %s %s %s %s", l.Event, l.Kind, l.Name, l.Actor, l.Reason))
	}
	assert.ElementsMatch(t, []string{
		"deleted Host stale dana PivotPruned",
		"deleted TalosCluster old dana PivotPruned",
	}, events)
}
