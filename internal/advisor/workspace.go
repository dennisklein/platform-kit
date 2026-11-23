package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// workspace loads what a recipe reads, once, and records it in reading.
type workspace struct {
	ctx context.Context
	t   Tools
	id  string

	ch       *api.Change
	imp      *api.Impact
	impErr   error
	impDone  bool
	runs     []api.LabRun
	runsDone bool
	files    []api.FileStatus
	nodes    []NodeInfo
	nodesOK  bool
	policies []BGPPolicy
	polOK    bool
	summ     []api.ChangeSummary
	summOK   bool
	content  map[string]string

	reading []llm.Source
	seen    map[string]bool
}

func newWorkspace(ctx context.Context, t Tools, id string) *workspace {
	return &workspace{ctx: ctx, t: t, id: id, content: map[string]string{}, seen: map[string]bool{}}
}

func (w *workspace) read(s llm.Source) {
	k := s.Kind + "\x00" + s.Label
	if !w.seen[k] {
		w.seen[k] = true
		w.reading = append(w.reading, s)
	}
}

func (w *workspace) change() (*api.Change, error) {
	if w.ch != nil {
		return w.ch, nil
	}
	ch, err := w.t.Change(w.ctx, w.id)
	if err != nil {
		return nil, err
	}
	w.ch = ch
	w.read(srcChange(ch.Id))
	if ch.Mr.Number > 0 {
		w.read(srcMR(ch.Mr.Number, ch.Mr.Url))
	}
	return ch, nil
}

func (w *workspace) impact() (*api.Impact, error) {
	if !w.impDone {
		w.impDone = true
		w.imp, w.impErr = w.t.Impact(w.ctx, w.id)
		if w.impErr == nil {
			w.read(srcImpact(w.id))
		}
	}
	return w.imp, w.impErr
}

// labRuns returns the runs of the change, newest first.
func (w *workspace) labRuns() []api.LabRun {
	if !w.runsDone {
		w.runsDone = true
		runs, err := w.t.LabRuns(w.ctx, w.id)
		if err == nil {
			sort.SliceStable(runs, func(i, j int) bool {
				if runs[i].Number != runs[j].Number {
					return runs[i].Number > runs[j].Number
				}
				return runs[i].StartedAt.After(runs[j].StartedAt)
			})
			w.runs = runs
		}
	}
	return w.runs
}

// lastRun returns the newest lab run (from the lab service, else the
// recorded runs of the change).
func (w *workspace) lastRun() (*api.LabRun, *api.LabRunRef) {
	if runs := w.labRuns(); len(runs) > 0 {
		w.read(srcLab(runs[0].Id))
		return &runs[0], nil
	}
	if w.ch != nil && len(w.ch.LabRuns) > 0 {
		ref := w.ch.LabRuns[len(w.ch.LabRuns)-1]
		w.read(srcLab(ref.Run))
		return nil, &ref
	}
	return nil, nil
}

// changedFiles returns files changed against the target (incl. saved files).
func (w *workspace) changedFiles() []api.FileStatus {
	if w.files != nil {
		return w.files
	}
	w.files = []api.FileStatus{}
	if w.ch != nil && len(w.ch.ChangedFiles) > 0 {
		w.files = append(w.files, w.ch.ChangedFiles...)
	} else if d, err := w.t.Diff(w.ctx, w.id); err == nil {
		for _, f := range d.Files {
			w.files = append(w.files, api.FileStatus{Path: f.Path, OldPath: f.OldPath, Status: f.Status})
		}
	}
	// Saved, uncommitted files the target diff may not list yet.
	if st, err := w.t.Status(w.ctx, w.id); err == nil {
		have := map[string]bool{}
		for _, f := range w.files {
			have[f.Path] = true
		}
		for _, f := range st.Files {
			if !have[f.Path] {
				w.files = append(w.files, f)
			}
		}
	}
	return w.files
}

func (w *workspace) file(ref, p string) (string, bool) {
	k := ref + "\x00" + p
	if c, ok := w.content[k]; ok {
		return c, true
	}
	fc, err := w.t.ReadFile(w.ctx, ref, p)
	if err != nil || !fc.Exists && fc.Content == "" {
		return "", false
	}
	w.content[k] = fc.Content
	return fc.Content, true
}

