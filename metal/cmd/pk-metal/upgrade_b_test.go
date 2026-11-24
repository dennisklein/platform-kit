package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

func TestUpgradeRefusesASkippedKubernetesMinor(t *testing.T) {
	ctx := context.Background()
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6", KubernetesVersion: "v1.35.4"}}

	cl := fakeClient(cluster.DeepCopy())
	err := upgrade(ctx, cl, "kubernetes", "lab", "v1.37.0", 0, time.Millisecond, &bytes.Buffer{})
	require.ErrorContains(t, err, "one minor at a time")
	c := &osv1alpha1.TalosCluster{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "lab"}, c))
	assert.Equal(t, "v1.35.4", c.Spec.KubernetesVersion, "nothing changed")

	// The spec already asks for v1.35, but the API server still runs v1.34:
	// v1.36 would skip a minor on the machines.
	behind := cluster.DeepCopy()
	behind.Status.KubernetesVersion = "v1.34.2"
	cl = fakeClient(behind)
	err = upgrade(ctx, cl, "kubernetes", "lab", "v1.36.2", 0, time.Millisecond, &bytes.Buffer{})
	require.ErrorContains(t, err, "one minor at a time")
}
