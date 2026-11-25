package pivot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
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
