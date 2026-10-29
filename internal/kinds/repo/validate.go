package repo

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// Problems returns all problems of the tree: YAML syntax, schema and the
// cross-file checks of the kinds. The result is computed once per Repo.
func (r *Repo) Problems() []kinds.Problem {
	r.once.Do(func() {
		var out []kinds.Problem
		for _, p := range r.paths {
			out = append(out, r.fileProblems(r.files[p])...)
		}
		out = append(out, r.duplicates()...)
		if r.reg != nil {
			for _, k := range r.reg.List() {
				if k.Validate != nil {
					out = append(out, k.Validate(r)...)
				}
			}
		}
		SortProblems(out)
		r.problems = dedupe(out)
	})
	return r.problems
}

// ProblemsOf returns the problems of one file.
func (r *Repo) ProblemsOf(p string) []kinds.Problem {
	p = path.Clean(p)
	out := []kinds.Problem{}
	for _, pr := range r.Problems() {
		if pr.Path == p {
			out = append(out, pr)
		}
	}
	return out
}

// SortProblems sorts by path, line and column.
func SortProblems(ps []kinds.Problem) {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Path != ps[j].Path {
			return ps[i].Path < ps[j].Path
		}
		if ps[i].Line != ps[j].Line {
			return ps[i].Line < ps[j].Line
		}
		return ps[i].Col < ps[j].Col
	})
}

