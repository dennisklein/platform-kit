// Package kinds is the kind registry. A kind is a type of repo content (a
// node, a rack, a Flux Kustomization). Each kind lives in a package below
// internal/kinds and returns a Kind value; kinds/all builds the registry.
package kinds

import (
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// TruthClass says where the truth of a field comes from.
type TruthClass string

const (
	Intent         TruthClass = "intent"          // people write it
	Generated      TruthClass = "generated"       // render output
	Asserted       TruthClass = "asserted"        // a claim about the physical world (serial number)
	Derived        TruthClass = "derived"         // computed from other data
	ImperativeOnly TruthClass = "imperative-only" // no reconciler
)

// ApplyPath says how desired state reaches the world.
type ApplyPath string

const (
	ApplyFlux   ApplyPath = "flux"
	ApplyManual ApplyPath = "manual" // talosctl, OpenBao configure script: open steps after merge
	ApplySync   ApplyPath = "sync"   // org database sync
	ApplyNone   ApplyPath = "none"   // documents, work items
)

// Severity of a problem.
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
	SevInfo    Severity = "info"
)

// Problem source values.
const (
	SourceYAML      = "yaml"
	SourceSchema    = "schema"
	SourceKind      = "kind"
	SourcePolicy    = "policy"
	SourceRender    = "render"
	SourceReference = "reference"
)

