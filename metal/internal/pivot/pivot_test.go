package pivot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestRewriteOwnerReferencesAndClaimUIDs(t *testing.T) {
	rw := &rewriter{
		uids:   map[types.UID]types.UID{"ep-src": "ep-dst", "claim-src": "claim-dst"},
		claims: map[types.UID]bool{"claim-src": true, "claim-later": true},
	}
	host := &unstructured.Unstructured{}
	host.SetGroupVersionKind(metalGV.WithKind("Host"))
	host.SetName("node-1")
	host.SetUID("host-src")
	host.SetResourceVersion("42")
	host.SetLabels(map[string]string{labelClaimUID: "claim-src"})
	host.SetAnnotations(map[string]string{annotationReleasePowerOff: "claim-gone"})
	host.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: metalGV.String(), Kind: "ManagementEndpoint", Name: "bmc-1", UID: "ep-src"},
		{APIVersion: "cert-manager.io/v1", Kind: "CertificateRequest", Name: "x", UID: "cr"},
	})
	require.NoError(t, unstructured.SetNestedField(host.Object, "claim-src", "spec", "claimRef", "uid"))

	out, n, complete := rw.rewrite(host)
	assert.True(t, complete)
	assert.Equal(t, 3, n, "one owner reference, one claimRef, the label")
	assert.Empty(t, out.GetUID())
	assert.Empty(t, out.GetResourceVersion())
	require.Len(t, out.GetOwnerReferences(), 1, "an owner the move does not carry is dropped")
	assert.Equal(t, types.UID("ep-dst"), out.GetOwnerReferences()[0].UID)
	uid, _, _ := unstructured.NestedString(out.Object, "spec", "claimRef", "uid")
	assert.Equal(t, "claim-dst", uid)
	assert.Equal(t, "claim-dst", out.GetLabels()[labelClaimUID])
	assert.Equal(t, "claim-gone", out.GetAnnotations()[annotationReleasePowerOff],
		"a UID of a claim gone on both sides is a record and stays")
	assert.Equal(t, types.UID("host-src"), host.GetUID(), "the source object is not changed")
}

func TestRewriteDefersUnknownReferences(t *testing.T) {
	rw := &rewriter{uids: map[types.UID]types.UID{}, claims: map[types.UID]bool{"claim-later": true}}
	secret := &unstructured.Unstructured{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	secret.SetName("node-1-talosconfig")
	secret.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: osGV.String(), Kind: "TalosNode", Name: "node-1", UID: "tn-src"},
	})
	out, n, complete := rw.rewrite(secret)
	assert.False(t, complete)
	assert.Zero(t, n)
	assert.Empty(t, out.GetOwnerReferences(), "a UID the target does not know is never written")

	host := &unstructured.Unstructured{}
	host.SetGroupVersionKind(metalGV.WithKind("Host"))
	host.SetName("node-1")
	require.NoError(t, unstructured.SetNestedField(host.Object, "claim-later", "spec", "claimRef", "uid"))
	_, _, complete = rw.rewrite(host)
	assert.False(t, complete, "the claim is copied later")

	rw.uids["tn-src"] = "tn-dst"
	rw.uids["claim-later"] = "claim-dst"
	out, n, complete = rw.rewrite(secret)
	assert.True(t, complete)
	assert.Equal(t, 1, n)
	assert.Equal(t, types.UID("tn-dst"), out.GetOwnerReferences()[0].UID)
	_, _, complete = rw.rewrite(host)
	assert.True(t, complete)
}

func TestMovedKindOf(t *testing.T) {
	assert.True(t, movedKindOf(metalGV.String(), "Host"))
	assert.True(t, movedKindOf(osGV.String(), "TalosNode"))
	assert.True(t, movedKindOf("v1", "Secret"))
	assert.False(t, movedKindOf("apps/v1", "Deployment"))
	assert.False(t, movedKindOf("a/b/c", "Host"))
}