// changeNodes returns the Node documents of files the change adds or
// modifies (head version).
func (w *workspace) changeNodes() []NodeInfo {
	if w.nodesOK {
		return w.nodes
	}
	w.nodesOK = true
	for _, f := range w.changedFiles() {
		if f.Status == api.FileChangeDeleted || !strings.HasPrefix(f.Path, "control/inventory/") || !isYAML(f.Path) {
			continue
		}
		c, ok := w.file(w.id, f.Path)
		if !ok {
			continue
		}
		for _, n := range ParseNodes(f.Path, c) {
			n.New = f.Status == api.FileChangeAdded || f.Status == api.FileChangeUntracked
			w.nodes = append(w.nodes, n)
			w.read(srcFile(f.Path, 0))
		}
	}
	sort.Slice(w.nodes, func(i, j int) bool { return w.nodes[i].Name < w.nodes[j].Name })
	return w.nodes
}

// bgpPolicies returns the BGP policies in the change head.
func (w *workspace) bgpPolicies() []BGPPolicy {
	if w.polOK {
		return w.policies
	}
	w.polOK = true
	nodes, err := w.t.Tree(w.ctx, w.id)
	if err != nil {
		return nil
	}
	var cands []string
	walkTree(nodes, func(n api.FileNode) {
		if n.Type != api.FileNodeTypeFile || !isYAML(n.Path) || !strings.HasPrefix(n.Path, "clusters/") {
			return
		}
		if n.Kind == "CiliumBGPPeeringPolicy" || (n.Kind == "" && strings.Contains(n.Path, "/cilium/")) {
			cands = append(cands, n.Path)
		}
	})
	for _, p := range cands {
		c, ok := w.file(w.id, p)
		if !ok {
			continue
		}
		w.policies = append(w.policies, ParseBGP(p, c)...)
	}
	return w.policies
}

func walkTree(ns []api.FileNode, fn func(api.FileNode)) {
	for _, n := range ns {
		fn(n)
		walkTree(n.Children, fn)
	}
}

func (w *workspace) summaries() []api.ChangeSummary {
	if !w.summOK {
		w.summOK = true
		w.summ, _ = w.t.Changes(w.ctx)
	}
	return w.summ
}

func (w *workspace) summary(id string) (api.ChangeSummary, bool) {
	for _, s := range w.summaries() {
		if s.Id == id {
			return s, true
		}
	}
	return api.ChangeSummary{}, false
}

func isYAML(p string) bool { return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") }

// bgpGapFindings returns one finding per BGP gap, with a suggested edit.
func (w *workspace) bgpGapFindings() []finding {
	var out []finding
	for _, g := range FindBGPGaps(w.changeNodes(), w.bgpPolicies()) {
		names := g.Names()
		w.read(srcFile(g.Policy.Path, 0))
		text := fmt.Sprintf("%s %s in the inventory of this change, in %s, but not in the BGP policy %s. %s no BGP session.",
			joinAnd(names), isAre(len(names)), g.Policy.Rack, g.Policy.Name, getsNo(len(names)))
		f := finding{Point: Point{Title: "BGP policy", Text: text, Severity: SevRisk,
			Sources: []llm.Source{srcFile(g.Policy.Path, 0), srcFile(g.Missing[0].Path, 0)}}}
		if before, ok := w.file(w.id, g.Policy.Path); ok {
			if after, err := AddHostnames(before, g.Policy.Name, names); err == nil && after != before {
				f.Suggestion = &Suggestion{
					Path: g.Policy.Path, Before: before, After: after,
					Title: fmt.Sprintf("Add %s to the BGP policy %s", joinAnd(names), g.Policy.Name),
				}
				f.Text += " Suggested fix: add " + plural(len(names), "line", "lines") + " to " + shortPath(g.Policy.Path) + "."
			}
		}
		out = append(out, f)
	}
	return out
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func getsNo(n int) string {
	if n == 1 {
		return "It gets"
	}
	return "They get"
}
