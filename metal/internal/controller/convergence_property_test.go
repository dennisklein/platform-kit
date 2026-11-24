package controller

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// Property tests for the pure half of the convergence lane. Each property runs
// over a few hundred generated inputs from a fixed seed, so a failure is
// reproducible: the message names the seed and the case. The generators draw
// from small pools on purpose, so that keys, layers and values collide often;
// collisions are where merge and conflict rules matter.

const propertyCases = 500

// forAll runs f over propertyCases generated cases, each with its own seeded
// source, and names the case on failure.
func forAll(t *testing.T, f func(t *testing.T, r *rand.Rand)) {
	t.Helper()
	for i := range propertyCases {
		r := rand.New(rand.NewPCG(1, uint64(i)))
		if !t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) { f(t, r) }) {
			return // one failing case is enough to read
		}
	}
}

var (
	attrKeys   = []string{"BootModeSelect", "QuietBoot", "SriovSupport", "ErrorThreshold"}
	attrValues = []any{"UEFI", "Legacy", true, false, float64(0), float64(512)}
	layers     = []int32{0, 10, 20}
)

func genBiosProfiles(r *rand.Rand) []metalv1alpha1.BiosProfile {
	n := r.IntN(6)
	out := make([]metalv1alpha1.BiosProfile, 0, n)
	for i := range n {
		attrs := map[string]apiextensionsv1.JSON{}
		for range r.IntN(len(attrKeys) + 1) {
			attrs[attrKeys[r.IntN(len(attrKeys))]] = jsonOf(attrValues[r.IntN(len(attrValues))])
		}
		out = append(out, biosProfile(fmt.Sprintf("p%d", i), layers[r.IntN(len(layers))], attrs))
	}
	return out
}

func shuffled[T any](r *rand.Rand, in []T) []T {
	out := slices.Clone(in)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func TestMergeBiosProperties(t *testing.T) {
	t.Run("the result does not depend on the order of the profiles", func(t *testing.T) {
		forAll(t, func(t *testing.T, r *rand.Rand) {
			profiles := genBiosProfiles(r)
			a, pa, ca := mergeBios(slices.Clone(profiles))
			b, pb, cb := mergeBios(shuffled(r, profiles))
			assert.Equal(t, decodeAttrs(a), decodeAttrs(b))
			assert.Equal(t, pa, pb)
			assert.Equal(t, ca, cb)
		})
	})

	t.Run("a same-layer disagreement is a conflict, and nothing else is", func(t *testing.T) {
		forAll(t, func(t *testing.T, r *rand.Rand) {
			profiles := genBiosProfiles(r)
			// The oracle: per (layer, key), the set of distinct values.
			type lk struct {
				layer int32
				key   string
			}
			values := map[lk]map[string]bool{}
			for _, p := range profiles {
				for k, v := range p.Spec.Attributes {
					id := lk{p.Spec.Layer, k}
					if values[id] == nil {
						values[id] = map[string]bool{}
					}
					values[id][fmt.Sprint(jsonAny(v))] = true
				}
			}
			_, _, conflicts := mergeBios(shuffled(r, profiles))
			for id, vs := range values {
				prefix := fmt.Sprintf("%s: ", id.key)
				suffix := fmt.Sprintf(" on layer %d", id.layer)
				found := slices.ContainsFunc(conflicts, func(c string) bool {
					return strings.HasPrefix(c, prefix) && strings.HasSuffix(c, suffix)
				})
				assert.Equal(t, len(vs) > 1, found, "key %s on layer %d has values %v; conflicts %v",
					id.key, id.layer, slices.Sorted(maps.Keys(vs)), conflicts)
			}
		})
	})

	t.Run("without a conflict the highest layer wins and provenance names it", func(t *testing.T) {
		forAll(t, func(t *testing.T, r *rand.Rand) {
			profiles := genBiosProfiles(r)
			attrs, prov, conflicts := mergeBios(shuffled(r, profiles))
			if len(conflicts) > 0 {
				return
			}
			want := map[string]any{}
			top := map[string]int32{}
			for _, p := range profiles {
				for k, v := range p.Spec.Attributes {
					if l, ok := top[k]; !ok || p.Spec.Layer > l {
						top[k], want[k] = p.Spec.Layer, jsonAny(v)
					}
				}
			}
			assert.Equal(t, want, decodeAttrs(attrs))
			for k, name := range prov {
				i := slices.IndexFunc(profiles, func(p metalv1alpha1.BiosProfile) bool { return p.Name == name })
				require.GreaterOrEqual(t, i, 0)
				assert.Equal(t, top[k], profiles[i].Spec.Layer, "provenance of %s names a top-layer profile", k)
				assert.Equal(t, want[k], jsonAny(profiles[i].Spec.Attributes[k]))
			}
			assert.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(prov)))
		})
	})
}