func TestMergeIntoKeepsTheTargetsApproval(t *testing.T) {
	cur := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": metalGV.String(), "kind": "HostOperation",
		"metadata": map[string]any{"name": "op", "namespace": "lab", "uid": "dst"},
		"spec":     map[string]any{"reason": "Release", "approval": map[string]any{"approvedBy": "ada"}},
	}}
	want := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": metalGV.String(), "kind": "HostOperation",
		"metadata": map[string]any{"name": "op", "namespace": "lab"},
		"spec":     map[string]any{"reason": "Release", "approval": map[string]any{}},
	}}
	assert.False(t, mergeInto(cur, want, true), "the same operation, its approval replayed")

	want.SetLabels(map[string]string{"a": "b"})
	assert.True(t, mergeInto(cur, want, true))
	assert.Equal(t, "b", cur.GetLabels()["a"])
	by, _, _ := unstructured.NestedString(cur.Object, "spec", "approval", "approvedBy")
	assert.Equal(t, "ada", by)
	assert.Equal(t, "dst", string(cur.GetUID()), "the target's metadata stays the target's")

	host := cur.DeepCopy()
	assert.True(t, mergeInto(host, want, false), "for any other kind the source's spec wins")
	_, found, _ := unstructured.NestedString(host.Object, "spec", "approval", "approvedBy")
	assert.False(t, found)
}

func TestBlockingOperations(t *testing.T) {
	executed := func(s metav1.ConditionStatus) []metav1.Condition {
		return []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: s}}
	}
	op := func(name string, mut func(*metalv1alpha1.HostOperation)) metalv1alpha1.HostOperation {
		o := metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lab"}}
		o.Spec.HostRef.Name = "node-1"
		mut(&o)
		return o
	}
	ops := []metalv1alpha1.HostOperation{
		op("done", func(o *metalv1alpha1.HostOperation) { o.Status.Conditions = executed(metav1.ConditionTrue) }),
		op("failed", func(o *metalv1alpha1.HostOperation) { o.Status.Conditions = executed(metav1.ConditionFalse) }),
		op("running", func(o *metalv1alpha1.HostOperation) {
			o.Status.Baseline = &metalv1alpha1.OperationBaseline{}
			o.Status.Conditions = executed(metav1.ConditionUnknown)
		}),
		op("waiting", func(*metalv1alpha1.HostOperation) {}),
		op("approved", func(o *metalv1alpha1.HostOperation) { o.Spec.Approval.ApprovedBy = "ada" }),
		op("automatic", func(o *metalv1alpha1.HostOperation) { o.Spec.Approval.Required = ptr.To(false) }),
	}
	got := blockingOperations(ops, false)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "lab/running is running")
	assert.Contains(t, got[1], "lab/waiting waits for approval")

	got = blockingOperations(ops, true)
	require.Len(t, got, 1, "--force-waiting moves a waiting operation as it is")
	assert.Contains(t, got[0], "lab/running")
}

func TestManagerForTarget(t *testing.T) {
	src := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "pk-metal-manager", Annotations: map[string]string{"deployment.kubernetes.io/revision": "3"},
	}}
	src.Spec.Replicas = ptr.To(int32(1))
	src.Spec.Template.Spec.Containers = []corev1.Container{
		{Name: "manager", Image: "old:1", Args: []string{"--leader-elect", "--media-url=https://10.0.0.1:30443"}},
		{Name: "sidecar", Image: "side:1", Args: []string{"--media-url=keep"}},
	}
	d := managerForTarget(src, "registry.example/pk-metal:2", "https://10.0.0.11:30443", nil)
	assert.Equal(t, int32(0), *d.Spec.Replicas)
	assert.NotContains(t, d.Annotations, "deployment.kubernetes.io/revision")
	assert.Equal(t, "registry.example/pk-metal:2", d.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, []string{"--leader-elect", "--media-url=https://10.0.0.11:30443"}, d.Spec.Template.Spec.Containers[0].Args)
	assert.Equal(t, "side:1", d.Spec.Template.Spec.Containers[1].Image, "only the manager container changes")
	assert.Equal(t, int32(1), *src.Spec.Replicas, "the source object is not changed")

	d = managerForTarget(src, "", "", nil)
	assert.Equal(t, "old:1", d.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, "https://10.0.0.1:30443", argValue(d, "--media-url"))
}

