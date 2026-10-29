package render

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"sigs.k8s.io/yaml"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// Impact renders base and head for all clusters and compares them.
func (e *Engine) Impact(ctx context.Context, base, head Source) (*Impact, error) {
	br, err := e.Load(base)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", base.Ref, err)
	}
	hr, err := e.Load(head)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", head.Ref, err)
	}
	return e.ImpactRepos(ctx, base, head, br, hr), nil
}

// ImpactRepos compares two parsed trees. Results are cached by content.
func (e *Engine) ImpactRepos(ctx context.Context, base, head Source, br, hr *repo.Repo) *Impact {
	key := contentHash(br) + "/" + contentHash(hr)
	e.mu.Lock()
	c, ok := e.impacts[key]
	if ok {
		c.at = time.Now()
	}
	e.mu.Unlock()
	var imp *Impact
	if ok {
		imp = c.v
	} else {
		imp = e.compare(ctx, br, hr)
		e.mu.Lock()
		e.impacts[key] = &cacheEntry[*Impact]{v: imp, at: time.Now()}
		evict(e.impacts, 64)
		e.mu.Unlock()
	}
	out := *imp
	out.Base, out.Head = base, head
	return &out
}

func (e *Engine) compare(ctx context.Context, br, hr *repo.Repo) *Impact {
	imp := &Impact{RenderedAt: time.Now(), Clusters: []ClusterImpact{}, Semantic: []kinds.SemanticLine{}, Files: []FileDiff{}, OpenSteps: []OpenStep{}, Problems: []kinds.Problem{}}

	// Clusters of both trees.
	clusters := map[string]Cluster{}
	for _, c := range ClustersOf(br) {
		clusters[c.ID] = c
	}
	for _, c := range ClustersOf(hr) {
		clusters[c.ID] = c
	}
	ids := make([]string, 0, len(clusters))
	for id := range clusters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seenProblem := map[string]bool{}
	for _, id := range ids {
		bs := e.RenderRepo(ctx, br, id)
		hs := e.RenderRepo(ctx, hr, id)
		ci := CompareSets(bs, hs)
		ci.Role, ci.Ring = clusters[id].Role, clusters[id].Ring
		imp.Clusters = append(imp.Clusters, ci)
		for _, p := range hs.Problems {
			k := p.Path + p.Message
			if !seenProblem[k] {
				seenProblem[k] = true
				imp.Problems = append(imp.Problems, p)
			}
		}
	}
	sort.SliceStable(imp.Clusters, func(i, j int) bool {
		ci, cj := len(imp.Clusters[i].Kustomizations) > 0, len(imp.Clusters[j].Kustomizations) > 0
		if ci != cj {
			return ci
		}
		return imp.Clusters[i].Cluster < imp.Clusters[j].Cluster
	})

	// Source files, semantic diff and open steps.
	for _, p := range ChangedPaths(br, hr) {
		bf, hf := br.File(p), hr.File(p)
		imp.Files = append(imp.Files, fileDiff(e.reg, p, bf, hf))
		if policy.IsMeta(p) {
			continue
		}
		imp.Semantic = append(imp.Semantic, semantic(e.reg, p, bf, hf)...)
		imp.OpenSteps = append(imp.OpenSteps, openSteps(e.reg, p, bf, hf)...)
	}
	orderLikeObjects(imp)
	return imp
}

// orderLikeObjects sorts semantic lines in the order of the impact objects
// their files render to (cluster, Kustomization, object), so the review reads
// in the same order as the impact. Lines of files without objects come last;
// the change metadata file comes last in the file list.
func orderLikeObjects(imp *Impact) {
	rank := map[string]int{}
	for _, c := range imp.Clusters {
		for _, k := range c.Kustomizations {
			for _, o := range k.Objects {
				if _, ok := rank[o.Source.Path]; !ok && o.Source.Path != "" {
					rank[o.Source.Path] = len(rank)
				}
			}
		}
	}
	rankOf := func(p string) int {
		if r, ok := rank[p]; ok {
			return r
		}
		return len(rank)
	}
	sort.SliceStable(imp.Semantic, func(i, j int) bool { return rankOf(imp.Semantic[i].Path) < rankOf(imp.Semantic[j].Path) })
	sort.SliceStable(imp.Files, func(i, j int) bool { return !policy.IsMeta(imp.Files[i].Path) && policy.IsMeta(imp.Files[j].Path) })
}

