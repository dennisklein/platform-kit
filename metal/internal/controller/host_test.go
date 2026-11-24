package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
)

// endpointFor builds a ManagementEndpoint whose PATH facts (name, address,
// firmware, conditions) vary freely around fixed machine facts.
func endpointFor(name, address, firmware string, sys metalv1alpha1.DiscoveredSystem, conds ...metav1.Condition) *metalv1alpha1.ManagementEndpoint {
	return &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       metalv1alpha1.ManagementEndpointSpec{Address: address},
		Status: metalv1alpha1.ManagementEndpointStatus{
			Vendor:          "Huawei",
			BoardSerial:     "SIMHW2288V5000391",
			FirmwareVersion: firmware,
			Systems:         []metalv1alpha1.DiscoveredSystem{sys},
			Conditions:      conds,
		},
	}
}

// TestIdentityInvariant is the identity invariant's pure-function guard: the same system
// seen through two managers — different endpoint names, addresses, manager
// firmware — must normalize to a byte-identical identity and the same Host name.
func TestIdentityInvariant(t *testing.T) {
	sys := metalv1alpha1.DiscoveredSystem{
		ID:           "1",
		UUID:         "5A1B0001-0000-4000-8000-000000000391",
		SerialNumber: "SIMHW2288V5000391",
		Manufacturer: "Huawei",
		Model:        "2288H V5",
	}
	epA := endpointFor("ci-a", "https://[2001:db8:0:100::14]", "6.72", sys)
	epB := endpointFor("ci-b", "https://127.0.0.1:8443", "6.58", sys)

	idA := identity.Normalize(epA.Status.Vendor, epA.Status.BoardSerial, epA.Status.Systems[0])
	idB := identity.Normalize(epB.Status.Vendor, epB.Status.BoardSerial, epB.Status.Systems[0])
	assert.Equal(t, idA, idB, "identity must be byte-identical across paths")
	assert.Equal(t, hostNames(epA), hostNames(epB), "both paths converge on the same Host")
	assert.Equal(t, []string{"simhw2288v5000391"}, hostNames(epA))

	// An aggregator path reports its own vendor on the endpoint; the system's
	// manufacturer keeps the identity stable.
	epB.Status.Vendor = "SomeAggregator"
	idAgg := identity.Normalize(epB.Status.Vendor, epB.Status.BoardSerial, epB.Status.Systems[0])
	assert.Equal(t, idA, idAgg, "endpoint vendor is a path fact, not an identity input")
}

func TestProjectHostAggregation(t *testing.T) {
	sys := metalv1alpha1.DiscoveredSystem{ID: "1", UUID: "U-1", SerialNumber: "S-1", Manufacturer: "Huawei"}
	cond := func(t string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
		return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg}
	}
	up := cond(metalv1alpha1.ConditionReachable, metav1.ConditionTrue, "SessionUp", "")
	down := cond(metalv1alpha1.ConditionReachable, metav1.ConditionFalse, "DialError", "refused")
	ok := cond(metalv1alpha1.ConditionVerified, metav1.ConditionTrue, "SerialMatch", "")
	bad := cond(metalv1alpha1.ConditionVerified, metav1.ConditionFalse, metalv1alpha1.ReasonSerialMismatch, "asserted X")

	t.Run("two healthy paths", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, []hostPath{
			{ep: *endpointFor("ci-a", "", "", sys, up, ok), sys: sys},
			{ep: *endpointFor("ci-b", "", "", sys, up, ok), sys: sys},
		}, nil, nil)
		require.NotNil(t, host.Status.Identity)
		assert.Equal(t, "S-1", host.Status.Identity.Serial)
		require.Len(t, host.Status.ManagedBy, 2)
		assert.Equal(t, "ci-a", host.Status.ManagedBy[0].EndpointRef.Name)
		assert.True(t, host.Status.ManagedBy[0].Reachable)
		assert.True(t, meta.IsStatusConditionTrue(host.Status.Conditions, metalv1alpha1.ConditionReachable))
		assert.True(t, meta.IsStatusConditionTrue(host.Status.Conditions, metalv1alpha1.ConditionInspected))
		assert.True(t, meta.IsStatusConditionTrue(host.Status.Conditions, metalv1alpha1.ConditionVerified))
	})

	t.Run("one alarming path condemns the host", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, []hostPath{
			{ep: *endpointFor("ci-a", "", "", sys, up, bad), sys: sys},
			{ep: *endpointFor("ci-b", "", "", sys, up, ok), sys: sys},
		}, nil, nil)
		v := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionVerified)
		require.NotNil(t, v)
		assert.Equal(t, metav1.ConditionFalse, v.Status)
		assert.Equal(t, metalv1alpha1.ReasonSerialMismatch, v.Reason)
		assert.Contains(t, v.Message, "ci-a")
	})

	t.Run("unverified path keeps verification in flight", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, []hostPath{
			{ep: *endpointFor("ci-a", "", "", sys, up, ok), sys: sys},
			{ep: *endpointFor("ci-b", "", "", sys, up), sys: sys},
		}, nil, nil)
		v := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionVerified)
		require.NotNil(t, v)
		assert.Equal(t, metav1.ConditionUnknown, v.Status)
	})

	t.Run("all paths down", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, []hostPath{
			{ep: *endpointFor("ci-a", "", "", sys, down, ok), sys: sys},
		}, nil, nil)
		r := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionReachable)
		require.NotNil(t, r)
		assert.Equal(t, metav1.ConditionFalse, r.Status)
		assert.Equal(t, "AllPathsDown", r.Reason)
		assert.False(t, host.Status.ManagedBy[0].Reachable)
	})

	t.Run("diverged identity across paths holds inspection", func(t *testing.T) {
		other := sys
		other.UUID = "U-2"
		host := &metalv1alpha1.Host{}
		projectHost(host, []hostPath{
			{ep: *endpointFor("ci-a", "", "", sys, up, ok), sys: sys},
			{ep: *endpointFor("ci-b", "", "", other, up, ok), sys: other},
		}, nil, nil)
		i := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionInspected)
		require.NotNil(t, i)
		assert.Equal(t, metav1.ConditionFalse, i.Status)
		assert.Equal(t, metalv1alpha1.ReasonIdentityDiverged, i.Reason)
	})
}