func TestMergeFirmwareProperties(t *testing.T) {
	targets := []metalv1alpha1.FirmwareTarget{
		{Version: "01.05.25", Image: "r/f@sha256:aa"},
		{Version: "01.07.09", Image: "r/f@sha256:bb"},
	}
	gen := func(r *rand.Rand) []metalv1alpha1.FirmwareProfile {
		var out []metalv1alpha1.FirmwareProfile
		for i := range r.IntN(5) {
			out = append(out, metalv1alpha1.FirmwareProfile{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i)},
				Spec: metalv1alpha1.FirmwareProfileSpec{
					Layer: layers[r.IntN(len(layers))], Manager: &targets[r.IntN(len(targets))]},
			})
		}
		return out
	}
	forAll(t, func(t *testing.T, r *rand.Rand) {
		profiles := gen(r)
		target, conflict := mergeFirmware(slices.Clone(profiles))
		target2, conflict2 := mergeFirmware(shuffled(r, profiles))
		assert.Equal(t, target, target2, "order of the profiles does not matter")
		assert.Equal(t, conflict, conflict2)

		// The oracle: a conflict exactly when some layer holds two targets;
		// otherwise the highest layer's target.
		perLayer := map[int32]map[metalv1alpha1.FirmwareTarget]bool{}
		var top *metalv1alpha1.FirmwareProfile
		for i, p := range profiles {
			if perLayer[p.Spec.Layer] == nil {
				perLayer[p.Spec.Layer] = map[metalv1alpha1.FirmwareTarget]bool{}
			}
			perLayer[p.Spec.Layer][*p.Spec.Manager] = true
			if top == nil || p.Spec.Layer > top.Spec.Layer {
				top = &profiles[i]
			}
		}
		split := slices.ContainsFunc(slices.Collect(maps.Values(perLayer)),
			func(m map[metalv1alpha1.FirmwareTarget]bool) bool { return len(m) > 1 })
		assert.Equal(t, split, conflict != "", "conflict %q", conflict)
		if !split && top != nil {
			assert.Equal(t, *top.Spec.Manager, target)
		}
	})
}

func TestPlanHashProperties(t *testing.T) {
	// genValue builds a nested JSON value; maps are filled in a random key
	// order, so two calls with the same content differ only in insertion order.
	var genValue func(r *rand.Rand, depth int) any
	genValue = func(r *rand.Rand, depth int) any {
		if depth == 0 || r.IntN(3) == 0 {
			return attrValues[r.IntN(len(attrValues))]
		}
		if r.IntN(2) == 0 {
			l := make([]any, r.IntN(3))
			for i := range l {
				l[i] = genValue(r, depth-1)
			}
			return l
		}
		m := map[string]any{}
		for _, k := range attrKeys[:r.IntN(len(attrKeys)+1)] {
			m[k] = genValue(r, depth-1)
		}
		return m
	}
	// rebuild copies v, inserting map keys in a shuffled order.
	var rebuild func(r *rand.Rand, v any) any
	rebuild = func(r *rand.Rand, v any) any {
		switch v := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for _, k := range shuffled(r, slices.Collect(maps.Keys(v))) {
				out[k] = rebuild(r, v[k])
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i := range v {
				out[i] = rebuild(r, v[i])
			}
			return out
		}
		return v
	}
	forAll(t, func(t *testing.T, r *rand.Rand) {
		v := genValue(r, 3)
		h := planHash(v)
		assert.Equal(t, h, planHash(rebuild(r, v)), "insertion order does not change the hash")
		assert.Equal(t, h, planHash(decodeAttrs(map[string]apiextensionsv1.JSON{"x": jsonOf(v)})["x"]),
			"a round trip through the API's JSON field does not change the hash")
		assert.NotEqual(t, h, planHash([]any{v}), "a different value hashes differently")
	})
}

