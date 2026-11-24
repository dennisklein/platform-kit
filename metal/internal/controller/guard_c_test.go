package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// An empty or blank guard annotation lifts nothing: only a reason does.
func TestGuardOverrideNeedsAReason(t *testing.T) {
	for value, lifts := range map[string]bool{"": false, "   ": false, "\t\n": false, "CVE fix": true, " repair ": true} {
		op := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{metalv1alpha1.AnnotationIgnoreWindow: value}}}
		_, got := guardOverride(op, metalv1alpha1.AnnotationIgnoreWindow)
		assert.Equal(t, lifts, got, "value %q", value)
	}
	_, got := guardOverride(&metalv1alpha1.HostOperation{}, metalv1alpha1.AnnotationIgnoreWindow)
	assert.False(t, got, "no annotation lifts nothing")
}

func TestDecideWindowEmptyIgnoreWindowHolds(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC) // a Wednesday
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h1", Labels: map[string]string{"rack": "r1"}}}
	sat := weekly("UTC", "22:00", 4*time.Hour, "Sat")
	op := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "bios",
		Annotations: map[string]string{metalv1alpha1.AnnotationIgnoreWindow: ""}},
		Spec: metalv1alpha1.HostOperationSpec{Reason: metalv1alpha1.OperationReasonBiosConvergence}}

	v := decideWindow([]metalv1alpha1.MaintenanceWindow{sat}, op, host, "", now)
	require.NotEmpty(t, v.held, "an empty reason does not lift the window")
	assert.Contains(t, v.held, "an empty reason does not count")
	assert.Empty(t, windowExempt(op))
}

func TestClusterGuardEmptyOverrideHolds(t *testing.T) {
	lab := newTalosLab(t)
	lab.machines["cp-3"].SetUnreachable(true)
	msg := guardCheck(t, lab, "cp-2", func(op *metalv1alpha1.HostOperation) {
		op.Annotations = map[string]string{metalv1alpha1.AnnotationOverrideClusterGuard: " "}
	})
	assert.Contains(t, msg, "1 healthy voting member(s) of 3", "a blank override lifts nothing")
	assert.Contains(t, msg, "an empty reason does not count")
}
