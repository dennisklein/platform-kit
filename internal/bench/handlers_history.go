package bench

// History of main and time travel (area A).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

// historyLimit is how many first-parent commits of main the history reads.
const historyLimit = 300

// histItem is one first-parent commit of main.
type histItem struct {
	entry   api.HistoryEntry
	parent  string
	message string
	kinds   []string
	// refs are entities the changed documents point to (the rack and
	// cluster of a Node); the entity filter matches them too.
	refs []string
}

// histData is the parsed history of one main head.
type histData struct {
	dir, head               string
	headAt                  time.Time
	headSubject, headChange string
	items                   []*histItem
}

// histCache keeps the parsed history per main head and the per-commit data
// (commits never change).
var histCache = struct {
	sync.Mutex
	cur     *histData
	commits map[string]*histItem
}{commits: map[string]*histItem{}}

// history returns the first-parent history of main (cached per head).
func (s *Server) history(ctx context.Context) (*histData, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	dir, err := svc.MainDir(ctx)
	if err != nil {
		return nil, err
	}
	g := gitx.Open(dir)
	head, err := g.Head(ctx)
	if err != nil {
		return nil, err
	}
	histCache.Lock()
	cur := histCache.cur
	histCache.Unlock()
	if cur != nil && cur.dir == dir && cur.head == head {
		return cur, nil
	}
	log, err := g.Log(ctx, head, "--first-parent", "-n", fmt.Sprint(historyLimit))
	if err != nil {
		return nil, err
	}
	h := &histData{dir: dir, head: head}
	for _, e := range log {
		h.items = append(h.items, s.histItem(ctx, g, e))
	}
	if len(h.items) > 0 {
		top := h.items[0]
		h.headAt, h.headSubject, h.headChange = top.entry.Date, chFirstLine(top.message), top.entry.Change
	}
	histCache.Lock()
	histCache.cur = h
	histCache.Unlock()
	return h, nil
}

// histItem builds the entry of one commit (cached by sha).
func (s *Server) histItem(ctx context.Context, g *gitx.Repo, e gitx.LogEntry) *histItem {
	histCache.Lock()
	if it, ok := histCache.commits[e.SHA]; ok {
		histCache.Unlock()
		return it
	}
	histCache.Unlock()

	ts := e.Trailers
	it := &histItem{message: strings.TrimRight(e.Subject+"\n\n"+e.Body, "\n")}
	if len(e.Parents) > 0 {
		it.parent = e.Parents[0]
	}
	en := api.HistoryEntry{
		Sha: e.SHA, Short: chShortSHA(e.SHA), Date: e.CommitDate, Title: e.Subject, Trailers: []api.Trailer{},
		Change: change.Trailer(ts, change.TrailerChange), Intent: change.Trailer(ts, change.TrailerIntent),
		Author: change.Trailer(ts, change.TrailerAuthor), Agent: change.Trailer(ts, change.TrailerAgent),
		ApprovedBy: change.TrailerList(ts, change.TrailerApprovedBy), WorkItems: change.TrailerList(ts, change.TrailerWorkItem),
		BreakGlass: change.Trailer(ts, change.TrailerBreakGlass), Mr: change.Trailer(ts, change.TrailerReviewedIn),
		Type: api.HistoryEntryTypeDirect,
	}
	for _, t := range ts {
		en.Trailers = append(en.Trailers, api.Trailer{Key: t.Key, Value: t.Value})
	}
	if _, title, mr := change.TitleFromMergeSubject(e.Subject); title != "" {
		en.Title = title
		if en.Mr == "" && mr > 0 {
			en.Mr = fmt.Sprintf("!%d", mr)
		}
	}
	if t := change.Trailer(ts, change.TrailerTitle); t != "" {
		en.Title = t
	}
	if en.Author == "" {
		en.Author = histUserOfEmail(e.AuthorEmail)
	}
	if r, ok := change.ParseLabRun(change.Trailer(ts, change.TrailerLabRun)); ok {
		en.LabRun = change.LabRunToAPI(r)
	}
	switch {
	case en.BreakGlass != "":
		en.Type = api.HistoryEntryTypeBreakGlass
	case en.Change != "":
		en.Type = api.HistoryEntryTypeChange
		en.State = api.ChangeState(change.Merged)
	}

	// Files, entities and kinds touched by the commit (against the first parent).
	files, _ := g.ChangedFiles(ctx, e.SHA)
	ents, kindSet, refSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range files {
		en.Files = append(en.Files, f.Path)
		rev := e.SHA
		if f.Code == "D" && it.parent != "" {
			rev = it.parent
		}
		content, err := g.ShowFile(ctx, rev, f.Path)
		if err != nil {
			continue
		}
		pf := repo.ParseFile(f.Path, content)
		for _, d := range pf.Docs {
			// Lab reports of the change itself are not entities.
			if d.Name != "" && !strings.HasPrefix(f.Path, change.MetaDir+"/") && !strings.HasPrefix(f.Path, "kb/labs/") {
				ents[d.Name] = true
			}
			if d.Kind == "Node" {
				for _, r := range histNodeRefs(d.Data) {
					refSet[r] = true
				}
			}
			if s.d.Kinds != nil {
				if k, ok := s.d.Kinds.Match(f.Path, d); ok {
					kindSet[k.Name] = true
				}
			}
		}
	}
	for n := range ents {
		en.Entities = append(en.Entities, n)
	}
	sort.Strings(en.Entities)
	for k := range kindSet {
		it.kinds = append(it.kinds, k)
	}
	sort.Strings(it.kinds)
	for r := range refSet {
		if !ents[r] {
			it.refs = append(it.refs, r)
		}
	}
	sort.Strings(it.refs)
	it.entry = en

	histCache.Lock()
	if len(histCache.commits) > 5000 {
		histCache.commits = map[string]*histItem{}
	}
	histCache.commits[e.SHA] = it
	histCache.Unlock()
	return it
}