func TestGuardsOperator(t *testing.T) {
	p := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	assert.False(t, guardsOperator(p))
	p.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{Rule: admissionregistrationv1.Rule{APIGroups: []string{"apps"}}},
	}}}
	assert.False(t, guardsOperator(p))
	p.Spec.MatchConstraints.ResourceRules[0].APIGroups = append(p.Spec.MatchConstraints.ResourceRules[0].APIGroups, "metal.pk.io")
	assert.True(t, guardsOperator(p))

	settings := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	settings.Spec.MatchConstraints = &admissionregistrationv1.MatchResources{ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
		ResourceNames: []string{"hostoperation-approval"},
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{Rule: admissionregistrationv1.Rule{
			APIGroups: []string{""}, Resources: []string{"configmaps"}}},
	}}}
	assert.True(t, guardsOperator(settings), "the guard of the site settings moves too")
	settings.Spec.MatchConstraints.ResourceRules[0].ResourceNames = nil
	assert.False(t, guardsOperator(settings), "a policy on other ConfigMaps stays behind")

	settings.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "os.pk.io/patch", Operator: metav1.LabelSelectorOpExists}}}
	assert.True(t, guardsOperator(settings), "the policy on the Talos config patches moves too")
	settings.Spec.MatchConstraints.ObjectSelector.MatchExpressions[0].Operator = metav1.LabelSelectorOpDoesNotExist
	assert.False(t, guardsOperator(settings), "a policy on the ConfigMaps that are no patches stays behind")
}

func TestLeaseFree(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	lease := func(holder string, renewed time.Duration) *coordinationv1.Lease {
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "pk-metal"},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holder),
				LeaseDurationSeconds: ptr.To(int32(15)),
				RenewTime:            &metav1.MicroTime{Time: now.Add(-renewed)},
			},
		}
	}
	for name, tc := range map[string]struct {
		objs []client.Object
		free bool
	}{
		"no lease":         {free: true},
		"released":         {objs: []client.Object{lease("", 0)}, free: true},
		"held":             {objs: []client.Object{lease("pod-1", 5*time.Second)}},
		"expired":          {objs: []client.Object{lease("pod-1", 20*time.Second)}, free: true},
		"other namespaces": {objs: []client.Object{func() client.Object { l := lease("pod-1", 0); l.Namespace = "x"; return l }()}, free: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(typedScheme).WithObjects(tc.objs...).Build()
			free, err := leaseFree(ctx, c, "pk-metal", now)
			require.NoError(t, err)
			assert.Equal(t, tc.free, free)
		})
	}
}

func TestTalosAddress(t *testing.T) {
	n := &osv1alpha1.TalosNode{}
	n.Status.Address = "10.0.0.3"
	assert.Equal(t, "10.0.0.3", talosAddress(n))
	n.Spec.Network.Links = []osv1alpha1.Link{{Addresses: []string{"10.0.0.2/24"}}}
	assert.Equal(t, "10.0.0.2", talosAddress(n))
	n.Spec.Address = "10.0.0.1"
	assert.Equal(t, "10.0.0.1", talosAddress(n))
}

// fakeMover is a move between two fake clusters.
func fakeMover(t *testing.T, src, dst []client.Object) *Mover {
	t.Helper()
	build := func(name string, objs []client.Object) *Cluster {
		c := fake.NewClientBuilder().WithScheme(typedScheme).WithObjects(objs...).
			WithStatusSubresource(&appsv1.Deployment{}).Build()
		cl := &Cluster{Name: name, Client: c}
		cl.impersonate = func(string, []string) (client.Client, error) { return c, nil }
		return cl
	}
	o := Options{}
	o.defaults()
	return &Mover{Source: build("kind", src), Target: build("talos", dst), Options: o, sourceID: "src-id", targetID: "dst-id"}
}

func operatorObjects(replicas int32, nsAnnotations map[string]string) []client.Object {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "pk-metal-manager", Namespace: "pk-metal"}}
	d.Spec.Replicas = ptr.To(replicas)
	d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pk-metal"}}
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "manager", Args: []string{"--media-url=https://10.0.0.1:30443"}}}
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pk-metal", Annotations: nsAnnotations}},
		d,
	}
}

