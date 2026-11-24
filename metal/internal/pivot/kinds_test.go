package pivot

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestNewKindsFromDiscovery(t *testing.T) {
	resources := []metav1.APIResource{
		{Name: "hosts", Kind: "Host", Verbs: []string{"get", "list"}},
		{Name: "hosts/status", Kind: "Host", Verbs: []string{"get", "patch"}},
		{Name: "talosrollouts", Kind: "TalosRollout", Verbs: []string{"get", "list", "watch"}},
		{Name: "etcdsnapshots", Kind: "EtcdSnapshot", Namespaced: true, Verbs: []string{"list"}},
		{Name: "reviews", Kind: "Review", Verbs: []string{"create"}},
	}
	got := newKinds(movedKinds, osGV, resources)
	require.Len(t, got, 3, "Host is new in this group version only; status and create-only resources are not kinds")
	kinds := map[string]bool{}
	for _, k := range got {
		kinds[k.gvk.Kind] = k.namespaced
	}
	assert.Contains(t, kinds, "TalosRollout")
	assert.True(t, kinds["EtcdSnapshot"], "scope comes from discovery")

	assert.Empty(t, newKinds(movedKinds, metalGV, resources[:2]), "a kind already listed by hand is not added twice")
	assert.True(t, movedKindOf(osGV.String(), "TalosRollout"), "an owner of any kind in the moved groups is moved")
}

func TestPreflightRefusesWorkInFlight(t *testing.T) {
	// Any kind of the moved groups that reports Progressing (a rollout
	// between nodes) holds the move. A window stands in for such a kind.
	busy := &metalv1alpha1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Name: "rollout-like"},
		Status: metalv1alpha1.MaintenanceWindowStatus{Conditions: []metav1.Condition{{
			Type: "Progressing", Status: metav1.ConditionTrue, Reason: "NodeInProgress", LastTransitionTime: metav1.Now(),
		}}}}
	m := fakeMover(t, append(operatorObjects(1, nil), busy), nil)
	_, err := m.Preflight(context.Background())
	require.ErrorContains(t, err, "MaintenanceWindow rollout-like is progressing")

	busy.Status.Conditions[0].Status = metav1.ConditionFalse
	m = fakeMover(t, append(operatorObjects(1, nil), client.Object(busy)), nil)
	_, err = m.Preflight(context.Background())
	require.NoError(t, err)
}

func TestPreflightRefusesARunningRollout(t *testing.T) {
	ro := &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: "lab-upgrade"}}
	ro.Spec.Approval.ApprovedBy = "alice"
	// Between nodes, waiting for etcd: Progressing is Unknown, and the
	// rollout still runs.
	ro.Status.Conditions = []metav1.Condition{{Type: "Progressing", Status: metav1.ConditionUnknown,
		Reason: "WaitingForHealth", LastTransitionTime: metav1.Now()}}
	preflight := func(ro *osv1alpha1.TalosRollout) error {
		m := fakeMover(t, append(operatorObjects(1, nil), ro.DeepCopy()), nil)
		m.kinds = append(slices.Clone(movedKinds), movedKind{gvk: osGV.WithKind("TalosRollout")})
		_, err := m.Preflight(context.Background())
		return err
	}
	require.ErrorContains(t, preflight(ro), "TalosRollout lab-upgrade is approved and not complete")

	ro.Status.Conditions = append(ro.Status.Conditions, metav1.Condition{Type: "Complete", Status: metav1.ConditionTrue,
		Reason: "RolloutSucceeded", LastTransitionTime: metav1.Now()})
	require.NoError(t, preflight(ro), "a finished rollout is only a record")

	plan := &osv1alpha1.TalosRollout{ObjectMeta: metav1.ObjectMeta{Name: "lab-config"}}
	require.NoError(t, preflight(plan), "an unapproved rollout is only a plan")
}

func TestCopyReadsTheCredentialsNamespace(t *testing.T) {
	objs := operatorObjects(1, nil)
	d := objs[1].(*appsv1.Deployment)
	d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--credentials-namespace=pk-creds")
	secret := func(ns, name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	objs = append(objs,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pk-creds"}},
		secret("pk-creds", "lab-secrets"),
		secret("pk-creds", "lab-bundle-applied"),
		secret("pk-creds", "machineconfig-node-1-3f2a9c"),
		secret("pk-creds", "backup-s3-credentials"),
		secret("pk-metal", "bmc-creds"),
		secret("elsewhere", "not-ours"))
	m := fakeMover(t, objs, nil)
	var err error
	m.coreNS, err = coreNamespaces(context.Background(), m.Source.Client, m.Options)
	require.NoError(t, err)
	assert.Equal(t, []string{"pk-metal", "pk-creds"}, m.coreNS)

	read, err := m.readSource(context.Background())
	require.NoError(t, err)
	var secrets []string
	for _, so := range read {
		if so.kind.gvk.Kind == "Secret" {
			secrets = append(secrets, objectRef(so.obj))
		}
	}
	assert.ElementsMatch(t, []string{"pk-metal/bmc-creds", "pk-creds/lab-secrets", "pk-creds/lab-bundle-applied",
		"pk-creds/machineconfig-node-1-3f2a9c", "pk-creds/backup-s3-credentials"}, secrets)
}
