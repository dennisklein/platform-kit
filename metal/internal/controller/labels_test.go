package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func TestLabelValue(t *testing.T) {
	assert.Equal(t, "2288H-V5", labelValue("2288H V5"), "spaces become dashes")
	assert.Equal(t, "X12DPi-N6", labelValue("X12DPi-N6"))
	assert.Equal(t, "Huawei", labelValue("Huawei"))
	assert.Equal(t, "a", labelValue("--a--"), "edges must be alphanumeric")
	assert.Equal(t, "", labelValue("---"), "nothing legal leaves nothing")
	assert.LessOrEqual(t, len(labelValue(string(make([]byte, 200)))), 63)
}

func TestStampLabels(t *testing.T) {
	claim := &metalv1alpha1.HostClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-w1", Namespace: "default"},
		Spec:       metalv1alpha1.HostClaimSpec{Labels: map[string]metalv1alpha1.LabelValue{"role": "worker"}},
	}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{
		Name:   "simx12b000001",
		Labels: map[string]string{"human": "kept"},
	}}

	t.Run("bind stamps both axes and bookkeeps", func(t *testing.T) {
		require.True(t, stampLabels(host, claim, "Supermicro", "X12DPi-N6"))
		assert.Equal(t, map[string]string{
			"human":              "kept",
			"role":               "worker",
			"metal.pk.io/vendor": "Supermicro",
			"metal.pk.io/model":  "X12DPi-N6",
		}, host.Labels)
		assert.Equal(t, "metal.pk.io/model,metal.pk.io/vendor,role",
			host.Annotations[managedLabelsAnnotation])
		assert.False(t, stampLabels(host, claim, "Supermicro", "X12DPi-N6"), "steady state changes nothing")
	})

	t.Run("intent edit follows", func(t *testing.T) {
		claim.Spec.Labels = map[string]metalv1alpha1.LabelValue{"role": "controller", "pool": "gpu"}
		require.True(t, stampLabels(host, claim, "Supermicro", "X12DPi-N6"))
		assert.Equal(t, "controller", host.Labels["role"])
		assert.Equal(t, "gpu", host.Labels["pool"])
	})

	t.Run("release clears intent, keeps discovery and humans", func(t *testing.T) {
		require.True(t, stampLabels(host, nil, "Supermicro", "X12DPi-N6"))
		assert.Equal(t, map[string]string{
			"human":              "kept",
			"metal.pk.io/vendor": "Supermicro",
			"metal.pk.io/model":  "X12DPi-N6",
		}, host.Labels)
		assert.Equal(t, "metal.pk.io/model,metal.pk.io/vendor",
			host.Annotations[managedLabelsAnnotation])
	})
}
