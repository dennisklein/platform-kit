package lab

import (
	"context"
	"path"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/render"
)

// repoRenderer is implemented by the render engine: it renders a tree that
// is already parsed.
type repoRenderer interface {
	RenderRepo(ctx context.Context, r *repo.Repo, cluster string) *render.ObjectSet
}

// BuildModel parses a source and renders one cluster of it. cluster may be
// "" (inventory only).
func BuildModel(ctx context.Context, reg kinds.Registry, rd render.Renderer, src render.Source, cluster string) (*Model, *repo.Repo, error) {
	r, err := repo.Load(src.Dir, reg)
	if err != nil {
		return nil, nil, err
	}
	m, err := ModelOf(ctx, rd, r, src, cluster)
	return m, r, err
}

// ModelOf renders one cluster of a parsed tree into a model.
func ModelOf(ctx context.Context, rd render.Renderer, r *repo.Repo, src render.Source, cluster string) (*Model, error) {
	var set *render.ObjectSet
	if cluster != "" {
		if rr, ok := rd.(repoRenderer); ok {
			set = rr.RenderRepo(ctx, r, cluster)
		} else {
			var err error
			if set, err = rd.Render(ctx, src, cluster); err != nil {
				return nil, err
			}
		}
	}
	m := NewModel(r, set, cluster, nil)
	m.Ref, m.SHA = src.Ref, src.SHA
	if cluster != "" {
		m.Errors = clusterErrors(r, m.Set, cluster)
	}
	return m, nil
}

// clusterErrors are the validation errors of the files that feed a
// cluster: the sources of its objects, the Flux Kustomization paths and the
// cluster files.
func clusterErrors(r *repo.Repo, set *render.ObjectSet, cluster string) []kinds.Problem {
	files := map[string]bool{}
	for _, o := range set.Objects {
		if o.Source.Path != "" {
			files[o.Source.Path] = true
		}
	}
	var dirs []string
	for _, p := range set.Kustomizations {
		p = strings.TrimPrefix(path.Clean(strings.TrimPrefix(p, "./")), "./")
		if p != "" && p != "." {
			dirs = append(dirs, p+"/")
		}
	}
	dirs = append(dirs, "clusters/"+cluster+"/")
	var out []kinds.Problem
	for _, p := range r.Problems() {
		// Flux fails on invalid YAML and schema errors; cross-file checks
		// (ports, units) are for the node checks.
		if p.Severity != kinds.SevError || (p.Source != kinds.SourceYAML && p.Source != kinds.SourceSchema) {
			continue
		}
		hit := files[p.Path]
		for _, d := range dirs {
			if strings.HasPrefix(p.Path, d) && !strings.HasPrefix(p.Path, "control/inventory/") {
				hit = true
			}
		}
		if hit {
			out = append(out, p)
		}
	}
	return out
}