func TestSubsetJSONProperties(t *testing.T) {
	gen := func(r *rand.Rand) map[string]any {
		m := map[string]any{}
		for _, k := range attrKeys {
			if r.IntN(2) == 0 {
				m[k] = attrValues[r.IntN(len(attrValues))]
			}
		}
		return m
	}
	forAll(t, func(t *testing.T, r *rand.Rand) {
		state, desired := gen(r), gen(r)
		got := subsetJSON(state, desired)
		var want []string
		for k := range desired {
			if _, ok := state[k]; ok {
				want = append(want, k)
			}
		}
		// The projection holds exactly the managed keys the machine serves,
		// each with the machine's value, and is nil when there are none.
		if len(want) == 0 {
			assert.Nil(t, got)
			return
		}
		assert.ElementsMatch(t, want, slices.Collect(maps.Keys(got)))
		for k, v := range got {
			assert.Equal(t, state[k], jsonAny(v), k)
		}
	})
}

func TestValidateBiosProperties(t *testing.T) {
	schema := []bmc.BiosAttribute{
		{Name: "BootModeSelect", Type: "Enumeration", AllowedValues: []string{"Legacy", "UEFI", "Dual"}},
		{Name: "QuietBoot", Type: "Boolean"},
		{Name: "ErrorThreshold", Type: "Integer"},
		{Name: "AssetTag", Type: "String"},
	}
	good := map[string][]any{
		"BootModeSelect": {"Legacy", "UEFI", "Dual"},
		"QuietBoot":      {true, false},
		"ErrorThreshold": {float64(0), float64(-1), float64(512), float64(1 << 40)},
		"AssetTag":       {"", "rack-7", "UEFI"},
	}
	bad := map[string][]any{
		"BootModeSelect": {"Insecure", true, float64(1)},
		"QuietBoot":      {"yes", float64(1), nil},
		"ErrorThreshold": {1.5, "512", true, float64(1<<53 + 2)},
		"AssetTag":       {float64(7), false, nil},
	}
	forAll(t, func(t *testing.T, r *rand.Rand) {
		attrs := map[string]apiextensionsv1.JSON{}
		var wantBad []string
		for _, a := range schema {
			switch r.IntN(3) {
			case 0: // not managed
			case 1:
				attrs[a.Name] = jsonOf(good[a.Name][r.IntN(len(good[a.Name]))])
			case 2:
				attrs[a.Name] = jsonOf(bad[a.Name][r.IntN(len(bad[a.Name]))])
				wantBad = append(wantBad, a.Name)
			}
		}
		if r.IntN(4) == 0 {
			attrs["NoSuchKnob"] = jsonOf("x")
			wantBad = append(wantBad, "NoSuchKnob")
		}
		got := validateBios(attrs, schema)
		named := make([]string, 0, len(got))
		for _, v := range got {
			named = append(named, v[:strings.Index(v, ":")])
		}
		assert.ElementsMatch(t, wantBad, named, "violations %v", got)
		assert.True(t, slices.IsSorted(got))

		if len(wantBad) > 0 {
			return
		}
		// What is accepted is what is staged: the executor stages the decoded
		// value. Re-encoding it must give back an accepted, equal value, or the
		// machine would get something nobody approved.
		staged := decodeAttrs(attrs)
		again := map[string]apiextensionsv1.JSON{}
		for k, v := range staged {
			again[k] = jsonOf(v)
			assert.JSONEq(t, string(attrs[k].Raw), string(again[k].Raw), k)
		}
		assert.Empty(t, validateBios(again, schema))
	})
}
