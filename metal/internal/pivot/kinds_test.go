package pivot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
