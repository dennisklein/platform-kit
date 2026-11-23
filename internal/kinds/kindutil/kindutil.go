// Package kindutil has helpers for kind packages.
package kindutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/schema"
)

const yamlScalar = yaml.ScalarNode

// Version is one version of a kind with its Go type.
type Version struct {
	Name string // v1beta1
	Type any    // zero value of the Go type
}

// SchemaFunc returns a Kind.Schema function for the versions (oldest first).
// Schemas are generated once and cached.
func SchemaFunc(group, kind, description string, versions ...Version) func(string) (json.RawMessage, error) {
	var (
		once  sync.Once
		cache map[string]json.RawMessage
		err   error
	)
	newest := versions[len(versions)-1].Name
	return func(v string) (json.RawMessage, error) {
		once.Do(func() {
			cache = map[string]json.RawMessage{}
			for _, ver := range versions {
				apiVersion := ver.Name
				if group != "" {
					apiVersion = group + "/" + ver.Name
				}
				var b json.RawMessage
				b, err = schema.JSON(ver.Type, schema.Options{
					Title: kind, Description: description, APIVersion: apiVersion, Kind: kind,
				})
				if err != nil {
					return
				}
				cache[ver.Name] = b
			}
		})
		if err != nil {
			return nil, err
		}
		if v == "" {
			v = newest
		}
		b, ok := cache[v]
		if !ok {
			return nil, fmt.Errorf("%s: unknown version %q", kind, v)
		}
		return b, nil
	}
}

// Names returns the version names.
func Names(versions ...Version) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Name
	}
	return out
}

// Pos returns the line and column of a field (dotted path with indexes) in
// a document; the document line when the field does not exist.
func Pos(d *kinds.Doc, field string) (line, col int) {
	if d == nil {
		return 0, 0
	}
	line, col = d.Line, 1
	if d.Node == nil || field == "" {
		return line, col
	}
	loc, err := schema.ParseFieldPath(field)
	if err != nil {
		return line, col
	}
	v, k, _ := schema.Lookup(d.Node, loc)
	n := v
	if k != nil && (v == nil || v.Kind != yamlScalar) {
		n = k
	}
	if n != nil && n.Line > 0 {
		return n.Line + d.LineOffset(), n.Column
	}
	return line, col
}

// Problem returns a kind problem at a field of a document.
func Problem(d *kinds.Doc, field string, sev kinds.Severity, code, format string, args ...any) kinds.Problem {
	line, col := Pos(d, field)
	p := kinds.Problem{Line: line, Col: col, Severity: sev, Message: fmt.Sprintf(format, args...), Source: kinds.SourceKind, Field: field, Code: code}
	if d != nil {
		p.Path = d.Path
	}
	return p
}

// RefProblem is Problem with source reference.
func RefProblem(d *kinds.Doc, field string, sev kinds.Severity, format string, args ...any) kinds.Problem {
	p := Problem(d, field, sev, "ref", format, args...)
	p.Source = kinds.SourceReference
	return p
}

// Str returns a string at a path of a decoded document (empty when missing).
func Str(data map[string]any, path ...string) string {
	v := Get(data, path...)
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// Int returns an int at a path of a decoded document (0 when missing).
func Int(data map[string]any, path ...string) int {
	switch x := Get(data, path...).(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// Get returns the value at a path of nested maps.
func Get(data map[string]any, path ...string) any {
	var cur any = data
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// Strings returns a list of strings at a path.
func Strings(data map[string]any, path ...string) []string {
	l, _ := Get(data, path...).([]any)
	out := make([]string, 0, len(l))
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		} else if v != nil {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

// EntityKinds are the kinds whose objects are entities (things people link
// to from articles and work items).
var EntityKinds = []string{"Node", "Rack", "Switch", "Jbod", "Cluster", "CablePlan"}

// FindEntity finds an entity by name across the entity kinds.
func FindEntity(r kinds.Repo, name string) *kinds.Doc {
	for _, k := range EntityKinds {
		if d := r.Find(k, name); d != nil {
			return d
		}
	}
	return nil
}

// FieldLines describes changed scalar fields between two decoded documents,
// for kinds without their own words: "spec.ports: 48 to 64". At most limit
// lines; the rest is counted in a last line.
func FieldLines(old, new map[string]any, limit int) []string {
	var changes []string
	diffValues("", old, new, &changes)
	if limit > 0 && len(changes) > limit {
		rest := len(changes) - limit
		changes = append(changes[:limit], fmt.Sprintf("%d more fields changed", rest))
	}
	return changes
}

func diffValues(prefix string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sortStrings(sorted)
		for _, k := range sorted {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			diffValues(p, am[k], bm[k], out)
		}
		return
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok && len(as) == len(bs) {
		for i := range as {
			diffValues(fmt.Sprintf("%s[%d]", prefix, i), as[i], bs[i], out)
		}
		return
	}
	if fmt.Sprint(a) == fmt.Sprint(b) {
		return
	}
	switch {
	case a == nil:
		*out = append(*out, fmt.Sprintf("%s set to %s", prefix, short(b)))
	case b == nil:
		*out = append(*out, fmt.Sprintf("%s removed", prefix))
	default:
		*out = append(*out, fmt.Sprintf("%s %s to %s", prefix, short(a), short(b)))
	}
}

func short(v any) string {
	switch x := v.(type) {
	case map[string]any:
		return fmt.Sprintf("{%d fields}", len(x))
	case []any:
		// Short lists of scalars read best inline: [intel_iommu=on, iommu=pt].
		parts := make([]string, 0, len(x))
		n := 0
		for _, y := range x {
			switch y.(type) {
			case map[string]any, []any:
				return fmt.Sprintf("[%d items]", len(x))
			}
			t := short(y)
			n += len(t) + 2
			parts = append(parts, t)
		}
		if len(x) > 0 && n <= 60 {
			return "[" + strings.Join(parts, ", ") + "]"
		}
		return fmt.Sprintf("[%d items]", len(x))
	case string:
		if schema.IsSOPS(x) {
			return schema.MaskText
		}
		if len(x) > 40 {
			return x[:37] + "..."
		}
		return x
	}
	return fmt.Sprint(v)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Describer returns a Describe hook that uses FieldLines with the subject
// name of the document: added, removed, or one line per changed field.
func Describer(kindName, what string) func(old, new *kinds.Doc) []kinds.SemanticLine {
	return func(old, new *kinds.Doc) []kinds.SemanticLine {
		switch {
		case old == nil && new == nil:
			return nil
		case old == nil:
			return []kinds.SemanticLine{{Subject: Subject(new), Text: "new " + what, Op: kinds.OpAdd, Path: new.Path, Line: new.Line, Kind: kindName}}
		case new == nil:
			return []kinds.SemanticLine{{Subject: Subject(old), Text: what + " removed", Op: kinds.OpDelete, Path: old.Path, Kind: kindName}}
		}
		var out []kinds.SemanticLine
		for _, t := range FieldLines(old.Data, new.Data, 5) {
			out = append(out, kinds.SemanticLine{Subject: Subject(new), Text: t, Op: kinds.OpChange, Path: new.Path, Line: new.Line, Kind: kindName})
		}
		return out
	}
}

// Subject is the short name of a document for semantic lines.
func Subject(d *kinds.Doc) string {
	if d.Name != "" {
		return d.Name
	}
	return d.Path
}