func TestProjectHostOperations(t *testing.T) {
	sys := metalv1alpha1.DiscoveredSystem{ID: "1", UUID: "U-1", SerialNumber: "S-1", Manufacturer: "Huawei"}
	up := metav1.Condition{Type: metalv1alpha1.ConditionReachable, Status: metav1.ConditionTrue, Reason: "SessionUp"}
	t0 := time.Date(2026, 7, 18, 14, 57, 9, 0, time.UTC)
	pathAt := func(at time.Time) []hostPath {
		s := sys
		s.LastResetTime = &metav1.MicroTime{Time: at}
		return []hostPath{{ep: *endpointFor("ci-a", "", "", s, up), sys: s}}
	}
	// No approval field: nil required means a closed gate (the safe default
	// every client lands on).
	proposed := metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cycle"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef: metalv1alpha1.Reference{Name: "h"},
			Actions: []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
		},
	}

	t.Run("pending approval surfaces as Approval(domain)", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, pathAt(t0), []metalv1alpha1.HostOperation{proposed}, nil)
		ap := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionApprovalPending)
		require.NotNil(t, ap)
		assert.Equal(t, metav1.ConditionTrue, ap.Status)
		assert.Equal(t, "PowerCycle", ap.Reason)
		assert.Contains(t, ap.Message, "default/cycle")
		// The Approval(PowerCycle) blockedOn rendering is the phase pkg's contract,
		// asserted there (lint-phase keeps derivation reads out of this package).
	})

	t.Run("reset advance with no operation drifts", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, pathAt(t0), nil, nil)
		assert.Nil(t, meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionDrifted),
			"the first observation only records the memory")
		require.NotNil(t, host.Status.LastResetTime)

		projectHost(host, pathAt(t0.Add(time.Second)), nil, nil)
		d := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionDrifted)
		require.NotNil(t, d)
		assert.Equal(t, metav1.ConditionTrue, d.Status)
		assert.Equal(t, metalv1alpha1.ReasonUnexpectedReset, d.Reason)
	})

	t.Run("advance covered by an operation's observation is explained", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, pathAt(t0), nil, nil)
		done := proposed
		done.Spec.Approval.ApprovedBy = "alice"
		done.Status = metalv1alpha1.HostOperationStatus{
			ObservedResetTime: &metav1.MicroTime{Time: t0.Add(2 * time.Second)},
			Conditions: []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted,
				Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonStepsSucceeded}},
		}
		projectHost(host, pathAt(t0.Add(time.Second)), []metalv1alpha1.HostOperation{done}, nil)
		d := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionDrifted)
		require.NotNil(t, d)
		assert.Equal(t, metav1.ConditionFalse, d.Status)
		assert.Equal(t, metalv1alpha1.ReasonResetExplained, d.Reason)
	})

	t.Run("an active operation explains any advance", func(t *testing.T) {
		host := &metalv1alpha1.Host{}
		projectHost(host, pathAt(t0), nil, nil)
		active := proposed
		active.Spec.Approval.ApprovedBy = "alice"
		projectHost(host, pathAt(t0.Add(time.Second)), []metalv1alpha1.HostOperation{active}, nil)
		d := meta.FindStatusCondition(host.Status.Conditions, metalv1alpha1.ConditionDrifted)
		require.NotNil(t, d)
		assert.Equal(t, metav1.ConditionFalse, d.Status)
	})
}

func TestEndpointOwnerRefs(t *testing.T) {
	foreign := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "keep", UID: "u0"}
	stale := metav1.OwnerReference{
		APIVersion: metalv1alpha1.GroupVersion.String(), Kind: "ManagementEndpoint", Name: "gone", UID: "u1"}
	refs := endpointOwnerRefs([]metav1.OwnerReference{foreign, stale}, []hostPath{
		{ep: metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ci-a", UID: "u2"}}},
	})
	require.Len(t, refs, 2)
	assert.Equal(t, "keep", refs[0].Name, "refs of other kinds survive")
	assert.Equal(t, "ci-a", refs[1].Name, "endpoint refs are rebuilt to the fronting set")
}