// ChangedPaths returns the paths whose content differs, sorted.
func ChangedPaths(a, b *repo.Repo) []string {
	set := map[string]bool{}
	for _, p := range a.Paths() {
		if !repo.SameContent(a, b, p) {
			set[p] = true
		}
	}
	for _, p := range b.Paths() {
		if a.File(p) == nil {
			set[p] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// CompareSets compares two renders of one cluster, grouped by Kustomization.
func CompareSets(base, head *ObjectSet) ClusterImpact {
	ci := ClusterImpact{Cluster: head.Cluster, ObjectsBase: len(base.Objects), ObjectsHead: len(head.Objects), Kustomizations: []KustomizationImpact{}}
	groups := map[string]*KustomizationImpact{}
	order := append([]string{}, head.Order...)
	for _, k := range base.Order {
		if _, ok := head.Kustomizations[k]; !ok {
			order = append(order, k)
		}
	}
	group := func(name string) *KustomizationImpact {
		g, ok := groups[name]
		if !ok {
			p := head.Kustomizations[name]
			if p == "" {
				p = base.Kustomizations[name]
			}
			g = &KustomizationImpact{Name: name, Path: p, Objects: []ObjectChange{}}
			groups[name] = g
		}
		return g
	}
	for _, k := range head.Keys() {
		h := head.Objects[k]
		b, ok := base.Objects[k]
		if !ok {
			group(h.Kustomization).Objects = append(group(h.Kustomization).Objects, ObjectChange{Op: kinds.OpAdd, Ref: h.ObjectRef, Kustomization: h.Kustomization, Source: h.Source, Diff: ObjectDiff(nil, &h)})
			continue
		}
		if d := ObjectDiff(&b, &h); d != "" {
			group(h.Kustomization).Objects = append(group(h.Kustomization).Objects, ObjectChange{Op: kinds.OpChange, Ref: h.ObjectRef, Kustomization: h.Kustomization, Source: h.Source, Diff: d})
		}
	}
	for _, k := range base.Keys() {
		if _, ok := head.Objects[k]; ok {
			continue
		}
		b := base.Objects[k]
		if _, failed := head.Failed[b.Kustomization]; failed {
			continue // Flux keeps the objects of a Kustomization that fails
		}
		group(b.Kustomization).Objects = append(group(b.Kustomization).Objects, ObjectChange{Op: kinds.OpDelete, Ref: b.ObjectRef, Kustomization: b.Kustomization, Source: b.Source, Diff: ObjectDiff(&b, nil)})
	}
	for _, name := range order {
		if _, failed := head.Failed[name]; failed {
			ci.Failed = append(ci.Failed, name)
		}
		if g, ok := groups[name]; ok {
			sortChanges(g.Objects)
			ci.Kustomizations = append(ci.Kustomizations, *g)
			delete(groups, name)
		}
	}
	// Objects of Kustomizations not in either order (should not happen).
	var rest []string
	for name := range groups {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		ci.Kustomizations = append(ci.Kustomizations, *groups[name])
	}
	return ci
}

func sortChanges(cs []ObjectChange) {
	rank := map[kinds.Op]int{kinds.OpAdd: 0, kinds.OpChange: 1, kinds.OpDelete: 2}
	sort.SliceStable(cs, func(i, j int) bool {
		if rank[cs[i].Op] != rank[cs[j].Op] {
			return rank[cs[i].Op] < rank[cs[j].Op]
		}
		if cs[i].Ref.Kind != cs[j].Ref.Kind {
			return cs[i].Ref.Kind < cs[j].Ref.Kind
		}
		return cs[i].Ref.Name < cs[j].Ref.Name
	})
}

// ObjectYAML renders an object body as YAML with sorted keys and masked
// secret values.
func ObjectYAML(o *Object) string {
	if o == nil {
		return ""
	}
	b, err := yaml.Marshal(maskValues(o.Body))
	if err != nil {
		return fmt.Sprintf("# cannot render: %v\n", err)
	}
	return string(b)
}

func maskValues(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[k] = maskValues(vv)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, vv := range x {
			s[i] = maskValues(vv)
		}
		return s
	case string:
		if schema.IsSOPS(x) {
			return schema.MaskText
		}
	}
	return v
}

// ObjectDiff is the unified diff of two rendered objects ("" when equal).
func ObjectDiff(a, b *Object) string {
	as, bs := ObjectYAML(a), ObjectYAML(b)
	if as == bs {
		return ""
	}
	name := ""
	if a != nil {
		name = a.Kind + "/" + a.Name
	} else if b != nil {
		name = b.Kind + "/" + b.Name
	}
	return Unified(name, as, bs)
}

// Unified returns a unified diff with 3 lines of context.
func Unified(name, a, b string) string {
	from, to := "a/"+name, "b/"+name
	if a == "" {
		from = "/dev/null"
	}
	if b == "" {
		to = "/dev/null"
	}
	s, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: splitLines(a), B: splitLines(b), FromFile: from, ToFile: to, Context: 3,
	})
	if err != nil {
		return ""
	}
	return s
}

