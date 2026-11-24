package drain

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func pod(name, node string, mut ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec:       corev1.PodSpec{NodeName: node},
	}
	for _, m := range mut {
		m(p)
	}
	return p
}

func ownedBy(kind string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: "x", UID: "u", Controller: ptr.To(true)}}
	}
}

func withEmptyDir(p *corev1.Pod) {
	p.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
}

// fakeCluster is a workload cluster with one node and the given pods. refuse
// names the pods whose evictions a disruption budget refuses.
func fakeCluster(t *testing.T, refuse map[string]bool, objs ...client.Object) (client.Client, *[]string) {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	var evictions []string
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}}
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(append(objs, node)...).
		WithIndex(&corev1.Pod{}, NodeNameField, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj client.Object, subObj client.Object, opts ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					evictions = append(evictions, obj.GetName())
					if refuse[obj.GetName()] {
						err := apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 10)
						err.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: policyv1.DisruptionBudgetCause,
							Message: "The disruption budget web-pdb needs 2 healthy pods and has 2 currently"}}
						return err
					}
				}
				return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
			},
		}).Build()
	return cl, &evictions
}

func TestCordonReportsWhetherItChangedTheNode(t *testing.T) {
	cl, _ := fakeCluster(t, nil)
	ctx := t.Context()

	changed, err := Cordon(ctx, cl, "node-2")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = Cordon(ctx, cl, "node-2")
	require.NoError(t, err)
	assert.False(t, changed, "cordoned already: not this call's doing")

	changed, err = Uncordon(ctx, cl, "node-2")
	require.NoError(t, err)
	assert.True(t, changed)
	n := &corev1.Node{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "node-2"}, n))
	assert.False(t, n.Spec.Unschedulable)

	_, err = Cordon(ctx, cl, "node-9")
	require.ErrorIs(t, err, ErrNodeNotFound)
}

func TestPassEvictsAllButDaemonSetAndMirrorPods(t *testing.T) {
	mirror := pod("kube-apiserver-node-2", "node-2", func(p *corev1.Pod) {
		p.Annotations = map[string]string{MirrorAnnotation: "abc"}
	})
	cl, evictions := fakeCluster(t, nil,
		pod("web-1", "node-2", ownedBy("ReplicaSet")),
		pod("bare", "node-2"),
		pod("agent", "node-2", ownedBy("DaemonSet")),
		mirror,
		pod("web-2", "node-3", ownedBy("ReplicaSet")),
	)

	p, cordoned, err := Pass(t.Context(), cl, "node-2", Options{})
	require.NoError(t, err)
	assert.True(t, cordoned, "a drain cordons first")
	assert.ElementsMatch(t, []string{"web-1", "bare"}, *evictions, "daemonset and mirror pods stay; other nodes are not touched")
	assert.Equal(t, 2, p.Evicted)

	// The fake deletes on eviction: the next pass finds the node drained.
	p, cordoned, err = Pass(t.Context(), cl, "node-2", Options{})
	require.NoError(t, err)
	assert.False(t, cordoned)
	assert.True(t, p.Done, "%+v", p)
	assert.Empty(t, p.Waiting())
}

func TestPassWaitsOnADisruptionBudget(t *testing.T) {
	cl, evictions := fakeCluster(t, map[string]bool{"web-1": true},
		pod("web-1", "node-2", ownedBy("ReplicaSet")),
		pod("web-2", "node-2", ownedBy("ReplicaSet")),
	)

	for i, remaining := range [][]string{{"apps/web-1", "apps/web-2"}, {"apps/web-1"}} {
		p, _, err := Pass(t.Context(), cl, "node-2", Options{})
		require.NoError(t, err, "a refused eviction is a wait, not a failure")
		assert.False(t, p.Done)
		assert.Equal(t, remaining, p.Remaining, "pass %d: an evicted pod counts until it is gone", i)
		require.Len(t, p.Blocked, 1)
		assert.Contains(t, p.Blocked[0], "web-pdb", "the budget's own words name it")
		assert.Contains(t, p.Waiting(), "eviction refused for apps/web-1")
	}
	assert.Equal(t, []string{"web-1", "web-2", "web-1"}, *evictions, "asked again, never deleted around the budget")
	assert.NoError(t, cl.Get(t.Context(), client.ObjectKey{Namespace: "apps", Name: "web-1"}, &corev1.Pod{}))
}

func TestPassRefusesToLoseEmptyDirDataUnlessAllowed(t *testing.T) {
	cl, evictions := fakeCluster(t, nil,
		pod("cache", "node-2", ownedBy("ReplicaSet"), withEmptyDir),
		pod("done", "node-2", withEmptyDir, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
	)

	_, _, err := Pass(t.Context(), cl, "node-2", Options{})
	var ed *EmptyDirError
	require.ErrorAs(t, err, &ed)
	assert.Equal(t, []string{"apps/cache"}, ed.Pods, "a finished pod's scratch space holds nothing a workload needs")
	assert.Empty(t, *evictions, "nothing is evicted when the pass refuses")

	p, _, err := Pass(t.Context(), cl, "node-2", Options{DeleteEmptyDirData: true, GracePeriodSeconds: ptr.To[int64](0)})
	require.NoError(t, err)
	assert.Equal(t, 2, p.Evicted)
}

func TestPassWaitsForTerminatingPods(t *testing.T) {
	now := metav1.Now()
	cl, evictions := fakeCluster(t, nil, pod("leaving", "node-2", func(p *corev1.Pod) {
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test/hold"}
	}))

	p, _, err := Pass(t.Context(), cl, "node-2", Options{})
	require.NoError(t, err)
	assert.False(t, p.Done)
	assert.Equal(t, []string{"apps/leaving"}, p.Remaining)
	assert.Empty(t, *evictions, "a pod on its way out is not evicted again")
}

func TestPassFailsOnOtherErrors(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}}, pod("web-1", "node-2")).
		WithIndex(&corev1.Pod{}, NodeNameField, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods/eviction"}, "web-1", nil)
			},
		}).Build()
	_, _, err := Pass(t.Context(), cl, "node-2", Options{})
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
}