func TestPreflight(t *testing.T) {
	ctx := context.Background()
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc-1"}}
	ep.Spec.Address = "https://10.0.0.101:8443"
	waiting := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "lab"}}
	k8sNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-1"}, Status: corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
	}}

	t.Run("a fresh target", func(t *testing.T) {
		m := fakeMover(t, append(operatorObjects(1, nil), ep), []client.Object{k8sNode})
		plan, err := m.Preflight(ctx)
		require.NoError(t, err)
		assert.False(t, plan.Resume)
		assert.False(t, plan.Prune)
		require.Len(t, plan.Reachable, 2)
		assert.Contains(t, plan.Reachable[0], "10.0.0.101:8443")
		assert.Contains(t, plan.Reachable[1], "10.0.0.11")
		assert.Contains(t, plan.Reachable[1], "give --media-url")
	})
	t.Run("an operation waits for approval", func(t *testing.T) {
		m := fakeMover(t, append(operatorObjects(1, nil), waiting), nil)
		_, err := m.Preflight(ctx)
		require.ErrorContains(t, err, "lab/op waits for approval")
		m.Options.ForceWaiting = true
		_, err = m.Preflight(ctx)
		require.NoError(t, err)
	})
	t.Run("the target runs a manager", func(t *testing.T) {
		m := fakeMover(t, operatorObjects(1, nil), operatorObjects(1, nil))
		_, err := m.Preflight(ctx)
		require.ErrorContains(t, err, "already runs a pk-metal manager")
	})
	t.Run("the target runs the manager this move started", func(t *testing.T) {
		m := fakeMover(t, operatorObjects(0, map[string]string{AnnotationMovedTo: "dst-id"}),
			operatorObjects(1, map[string]string{AnnotationSource: "src-id"}))
		plan, err := m.Preflight(ctx)
		require.NoError(t, err)
		assert.True(t, plan.Resume)
	})
	t.Run("the target holds a fleet of its own", func(t *testing.T) {
		m := fakeMover(t, operatorObjects(1, nil), []client.Object{host})
		_, err := m.Preflight(ctx)
		require.ErrorContains(t, err, "does not merge two fleets")
	})
	t.Run("the target holds an older copy", func(t *testing.T) {
		m := fakeMover(t, operatorObjects(1, nil),
			append(operatorObjects(0, map[string]string{AnnotationMovedTo: "src-id"}), host))
		plan, err := m.Preflight(ctx)
		require.NoError(t, err)
		assert.True(t, plan.Prune, "a move back removes what the source no longer has")
	})
	t.Run("an object is being deleted", func(t *testing.T) {
		gone := host.DeepCopy()
		gone.Finalizers = []string{"metal.pk.io/claim-protection"}
		gone.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		m := fakeMover(t, append(operatorObjects(1, nil), gone), nil)
		_, err := m.Preflight(ctx)
		require.ErrorContains(t, err, "Host node-1 is being deleted")
	})
}

func TestPauseAndStart(t *testing.T) {
	ctx := context.Background()
	m := fakeMover(t, operatorObjects(2, nil), operatorObjects(0, nil))
	m.Options.Poll = time.Millisecond
	require.NoError(t, m.Pause(ctx))

	d := &appsv1.Deployment{}
	require.NoError(t, m.Source.Client.Get(ctx, client.ObjectKey{Namespace: "pk-metal", Name: "pk-metal-manager"}, d))
	assert.Equal(t, int32(0), *d.Spec.Replicas)
	ns := &corev1.Namespace{}
	require.NoError(t, m.Source.Client.Get(ctx, client.ObjectKey{Name: "pk-metal"}, ns))
	assert.Equal(t, "dst-id", ns.Annotations[AnnotationMovedTo])
	assert.Equal(t, "2", ns.Annotations[AnnotationReplicas])

	require.NoError(t, m.Pause(ctx), "a second pause keeps the replica count from before the first")
	require.NoError(t, m.Source.Client.Get(ctx, client.ObjectKey{Name: "pk-metal"}, ns))
	assert.Equal(t, "2", ns.Annotations[AnnotationReplicas])

	require.NoError(t, m.Start(ctx))
	require.NoError(t, m.Target.Client.Get(ctx, client.ObjectKey{Namespace: "pk-metal", Name: "pk-metal-manager"}, d))
	assert.Equal(t, int32(2), *d.Spec.Replicas)
}

func TestPauseWaitsForTheLease(t *testing.T) {
	ctx := context.Background()
	held := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "pk-metal"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To("pod-1"), LeaseDurationSeconds: ptr.To(int32(3600)),
			RenewTime: &metav1.MicroTime{Time: time.Now()},
		},
	}
	m := fakeMover(t, append(operatorObjects(1, nil), held), nil)
	m.Options.Poll = time.Millisecond
	m.Options.Timeout = 20 * time.Millisecond
	require.ErrorContains(t, m.Pause(ctx), "leader lease")
}
