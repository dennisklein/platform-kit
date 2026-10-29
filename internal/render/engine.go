package render

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

// FluxSystem is the pseudo Kustomization of the objects in flux.yaml.
const FluxSystem = "flux-system"

// Engine is the Renderer: Flux Kustomizations per cluster, kustomize in
// process, kind render hooks, a source map, and a cache by tree content.
type Engine struct {
	reg    kinds.Registry
	loader *repo.Loader

	mu      sync.Mutex
	sets    map[string]*cacheEntry[*ObjectSet]
	impacts map[string]*cacheEntry[*Impact]
}

type cacheEntry[T any] struct {
	v  T
	at time.Time
}

var _ Renderer = (*Engine)(nil)

// New returns a render engine for the kind registry.
func New(reg kinds.Registry) *Engine {
	return &Engine{reg: reg, loader: repo.NewLoader(reg), sets: map[string]*cacheEntry[*ObjectSet]{}, impacts: map[string]*cacheEntry[*Impact]{}}
}

// Loader returns the tree loader (shared with validation).
func (e *Engine) Loader() *repo.Loader { return e.loader }

// Load parses the tree of a source.
func (e *Engine) Load(src Source) (*repo.Repo, error) {
	if src.Dir == "" {
		return nil, fmt.Errorf("render: source %s has no directory", src.Ref)
	}
	return e.loader.Load(src.Dir)
}

// Clusters lists the clusters of a tree (clusters/<id>/cluster.yaml).
func (e *Engine) Clusters(ctx context.Context, src Source) ([]Cluster, error) {
	r, err := e.Load(src)
	if err != nil {
		return nil, err
	}
	return ClustersOf(r), nil
}

