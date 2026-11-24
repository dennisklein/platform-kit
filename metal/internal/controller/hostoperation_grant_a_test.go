package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
)

// The grant of a config medium ends when the node checks in: from then on
// the machine runs its config, and a URL that still serves its secrets is
// only a risk.
func TestCheckInRevokesTheGrants(t *testing.T) {
	check := metalv1alpha1.HostOperationAction{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{
		NodeRef: metalv1alpha1.Reference{Name: "node-a"}, TimeoutSeconds: 600}}
	op := preClearedOp(check)
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	r, _, rec := executorFor(t, "generic", op, node)
	r.Media = startMedia(t)
	handle := grantHandle(client.ObjectKeyFromObject(op))
	creds, err := asset.MintCredentials()
	require.NoError(t, err)
	_, err = r.Media.Grants.Issue(handle, "node-a.iso", []byte("config"), time.Hour, creds)
	require.NoError(t, err)

	x := &exec{r: r, op: op, cl: rec, systemID: "1", host: &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}}
	st, err := x.awaitCheckIn(t.Context(), check.AwaitCheckIn)
	require.NoError(t, err)
	require.False(t, st.done)
	_, live := r.Media.Grants.Live(handle, "node-a.iso")
	assert.True(t, live, "the grant lives while the machine boots")

	node.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionMachineReachable,
		Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonMachineAPI, LastTransitionTime: metav1.Now()}}
	require.NoError(t, r.Update(t.Context(), node))
	st, err = x.awaitCheckIn(t.Context(), check.AwaitCheckIn)
	require.NoError(t, err)
	require.True(t, st.done)
	_, live = r.Media.Grants.Live(handle, "node-a.iso")
	assert.False(t, live, "the check-in ends the grant")
}
