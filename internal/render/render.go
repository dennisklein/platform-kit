// Package render renders the state repo per cluster (Flux Kustomizations,
// kustomize in-process, kind render hooks), builds object sets with a source
// map, and compares two renders into an impact with semantic, source and
// rendered diffs.
package render

import (
	"context"
	"sort"
	"time"

	"github.com/dennisklein/platform-kit/internal/kinds"
)

// Object is one rendered object (shared with kind render hooks).
type Object = kinds.Object

// SourceRef is the file and line that made an object.
type SourceRef = kinds.SourceRef

// ObjectRef identifies an object.
type ObjectRef = kinds.ObjectRef

// ObjectKey is cluster/apiVersion/kind/namespace/name.
type ObjectKey string

// KeyOf returns the key of an object.
func KeyOf(r ObjectRef) ObjectKey { return ObjectKey(r.Key()) }

// ObjectSet is the render of one cluster.
type ObjectSet struct {
	Cluster string
	// Objects by key.
	Objects map[ObjectKey]Object
	// Kustomizations are the Flux Kustomizations of the cluster (name to path).
	Kustomizations map[string]string
	// Order is the order of the Kustomizations in flux.yaml.
	Order    []string
	Problems []kinds.Problem
	// Failed are the Kustomizations that do not build (name to reason).
	// Flux keeps their last applied objects.
	Failed map[string]string
}

// NewObjectSet returns an empty set.
func NewObjectSet(cluster string) *ObjectSet {
	return &ObjectSet{Cluster: cluster, Objects: map[ObjectKey]Object{}, Kustomizations: map[string]string{}, Failed: map[string]string{}}
}

// Add adds or replaces an object.
func (s *ObjectSet) Add(o Object) { s.Objects[KeyOf(o.ObjectRef)] = o }

// Keys returns the sorted keys.
func (s *ObjectSet) Keys() []ObjectKey {
	keys := make([]ObjectKey, 0, len(s.Objects))
	for k := range s.Objects {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// Source is a tree to render: a checked-out directory and a cache key.
type Source struct {
	// Ref is the display name (main, c-k7m2qz, a sha).
	Ref string
	SHA string
	// Dir is a directory with the tree (a worktree or an export).
	Dir string
	// Key identifies the content for the render cache (tree hash, or tree hash
	// plus worktree mtime for a dirty worktree).
	Key string
	// Dirty marks saved, uncommitted files.
	Dirty bool
}

// Cluster is a cluster found in clusters/<id>/cluster.yaml.
type Cluster struct {
	ID   string
	Role string
	Ring string
	Path string
}

// ObjectChange is one changed object.
type ObjectChange struct {
	Op            kinds.Op
	Ref           ObjectRef
	Kustomization string
	Source        SourceRef
	// Diff is the unified diff of the rendered YAML.
	Diff string
}

// KustomizationImpact groups changes by Flux Kustomization.
type KustomizationImpact struct {
	Name    string
	Path    string
	Objects []ObjectChange
}

// ClusterImpact is the impact on one cluster.
type ClusterImpact struct {
	Cluster        string
	Role, Ring     string
	ObjectsBase    int
	ObjectsHead    int
	Kustomizations []KustomizationImpact
	// Failed are the Kustomizations of the head that do not build; their
	// objects count as unchanged.
	Failed []string
}

// Counts returns added, changed and deleted objects.
func (c ClusterImpact) Counts() (added, changed, deleted int) {
	for _, k := range c.Kustomizations {
		for _, o := range k.Objects {
			switch o.Op {
			case kinds.OpAdd:
				added++
			case kinds.OpChange:
				changed++
			case kinds.OpDelete:
				deleted++
			}
		}
	}
	return
}

// OpenStep is a manual or sync apply step after merge.
type OpenStep struct {
	Kind      string
	Name      string
	Path      string
	ApplyPath kinds.ApplyPath
	Text      string
}

// FileDiff is the source diff of one file.
type FileDiff struct {
	Path    string
	OldPath string
	// Status: added, modified, deleted, renamed.
	Status  string
	Unified string
	Old     string // masked
	New     string // masked
}

// Impact compares a base and a head render.
type Impact struct {
	Base, Head Source
	Clusters   []ClusterImpact
	Semantic   []kinds.SemanticLine
	Files      []FileDiff
	OpenSteps  []OpenStep
	Problems   []kinds.Problem
	RenderedAt time.Time
}

// Objects returns the refs of all changed objects (for policy risk rules and overlap).
func (i *Impact) Objects() []ObjectRef {
	var out []ObjectRef
	for _, c := range i.Clusters {
		for _, k := range c.Kustomizations {
			for _, o := range k.Objects {
				out = append(out, o.Ref)
			}
		}
	}
	return out
}

// Summary is "defiant +2 ~1" style counts.
func (i *Impact) Summary() (added, changed, deleted int) {
	for _, c := range i.Clusters {
		a, ch, d := c.Counts()
		added, changed, deleted = added+a, changed+ch, deleted+d
	}
	return
}

// Renderer renders trees and compares them. Implementations cache renders by
// (Source.Key, cluster).
type Renderer interface {
	// Clusters lists the clusters of a tree.
	Clusters(ctx context.Context, src Source) ([]Cluster, error)
	// Render renders one cluster.
	Render(ctx context.Context, src Source, cluster string) (*ObjectSet, error)
	// Impact renders base and head for all clusters and compares them,
	// including the semantic diff from kind Describe hooks and the source diff.
	Impact(ctx context.Context, base, head Source) (*Impact, error)
}