// Problem is a validation finding with a position in a file.
type Problem struct {
	Path     string   `json:"path"`
	Line     int      `json:"line,omitempty"`
	Col      int      `json:"col,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Source   string   `json:"source"`
	// Field is the dotted path of the field, for example spec.interval.
	Field string `json:"field,omitempty"`
	Code  string `json:"code,omitempty"`
}

// SourceRef points to the file and line that made an object.
type SourceRef struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// ObjectRef identifies a rendered object.
type ObjectRef struct {
	Cluster    string `json:"cluster"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// Key is cluster/apiVersion/kind/namespace/name.
func (r ObjectRef) Key() string {
	return r.Cluster + "/" + r.APIVersion + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

// String is Kind/name, the short form used in the UI.
func (r ObjectRef) String() string { return r.Kind + "/" + r.Name }

// Object is one rendered Kubernetes object.
type Object struct {
	ObjectRef
	// Kustomization is the Flux Kustomization that applies the object.
	Kustomization string         `json:"kustomization,omitempty"`
	Body          map[string]any `json:"body"`
	Source        SourceRef      `json:"source"`
}

// Op is the kind of a change to an object or document.
type Op string

const (
	OpAdd    Op = "add"
	OpChange Op = "change"
	OpDelete Op = "delete"
)

// SemanticLine describes one change in domain words, for example
// "cn-035: new compute node in GC5A-6Z, U6".
type SemanticLine struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
	Op      Op     `json:"op"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// Doc is one YAML document of a repo file (or the front matter of a
// Markdown file).
type Doc struct {
	Path string
	// Index is the position of the document in a multi-document file.
	Index int
	// Line is the first line of the document in the file (1-based).
	Line       int
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	// Data is the decoded document.
	Data map[string]any
	// Node is the parsed YAML node with line numbers.
	Node *yaml.Node
	// Offset is added to Node lines to get file lines (front matter of
	// Markdown files starts after the --- line).
	Offset int
	// Body is the Markdown body for articles.
	Body string
}

// LineOffset returns the offset of Node lines in the file.
func (d *Doc) LineOffset() int { return d.Offset }

// Key is kind/namespace/name, the identity of a document across trees.
func (d *Doc) Key() string { return d.Kind + "/" + d.Namespace + "/" + d.Name }

// Group returns the API group of the document ("" for core).
func (d *Doc) Group() string {
	if g, _, ok := strings.Cut(d.APIVersion, "/"); ok {
		return g
	}
	return ""
}

// Version returns the version part of apiVersion.
func (d *Doc) Version() string {
	if _, v, ok := strings.Cut(d.APIVersion, "/"); ok {
		return v
	}
	return d.APIVersion
}

// Decode decodes the document into v (using JSON tags).
func (d *Doc) Decode(v any) error {
	b, err := json.Marshal(d.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Repo is a parsed view of a repo tree at one ref (main, a change worktree,
// or a commit). Cross-file validation and render hooks read it.
type Repo interface {
	// FS is the file tree.
	FS() fs.FS
	// Docs returns all parsed documents.
	Docs() []*Doc
	// ByKind returns the documents of one kind name.
	ByKind(kind string) []*Doc
	// Find returns the document of a kind with a name, or nil.
	Find(kind, name string) *Doc
}

// RenderContext is passed to render hooks.
type RenderContext struct {
	Context context.Context
	Cluster string
	Repo    Repo
	// Kustomization is the Flux Kustomization being rendered.
	Kustomization string
}

// Kind describes one kind of repo content.
type Kind struct {
	// Name is the kind, for example Node.
	Name  string
	Group string
	// Version is the newest version; Versions lists all, oldest first.
	Version  string
	Versions []string
	// Own kinds have Go types; others are third-party overlays (CRD schema subset).
	Own         bool
	Description string
	// Paths are doublestar globs where the kind's files live (for new files
	// and for Match without apiVersion, for example kb/**/*.md).
	Paths []string
	// NewPath returns the default path for a new object of this kind.
	NewPath func(name string) string
	// Truth is the default truth class of fields without x-pk-truth.
	Truth     TruthClass
	ApplyPath ApplyPath
	// Views are extra UI views, for example "rack" or "node".
	Views []string
	// Schema returns the JSON Schema of a version ("" means the newest).
	Schema func(version string) (json.RawMessage, error)

	// Match reports whether a document is of this kind. Nil uses the default:
	// same group and kind, or a path match for documents without a kind.
	Match func(path string, doc *Doc) bool
	// Render emits rendered objects for a cluster (for example Node to Machine).
	Render func(ctx RenderContext, doc *Doc) ([]Object, error)
	// Describe returns semantic diff lines (old or new may be nil).
	Describe func(old, new *Doc) []SemanticLine
	// Validate runs cross-file checks over the whole repo.
	Validate func(repo Repo) []Problem
	// Convert converts a document from an old version to the newest one.
	Convert func(fromVersion string, data map[string]any) (map[string]any, error)
	// Skeleton returns the content of a new file with schema defaults.
	Skeleton func(name string) ([]byte, error)
}

// APIVersion returns group/version of the newest version.
func (k *Kind) APIVersion() string {
	if k.Group == "" {
		return k.Version
	}
	return k.Group + "/" + k.Version
}

// Hooks lists the hooks the kind has (render, describe, validate, convert, skeleton).
func (k *Kind) Hooks() []string {
	var h []string
	if k.Render != nil {
		h = append(h, "render")
	}
	if k.Describe != nil {
		h = append(h, "describe")
	}
	if k.Validate != nil {
		h = append(h, "validate")
	}
	if k.Convert != nil {
		h = append(h, "convert")
	}
	if k.Skeleton != nil {
		h = append(h, "skeleton")
	}
	return h
}

// Matches applies Match or the default rule.
func (k *Kind) Matches(p string, doc *Doc) bool {
	if k.Match != nil {
		return k.Match(p, doc)
	}
	if doc != nil && doc.Kind != "" {
		return doc.Kind == k.Name && doc.Group() == k.Group
	}
	for _, g := range k.Paths {
		if ok, _ := doublestar.Match(g, p); ok {
			return true
		}
	}
	return false
}

// Registry looks up kinds.
type Registry interface {
	// List returns all kinds, sorted by name.
	List() []*Kind
	// Get returns a kind by name (case-insensitive).
	Get(name string) (*Kind, bool)
	// Match returns the kind of a document in a file, if any.
	Match(path string, doc *Doc) (*Kind, bool)
}

// MapRegistry is the default Registry.
type MapRegistry struct {
	kinds []*Kind
}

// NewRegistry returns a registry of ks. Own kinds match before overlays.
func NewRegistry(ks ...Kind) *MapRegistry {
	r := &MapRegistry{}
	for i := range ks {
		k := ks[i]
		r.kinds = append(r.kinds, &k)
	}
	sort.SliceStable(r.kinds, func(i, j int) bool { return r.kinds[i].Name < r.kinds[j].Name })
	return r
}

func (r *MapRegistry) List() []*Kind { return r.kinds }

func (r *MapRegistry) Get(name string) (*Kind, bool) {
	for _, k := range r.kinds {
		if strings.EqualFold(k.Name, name) {
			return k, true
		}
	}
	return nil, false
}

func (r *MapRegistry) Match(p string, doc *Doc) (*Kind, bool) {
	p = path.Clean(p)
	for _, own := range []bool{true, false} {
		for _, k := range r.kinds {
			if k.Own == own && k.Matches(p, doc) {
				return k, true
			}
		}
	}
	return nil, false
}

// Metadata is the metadata block of own kinds.
type Metadata struct {
	Name        string            `json:"name" yaml:"name" jsonschema:"title=Name,pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$" pk:"truth=intent,label=Name,order=0"`
	Namespace   string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty" pk:"truth=intent,label=Labels,order=90"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}
