package controller

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The pure half of the convergence lane: profile selection, the layered merge,
// schema validation, and plan hashing. Everything here is a function of its
// inputs — the reconciler supplies the cluster and BMC observations.

// selectorMatches evaluates a profile's match against a host's labels. An
// empty selector matches everything (the k8s convention — a fleet-wide
// profile); a malformed selector matches nothing and surfaces as an error.
func selectorMatches(sel metav1.LabelSelector, lbls map[string]string) (bool, error) {
	s, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return false, err
	}
	return s.Matches(labels.Set(lbls)), nil
}

// mergeBios renders matched profiles into one desired attribute map. Merge
// order is (layer, name); a later layer deliberately overrides an earlier one,
// same-name determinism comes from the name sort. Two profiles on the SAME
// layer setting the same key to different values are a conflict — conflicts
// hold the whole materialization, so the caller must not apply attrs when any
// are returned. Provenance names the winning profile per key.
func mergeBios(profiles []metalv1alpha1.BiosProfile) (attrs map[string]apiextensionsv1.JSON, provenance map[string]string, conflicts []string) {
	slices.SortFunc(profiles, func(a, b metalv1alpha1.BiosProfile) int {
		if c := int(a.Spec.Layer) - int(b.Spec.Layer); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	attrs = map[string]apiextensionsv1.JSON{}
	provenance = map[string]string{}
	layer := map[string]int32{}
	for _, p := range profiles {
		for k, v := range p.Spec.Attributes {
			if prev, ok := attrs[k]; ok && layer[k] == p.Spec.Layer && !jsonEqual(jsonAny(prev), jsonAny(v)) {
				conflicts = append(conflicts,
					fmt.Sprintf("%s: profiles %s and %s disagree on layer %d", k, provenance[k], p.Name, p.Spec.Layer))
				continue
			}
			attrs[k] = v
			provenance[k] = p.Name
			layer[k] = p.Spec.Layer
		}
	}
	sort.Strings(conflicts)
	return attrs, provenance, conflicts
}

// mergeFirmware resolves matched profiles to one firmware target, same
// layering rules — the target overrides whole, not per field.
func mergeFirmware(profiles []metalv1alpha1.FirmwareProfile) (target metalv1alpha1.FirmwareTarget, conflict string) {
	slices.SortFunc(profiles, func(a, b metalv1alpha1.FirmwareProfile) int {
		if c := int(a.Spec.Layer) - int(b.Spec.Layer); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	winner := ""
	var layer int32
	for _, p := range profiles {
		if p.Spec.Manager == nil {
			continue // a system-firmware-only profile says nothing about the manager
		}
		if winner != "" && layer == p.Spec.Layer && *p.Spec.Manager != target {
			return target, fmt.Sprintf("profiles %s and %s disagree on layer %d", winner, p.Name, p.Spec.Layer)
		}
		target = *p.Spec.Manager
		winner = p.Name
		layer = p.Spec.Layer
	}
	return target, ""
}

// validateBios checks desired values against the discovered attribute
// registry: unknown attributes would be rejected at PATCH, out-of-enum values
// silently dropped at apply — both must fail loudly here instead, before
// anything is proposed. A nil schema (endpoint serves no registry) validates
// nothing: absence is data, convergence then proves itself only by the
// verify-after-reboot read.
func validateBios(attrs map[string]apiextensionsv1.JSON, schema []bmc.BiosAttribute) []string {
	if schema == nil {
		return nil
	}
	byName := map[string]bmc.BiosAttribute{}
	for _, a := range schema {
		byName[a.Name] = a
	}
	var violations []string
	for k, j := range attrs {
		a, ok := byName[k]
		if !ok {
			violations = append(violations, k+": not in the attribute registry")
			continue
		}
		v := jsonAny(j)
		switch a.Type {
		case "Enumeration":
			s, ok := v.(string)
			if !ok || !slices.Contains(a.AllowedValues, s) {
				violations = append(violations,
					fmt.Sprintf("%s: %v not among allowed values %v", k, v, a.AllowedValues))
			}
		case "Boolean":
			if _, ok := v.(bool); !ok {
				violations = append(violations, fmt.Sprintf("%s: %v is not a boolean", k, v))
			}
		case "Integer":
			f, ok := v.(float64)
			switch {
			case !ok || math.Trunc(f) != f:
				violations = append(violations, fmt.Sprintf("%s: %v is not an integer", k, v))
			case math.Abs(f) > maxExactInteger:
				// Values travel as float64 (the seam's JSON typing). From 2^53 on
				// the decode rounds, so the operator would stage, and compare
				// against, a number nobody wrote.
				violations = append(violations,
					fmt.Sprintf("%s: %s is beyond ±(2^53-1), the largest integer staged exactly", k, j.Raw))
			}
		case "String":
			if _, ok := v.(string); !ok {
				violations = append(violations, fmt.Sprintf("%s: %v is not a string", k, v))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// maxExactInteger is 2^53-1, the largest integer that a float64 holds
// exactly with no other integer rounding to it (2^53+1 decodes to 2^53).
const maxExactInteger = 1<<53 - 1

// planHash hashes a desired state canonically (JSON with sorted map keys) —
// the identity an operation proposal is bound to.
func planHash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Desired states are decoded JSON values and API structs — both always
		// marshal; a failure here is a programming error worth a loud hash.
		return "sha256:unhashable"
	}
	return digest.FromBytes(raw).String()
}

// hash8 is the operation-name suffix for a plan hash.
func hash8(planHash string) string {
	return digest.Digest(planHash).Encoded()[:8]
}

// jsonAny decodes an arbitrary-JSON API value to its Go JSON type (string,
// bool, float64) — the same typing the seam serves, so the two compare.
func jsonAny(j apiextensionsv1.JSON) any {
	var v any
	_ = json.Unmarshal(j.Raw, &v)
	return v
}

// jsonOf re-encodes a seam value for storage in an API JSON field.
func jsonOf(v any) apiextensionsv1.JSON {
	raw, _ := json.Marshal(v)
	return apiextensionsv1.JSON{Raw: raw}
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// decodeAttrs maps an API attribute map to seam typing.
func decodeAttrs(attrs map[string]apiextensionsv1.JSON) map[string]any {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		out[k] = jsonAny(v)
	}
	return out
}

// subsetJSON projects a machine-served map onto the managed keys, re-encoded
// for status — the observed/pending caches carry only what a profile manages.
func subsetJSON(state map[string]any, desired map[string]any) map[string]apiextensionsv1.JSON {
	out := map[string]apiextensionsv1.JSON{}
	for k := range desired {
		if v, ok := state[k]; ok {
			out[k] = jsonOf(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