// histNodeRefs returns the rack and cluster of a Node document (v1beta1
// spec.location.rack or v1alpha1 spec.rack).
func histNodeRefs(data map[string]any) []string {
	spec, _ := data["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	add(spec["cluster"])
	add(spec["rack"])
	if loc, ok := spec["location"].(map[string]any); ok {
		add(loc["rack"])
	}
	return out
}

// userOfEmail maps dennis@pk.example to dennis.
func histUserOfEmail(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// matches applies the history filters.
func (it *histItem) matches(p api.ListHistoryParams) bool {
	e := it.entry
	if p.Entity != "" && !histHasFold(e.Entities, p.Entity) && !histHasFold(it.refs, p.Entity) {
		return false
	}
	if p.Author != "" && !strings.EqualFold(e.Author, p.Author) && !strings.EqualFold(e.Agent, p.Author) {
		return false
	}
	if p.Kind != "" && !histHasFold(it.kinds, p.Kind) {
		return false
	}
	if p.Path != "" {
		ok := false
		for _, f := range e.Files {
			if f == p.Path || strings.HasPrefix(f, strings.TrimSuffix(p.Path, "/")+"/") {
				ok = true
				break
			}
			if m, _ := doublestar.Match(p.Path, f); m {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q := strings.ToLower(strings.TrimSpace(p.Q)); q != "" {
		hay := strings.ToLower(e.Title + "\n" + e.Intent + "\n" + e.Change + "\n" + it.message + "\n" + strings.Join(e.Entities, " "))
		for _, w := range strings.Fields(q) {
			if !strings.Contains(hay, w) {
				return false
			}
		}
	}
	return true
}

func histHasFold(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// changeStates returns the derived state of the recent changes (deployed,
// verified come from Flux).
func (s *Server) changeStates(ctx context.Context) map[string]api.ChangeState {
	out := map[string]api.ChangeState{}
	svc, err := s.changeSvc()
	if err != nil {
		return out
	}
	list, err := svc.List(ctx, true)
	if err != nil {
		return out
	}
	for _, c := range list {
		out[c.ID] = api.ChangeState(c.State)
	}
	return out
}

func (s *Server) ListHistory(ctx context.Context, req api.ListHistoryRequestObject) (api.ListHistoryResponseObject, error) {
	h, err := s.history(ctx)
	if err != nil {
		return nil, err
	}
	limit := req.Params.Limit
	if limit <= 0 {
		limit = 50
	}
	states := s.changeStates(ctx)
	out := api.HistoryList{Entries: []api.HistoryEntry{}}
	for _, it := range h.items {
		if !it.matches(req.Params) {
			continue
		}
		out.Total++
		if len(out.Entries) >= limit {
			continue
		}
		e := it.entry
		if st, ok := states[e.Change]; ok && e.Change != "" {
			e.State = st
		}
		out.Entries = append(out.Entries, e)
	}
	return api.ListHistory200JSONResponse(out), nil
}

// shaParam is what GET /history/{sha} takes: a full or abbreviated commit
// sha. Anything else (for example "--output=<file>") never reaches git.
var shaParam = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

func (s *Server) GetHistoryEntry(ctx context.Context, req api.GetHistoryEntryRequestObject) (api.GetHistoryEntryResponseObject, error) {
	h, err := s.history(ctx)
	if err != nil {
		return nil, err
	}
	if !shaParam.MatchString(req.Sha) {
		return nil, Errorf(404, "commit %s not found", req.Sha)
	}
	var it *histItem
	for _, x := range h.items {
		if x.entry.Sha == req.Sha || (len(req.Sha) >= 7 && strings.HasPrefix(x.entry.Sha, req.Sha)) {
			it = x
			break
		}
	}
	g := gitx.Open(h.dir)
	if it == nil {
		log, err := g.Log(ctx, req.Sha, "-n", "1")
		if err != nil || len(log) == 0 {
			return nil, Errorf(404, "commit %s not found", req.Sha)
		}
		it = s.histItem(ctx, g, log[0])
	}
	out := api.HistoryDetail{Entry: it.entry, Message: it.message, Parent: it.parent,
		Files: []api.FileDiff{}, Semantic: []api.SemanticLine{}}
	svc, _ := s.changeSvc()
	if it.entry.Change != "" && svc != nil {
		if ch, err := svc.Get(ctx, it.entry.Change); err == nil && ch.MergeSHA == it.entry.Sha {
			out.Entry.State = api.ChangeState(ch.State)
			out.Rollout = change.SummaryToAPI(ch.Summary).Rollout
		}
	}
	base := it.parent
	if base == "" {
		base = emptyTreeSHA
	}

	// Semantic and source diff through the renderer (kind Describe hooks,
	// masked secrets); without it a plain git diff.
	if s.d.Renderer != nil && svc != nil && it.parent != "" {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		bs, err1 := svc.Source(rctx, it.parent)
		hs, err2 := svc.Source(rctx, it.entry.Sha)
		if err1 == nil && err2 == nil {
			if imp, err := s.d.Renderer.Impact(rctx, bs, hs); err == nil {
				for _, l := range imp.Semantic {
					out.Semantic = append(out.Semantic, histSemanticToAPI(l))
				}
				for _, f := range imp.Files {
					out.Files = append(out.Files, api.FileDiff{Path: f.Path, OldPath: f.OldPath, Status: api.FileChange(f.Status),
						Unified: f.Unified, Old: f.Old, New: f.New})
				}
				return api.GetHistoryEntry200JSONResponse(out), nil
			}
		}
	}
	files, _ := g.DiffNames(ctx, base, it.entry.Sha)
	for _, f := range files {
		unified, _ := g.Diff(ctx, base, it.entry.Sha, f.Path)
		out.Files = append(out.Files, api.FileDiff{Path: f.Path, OldPath: f.OldPath, Status: api.FileChange(histStatusWord(f.Code)), Unified: unified})
	}
	out.Semantic = append(out.Semantic, s.describeCommit(ctx, g, base, it.entry.Sha, files)...)
	return api.GetHistoryEntry200JSONResponse(out), nil
}

const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func histSemanticToAPI(l kinds.SemanticLine) api.SemanticLine {
	return api.SemanticLine{Subject: l.Subject, Text: l.Text, Op: api.Op(l.Op), Path: l.Path, Line: l.Line, Kind: l.Kind}
}

// describeCommit runs the kind Describe hooks over the changed documents
// (fallback when the renderer is not wired).
func (s *Server) describeCommit(ctx context.Context, g *gitx.Repo, base, head string, files []gitx.FileStatus) []api.SemanticLine {
	if s.d.Kinds == nil {
		return nil
	}
	var out []api.SemanticLine
	for _, f := range files {
		oldPath := f.Path
		if f.OldPath != "" {
			oldPath = f.OldPath
		}
		olds, news := map[string]*kinds.Doc{}, map[string]*kinds.Doc{}
		if b, err := g.ShowFile(ctx, base, oldPath); err == nil {
			for _, d := range repo.ParseFile(oldPath, b).Docs {
				olds[d.Key()] = d
			}
		}
		if b, err := g.ShowFile(ctx, head, f.Path); err == nil {
			for _, d := range repo.ParseFile(f.Path, b).Docs {
				news[d.Key()] = d
			}
		}
		keys := map[string]bool{}
		for k := range olds {
			keys[k] = true
		}
		for k := range news {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			o, n := olds[k], news[k]
			d := n
			if d == nil {
				d = o
			}
			kd, ok := s.d.Kinds.Match(d.Path, d)
			if !ok || kd.Describe == nil {
				continue
			}
			for _, l := range kd.Describe(o, n) {
				out = append(out, histSemanticToAPI(l))
			}
		}
	}
	return out
}

func histStatusWord(code string) string {
	switch code {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	case "?":
		return "untracked"
	}
	return "modified"
}

func (s *Server) GetRefTree(ctx context.Context, req api.GetRefTreeRequestObject) (api.GetRefTreeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	src, err := svc.Source(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	entries, err := s.buildTree(src.Dir, nil, nil)
	if err != nil {
		return nil, err
	}
	return api.GetRefTree200JSONResponse(api.FileTree{Ref: req.Ref, Sha: src.SHA, Entries: entries}), nil
}

func (s *Server) ReadFileAtRef(ctx context.Context, req api.ReadFileAtRefRequestObject) (api.ReadFileAtRefResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	p, err := repoRelPath(string(req.Params.Path))
	if err != nil {
		return nil, err
	}
	src, err := svc.Source(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(filepath.Join(src.Dir, filepath.FromSlash(p)))
	if err != nil {
		if os.IsNotExist(err) {
			fc := s.fileContent(ctx, src.Dir, p, nil)
			fc.Exists, fc.ReadOnly, fc.Ref = false, true, req.Ref
			return api.ReadFileAtRef200JSONResponse(fc), nil
		}
		return nil, err
	}
	stored, converted := "", ""
	if req.Params.Convert && s.d.Kinds != nil {
		if out, from, ok := s.convertFile(p, content); ok {
			content, converted = out, from
		}
	}
	if pf := repo.ParseFile(p, content); len(pf.Docs) > 0 {
		stored = pf.Docs[0].APIVersion
	}
	fc := s.fileContent(ctx, src.Dir, p, content)
	fc.Exists, fc.ReadOnly, fc.Ref, fc.Sha = true, true, req.Ref, fileBlobSHA(content)
	if converted != "" {
		fc.ConvertedFrom, fc.StoredVersion = converted, converted
	} else if fc.StoredVersion == "" {
		fc.StoredVersion = stored
	}
	return api.ReadFileAtRef200JSONResponse(fc), nil
}

// convertFile converts the documents of old kind versions to the newest
// version. It returns the new content and the old apiVersion.
func (s *Server) convertFile(p string, content []byte) ([]byte, string, bool) {
	pf := repo.ParseFile(p, content)
	if pf.Language != "yaml" || len(pf.Docs) == 0 {
		return nil, "", false
	}
	var parts []string
	from := ""
	for _, d := range pf.Docs {
		k, ok := s.d.Kinds.Match(p, d)
		data := d.Data
		if ok && k.Convert != nil && d.APIVersion != "" && d.APIVersion != k.APIVersion() {
			conv, err := k.Convert(d.Version(), d.Data)
			if err != nil {
				return nil, "", false
			}
			if from == "" {
				from = d.APIVersion
			}
			data = conv
			if _, has := data["apiVersion"]; !has {
				data["apiVersion"] = k.APIVersion()
			}
		}
		b, err := histEncode(data, d.Node)
		if err != nil {
			return nil, "", false
		}
		parts = append(parts, string(b))
	}
	if from == "" {
		return nil, "", false
	}
	return []byte(strings.Join(parts, "---\n")), from, true
}

// histEncode encodes a converted document with 2-space indent, in the key
// order and flow style of the stored document; new keys take the place of
// the first removed key of their mapping (rack, unit → location).
func histEncode(data map[string]any, orig *yaml.Node) ([]byte, error) {
	n, err := histOrdered(data, orig)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func histOrdered(v any, orig *yaml.Node) (*yaml.Node, error) {
	for orig != nil && (orig.Kind == yaml.DocumentNode || orig.Kind == yaml.AliasNode) {
		if orig.Kind == yaml.AliasNode {
			orig = orig.Alias
			continue
		}
		if len(orig.Content) == 0 {
			orig = nil
			break
		}
		orig = orig.Content[0]
	}
	switch x := v.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		var origKeys []string
		origVal := map[string]*yaml.Node{}
		if orig != nil && orig.Kind == yaml.MappingNode {
			n.Style = orig.Style & yaml.FlowStyle
			for i := 0; i+1 < len(orig.Content); i += 2 {
				origKeys = append(origKeys, orig.Content[i].Value)
				origVal[orig.Content[i].Value] = orig.Content[i+1]
			}
		}
		var fresh []string
		for k := range x {
			if _, ok := origVal[k]; !ok {
				fresh = append(fresh, k)
			}
		}
		sort.Strings(fresh)
		var keys []string
		placed := false
		for _, k := range origKeys {
			if _, ok := x[k]; ok {
				keys = append(keys, k)
			} else if !placed {
				keys, placed = append(keys, fresh...), true
			}
		}
		if !placed {
			keys = append(keys, fresh...)
		}
		for _, k := range keys {
			val, err := histOrdered(x[k], origVal[k])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, val)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		if orig != nil && orig.Kind == yaml.SequenceNode {
			n.Style = orig.Style & yaml.FlowStyle
		}
		for i, e := range x {
			var o *yaml.Node
			if orig != nil && orig.Kind == yaml.SequenceNode && i < len(orig.Content) {
				o = orig.Content[i]
			}
			val, err := histOrdered(e, o)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, val)
		}
		return n, nil
	}
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	if orig != nil && orig.Kind == yaml.ScalarNode && n.Kind == yaml.ScalarNode && orig.Value == n.Value {
		n.Style = orig.Style
	}
	return n, nil
}
