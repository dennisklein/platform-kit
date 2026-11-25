package render

import (
	"errors"
	"fmt"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// ErrPatchUnresolvable marks a patch whose ConfigMap or key does not exist. The
// controller parks the node on PatchesUnresolvable for it, and the ConfigMap
// watch re-renders once it appears.
var ErrPatchUnresolvable = errors.New("patch unresolvable")

// PatchConfigMapLabeled refuses a patch ConfigMap without the label
// os.pk.io/patch. Admission records who changed a labeled one
// (metal.pk.io/requested-by); a patch from any other ConfigMap would change
// the machines with nobody named.
func PatchConfigMapLabeled(labels map[string]string, ref osv1alpha1.ConfigMapKeyReference) error {
	if _, ok := labels[osv1alpha1.LabelPatch]; ok {
		return nil
	}
	return fmt.Errorf("%w: ConfigMap %s/%s has no label %s; add it: kubectl -n %s label configmap %s %s=true",
		ErrPatchUnresolvable, ref.Namespace, ref.Name, osv1alpha1.LabelPatch, ref.Namespace, ref.Name, osv1alpha1.LabelPatch)
}

// ConfigMapReader reads one ConfigMap key. It returns an error wrapping
// ErrPatchUnresolvable when the ConfigMap or the key is missing; any other
// error is a transport failure.
type ConfigMapReader func(ref osv1alpha1.ConfigMapKeyReference) (string, error)

// ResolvePatches turns patch references into patch texts, in order.
func ResolvePatches(patches []osv1alpha1.Patch, read ConfigMapReader) ([][]byte, error) {
	out := make([][]byte, 0, len(patches))
	for i, p := range patches {
		switch {
		case p.ConfigMapRef != nil && p.Inline != "":
			return nil, fmt.Errorf("patch %d sets both inline and configMapRef", i)
		case p.ConfigMapRef != nil:
			if read == nil {
				return nil, fmt.Errorf("patch %d: %w: no ConfigMap source", i, ErrPatchUnresolvable)
			}
			text, err := read(*p.ConfigMapRef)
			if err != nil {
				return nil, fmt.Errorf("patch %d: %w", i, err)
			}
			out = append(out, []byte(text))
		default:
			out = append(out, []byte(p.Inline))
		}
	}
	return out, nil
}