func dedupe(ps []kinds.Problem) []kinds.Problem {
	out := ps[:0]
	seen := map[string]bool{}
	for _, p := range ps {
		k := fmt.Sprintf("%s\x00%d\x00%s", p.Path, p.Line, p.Message)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

func (r *Repo) fileProblems(f *File) []kinds.Problem {
	var out []kinds.Problem
	if f.Syntax != nil {
		out = append(out, kinds.Problem{Path: f.Path, Line: f.Syntax.Line, Col: f.Syntax.Col, Severity: kinds.SevError,
			Message: f.Syntax.Message, Source: kinds.SourceYAML, Code: "syntax"})
	}
	for _, d := range f.Docs {
		out = append(out, ValidateDoc(r.reg, d)...)
	}
	return out
}

// ValidateDoc validates one document against the schema of its kind and
// version. Documents without a kind are plain YAML and have no schema.
func ValidateDoc(reg kinds.Registry, d *kinds.Doc) []kinds.Problem {
	if reg == nil {
		return nil
	}
	k, ok := reg.Match(d.Path, d)
	if !ok || k.Schema == nil {
		return nil
	}
	var out []kinds.Problem
	version := VersionOf(k, d)
	if version == "" {
		out = append(out, kinds.Problem{Path: d.Path, Line: d.Line, Col: 1, Severity: kinds.SevError, Source: kinds.SourceSchema,
			Field: "apiVersion", Code: "version",
			Message: fmt.Sprintf("apiVersion %s: %s has no version %s (known: %s)", d.APIVersion, k.Name, d.Version(), strings.Join(k.Versions, ", "))})
		return out
	}
	v, err := validator(k, version)
	if err != nil {
		return []kinds.Problem{{Path: d.Path, Line: d.Line, Severity: kinds.SevWarning, Source: kinds.SourceSchema,
			Message: fmt.Sprintf("schema of %s %s: %v", k.Name, version, err)}}
	}
	for _, e := range v.Validate(d.Node) {
		out = append(out, kinds.Problem{Path: d.Path, Line: e.Line + d.Offset, Col: e.Col, Severity: kinds.SevError,
			Message: e.Message, Source: kinds.SourceSchema, Field: e.Field, Code: e.Keyword})
	}
	if version != k.Version && len(k.Versions) > 1 {
		out = append(out, kinds.Problem{Path: d.Path, Line: d.Line, Col: 1, Severity: kinds.SevInfo, Source: kinds.SourceSchema,
			Field: "apiVersion", Code: "old-version",
			Message: fmt.Sprintf("stored as %s; the newest version is %s", d.APIVersion, k.APIVersion())})
	}
	return out
}

// VersionOf returns the kind version of a document: its apiVersion version
// when the kind knows it, the newest for documents without apiVersion
// (articles), and "" for an unknown version.
func VersionOf(k *kinds.Kind, d *kinds.Doc) string {
	if d.APIVersion == "" {
		return k.Version
	}
	v := d.Version()
	if slices.Contains(k.Versions, v) || len(k.Versions) == 0 && v == k.Version {
		return v
	}
	return ""
}

type schemaEntry struct {
	raw json.RawMessage
	m   map[string]any
	v   *schema.Validator
	err error
}

var schemas sync.Map // kind/version -> *schemaEntry

func entry(k *kinds.Kind, version string) *schemaEntry {
	key := k.Group + "/" + k.Name + "/" + version
	if e, ok := schemas.Load(key); ok {
		return e.(*schemaEntry)
	}
	e := &schemaEntry{}
	e.raw, e.err = k.Schema(version)
	if e.err == nil {
		e.err = json.Unmarshal(e.raw, &e.m)
	}
	if e.err == nil {
		e.v, e.err = schema.Compile(e.raw)
	}
	schemas.Store(key, e)
	return e
}

func validator(k *kinds.Kind, version string) (*schema.Validator, error) {
	e := entry(k, version)
	return e.v, e.err
}

// SchemaMap returns the parsed JSON Schema of a kind version.
func SchemaMap(k *kinds.Kind, version string) map[string]any {
	if k == nil || k.Schema == nil {
		return nil
	}
	return entry(k, version).m
}

// SchemaLookup returns a schema.SchemaLookup for documents of a file.
func SchemaLookup(reg kinds.Registry, p string) schema.SchemaLookup {
	return func(n *yaml.Node) map[string]any {
		if reg == nil {
			return nil
		}
		d := newDoc(p, 0, n, 0)
		if d == nil {
			return nil
		}
		k, ok := reg.Match(p, d)
		if !ok {
			return nil
		}
		v := VersionOf(k, d)
		if v == "" {
			v = k.Version
		}
		return SchemaMap(k, v)
	}
}

// duplicates finds own-kind objects defined twice and files whose name
// does not match the object name.
func (r *Repo) duplicates() []kinds.Problem {
	var out []kinds.Problem
	seen := map[string]*kinds.Doc{}
	for _, d := range r.docs {
		k := r.KindOf(d)
		if k == nil || !k.Own || d.Name == "" {
			continue
		}
		key := k.Name + "/" + d.Name
		if prev, ok := seen[key]; ok {
			out = append(out, kinds.Problem{Path: d.Path, Line: d.Line, Col: 1, Severity: kinds.SevError, Source: kinds.SourceKind,
				Field: "metadata.name", Code: "duplicate",
				Message: fmt.Sprintf("%s %s is also defined in %s", k.Name, d.Name, prev.Path)})
			continue
		}
		seen[key] = d
		if k.NewPath != nil && k.Name != "Article" {
			want := k.NewPath(d.Name)
			if want != d.Path && path.Dir(want) == path.Dir(d.Path) {
				out = append(out, kinds.Problem{Path: d.Path, Line: d.Line, Col: 1, Severity: kinds.SevWarning, Source: kinds.SourceKind,
					Field: "metadata.name", Code: "file-name",
					Message: fmt.Sprintf("the file name does not match the name %s (expected %s)", d.Name, want)})
			}
		}
	}
	return out
}

// SchemaRawLookup returns the raw JSON Schema (property order kept) of a
// document of a file, or nil.
func SchemaRawLookup(reg kinds.Registry, p string) func(*yaml.Node) []byte {
	return func(n *yaml.Node) []byte {
		if reg == nil {
			return nil
		}
		d := newDoc(p, 0, n, 0)
		if d == nil {
			return nil
		}
		k, ok := reg.Match(p, d)
		if !ok || k.Schema == nil {
			return nil
		}
		v := VersionOf(k, d)
		if v == "" {
			v = k.Version
		}
		return entry(k, v).raw
	}
}
