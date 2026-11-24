package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

func memberNode(name string, role osv1alpha1.NodeRole, ready, deleting bool) osv1alpha1.TalosNode {
	n := osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: osv1alpha1.TalosNodeSpec{Role: role}}
	if ready {
		n.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonNodeReady}}
	}
	if deleting {
		n.DeletionTimestamp = ptr.To(metav1.Now())
		n.Finalizers = []string{talosNodeMemberFinalizer}
	}
	return n
}

func TestMemberCountLeavesOutNodesWithoutAMachine(t *testing.T) {
	nodes := []osv1alpha1.TalosNode{
		memberNode("cp-1", osv1alpha1.RoleControlPlane, true, false),
		// Written ahead of its machine: the next control plane of a scale-up.
		memberNode("cp-2", osv1alpha1.RoleControlPlane, false, false),
		// Being removed: its machine is still assigned, it is not a member.
		memberNode("cp-3", osv1alpha1.RoleControlPlane, false, true),
		memberNode("w-1", osv1alpha1.RoleWorker, true, false),
		memberNode("w-2", osv1alpha1.RoleWorker, false, false),
	}
	st := &osv1alpha1.TalosClusterStatus{}
	countMembers(st, nodes, map[string]bool{"cp-1": true, "cp-3": true, "w-1": true, "w-2": true})

	assert.EqualValues(t, 1, st.ControlPlanes)
	assert.EqualValues(t, 1, st.ReadyControlPlanes)
	assert.EqualValues(t, 2, st.Workers)
	assert.EqualValues(t, 1, st.ReadyWorkers, "an assigned worker that is not Ready still holds Available")
	assert.EqualValues(t, 2, st.UnassignedNodes, "the node without a machine and the one being removed")
}