// splitLines splits text into lines that keep their newline. Unlike
// difflib.SplitLines it adds no empty line after a final newline, and empty
// text has no lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	} else {
		lines[len(lines)-1] += "\n"
	}
	return lines
}

// fileDiff is the masked source diff of one file.
func fileDiff(reg kinds.Registry, p string, bf, hf *repo.File) FileDiff {
	fd := FileDiff{Path: p}
	var old, new []byte
	switch {
	case bf == nil:
		fd.Status = "added"
	case hf == nil:
		fd.Status = "deleted"
	default:
		fd.Status = "modified"
	}
	if bf != nil {
		old = maskFile(reg, p, bf)
	}
	if hf != nil {
		new = maskFile(reg, p, hf)
	}
	fd.Old, fd.New = string(old), string(new)
	if isBinary(old) || isBinary(new) {
		fd.Unified = "Binary file differs\n"
		fd.Old, fd.New = "", ""
		return fd
	}
	fd.Unified = Unified(p, fd.Old, fd.New)
	return fd
}

func maskFile(reg kinds.Registry, p string, f *repo.File) []byte {
	if f.Language != "yaml" {
		return f.Content
	}
	m, _ := schema.Mask(f.Content, repo.SchemaLookup(reg, p))
	return m
}

func isBinary(b []byte) bool { return bytes.IndexByte(b, 0) >= 0 }

// semantic describes the changes of one file with the kind hooks.
func semantic(reg kinds.Registry, p string, bf, hf *repo.File) []kinds.SemanticLine {
	var bdocs, hdocs []*kinds.Doc
	if bf != nil {
		bdocs = bf.Docs
	}
	if hf != nil {
		hdocs = hf.Docs
	}
	if len(bdocs) == 0 && len(hdocs) == 0 {
		op := kinds.OpChange
		text := "changed"
		switch {
		case bf == nil:
			op, text = kinds.OpAdd, "new file"
		case hf == nil:
			op, text = kinds.OpDelete, "file removed"
		}
		return []kinds.SemanticLine{{Subject: p, Text: text, Op: op, Path: p}}
	}
	key := func(d *kinds.Doc) string {
		if d.Name == "" {
			return fmt.Sprintf("%s#%d", d.Kind, d.Index)
		}
		return d.Key()
	}
	old := map[string]*kinds.Doc{}
	for _, d := range bdocs {
		old[key(d)] = d
	}
	var out []kinds.SemanticLine
	for _, d := range hdocs {
		o := old[key(d)]
		delete(old, key(d))
		out = append(out, describeDoc(reg, p, o, d)...)
	}
	for _, d := range bdocs {
		if _, ok := old[key(d)]; ok {
			out = append(out, describeDoc(reg, p, d, nil)...)
		}
	}
	return out
}

