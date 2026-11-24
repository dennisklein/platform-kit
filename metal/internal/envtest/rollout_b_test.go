package envtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// The drain budget is part of the approved plan: the schema keeps it as it
// was created, defaulted or not.
func TestRolloutDrainTimeoutIsFrozen(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	ro := &osv1alpha1.TalosRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "drain-frozen"},
		Spec: osv1alpha1.TalosRolloutSpec{ClusterRef: osv1alpha1.Reference{Name: "c"},
			Kind: osv1alpha1.RolloutReboot, ProposedBy: "alice"},
	}
	require.NoError(t, st.cl.Create(ctx, ro))
	assert.Equal(t, int32(900), ro.Spec.DrainTimeoutSeconds, "the schema default")

	ro.Spec.DrainTimeoutSeconds = 60
	err := st.cl.Update(ctx, ro)
	require.True(t, apierrors.IsInvalid(err), "a changed drain budget is refused: %v", err)
	assert.Contains(t, err.Error(), "drainTimeoutSeconds is frozen")
}
