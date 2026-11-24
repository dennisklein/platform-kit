package envtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

// invariantsSettle bounds how long the objects may take to stop changing
// after a subtest before the invariants must hold.
const invariantsSettle = 10 * time.Second

// run is t.Run with the cross-cutting invariants checked once the subtest
// ends: whatever a subtest did, the objects it leaves behind must agree with
// their own conditions, and every status writer must own only its share.
func (s *suite) run(t *testing.T, name string, fn func(t *testing.T)) bool {
	t.Helper()
	return t.Run(name, func(t *testing.T) {
		t.Cleanup(func() {
			if !t.Failed() {
				checkInvariants(t, s.cl)
			}
		})
		fn(t)
	})
}

// checkInvariants waits for the invariants to hold on every object, and fails
// with the last violations when they do not settle.
func checkInvariants(t *testing.T, cl client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), invariantsSettle+5*time.Second)
	defer cancel()
	var last []string
	require.Eventually(t, func() bool {
		v, err := violations(ctx, cl)
		if err != nil {
			last = []string{err.Error()}
			return false
		}
		last = v
		return len(v) == 0
	}, invariantsSettle, tick, "invariants never settled")
	assert.Empty(t, last)
	if t.Failed() {
		t.Logf("invariant violations:\n  %s", strings.Join(last, "\n  "))
	}
}

// statusOwners names, per kind, the status fields a field manager may own:
// conditions by type, and other top-level status fields. A manager listed
// with owns may own only those; one listed with never may own any but those.
// A writer that names a field it does not own deletes another writer's memory
// on its next apply, or claims a condition it does not maintain.
type ownerRule struct {
	owns  []string // empty: no allow-list
	never []string
}

var (
	hostOwners = map[string]ownerRule{
		"pk-metal-host-convergence": {owns: []string{
			"condition:" + metalv1alpha1.ConditionBiosConverged,
			"condition:" + metalv1alpha1.ConditionFirmwareConverged,
			"firmwareProposed", "phase", "blockedOn",
		}},
		"pk-metal-host": {never: []string{
			"condition:" + metalv1alpha1.ConditionBiosConverged,
			"condition:" + metalv1alpha1.ConditionFirmwareConverged,
			"firmwareProposed",
		}},
	}
	endpointOwners = map[string]ownerRule{
		"pk-metal-endpoint-convergence": {owns: []string{
			"condition:" + metalv1alpha1.ConditionConfigured,
			"identityPushed", "phase", "blockedOn",
		}},
		"pk-metal-protection": {owns: []string{
			"condition:" + metalv1alpha1.ConditionDeletionHeld,
		}},
		"pk-metal-endpoint": {never: []string{
			"condition:" + metalv1alpha1.ConditionConfigured,
			"condition:" + metalv1alpha1.ConditionDeletionHeld,
			"identityPushed",
		}},
	}
)

