package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func weekly(tz string, start string, d time.Duration, days ...metalv1alpha1.Weekday) metalv1alpha1.MaintenanceWindow {
	return metalv1alpha1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: "w"},
		Spec: metalv1alpha1.MaintenanceWindowSpec{
			HostSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "r1"}},
			TimeZone:     tz,
			Weekly:       []metalv1alpha1.WeeklyWindow{{Days: days, Start: start, Duration: metav1.Duration{Duration: d}}},
		},
	}
}

func TestWindowStateWeekly(t *testing.T) {
	w := weekly("Europe/Berlin", "22:00", 4*time.Hour, "Sat")
	loc, err := parseWindow(&w)
	require.NoError(t, err)
	berlin, _ := time.LoadLocation("Europe/Berlin")

	// Saturday 2026-03-07, before, inside, past midnight, after.
	at := func(day, h, m int) time.Time { return time.Date(2026, 3, day, h, m, 0, 0, berlin) }
	open, next := windowState(&w, loc, at(7, 21, 0))
	assert.False(t, open)
	assert.Equal(t, at(7, 22, 0), next.In(berlin))

	open, next = windowState(&w, loc, at(7, 23, 0))
	assert.True(t, open)
	assert.Equal(t, at(8, 2, 0), next.In(berlin), "open: the next change is the close")

	open, _ = windowState(&w, loc, at(8, 1, 59))
	assert.True(t, open, "a window that runs past midnight is still open on Sunday morning")

	open, next = windowState(&w, loc, at(8, 2, 0))
	assert.False(t, open, "the end is exclusive")
	assert.Equal(t, at(14, 22, 0), next.In(berlin), "next Saturday")
}

func TestWindowStateFollowsDaylightSaving(t *testing.T) {
	// Europe/Berlin moves to summer time on Sunday 2026-03-29: a 03:00
	// Sunday window is read in local time on both sides of the change.
	w := weekly("Europe/Berlin", "03:00", time.Hour, "Sun")
	loc, err := parseWindow(&w)
	require.NoError(t, err)
	before := time.Date(2026, 3, 22, 3, 30, 0, 0, loc)
	after := time.Date(2026, 3, 29, 3, 30, 0, 0, loc)
	open, _ := windowState(&w, loc, before)
	assert.True(t, open)
	open, _ = windowState(&w, loc, after)
	assert.True(t, open, "03:00 local, not 03:00 of the old offset")
	assert.NotEqual(t, before.UTC().Hour(), after.UTC().Hour(), "the UTC hour moved, the local one did not")
}

func TestWindowStateOnceAndChained(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	w := metalv1alpha1.MaintenanceWindow{Spec: metalv1alpha1.MaintenanceWindowSpec{
		Once: []metalv1alpha1.OnceWindow{
			{Start: metav1.NewTime(now.Add(-time.Hour)), End: metav1.NewTime(now.Add(time.Hour))},
			{Start: metav1.NewTime(now.Add(time.Hour)), End: metav1.NewTime(now.Add(3 * time.Hour))},
			{Start: metav1.NewTime(now.Add(48 * time.Hour)), End: metav1.NewTime(now.Add(49 * time.Hour))},
		},
	}}
	open, next := windowState(&w, time.UTC, now)
	assert.True(t, open)
	assert.Equal(t, now.Add(3*time.Hour), next, "windows back to back stay open until the last one closes")

	open, next = windowState(&w, time.UTC, now.Add(4*time.Hour))
	assert.False(t, open)
	assert.Equal(t, now.Add(48*time.Hour), next)

	open, next = windowState(&w, time.UTC, now.Add(50*time.Hour))
	assert.False(t, open)
	assert.True(t, next.IsZero(), "nothing comes again")
}

func TestParseWindowRefusesWhatTheSchemaCannot(t *testing.T) {
	w := weekly("Mars/Olympus", "01:00", time.Hour, "Mon")
	_, err := parseWindow(&w)
	require.Error(t, err)
	w = weekly("UTC", "01:00", 8*24*time.Hour, "Mon")
	_, err = parseWindow(&w)
	require.Error(t, err)
	w = weekly("", "01:00", time.Hour, "Mon")
	loc, err := parseWindow(&w)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, loc, "empty time zone is UTC")
}

func TestDecideWindow(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC) // a Wednesday
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h1", Labels: map[string]string{"rack": "r1"}}}
	other := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h2", Labels: map[string]string{"rack": "r2"}}}
	op := &metalv1alpha1.HostOperation{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: "bios"},
		Spec: metalv1alpha1.HostOperationSpec{Reason: metalv1alpha1.OperationReasonBiosConvergence}}
	sat := weekly("UTC", "22:00", 4*time.Hour, "Sat")

	v := decideWindow([]metalv1alpha1.MaintenanceWindow{sat}, op, other, "", now)
	assert.Empty(t, v.held, "a host no window covers is not held")

	v = decideWindow([]metalv1alpha1.MaintenanceWindow{sat}, op, host, "", now)
	require.NotEmpty(t, v.held)
	assert.Contains(t, v.held, "2026-03-07T22:00:00Z")
	assert.Contains(t, v.held, metalv1alpha1.AnnotationIgnoreWindow, "the message says how to start it anyway")
	assert.Equal(t, now.Add(windowRecheck), v.until, "far-off windows are looked at again periodically")

	wed := weekly("UTC", "11:00", 2*time.Hour, "Wed")
	wed.Name = "wed"
	v = decideWindow([]metalv1alpha1.MaintenanceWindow{sat, wed}, op, host, "", now)
	assert.Empty(t, v.held, "one open window of those that cover the host is enough")

	byCluster := sat
	byCluster.Spec.HostSelector = nil
	byCluster.Spec.Clusters = []string{"prod"}
	v = decideWindow([]metalv1alpha1.MaintenanceWindow{byCluster}, op, other, "prod", now)
	assert.NotEmpty(t, v.held, "a cluster name covers the hosts of its nodes")
	v = decideWindow([]metalv1alpha1.MaintenanceWindow{byCluster}, op, other, "lab", now)
	assert.Empty(t, v.held)

	urgent := op.DeepCopy()
	urgent.Annotations = map[string]string{metalv1alpha1.AnnotationIgnoreWindow: "CVE fix"}
	assert.Empty(t, decideWindow([]metalv1alpha1.MaintenanceWindow{sat}, urgent, host, "", now).held)
	install := op.DeepCopy()
	install.Spec.Reason = reasonProvisioning
	assert.Empty(t, decideWindow([]metalv1alpha1.MaintenanceWindow{sat}, install, host, "", now).held,
		"a new install is not held")

	broken := weekly("Nowhere/Never", "11:00", 2*time.Hour, "Wed")
	v = decideWindow([]metalv1alpha1.MaintenanceWindow{broken}, op, host, "", now)
	assert.Contains(t, v.held, "cannot read", "a window that cannot be read holds instead of letting work through")

	soon := weekly("UTC", "12:03", time.Hour, "Wed")
	v = decideWindow([]metalv1alpha1.MaintenanceWindow{soon}, op, host, "", now)
	assert.Equal(t, now.Add(3*time.Minute), v.until, "an opening sooner than the recheck is woken for exactly")
}