func describeDoc(reg kinds.Registry, p string, old, new *kinds.Doc) []kinds.SemanticLine {
	if old != nil && new != nil && equalData(old.Data, new.Data) && old.Body == new.Body {
		return nil
	}
	d := new
	if d == nil {
		d = old
	}
	k, ok := reg.Match(p, d)
	if ok && k.Describe != nil {
		return k.Describe(old, new)
	}
	name := "document"
	if ok {
		name = k.Name
	}
	subject := p
	if d.Kind != "" && d.Name != "" {
		subject = d.Kind + "/" + d.Name
	}
	lines := kindutil.Describer(name, strings.ToLower(name))(old, new)
	for i := range lines {
		lines[i].Subject = subject
	}
	return lines
}

func equalData(a, b map[string]any) bool {
	ab, err1 := yaml.Marshal(a)
	bb, err2 := yaml.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

// openSteps lists manual and sync apply steps for changed documents.
func openSteps(reg kinds.Registry, p string, bf, hf *repo.File) []OpenStep {
	var docs []*kinds.Doc
	if hf != nil {
		docs = hf.Docs
	} else if bf != nil {
		docs = bf.Docs
	}
	var out []OpenStep
	for _, d := range docs {
		if bf != nil && hf != nil {
			// Only documents that changed.
			changed := true
			for _, o := range bf.Docs {
				if o.Key() == d.Key() && equalData(o.Data, d.Data) {
					changed = false
				}
			}
			if !changed {
				continue
			}
		}
		k, ok := reg.Match(p, d)
		if !ok || (k.ApplyPath != kinds.ApplyManual && k.ApplyPath != kinds.ApplySync) {
			continue
		}
		name := d.Name
		if name == "" {
			name = p
		}
		out = append(out, OpenStep{Kind: k.Name, Name: name, Path: p, ApplyPath: k.ApplyPath, Text: stepText(k.Name, name, k.ApplyPath)})
	}
	return out
}

func stepText(kind, name string, ap kinds.ApplyPath) string {
	switch kind {
	case "CablePlan":
		return "Cable the uplinks of " + name + " as planned, then check the links on the switches."
	case "Switch":
		return "Configure switch " + name + " by hand (ports and uplinks)."
	case "Jbod":
		return "Swap the disks of " + name + " that are marked replace; the asset DB sync brings the new serials."
	case "TalosConfig":
		return "Generate the Talos config of " + name + " (talhelper genconfig) and apply it with talosctl, node by node."
	case "Policy":
		return "pk policy sync updates the forge rules (branch protection, CODEOWNERS) after the merge."
	}
	if ap == kinds.ApplySync {
		return kind + " " + name + ": the sync applies it after the merge."
	}
	return kind + " " + name + ": apply by hand after the merge."
}

// Overlaps returns, for each other change, the objects it shares with mine
// (by object key). Changes without shared objects are left out.
func Overlaps(mine []ObjectRef, others map[string][]ObjectRef) map[string][]ObjectRef {
	keys := map[string]bool{}
	for _, o := range mine {
		keys[o.Key()] = true
	}
	out := map[string][]ObjectRef{}
	for id, objs := range others {
		for _, o := range objs {
			if keys[o.Key()] {
				out[id] = append(out[id], o)
			}
		}
	}
	return out
}

// SummaryText is "defiant +2 ~1" for the clusters with changes, or "no
// change".
func (i *Impact) SummaryText() string {
	var parts []string
	for _, c := range i.Clusters {
		a, ch, d := c.Counts()
		if a+ch+d == 0 && len(c.Failed) == 0 {
			continue
		}
		s := c.Cluster
		if a > 0 {
			s += fmt.Sprintf(" +%d", a)
		}
		if ch > 0 {
			s += fmt.Sprintf(" ~%d", ch)
		}
		if d > 0 {
			s += fmt.Sprintf(" -%d", d)
		}
		if len(c.Failed) > 0 {
			s += " (" + strings.Join(c.Failed, ", ") + " fails to build)"
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "no change"
	}
	return strings.Join(parts, ", ")
}

// Paths returns the changed paths.
func (i *Impact) Paths() []string {
	out := make([]string, 0, len(i.Files))
	for _, f := range i.Files {
		out = append(out, f.Path)
	}
	return out
}
