package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// The factory strategy hands a URL to a BMC, which mounts it hours later and
// far away. A schematic the factory never heard of is a 404 there and nowhere
// else, so the URL is only worth stamping once the factory has been told.
func TestFactoryStrategyHoldsWhenTheSchematicCannotBeRegistered(t *testing.T) {
	ctx := context.Background()
	factoryAsset := func() *osv1alpha1.BootAsset {
		return &osv1alpha1.BootAsset{
			ObjectMeta: metav1.ObjectMeta{Name: "install", Generation: 3},
			Spec: osv1alpha1.BootAssetSpec{
				Strategy:     osv1alpha1.StrategyFactory,
				TalosVersion: "v1.13.4",
			},
		}
	}

	for name, factory := range map[string]*render.Factory{
		// Nothing listens on port 1; this is a factory that cannot be reached.
		"unreachable": render.NewFactory("127.0.0.1:1"),
		// A manager wired without one — the URL would still compute.
		"unconfigured": nil,
	} {
		t.Run(name, func(t *testing.T) {
			ba := factoryAsset()
			(&BootAssetReconciler{Factory: factory}).addressImage(ctx, ba)

			c := meta.FindStatusCondition(ba.Status.Conditions, osv1alpha1.ConditionReady)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, osv1alpha1.ReasonSchematicUnavailable, c.Reason)
			assert.EqualValues(t, 3, c.ObservedGeneration)
			assert.Empty(t, ba.Status.URL,
				"an image that may not exist must not be handed out as a URL")
		})
	}
}