// ClustersOf lists the clusters of a parsed tree, sorted by id.
func ClustersOf(r *repo.Repo) []Cluster {
	var out []Cluster
	for _, d := range r.ByKind("Cluster") {
		if d.Group() != "pk" || !strings.HasPrefix(d.Path, "clusters/") {
			continue
		}
		out = append(out, Cluster{ID: d.Name, Role: kindutil.Str(d.Data, "spec", "role"), Ring: kindutil.Str(d.Data, "spec", "ring"), Path: d.Path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Render renders one cluster of a source.
func (e *Engine) Render(ctx context.Context, src Source, cluster string) (*ObjectSet, error) {
	r, err := e.Load(src)
	if err != nil {
		return nil, err
	}
	return e.RenderRepo(ctx, r, cluster), nil
}

// RenderRepo renders one cluster of a parsed tree (cached by content).
func (e *Engine) RenderRepo(ctx context.Context, r *repo.Repo, cluster string) *ObjectSet {
	key := contentHash(r) + "/" + cluster
	e.mu.Lock()
	if c, ok := e.sets[key]; ok {
		c.at = time.Now()
		e.mu.Unlock()
		return c.v
	}
	e.mu.Unlock()
	set := renderCluster(ctx, e.reg, r, cluster)
	e.mu.Lock()
	e.sets[key] = &cacheEntry[*ObjectSet]{v: set, at: time.Now()}
	evict(e.sets, 96)
	e.mu.Unlock()
	return set
}

func evict[T any](m map[string]*cacheEntry[T], max int) {
	for len(m) > max {
		var oldest string
		var t time.Time
		for k, v := range m {
			if oldest == "" || v.at.Before(t) {
				oldest, t = k, v.at
			}
		}
		delete(m, oldest)
	}
}

// contentHash is the render cache key of a parsed tree. repo.Repo memoizes
// it, so no global map keeps parsed trees alive.
func contentHash(r *repo.Repo) string { return r.ContentHash() }

// fluxKustomization is one Flux Kustomization of a cluster.
type fluxKustomization struct {
	Name            string
	Path            string // repo path without ./
	TargetNamespace string
	Doc             *kinds.Doc
}

func fluxKustomizations(r *repo.Repo, cluster string) ([]fluxKustomization, *repo.File) {
	f := r.File("clusters/" + cluster + "/flux.yaml")
	if f == nil {
		return nil, nil
	}
	var out []fluxKustomization
	for _, d := range f.Docs {
		if d.Kind != "Kustomization" || d.Group() != "kustomize.toolkit.fluxcd.io" {
			continue
		}
		p := strings.TrimPrefix(path.Clean(strings.TrimPrefix(kindutil.Str(d.Data, "spec", "path"), "./")), "/")
		if p == "." {
			p = ""
		}
		out = append(out, fluxKustomization{Name: d.Name, Path: p, TargetNamespace: kindutil.Str(d.Data, "spec", "targetNamespace"), Doc: d})
	}
	return out, f
}

func renderCluster(ctx context.Context, reg kinds.Registry, r *repo.Repo, cluster string) *ObjectSet {
	set := NewObjectSet(cluster)
	ks, flux := fluxKustomizations(r, cluster)
	if flux == nil {
		set.Problems = append(set.Problems, kinds.Problem{Path: "clusters/" + cluster + "/flux.yaml", Severity: kinds.SevWarning, Source: kinds.SourceRender,
			Message: fmt.Sprintf("cluster %s has no flux.yaml; nothing renders", cluster)})
		return set
	}
	// The Flux objects themselves (bootstrap).
	set.Kustomizations[FluxSystem] = flux.Path
	set.Order = append(set.Order, FluxSystem)
	for _, d := range flux.Docs {
		if d.Kind == "" || d.APIVersion == "" {
			continue
		}
		set.add(Object{ObjectRef: ObjectRef{Cluster: cluster, APIVersion: d.APIVersion, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name},
			Kustomization: FluxSystem, Body: cleanBody(d.Data), Source: SourceRef{Path: d.Path, Line: d.Line}})
	}
	for _, k := range ks {
		set.Kustomizations[k.Name] = k.Path
		set.Order = append(set.Order, k.Name)
		var objs []Object
		var problems []kinds.Problem
		if kf := kustomizationFile(r, k.Path); kf != "" {
			objs, problems = buildKustomize(r, k.Path, kf)
		} else {
			objs, problems = loadPlain(r, k)
		}
		for _, p := range problems {
			set.Problems = append(set.Problems, p)
			if p.Severity == kinds.SevError && set.Failed[k.Name] == "" {
				set.Failed[k.Name] = p.Message
			}
		}
		if set.Failed[k.Name] != "" {
			continue
		}
		for _, o := range objs {
			for _, out := range applyHooks(ctx, reg, r, cluster, k.Name, o, &set.Problems) {
				out.Cluster = cluster
				out.Kustomization = k.Name
				if k.TargetNamespace != "" && out.Kind != "Namespace" {
					out.Namespace = k.TargetNamespace
					if md, ok := out.Body["metadata"].(map[string]any); ok {
						md["namespace"] = k.TargetNamespace
					}
				}
				set.add(out)
			}
		}
	}
	return set
}

// add adds an object and reports duplicates.
func (s *ObjectSet) add(o Object) {
	k := KeyOf(o.ObjectRef)
	if prev, ok := s.Objects[k]; ok {
		s.Problems = append(s.Problems, kinds.Problem{Path: o.Source.Path, Line: o.Source.Line, Severity: kinds.SevError, Source: kinds.SourceRender,
			Message: fmt.Sprintf("%s/%s is rendered twice on %s (also from %s, Kustomization %s)", o.Kind, o.Name, s.Cluster, prev.Source.Path, prev.Kustomization)})
	}
	s.Objects[k] = o
}

func kustomizationFile(r *repo.Repo, dir string) string {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		p := path.Join(dir, n)
		if dir == "" {
			p = n
		}
		if r.File(p) != nil {
			return p
		}
	}
	return ""
}

// loadPlain loads all YAML documents below a path (as Flux does for a path
// without kustomization.yaml).
func loadPlain(r *repo.Repo, k fluxKustomization) ([]Object, []kinds.Problem) {
	var out []Object
	var problems []kinds.Problem
	prefix := k.Path + "/"
	found := false
	for _, p := range r.Paths() {
		if k.Path != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		f := r.File(p)
		if f.Language != "yaml" {
			continue
		}
		found = true
		if f.Syntax != nil {
			problems = append(problems, kinds.Problem{Path: p, Line: f.Syntax.Line, Severity: kinds.SevError, Source: kinds.SourceRender,
				Message: fmt.Sprintf("Kustomization %s fails: %s line %d: %s", k.Name, p, f.Syntax.Line, f.Syntax.Message)})
			continue
		}
		for _, d := range f.Docs {
			if d.Kind == "" || d.APIVersion == "" {
				if len(d.Data) > 0 {
					problems = append(problems, kinds.Problem{Path: p, Line: d.Line, Severity: kinds.SevWarning, Source: kinds.SourceRender,
						Message: fmt.Sprintf("Kustomization %s: document without apiVersion and kind is not applied", k.Name)})
				}
				continue
			}
			out = append(out, Object{ObjectRef: ObjectRef{APIVersion: d.APIVersion, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name},
				Body: d.Data, Source: SourceRef{Path: d.Path, Line: d.Line}})
		}
	}
	if !found {
		problems = append(problems, kinds.Problem{Path: k.Doc.Path, Line: k.Doc.Line, Severity: kinds.SevError, Source: kinds.SourceRender,
			Message: fmt.Sprintf("Kustomization %s: path ./%s has no YAML files", k.Name, k.Path)})
	}
	return out, problems
}

// buildKustomize runs kustomize in process on an in-memory copy of the
// tree, with origin annotations for the source map.
func buildKustomize(r *repo.Repo, dir, kfile string) ([]Object, []kinds.Problem) {
	fs := filesys.MakeFsInMemory()
	for _, p := range r.Paths() {
		content := r.File(p).Content
		if p == kfile {
			content = withOriginAnnotations(content)
		}
		if err := fs.WriteFile("/"+p, content); err != nil {
			return nil, []kinds.Problem{{Path: kfile, Severity: kinds.SevError, Source: kinds.SourceRender, Message: "kustomize: " + err.Error()}}
		}
	}
	opts := krusty.MakeDefaultOptions()
	opts.LoadRestrictions = types.LoadRestrictionsNone
	k := krusty.MakeKustomizer(opts)
	rm, err := k.Run(fs, "/"+dir)
	if err != nil {
		return nil, []kinds.Problem{{Path: kfile, Line: 1, Severity: kinds.SevError, Source: kinds.SourceRender, Message: "kustomize build: " + err.Error()}}
	}
	var out []Object
	var problems []kinds.Problem
	for _, res := range rm.Resources() {
		src := SourceRef{Path: kfile, Line: 1}
		if o, err := res.GetOrigin(); err == nil && o != nil && o.Path != "" {
			p := path.Clean(path.Join(dir, o.Path))
			src = SourceRef{Path: p, Line: sourceLine(r, p, res.GetKind(), res.GetName())}
			_ = res.SetOrigin(nil)
		}
		body, err := res.Map()
		if err != nil {
			problems = append(problems, kinds.Problem{Path: src.Path, Line: src.Line, Severity: kinds.SevError, Source: kinds.SourceRender, Message: err.Error()})
			continue
		}
		if md, ok := body["metadata"].(map[string]any); ok {
			if a, ok := md["annotations"].(map[string]any); ok && len(a) == 0 {
				delete(md, "annotations")
			}
		}
		out = append(out, Object{ObjectRef: ObjectRef{APIVersion: res.GetApiVersion(), Kind: res.GetKind(), Namespace: res.GetNamespace(), Name: res.GetName()},
			Body: body, Source: src})
	}
	return out, problems
}

// withOriginAnnotations adds buildMetadata: [originAnnotations] to a
// kustomization file.
func withOriginAnnotations(content []byte) []byte {
	var m map[string]any
	if err := yaml.Unmarshal(content, &m); err != nil || m == nil {
		return content
	}
	bm, _ := m["buildMetadata"].([]any)
	for _, v := range bm {
		if v == "originAnnotations" {
			return content
		}
	}
	m["buildMetadata"] = append(bm, "originAnnotations")
	b, err := yaml.Marshal(m)
	if err != nil {
		return content
	}
	return b
}

// sourceLine finds the line of the document of kind and name in a file.
func sourceLine(r *repo.Repo, p, kind, name string) int {
	f := r.File(p)
	if f == nil || len(f.Docs) == 0 {
		return 1
	}
	for _, d := range f.Docs {
		if d.Kind == kind && d.Name == name {
			return d.Line
		}
	}
	for _, d := range f.Docs {
		if d.Kind == kind {
			return d.Line
		}
	}
	return f.Docs[0].Line
}

// applyHooks runs the kind render hook of an object. Own kinds without a
// hook render nothing; other objects render as they are.
func applyHooks(ctx context.Context, reg kinds.Registry, r *repo.Repo, cluster, kust string, o Object, problems *[]kinds.Problem) []Object {
	src := &kinds.Doc{Path: o.Source.Path, Line: o.Source.Line, APIVersion: o.APIVersion, Kind: o.Kind, Name: o.Name, Namespace: o.Namespace, Data: o.Body}
	if f := r.File(o.Source.Path); f != nil {
		for _, d := range f.Docs {
			if d.Kind == o.Kind && d.Name == o.Name {
				src.Node, src.Offset = d.Node, d.Offset
			}
		}
	}
	k, ok := reg.Match(src.Path, src)
	if !ok {
		o.Body = cleanBody(o.Body)
		return []Object{o}
	}
	if k.Render != nil {
		objs, err := k.Render(kinds.RenderContext{Context: ctx, Cluster: cluster, Repo: r, Kustomization: kust}, src)
		if err != nil {
			*problems = append(*problems, kinds.Problem{Path: src.Path, Line: src.Line, Severity: kinds.SevError, Source: kinds.SourceRender, Message: err.Error()})
			return nil
		}
		return objs
	}
	if k.Own {
		return nil
	}
	o.Body = cleanBody(o.Body)
	return []Object{o}
}

// cleanBody drops the sops metadata and status from a rendered object.
func cleanBody(b map[string]any) map[string]any {
	if _, ok := b["sops"]; !ok {
		if _, ok := b["status"]; !ok {
			return b
		}
	}
	out := make(map[string]any, len(b))
	for k, v := range b {
		if k != "sops" && k != "status" {
			out[k] = v
		}
	}
	return out
}