func violations(ctx context.Context, cl client.Client) ([]string, error) {
	var out []string
	add := func(kind, name, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s %s: %s", kind, name, fmt.Sprintf(format, args...)))
	}
	checkGen := func(kind string, obj metav1.Object, conds []metav1.Condition) {
		for _, c := range conds {
			if c.ObservedGeneration > obj.GetGeneration() {
				add(kind, obj.GetName(), "condition %s observedGeneration %d > generation %d",
					c.Type, c.ObservedGeneration, obj.GetGeneration())
			}
		}
	}
	checkOwners := func(kind string, obj metav1.Object, rules map[string]ownerRule) error {
		for _, mf := range obj.GetManagedFields() {
			rule, ok := rules[mf.Manager]
			if !ok || mf.FieldsV1 == nil {
				continue
			}
			owned, err := statusFieldsOf(mf.FieldsV1.GetRawBytes())
			if err != nil {
				return fmt.Errorf("%s %s: managed fields of %s: %w", kind, obj.GetName(), mf.Manager, err)
			}
			for _, f := range owned {
				if len(rule.owns) > 0 && !slices.Contains(rule.owns, f) {
					add(kind, obj.GetName(), "%s owns %s, which is not its field", mf.Manager, f)
				}
				if slices.Contains(rule.never, f) {
					add(kind, obj.GetName(), "%s owns %s, which another writer maintains", mf.Manager, f)
				}
			}
		}
		return nil
	}

	eps := &metalv1alpha1.ManagementEndpointList{}
	if err := cl.List(ctx, eps); err != nil {
		return nil, err
	}
	for i := range eps.Items {
		ep := &eps.Items[i]
		if len(ep.Status.Conditions) > 0 || ep.Status.Phase != "" {
			if p, b := phase.ForEndpoint(ep.Status.Conditions); p != ep.Status.Phase || b != ep.Status.BlockedOn {
				add("endpoint", ep.Name, "phase %q/%q, conditions say %q/%q", ep.Status.Phase, ep.Status.BlockedOn, p, b)
			}
		}
		checkGen("endpoint", ep, ep.Status.Conditions)
		if err := checkOwners("endpoint", ep, endpointOwners); err != nil {
			return nil, err
		}
	}

	hosts := &metalv1alpha1.HostList{}
	if err := cl.List(ctx, hosts); err != nil {
		return nil, err
	}
	for i := range hosts.Items {
		h := &hosts.Items[i]
		if h.Status.Phase != "" {
			facts, err := hostFactsOf(ctx, cl, h)
			if err != nil {
				return nil, err
			}
			if p, b := phase.ForHost(h.Status.Conditions, facts); p != h.Status.Phase || b != h.Status.BlockedOn {
				add("host", h.Name, "phase %q/%q, conditions and %+v say %q/%q", h.Status.Phase, h.Status.BlockedOn, facts, p, b)
			}
		}
		checkGen("host", h, h.Status.Conditions)
		if err := checkOwners("host", h, hostOwners); err != nil {
			return nil, err
		}
	}

	claims := &metalv1alpha1.HostClaimList{}
	if err := cl.List(ctx, claims); err != nil {
		return nil, err
	}
	for i := range claims.Items {
		c := &claims.Items[i]
		if c.Status.Phase != "" {
			if p, b := phase.ForClaim(c.Status.Conditions); p != c.Status.Phase || b != c.Status.BlockedOn {
				add("claim", c.Namespace+"/"+c.Name, "phase %q/%q, conditions say %q/%q", c.Status.Phase, c.Status.BlockedOn, p, b)
			}
		}
		checkGen("claim", c, c.Status.Conditions)
	}

	ops := &metalv1alpha1.HostOperationList{}
	if err := cl.List(ctx, ops); err != nil {
		return nil, err
	}
	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Status.Phase != "" {
			if p, b := phase.ForOperation(op.Status.Conditions); p != op.Status.Phase || b != op.Status.BlockedOn {
				add("operation", op.Namespace+"/"+op.Name, "phase %q/%q, conditions say %q/%q", op.Status.Phase, op.Status.BlockedOn, p, b)
			}
		}
		checkGen("operation", op, op.Status.Conditions)
	}
	return out, nil
}

// hostFactsOf recomputes the spec-derived host facts the way the rule states
// them, independent of the reconcilers' code: claimed from spec.claimRef,
// releasing from the deletion of the claim it names (same UID), maintenance
// from the annotation.
func hostFactsOf(ctx context.Context, cl client.Client, h *metalv1alpha1.Host) (phase.HostFacts, error) {
	_, maintenance := h.Annotations[metalv1alpha1.AnnotationMaintenance]
	f := phase.HostFacts{Claimed: h.Spec.ClaimRef != nil, Maintenance: maintenance}
	if ref := h.Spec.ClaimRef; ref != nil {
		c := &metalv1alpha1.HostClaim{}
		err := cl.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, c)
		switch {
		case err == nil:
			f.Releasing = c.UID == ref.UID && !c.DeletionTimestamp.IsZero()
		case client.IgnoreNotFound(err) != nil:
			return f, err
		}
	}
	return f, nil
}

// statusFieldsOf lists the status fields one managedFields entry owns:
// "condition:<type>" for each condition, the field name for anything else.
func statusFieldsOf(raw []byte) ([]string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	statusRaw, ok := fields["f:status"]
	if !ok {
		return nil, nil
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		return nil, err
	}
	var out []string
	for k, v := range status {
		if k == "." {
			continue
		}
		name := strings.TrimPrefix(k, "f:")
		if name != "conditions" {
			out = append(out, name)
			continue
		}
		var conds map[string]json.RawMessage
		if err := json.Unmarshal(v, &conds); err != nil {
			return nil, err
		}
		for ck := range conds {
			key, ok := strings.CutPrefix(ck, "k:")
			if !ok {
				continue
			}
			var id struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(key), &id); err != nil {
				return nil, err
			}
			out = append(out, "condition:"+id.Type)
		}
	}
	slices.Sort(out)
	return out, nil
}

func TestStatusFieldsOf(t *testing.T) {
	raw := []byte(`{"f:status":{".":{},"f:blockedOn":{},"f:conditions":{".":{},` +
		`"k:{\"type\":\"BiosConverged\"}":{".":{},"f:status":{}},"k:{\"type\":\"FirmwareConverged\"}":{}},` +
		`"f:firmwareProposed":{".":{},"f:planHash":{}},"f:phase":{}}}`)
	got, err := statusFieldsOf(raw)
	require.NoError(t, err)
	assert.Equal(t, []string{"blockedOn", "condition:BiosConverged", "condition:FirmwareConverged", "firmwareProposed", "phase"}, got)

	got, err = statusFieldsOf([]byte(`{"f:metadata":{"f:labels":{}}}`))
	require.NoError(t, err)
	assert.Empty(t, got, "an entry that owns no status")
}
