package pivot

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// movedGroups are the API groups whose every kind is the operator's state.
// A kind added to either group moves without an edit here: the source's
// discovery names it (discoverKinds), and the copy carries it after the
// kinds movedKinds orders by hand.
var movedGroups = []string{metalGV.Group, osGV.Group}

// discoverKinds is the copy list for one move: movedKinds in their order,
// then every other kind the source serves in movedGroups, by name. A kind
// the copy does not know by name needs nothing it would not do anyway:
// owner references are rewritten in the second pass, statuses are copied
// last. A source without a config (a test) moves movedKinds only.
func discoverKinds(cfg *rest.Config) ([]movedKind, error) {
	kinds := slices.Clone(movedKinds)
	if cfg == nil {
		return kinds, nil
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	groups, err := dc.ServerGroups()
	if err != nil {
		return nil, fmt.Errorf("discover the API groups: %w", err)
	}
	var found []movedKind
	for _, g := range groups.Groups {
		if !slices.Contains(movedGroups, g.Name) {
			continue
		}
		gv := g.PreferredVersion.GroupVersion
		list, err := dc.ServerResourcesForGroupVersion(gv)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("discover the kinds of %s: %w", gv, err)
		}
		parsed, err := schema.ParseGroupVersion(gv)
		if err != nil {
			return nil, err
		}
		found = append(found, newKinds(kinds, parsed, list.APIResources)...)
	}
	slices.SortFunc(found, func(a, b movedKind) int { return strings.Compare(a.gvk.String(), b.gvk.String()) })
	return append(kinds, found...), nil
}

// newKinds are the listable top-level kinds of a discovery answer that known
// does not list yet. Subresources (hosts/status) are not kinds.
func newKinds(known []movedKind, gv schema.GroupVersion, resources []metav1.APIResource) []movedKind {
	var out []movedKind
	for _, r := range resources {
		if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
			continue
		}
		gvk := gv.WithKind(r.Kind)
		if slices.ContainsFunc(known, func(k movedKind) bool { return k.gvk == gvk }) ||
			slices.ContainsFunc(out, func(k movedKind) bool { return k.gvk == gvk }) {
			continue
		}
		out = append(out, movedKind{gvk: gvk, namespaced: r.Namespaced})
	}
	return out
}

// movedKinds is this move's copy list: discovered at New, or the ordered
// base list for a Mover built without New.
func (m *Mover) movedKinds() []movedKind {
	if m.kinds != nil {
		return m.kinds
	}
	return movedKinds
}

// Conditions that say an object is in the middle of work its controller
// drives step by step (a rollout between nodes). The step after the move
// would be the target's manager's, from state copied while the source's
// was still acting on it.
var inFlightConditions = []string{"Progressing", "Paused"}

// inFlight names the objects of the moved kinds whose controller is in the
// middle of a multi-step change.
func inFlight(ctx context.Context, c client.Client, kinds []movedKind) ([]string, error) {
	var out []string
	for _, k := range kinds {
		if k.core {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
		if err := c.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for i := range list.Items {
			u := &list.Items[i]
			if k.gvk == osGV.WithKind("TalosRollout") && rolloutRuns(u) {
				out = append(out, fmt.Sprintf("TalosRollout %s is approved and not complete: let it finish, or abort it "+
					"(pk-metal rollout abort %s), before moving the operator", u.GetName(), u.GetName()))
				continue
			}
			conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			for _, raw := range conds {
				cond, _ := raw.(map[string]any)
				t, _ := cond["type"].(string)
				if cond["status"] == string(metav1.ConditionTrue) && slices.Contains(inFlightConditions, t) {
					out = append(out, fmt.Sprintf("%s %s is %s: let it finish, or stop it, before moving the operator",
						k.gvk.Kind, objectRef(u), strings.ToLower(t)))
					break
				}
			}
		}
	}
	return out, nil
}

// rolloutRuns: an approved TalosRollout whose Complete condition is not
// decided yet. Its Progressing condition is not enough: a rollout waiting
// for etcd's health between nodes reports it as Unknown, and the next node
// would start under the target's manager from a plan the source's was
// running.
func rolloutRuns(u *unstructured.Unstructured) bool {
	by, _, _ := unstructured.NestedString(u.Object, "spec", "approval", "approvedBy")
	if by == "" {
		return false
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, raw := range conds {
		cond, _ := raw.(map[string]any)
		if cond["type"] == "Complete" {
			return cond["status"] == string(metav1.ConditionUnknown)
		}
	}
	return true
}
