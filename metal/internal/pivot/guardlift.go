package pivot

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// A guard lift ("<username>: <reason>") may be set only by the person it
// names (config/policy/guard-lift.yaml). The move creates an object as an
// admin or as its proposer, and brings its annotations up to the source's
// as the person who runs the move; neither may write another person's
// lift. So the lifts are taken out of what the move writes, and each one is
// set again afterwards, as the person it names, as the approvals are.

// takeLifts removes the guard lifts from obj's annotations and returns them.
func takeLifts(obj *unstructured.Unstructured) map[string]string {
	ann := obj.GetAnnotations()
	lifts := map[string]string{}
	for _, key := range metalv1alpha1.NamedGuardLifts {
		if v, ok := ann[key]; ok {
			lifts[key] = v
			delete(ann, key)
		}
	}
	if len(lifts) > 0 {
		obj.SetAnnotations(ann)
	}
	return lifts
}

// keepLifts copies cur's guard lifts into want, so that bringing cur's
// annotations up to want's neither writes a lift nor removes one the
// source still has. A lift the source no longer has stays out of want and
// is removed: removing a lift needs no name.
func keepLifts(cur, want *unstructured.Unstructured, lifts map[string]string) {
	ann := want.GetAnnotations()
	for key := range lifts {
		if v, ok := cur.GetAnnotations()[key]; ok {
			if ann == nil {
				ann = map[string]string{}
			}
			ann[key] = v
		}
	}
	want.SetAnnotations(ann)
}

// replayLifts sets on cur each lift of the source that cur does not carry
// yet, as the person it names. A value that names nobody (set before
// admission asked for a name) cannot be set again as anyone; it is left
// behind, which puts the guard back on the target, and the move says so.
func (m *Mover) replayLifts(ctx context.Context, so sourceObject, cur *unstructured.Unstructured, lifts map[string]string) error {
	for _, key := range metalv1alpha1.NamedGuardLifts {
		v, ok := lifts[key]
		if !ok || cur.GetAnnotations()[key] == v {
			continue
		}
		ref := so.kind.gvk.Kind + " " + objectRef(cur)
		user, _, named := metalv1alpha1.ParseGuardLift(v)
		if !named {
			m.logf("left %s off %s on %s: its value %q names nobody, so the guard holds there again; "+
				"set it again as \"<you>: <reason>\" if it is still needed", key, ref, m.Target.Name, v)
			continue
		}
		as, err := m.Target.as(user, m.adminGroups(ctx)...)
		if err != nil {
			return err
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: v}}})
		if err != nil {
			return err
		}
		if err := as.Patch(ctx, cur, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("set %s on %s again as %s, who lifted it: %w", key, ref, user, err)
		}
		m.Options.Audit.GuardLiftReplayed(auditObject(cur), key, v)
	}
	return nil
}
