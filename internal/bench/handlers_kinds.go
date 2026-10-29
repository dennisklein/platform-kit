package bench

// Kinds, entities, racks, KB and the work board (area B).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/kinds/emulation"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/node"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// ---------------------------------------------------------------- shared state

// bState is the area B state of a server: the render engine (shared with
// Deps.Renderer when that is a *render.Engine) and small caches.
type bState struct {
	reg kinds.Registry
	eng *render.Engine
	pol policy.Engine

	mu      sync.Mutex
	changed map[string][]string // base key + head key -> changed paths
	order   []string
}

var bStates sync.Map // *Server -> *bState

func (s *Server) b() *bState {
	if v, ok := bStates.Load(s); ok {
		return v.(*bState)
	}
	reg := s.d.Kinds
	if reg == nil {
		reg = all.Registry()
	}
	st := &bState{reg: reg, changed: map[string][]string{}}
	if e, ok := s.d.Renderer.(*render.Engine); ok {
		st.eng = e
	} else {
		st.eng = render.New(reg)
	}
	st.pol = s.d.Policy
	if st.pol == nil {
		st.pol = policy.NewEngine(func(ctx context.Context) (string, error) {
			if s.d.Changes == nil {
				return "", unavailable("the change service")
			}
			return s.d.Changes.MainDir(ctx)
		})
	}
	v, _ := bStates.LoadOrStore(s, st)
	return v.(*bState)
}

// bRenderer returns the renderer for impacts: Deps.Renderer when set (it may
// wrap the engine), else the shared engine.
func (s *Server) bRenderer() render.Renderer {
	if s.d.Renderer != nil {
		return s.d.Renderer
	}
	return s.b().eng
}

// bTree is a parsed tree at a ref. For a change id it also has the base
// (merge base with the target) so views can highlight what the change does.
type bTree struct {
	Ref      string
	Src      render.Source
	Repo     *repo.Repo
	ChangeID string
	BaseSrc  render.Source
	Base     *repo.Repo
}

func (s *Server) bTreeAt(ctx context.Context, ref string) (*bTree, error) {
	if ref == "" {
		ref = "main"
	}
	if s.d.Changes == nil {
		return nil, unavailable("the change service")
	}
	eng := s.b().eng
	t := &bTree{Ref: ref}
	if change.ValidID(ref) {
		base, head, err := s.d.Changes.Sources(ctx, ref)
		if err != nil {
			return nil, err
		}
		t.ChangeID, t.Src, t.BaseSrc = ref, head, base
		if t.Base, err = eng.Load(base); err != nil {
			return nil, err
		}
	} else {
		src, err := s.d.Changes.Source(ctx, ref)
		if err != nil {
			return nil, err
		}
		t.Src = src
	}
	r, err := eng.Load(t.Src)
	if err != nil {
		return nil, err
	}
	t.Repo = r
	return t, nil
}

// bChangedPaths returns the paths that differ between two sources (cached by
// source keys).
func (s *Server) bChangedPaths(base, head render.Source) ([]string, error) {
	st := s.b()
	key := base.Key + "|" + head.Key
	cacheable := base.Key != "" && head.Key != ""
	if cacheable {
		st.mu.Lock()
		if p, ok := st.changed[key]; ok {
			st.mu.Unlock()
			return p, nil
		}
		st.mu.Unlock()
	}
	br, err := st.eng.Load(base)
	if err != nil {
		return nil, err
	}
	hr, err := st.eng.Load(head)
	if err != nil {
		return nil, err
	}
	paths := render.ChangedPaths(br, hr)
	if cacheable {
		st.mu.Lock()
		st.changed[key] = paths
		st.order = append(st.order, key)
		if len(st.order) > 128 {
			delete(st.changed, st.order[0])
			st.order = st.order[1:]
		}
		st.mu.Unlock()
	}
	return paths, nil
}

// bOpenChange is an open change with the paths it changes.
type bOpenChange struct {
	Head  change.Head
	Base  render.Source
	Tip   render.Source
	Paths []string
}

// bOpenChanges lists the open changes with their changed paths. Changes whose
// tree cannot be read are left out.
func (s *Server) bOpenChanges(ctx context.Context) []bOpenChange {
	if s.d.Changes == nil {
		return nil
	}
	heads, err := s.d.Changes.Heads(ctx)
	if err != nil {
		s.log.Debug("heads", "err", err)
		return nil
	}
	var out []bOpenChange
	for _, h := range heads {
		base, tip, err := s.d.Changes.Sources(ctx, h.ID)
		if err != nil {
			continue
		}
		paths, err := s.bChangedPaths(base, tip)
		if err != nil {
			continue
		}
		out = append(out, bOpenChange{Head: h, Base: base, Tip: tip, Paths: paths})
	}
	return out
}

// bLinkData is what backlinks need from area A: changes by id, history
// entries and open changes with their paths. bGather fetches them in
// parallel (each one is a git or forge round trip).
type bLinkData struct {
	idx  map[string]change.Summary
	hist []api.HistoryEntry
	open []bOpenChange
}

func (s *Server) bGather(ctx context.Context, hp *api.ListHistoryParams, open bool) bLinkData {
	var out bLinkData
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); out.idx = s.bChangeIndex(ctx) }()
	if hp != nil {
		wg.Add(1)
		go func() { defer wg.Done(); out.hist = s.bHistory(ctx, *hp) }()
	}
	if open {
		wg.Add(1)
		go func() { defer wg.Done(); out.open = s.bOpenChanges(ctx) }()
	}
	wg.Wait()
	return out
}

func bLinkOfOpen(oc bOpenChange, idx map[string]change.Summary) api.ChangeLink {
	if c, ok := idx[oc.Head.ID]; ok {
		return bLinkOfSummary(c)
	}
	return api.ChangeLink{Id: oc.Head.ID, Title: oc.Head.Title, Author: oc.Head.Author, State: api.ChangeState(oc.Head.State)}
}

// bChangeIndex returns the changes by id (open, recent merges, closed).
func (s *Server) bChangeIndex(ctx context.Context) map[string]change.Summary {
	out := map[string]change.Summary{}
	if s.d.Changes == nil {
		return out
	}
	list, err := s.d.Changes.List(ctx, true)
	if err != nil {
		s.log.Debug("list changes", "err", err)
		return out
	}
	for _, c := range list {
		out[c.ID] = c
	}
	return out
}

// bHistory calls the history handler (area A); errors give no entries.
func (s *Server) bHistory(ctx context.Context, p api.ListHistoryParams) (out []api.HistoryEntry) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn("history for backlinks", "panic", r)
			out = nil
		}
	}()
	res, err := s.ListHistory(ctx, api.ListHistoryRequestObject{Params: p})
	if err != nil {
		return nil
	}
	if l, ok := res.(api.ListHistory200JSONResponse); ok {
		return l.Entries
	}
	return nil
}

func bLinkOfSummary(c change.Summary) api.ChangeLink {
	l := api.ChangeLink{Id: c.ID, Title: c.Title, Author: c.Author, State: api.ChangeState(c.State), Sha: c.MergeSHA}
	if c.MR != nil && c.MR.Number > 0 {
		l.Mr = fmt.Sprintf("!%d", c.MR.Number)
	}
	l.Date = c.UpdatedAt
	if !c.MergedAt.IsZero() {
		l.Date = c.MergedAt
	}
	return l
}

func bLinkOfHistory(e api.HistoryEntry) api.ChangeLink {
	id := e.Change
	if id == "" {
		id = e.Short
	}
	return api.ChangeLink{Id: id, Title: e.Title, Author: e.Author, Date: e.Date, Mr: e.Mr, Sha: e.Sha, State: e.State}
}

// bChangeLinks merges links by id; the first one of an id wins, later ones
// only fill missing fields.
type bChangeLinks struct {
	list []api.ChangeLink
	idx  map[string]int
}

func (c *bChangeLinks) add(l api.ChangeLink) {
	if l.Id == "" {
		return
	}
	if c.idx == nil {
		c.idx = map[string]int{}
	}
	if i, ok := c.idx[l.Id]; ok {
		x := &c.list[i]
		if x.Title == "" || x.Title == x.Id {
			x.Title = l.Title
		}
		if x.State == "" {
			x.State = l.State
		}
		if x.Mr == "" {
			x.Mr = l.Mr
		}
		if x.Sha == "" {
			x.Sha = l.Sha
		}
		if x.Author == "" {
			x.Author = l.Author
		}
		if x.Date.IsZero() {
			x.Date = l.Date
		}
		return
	}
	c.idx[l.Id] = len(c.list)
	c.list = append(c.list, l)
}

func (c *bChangeLinks) get() []api.ChangeLink {
	if c.list == nil {
		return []api.ChangeLink{}
	}
	return c.list
}

// ---------------------------------------------------------------- conversions

func bAPIProblems(ps []kinds.Problem) []api.Problem {
	out := make([]api.Problem, 0, len(ps))
	for _, p := range ps {
		out = append(out, api.Problem{Path: p.Path, Line: p.Line, Col: p.Col, Severity: api.ProblemSeverity(p.Severity),
			Message: p.Message, Source: api.ProblemSource(p.Source), Field: p.Field, Code: p.Code})
	}
	return out
}

func bAPISemantic(ls []kinds.SemanticLine) []api.SemanticLine {
	out := make([]api.SemanticLine, 0, len(ls))
	for _, l := range ls {
		out = append(out, api.SemanticLine{Subject: l.Subject, Text: l.Text, Op: api.Op(l.Op), Path: l.Path, Line: l.Line, Kind: l.Kind})
	}
	return out
}

// bConvertJSON converts between types with the same JSON shape (domain types
// and generated API types with anonymous structs).
func bConvertJSON(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func bNotFound(format string, args ...any) error {
	return Errorf(http.StatusNotFound, format, args...)
}

func bBadRequest(format string, args ...any) error {
	return Errorf(http.StatusBadRequest, format, args...)
}

// ---------------------------------------------------------------- kinds

func (s *Server) ListKinds(ctx context.Context, req api.ListKindsRequestObject) (api.ListKindsResponseObject, error) {
	out := api.ListKinds200JSONResponse{}
	for _, k := range s.b().reg.List() {
		out = append(out, apiKind(k))
	}
	return out, nil
}

func apiKind(k *kinds.Kind) api.Kind {
	a := api.Kind{Name: k.Name, Group: k.Group, ApiVersion: k.APIVersion(), Own: k.Own, Description: k.Description,
		TruthClass: api.TruthClass(k.Truth), ApplyPath: api.ApplyPath(k.ApplyPath),
		Versions: bNonNil(k.Versions), Views: bNonNil(k.Views), Paths: bNonNil(k.Paths), Hooks: []api.KindHooks{}}
	if a.ApplyPath == "" {
		a.ApplyPath = api.ApplyPathNone
	}
	if len(a.Versions) == 0 && k.Version != "" {
		a.Versions = []string{k.Version}
	}
	for _, h := range k.Hooks() {
		a.Hooks = append(a.Hooks, api.KindHooks(h))
	}
	if k.Schema != nil {
		a.SchemaUrl = schemaURL(k, "")
	}
	return a
}

func schemaURL(k *kinds.Kind, version string) string {
	u := BasePath + "/kinds/" + k.Name + "/schema"
	if version != "" && version != k.Version {
		u += "?version=" + version
	}
	return u
}

func bNonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) GetKindSchema(ctx context.Context, req api.GetKindSchemaRequestObject) (api.GetKindSchemaResponseObject, error) {
	k, ok := s.b().reg.Get(req.Kind)
	if !ok {
		return nil, bNotFound("unknown kind %s", req.Kind)
	}
	if k.Schema == nil {
		return nil, bNotFound("kind %s has no schema", k.Name)
	}
	v := req.Params.Version
	if v != "" && v != k.Version && !slices.Contains(k.Versions, v) {
		return nil, bNotFound("kind %s has no version %s (versions: %s)", k.Name, v, strings.Join(k.Versions, ", "))
	}
	raw, err := k.Schema(v)
	if err != nil {
		return nil, err
	}
	var out api.GetKindSchema200JSONResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- entities

func (s *Server) ListEntities(ctx context.Context, req api.ListEntitiesRequestObject) (api.ListEntitiesResponseObject, error) {
	t, err := s.bTreeAt(ctx, req.Params.Ref)
	if err != nil {
		return nil, err
	}
	want := ""
	if req.Params.Kind != "" {
		k, ok := s.b().reg.Get(req.Params.Kind)
		if !ok {
			return nil, bNotFound("unknown kind %s", req.Params.Kind)
		}
		want = k.Name
	}
	errs := map[string]bool{}
	for _, p := range t.Repo.Problems() {
		if p.Severity == kinds.SevError {
			errs[p.Path] = true
		}
	}
	out := api.ListEntities200JSONResponse{}
	for _, d := range t.Repo.Docs() {
		k := t.Repo.KindOf(d)
		if k == nil || want != "" && k.Name != want {
			continue
		}
		e := api.EntitySummary{Kind: k.Name, ApiVersion: d.APIVersion, Name: d.Name, Path: d.Path, Title: titleOf(d),
			Cluster: clusterOf(d), Role: kindutil.Str(d.Data, "spec", "role"), Status: "ok"}
		if e.Name == "" {
			e.Name = d.Path
		}
		if l := labelsOf(d); len(l) > 0 {
			e.Labels = l
		}
		if errs[d.Path] {
			e.Status = "error"
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func titleOf(d *kinds.Doc) string {
	if t := kindutil.Str(d.Data, "title"); t != "" {
		return t
	}
	if t := kindutil.Str(d.Data, "spec", "title"); t != "" {
		return t
	}
	if d.Kind == node.Name {
		if r := kindutil.Str(d.Data, "spec", "role"); r != "" {
			return node.RoleWords(node.Role(r))
		}
	}
	return ""
}

var clusterPathRe = regexp.MustCompile(`^clusters/([^/]+)/`)

func clusterOf(d *kinds.Doc) string {
	if c := kindutil.Str(d.Data, "spec", "cluster"); c != "" {
		return c
	}
	if m := clusterPathRe.FindStringSubmatch(d.Path); m != nil {
		return m[1]
	}
	return ""
}

func labelsOf(d *kinds.Doc) map[string]string {
	m, _ := kindutil.Get(d.Data, "metadata", "labels").(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// bFindDoc finds a document by kind and name (articles by path).
func bFindDoc(r *repo.Repo, kind, name string) *kinds.Doc {
	if d := r.Find(kind, name); d != nil {
		return d
	}
	if kind == "" {
		return kindutil.FindEntity(r, name)
	}
	return nil
}

func (s *Server) GetEntity(ctx context.Context, req api.GetEntityRequestObject) (api.GetEntityResponseObject, error) {
	k, ok := s.b().reg.Get(req.Kind)
	if !ok {
		return nil, bNotFound("unknown kind %s", req.Kind)
	}
	t, err := s.bTreeAt(ctx, req.Params.Ref)
	if err != nil {
		return nil, err
	}
	all := bFindDocs(t.Repo, k.Name, req.Name)
	d := bPickDoc(all, req.Params.Cluster, req.Params.Path)
	if d == nil {
		where := t.Ref
		if req.Params.Cluster != "" {
			where = "cluster " + req.Params.Cluster + " of " + t.Ref
		}
		if req.Params.Path != "" {
			where = req.Params.Path + " in " + t.Ref
		}
		if t.Base != nil && len(all) == 0 {
			if bd := bPickDoc(bFindDocs(t.Base, k.Name, req.Name), req.Params.Cluster, req.Params.Path); bd != nil {
				return nil, &HTTPError{Status: http.StatusNotFound, Msg: fmt.Sprintf("%s %s is deleted in %s", k.Name, req.Name, t.ChangeID)}
			}
		}
		return nil, bNotFound("%s %s not found in %s", k.Name, req.Name, where)
	}
	e, err := s.entity(ctx, t, k, d)
	if err != nil {
		return nil, err
	}
	e.Cluster = clusterOf(d)
	for _, o := range all {
		if o != d {
			e.Alternatives = append(e.Alternatives, api.EntitySummary{Kind: k.Name, ApiVersion: o.APIVersion, Name: o.Name,
				Path: o.Path, Title: titleOf(o), Cluster: clusterOf(o)})
		}
	}
	return api.GetEntity200JSONResponse(e), nil
}

// bFindDocs returns all documents of a kind with a name, in path order
// (third-party objects such as HelmRelease cilium exist once per cluster).
// Exact names win over case-insensitive matches.
func bFindDocs(r *repo.Repo, kind, name string) []*kinds.Doc {
	var exact, fold []*kinds.Doc
	for _, d := range r.ByKind(kind) {
		switch {
		case d.Name == name:
			exact = append(exact, d)
		case strings.EqualFold(d.Name, name):
			fold = append(fold, d)
		}
	}
	out := exact
	if len(out) == 0 {
		out = fold
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// bPickDoc picks one of several documents by cluster or path ("" = any);
// without both it is the first in path order.
func bPickDoc(docs []*kinds.Doc, cluster, path string) *kinds.Doc {
	for _, d := range docs {
		if cluster != "" && clusterOf(d) != cluster {
			continue
		}
		if path != "" && d.Path != path {
			continue
		}
		return d
	}
	return nil
}

func (s *Server) entity(ctx context.Context, t *bTree, k *kinds.Kind, d *kinds.Doc) (api.Entity, error) {
	r := t.Repo
	e := api.Entity{Kind: k.Name, ApiVersion: k.APIVersion(), Name: d.Name, Path: d.Path, Title: titleOf(d),
		Fields: []api.EntityField{}, References: []api.EntityRef{}, Problems: bAPIProblems(r.ProblemsOf(d.Path))}
	if t.Ref != "main" {
		e.Ref = t.Ref
	}
	if e.Name == "" {
		e.Name = d.Path
	}
	if d.APIVersion != "" && d.APIVersion != k.APIVersion() {
		e.StoredVersion = d.APIVersion
	}
	e.ApplyPath = applyPathText(k)
	e.Backlinks.Articles = []api.ArticleSummary{}
	e.Backlinks.Changes = []api.ChangeLink{}
	e.Backlinks.WorkItems = []api.WorkItem{}
	e.Backlinks.Entities = []api.EntityRef{}

	// Masked data and fields.
	masked := maskedDoc(s.b().reg, r, d)
	e.Data = masked.Data
	sch := repo.SchemaMap(k, bVersionOr(k, d))
	e.Fields = entityFields(k, masked, sch)

	if k.Name == node.Name {
		deriveFixture(r, d, e.Fields)
		inv := node.LoadInventory(r)
		e.Ports = []api.PortCheck{}
		for _, c := range inv.CheckPorts(d.Name) {
			e.Ports = append(e.Ports, api.PortCheck{Port: c.Port, Cable: c.Cable, Peer: c.Peer, Network: c.Network, Ok: c.OK, Message: c.Message})
		}
	}

	// References out and back.
	ix := refIndexOf(r)
	e.References = append(e.References, ix.out[d]...)
	for _, b := range ix.in[docKey(d)] {
		switch b.doc.Kind {
		case "Article":
			e.Backlinks.Articles = append(e.Backlinks.Articles, articleSummary(b.doc))
		case "WorkItem":
		default:
			e.Backlinks.Entities = append(e.Backlinks.Entities, api.EntityRef{Field: b.field, Kind: b.doc.Kind, Name: b.doc.Name, Path: b.doc.Path, Exists: true})
		}
	}

	// Work items that name the entity (status from the changes).
	ld := s.bGather(ctx, &api.ListHistoryParams{Entity: d.Name, Limit: 20}, true)
	board := s.workBoard(ctx, r, ld.idx, nil)
	for _, it := range board.Items {
		if slices.ContainsFunc(it.Assets, func(a string) bool { return strings.EqualFold(a, d.Name) }) {
			e.Backlinks.WorkItems = append(e.Backlinks.WorkItems, it)
		}
	}

	// Changes: open changes that touch the file, then history.
	var links bChangeLinks
	for _, oc := range ld.open {
		if slices.Contains(oc.Paths, d.Path) {
			links.add(bLinkOfOpen(oc, ld.idx))
		}
	}
	for _, h := range ld.hist {
		links.add(bLinkOfHistory(h))
	}
	e.Backlinks.Changes = links.get()

	// The change view: status of the file and the semantic diff.
	if t.ChangeID != "" && t.Base != nil {
		bf, hf := t.Base.File(d.Path), r.File(d.Path)
		status := api.FileChange("")
		switch {
		case bf == nil && hf != nil:
			status = api.FileChangeAdded
		case bf != nil && hf != nil && !bytes.Equal(bf.Content, hf.Content):
			status = api.FileChangeModified
		}
		if status != "" {
			e.Change.Id = t.ChangeID
			e.Change.Status = status
			if imp, err := s.bRenderer().Impact(ctx, t.BaseSrc, t.Src); err == nil {
				e.Semantic = []api.SemanticLine{}
				for _, l := range imp.Semantic {
					if l.Path == d.Path {
						e.Semantic = append(e.Semantic, bAPISemantic([]kinds.SemanticLine{l})...)
					}
				}
			}
		}
	}
	return e, nil
}

// deriveFixture fills the derived emulation field of a node from the
// emulation profile (.pk/emulation.yaml) when the node file does not set it.
func deriveFixture(r *repo.Repo, d *kinds.Doc, fields []api.EntityField) {
	for i := range fields {
		f := &fields[i]
		if f.Field != "spec.bmc.fixture" || f.Value != nil {
			continue
		}
		for _, pd := range r.ByKind(emulation.Name) {
			var p emulation.EmulationProfile
			if bConvertJSON(pd.Data, &p) != nil {
				continue
			}
			i := p.RuleIndex(kindutil.Str(d.Data, "spec", "model"), kindutil.Str(d.Data, "spec", "role"))
			if i < 0 {
				f.Display = "no emulation rule matches the model"
				f.Source = api.SourceRef{Path: pd.Path, Line: pd.Line}
				return
			}
			f.Display = p.Spec.Machines[i].Describe()
			f.Value = f.Display
			line, _ := kindutil.Pos(pd, fmt.Sprintf("spec.machines[%d]", i))
			f.Source = api.SourceRef{Path: pd.Path, Line: line}
			return
		}
	}
}

func bVersionOr(k *kinds.Kind, d *kinds.Doc) string {
	if v := repo.VersionOf(k, d); v != "" {
		return v
	}
	return k.Version
}

func applyPathText(k *kinds.Kind) string {
	switch k.ApplyPath {
	case kinds.ApplyFlux:
		if k.Name == node.Name {
			return "Flux, metal operator"
		}
		return "Flux"
	case kinds.ApplyManual:
		return "manual step after merge"
	case kinds.ApplySync:
		return "org database sync"
	}
	return ""
}

// maskedDoc returns the document with secret values masked.
func maskedDoc(reg kinds.Registry, r *repo.Repo, d *kinds.Doc) *kinds.Doc {
	f := r.File(d.Path)
	if f == nil || f.Language != "yaml" && f.Language != "json" {
		return d
	}
	m, secrets := schema.Mask(f.Content, repo.SchemaLookup(reg, d.Path))
	if len(secrets) == 0 {
		return d
	}
	mf := repo.ParseFile(d.Path, m)
	if d.Index < len(mf.Docs) {
		return mf.Docs[d.Index]
	}
	return d
}

// ---------------------------------------------------------------- fields

// entityFields lists the fields of a document from its schema: one field
// per labelled property (objects with a widget count as one field), in UI
// order. Truth class and owner are inherited from parents.
func entityFields(k *kinds.Kind, d *kinds.Doc, sch map[string]any) []api.EntityField {
	out := []api.EntityField{}
	if sch == nil {
		return out
	}
	def := string(k.Truth)
	if def == "" {
		def = string(kinds.Intent)
	}
	w := fieldWalker{d: d, out: &out}
	w.walk(sch, d.Data, nil, fieldCtx{truth: def}, 0)
	return out
}

type fieldCtx struct {
	truth, owner, group string
}

type fieldWalker struct {
	d   *kinds.Doc
	out *[]api.EntityField
}

var skipTop = map[string]bool{"apiVersion": true, "kind": true, "status": true, "sops": true}

func (w fieldWalker) walk(sch map[string]any, val any, loc []string, fc fieldCtx, depth int) {
	props, _ := sch["properties"].(map[string]any)
	data, _ := val.(map[string]any)
	for _, name := range orderedProps(props) {
		if depth == 0 && skipTop[name] {
			continue
		}
		if len(loc) == 1 && loc[0] == "metadata" && name != "labels" {
			continue
		}
		sub, _ := props[name].(map[string]any)
		v, present := data[name]
		l := append(slices.Clip(loc), name)
		c := fc
		if t, ok := sub[schema.XTruth].(string); ok {
			c.truth = t
		}
		if o, ok := sub[schema.XOwner].(string); ok {
			c.owner = o
		}
		ui, _ := sub[schema.XUI].(map[string]any)
		if g, ok := ui["group"].(string); ok {
			c.group = g
		}
		label, _ := ui["label"].(string)
		widget, _ := ui["widget"].(string)
		_, hasProps := sub["properties"].(map[string]any)
		if hasProps && widget == "" && depth < 6 {
			if present || label == "" {
				if present {
					w.walk(sub, v, l, c, depth+1)
				}
				continue
			}
		}
		if !present && label == "" {
			continue
		}
		if label == "" {
			label = name
		}
		f := api.EntityField{Field: schema.FieldPath(l), Label: label, Truth: api.TruthClass(c.truth), Owner: c.owner, Group: c.group}
		secret, _ := sub[schema.XSecret].(bool)
		if sv, ok := v.(string); ok && (schema.IsSOPS(sv) || schema.IsMasked(sv)) {
			secret = true
		}
		f.Secret = secret
		if present {
			if secret {
				f.Value = schema.MaskText
			} else {
				f.Value = v
			}
			f.Display = displayOf(widget, v, secret)
			line, _ := kindutil.Pos(w.d, f.Field)
			f.Source = api.SourceRef{Path: w.d.Path, Line: line}
		}
		*w.out = append(*w.out, f)
	}
}

// orderedProps returns property names by x-pk-ui order, then name.
func orderedProps(props map[string]any) []string {
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	order := func(n string) int {
		sub, _ := props[n].(map[string]any)
		ui, _ := sub[schema.XUI].(map[string]any)
		switch o := ui["order"].(type) {
		case float64:
			return int(o)
		case int:
			return o
		case int64:
			return int(o)
		}
		return 1000
	}
	sort.SliceStable(names, func(i, j int) bool {
		oi, oj := order(names[i]), order(names[j])
		if oi != oj {
			return oi < oj
		}
		return names[i] < names[j]
	})
	return names
}

func displayOf(widget string, v any, secret bool) string {
	if secret {
		return schema.MaskText
	}
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool, int, int64, float64:
		return fmt.Sprint(x)
	case map[string]any:
		switch widget {
		case "rack-picker":
			s := fmt.Sprintf("%s U%v", kindutil.Str(x, "rack"), kindutil.Get(x, "unit"))
			if size := kindutil.Int(x, "size"); size > 1 {
				s += fmt.Sprintf(" (%dU)", size)
			}
			return s
		case "port-picker":
			return fmt.Sprintf("%s %v", kindutil.Str(x, "switch"), kindutil.Get(x, "port"))
		case "chart":
			sp, _ := x["spec"].(map[string]any)
			if c := kindutil.Str(sp, "chart"); c != "" {
				return strings.TrimSpace(c + " " + kindutil.Str(sp, "version"))
			}
		}
		if t, ok := selectorText(x); ok {
			return t
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			switch y := x[k].(type) {
			case map[string]any:
				if t := displayOf("", y, false); len(y) <= 3 && !strings.Contains(t, "…") {
					parts = append(parts, k+": "+t)
				} else {
					parts = append(parts, k+"=…")
				}
			case []any:
				parts = append(parts, k+"=…")
			default:
				parts = append(parts, fmt.Sprintf("%s=%v", k, y))
			}
		}
		return strings.Join(parts, ", ")
	case []any:
		if t := namesText(x); t != "" {
			return t
		}
		if len(x) == 1 {
			return "1 item"
		}
		return fmt.Sprintf("%d items", len(x))
	}
	return fmt.Sprint(v)
}

// selectorText shows a Kubernetes label selector in words:
// "app=podinfo" or "kubernetes.io/hostname in cn-025 … cn-036 (12)".
func selectorText(m map[string]any) (string, bool) {
	ml, hasML := m["matchLabels"].(map[string]any)
	me, hasME := m["matchExpressions"].([]any)
	if !hasML && !hasME || len(m) > 2 {
		return "", false
	}
	var parts []string
	keys := make([]string, 0, len(ml))
	for k := range ml {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, ml[k]))
	}
	for _, e := range me {
		em, _ := e.(map[string]any)
		vals := kindutil.Strings(em, "values")
		t := kindutil.Str(em, "key") + " " + strings.ToLower(kindutil.Str(em, "operator"))
		switch {
		case len(vals) > 3:
			t += fmt.Sprintf(" %s … %s (%d)", vals[0], vals[len(vals)-1], len(vals))
		case len(vals) > 0:
			t += " " + strings.Join(vals, ", ")
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "; "), true
}

// namesText lists the names of a list of objects ("bmc, p1, p2"), or the
// images of containers; "" when the items have neither.
func namesText(items []any) string {
	var out []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return ""
		}
		switch {
		case kindutil.Str(m, "image") != "":
			out = append(out, kindutil.Str(m, "image"))
		case kindutil.Str(m, "name") != "":
			out = append(out, kindutil.Str(m, "name"))
		default:
			return ""
		}
	}
	return strings.Join(out, ", ")
}

// ---------------------------------------------------------------- references

type backRef struct {
	doc   *kinds.Doc
	field string
}

type refIndex struct {
	out map[*kinds.Doc][]api.EntityRef
	in  map[string][]backRef // kind/name of the target
}

var refCache struct {
	mu    sync.Mutex
	m     map[*repo.Repo]*refIndex
	order []*repo.Repo
}

func docKey(d *kinds.Doc) string { return strings.ToLower(d.Kind + "/" + d.Name) }

// refIndexOf builds (and caches) the references of all documents of a tree.
func refIndexOf(r *repo.Repo) *refIndex {
	refCache.mu.Lock()
	if ix, ok := refCache.m[r]; ok {
		refCache.mu.Unlock()
		return ix
	}
	refCache.mu.Unlock()
	ix := &refIndex{out: map[*kinds.Doc][]api.EntityRef{}, in: map[string][]backRef{}}
	for _, d := range r.Docs() {
		refs := docRefs(r, d)
		ix.out[d] = refs
		for _, ref := range refs {
			if !ref.Exists {
				continue
			}
			t := bFindDoc(r, ref.Kind, ref.Name)
			if t == nil || t == d {
				continue
			}
			ix.in[docKey(t)] = append(ix.in[docKey(t)], backRef{doc: d, field: ref.Field})
		}
	}
	refCache.mu.Lock()
	if refCache.m == nil {
		refCache.m = map[*repo.Repo]*refIndex{}
	}
	refCache.m[r] = ix
	refCache.order = append(refCache.order, r)
	if len(refCache.order) > 16 {
		delete(refCache.m, refCache.order[0])
		refCache.order = refCache.order[1:]
	}
	refCache.mu.Unlock()
	return ix
}

type refHit struct{ field, kind, name string }

// docRefs returns the references of a document: x-pk-ref fields of its
// schema, the rack label, article entities and work item assets and KB
// links.
func docRefs(r *repo.Repo, d *kinds.Doc) []api.EntityRef {
	var hits []refHit
	if k := r.KindOf(d); k != nil {
		walkRefs(repo.SchemaMap(k, bVersionOr(k, d)), d.Data, nil, &hits)
	}
	if rack := kindutil.Str(d.Data, "metadata", "labels", "inventory.pk.io/rack"); rack != "" {
		hits = append(hits, refHit{"metadata.labels[inventory.pk.io/rack]", "Rack", rack})
	}
	switch d.Kind {
	case "Article":
		for i, n := range kindutil.Strings(d.Data, "entities") {
			hits = append(hits, refHit{fmt.Sprintf("entities[%d]", i), "", n})
		}
	case "WorkItem":
		for i, n := range kindutil.Strings(d.Data, "assets") {
			hits = append(hits, refHit{fmt.Sprintf("assets[%d]", i), "", n})
		}
		for i, n := range kindutil.Strings(d.Data, "kb") {
			hits = append(hits, refHit{fmt.Sprintf("kb[%d]", i), "Article", n})
		}
	}
	out := []api.EntityRef{}
	seen := map[string]bool{}
	for _, h := range hits {
		key := h.field + "|" + h.kind + "|" + h.name
		if h.name == "" || seen[key] {
			continue
		}
		seen[key] = true
		e := api.EntityRef{Field: h.field, Kind: h.kind, Name: h.name}
		if t := bFindDoc(r, h.kind, h.name); t != nil {
			e.Exists, e.Kind, e.Name, e.Path = true, t.Kind, t.Name, t.Path
		}
		out = append(out, e)
	}
	return out
}

func walkRefs(sch map[string]any, v any, loc []string, out *[]refHit) {
	if sch == nil || v == nil {
		return
	}
	if ref, ok := sch[schema.XRef].(map[string]any); ok {
		kind, _ := ref["kind"].(string)
		if kind == "Entity" {
			kind = "" // any entity kind
		}
		switch x := v.(type) {
		case string:
			*out = append(*out, refHit{schema.FieldPath(loc), kind, x})
		case []any:
			for i, y := range x {
				if s, ok := y.(string); ok {
					*out = append(*out, refHit{schema.FieldPath(append(slices.Clip(loc), strconv.Itoa(i))), kind, s})
				}
			}
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := sch["properties"].(map[string]any)
		addl, _ := sch["additionalProperties"].(map[string]any)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, _ := props[k].(map[string]any)
			if sub == nil {
				sub = addl
			}
			walkRefs(sub, x[k], append(slices.Clip(loc), k), out)
		}
	case []any:
		if _, isRef := sch[schema.XRef]; isRef {
			return // counted above
		}
		items, _ := sch["items"].(map[string]any)
		for i, y := range x {
			walkRefs(items, y, append(slices.Clip(loc), strconv.Itoa(i)), out)
		}
	}
}

// ---------------------------------------------------------------- racks

func (s *Server) GetRack(ctx context.Context, req api.GetRackRequestObject) (api.GetRackResponseObject, error) {
	t, err := s.bTreeAt(ctx, req.Params.Ref)
	if err != nil {
		return nil, err
	}
	rk, err := rackView(t, req.Name)
	if err != nil {
		return nil, err
	}
	return api.GetRack200JSONResponse(rk), nil
}

func rackView(t *bTree, name string) (api.Rack, error) {
	inv := node.LoadInventory(t.Repo)
	key := strings.ToUpper(name)
	rd := inv.Racks[key]
	occ := inv.InRack(name)
	if rd == nil && len(occ) == 0 {
		return api.Rack{}, bNotFound("rack %s not found", name)
	}
	rk := api.Rack{Name: key, Units: inv.RackUnits[key], Occupants: []api.RackOccupant{}, Free: inv.Free(name), Problems: []api.Problem{}}
	if rk.Units == 0 {
		rk.Units = 47
		rk.Free = []int{}
		used := map[int]bool{}
		for _, o := range occ {
			for u := o.Unit; u <= o.Top(); u++ {
				used[u] = true
			}
		}
		for u := 1; u <= rk.Units; u++ {
			if !used[u] {
				rk.Free = append(rk.Free, u)
			}
		}
	}
	if t.Ref != "main" {
		rk.Ref = t.Ref
	}
	if rd != nil {
		rk.Name, rk.Path = rd.Name, rd.Path
		rk.Row = kindutil.Str(rd.Data, "spec", "row")
		rk.Location = kindutil.Str(rd.Data, "spec", "location")
		rk.Problems = bAPIProblems(t.Repo.ProblemsOf(rd.Path))
	}

	var base *node.Inventory
	if t.Base != nil {
		base = node.LoadInventory(t.Base)
	}
	baseOf := func(o node.Occupant) (node.Occupant, bool) {
		if base == nil {
			return node.Occupant{}, false
		}
		for _, b := range base.Occupants {
			if b.Kind == o.Kind && b.Name == o.Name {
				return b, true
			}
		}
		return node.Occupant{}, false
	}
	for _, o := range occ {
		ro := api.RackOccupant{Kind: o.Kind, Name: o.Name, Unit: o.Unit, Size: max(o.Size, 1), Role: o.Role, Cluster: o.Cluster,
			Category: api.RackOccupantCategory(o.Category()), Highlight: api.RackOccupantHighlightNone}
		for _, p := range t.Repo.ProblemsOf(o.Doc.Path) {
			if p.Severity == kinds.SevError || p.Severity == kinds.SevWarning {
				ro.Problems = append(ro.Problems, p.Message)
			}
		}
		if base != nil {
			b, ok := baseOf(o)
			switch {
			case !ok || !strings.EqualFold(b.Rack, o.Rack):
				ro.Highlight = api.RackOccupantHighlightAdded
			case !repo.SameContent(t.Base, t.Repo, o.Doc.Path):
				ro.Highlight = api.RackOccupantHighlightChanged
			}
		}
		if len(inv.Overlaps(o)) > 0 {
			ro.Highlight = api.RackOccupantHighlightConflict
		}
		if ro.Highlight != api.RackOccupantHighlightNone && t.ChangeID != "" {
			ro.ChangeId = t.ChangeID
		}
		rk.Occupants = append(rk.Occupants, ro)
	}
	if base != nil {
		for _, b := range base.InRack(name) {
			still := false
			for _, o := range inv.Occupants {
				if o.Kind == b.Kind && o.Name == b.Name && strings.EqualFold(o.Rack, b.Rack) {
					still = true
					break
				}
			}
			if !still {
				rk.Occupants = append(rk.Occupants, api.RackOccupant{Kind: b.Kind, Name: b.Name, Unit: b.Unit, Size: max(b.Size, 1), Role: b.Role,
					Cluster: b.Cluster, Category: api.RackOccupantCategory(b.Category()), Highlight: api.RackOccupantHighlightDeleted, ChangeId: t.ChangeID})
			}
		}
	}
	sort.SliceStable(rk.Occupants, func(i, j int) bool { return rk.Occupants[i].Unit < rk.Occupants[j].Unit })
	return rk, nil
}

// ---------------------------------------------------------------- kb

func articleSummary(d *kinds.Doc) api.ArticleSummary {
	a := api.ArticleSummary{Path: d.Path, Title: kindutil.Str(d.Data, "title"), Type: api.ArticleType(kindutil.Str(d.Data, "type"))}
	if a.Title == "" {
		a.Title = firstHeading(d.Body, d.Path)
	}
	if a.Type == "" {
		a.Type = api.ArticleTypeReference
	}
	if e := kindutil.Strings(d.Data, "entities"); len(e) > 0 {
		a.Entities = e
	}
	if c := kindutil.Strings(d.Data, "changes"); len(c) > 0 {
		a.Changes = c
	}
	return a
}

func firstHeading(body, fallback string) string {
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(l[2:])
		}
	}
	return fallback
}

func (s *Server) ListArticles(ctx context.Context, req api.ListArticlesRequestObject) (api.ListArticlesResponseObject, error) {
	t, err := s.bTreeAt(ctx, req.Params.Ref)
	if err != nil {
		return nil, err
	}
	out := api.ListArticles200JSONResponse{}
	for _, d := range t.Repo.ByKind("Article") {
		a := articleSummary(d)
		if req.Params.Type != "" && a.Type != req.Params.Type {
			continue
		}
		if req.Params.Entity != "" && !slices.ContainsFunc(a.Entities, func(e string) bool { return strings.EqualFold(e, req.Params.Entity) }) {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Server) GetArticle(ctx context.Context, req api.GetArticleRequestObject) (api.GetArticleResponseObject, error) {
	t, err := s.bTreeAt(ctx, req.Params.Ref)
	if err != nil {
		return nil, err
	}
	p := strings.TrimPrefix(req.Params.Path, "/")
	f := t.Repo.File(p)
	if f == nil {
		return nil, bNotFound("no article %s in %s", p, t.Ref)
	}
	var d *kinds.Doc
	for _, x := range f.Docs {
		if x.Kind == "Article" {
			d = x
		}
	}
	a := api.Article{Path: p, Entities: []api.EntityRef{}, Changes: []api.ChangeLink{}, CitedBy: []api.ChangeLink{}, Problems: bAPIProblems(t.Repo.ProblemsOf(p))}
	if t.Ref != "main" {
		a.Ref = t.Ref
	}
	if d == nil {
		// A Markdown file without front matter is still readable.
		_, body, _, _ := repo.FrontMatter(f.Content)
		a.Body = body
		a.Title = firstHeading(body, p)
		a.Type = api.ArticleTypeReference
		return api.GetArticle200JSONResponse(a), nil
	}
	sum := articleSummary(d)
	a.Title, a.Type, a.Body, a.FrontMatter = sum.Title, sum.Type, d.Body, d.Data
	for _, r := range refIndexOf(t.Repo).out[d] {
		if strings.HasPrefix(r.Field, "entities[") {
			a.Entities = append(a.Entities, r)
		}
	}
	ld := s.bGather(ctx, &api.ListHistoryParams{Path: p, Limit: 20}, true)
	idx, hist := ld.idx, ld.hist
	byID := map[string]api.HistoryEntry{}
	for _, h := range hist {
		if h.Change != "" {
			byID[h.Change] = h
		}
	}
	for _, id := range sum.Changes {
		l := api.ChangeLink{Id: id, Title: id}
		if c, ok := idx[id]; ok {
			l = bLinkOfSummary(c)
		} else if h, ok := byID[id]; ok {
			l = bLinkOfHistory(h)
		}
		a.Changes = append(a.Changes, l)
	}
	var cited bChangeLinks
	for _, oc := range ld.open {
		if slices.Contains(oc.Paths, p) {
			cited.add(bLinkOfOpen(oc, idx))
		}
	}
	for _, h := range hist {
		if h.Change != "" { // direct commits (the initial state) cite nothing
			cited.add(bLinkOfHistory(h))
		}
	}
	// Changes of work items that list the article (kb: [kb/runbooks/add-node]).
	for _, it := range s.workBoard(ctx, t.Repo, idx, nil).Items {
		if slices.ContainsFunc(it.Kb, func(k string) bool { return k == p || k+".md" == p }) {
			for _, l := range it.Changes {
				cited.add(l)
			}
		}
	}
	// Changes whose lab runs wrote this report cite it too.
	for _, c := range idx {
		if strings.HasPrefix(p, "kb/labs/"+c.ID+"-run-") {
			cited.add(bLinkOfSummary(c))
		}
	}
	a.CitedBy = cited.get()
	if len(hist) > 0 {
		a.LastChange = bLinkOfHistory(hist[0])
	}
	return api.GetArticle200JSONResponse(a), nil
}

// ---------------------------------------------------------------- work

func (s *Server) GetWorkBoard(ctx context.Context, req api.GetWorkBoardRequestObject) (api.GetWorkBoardResponseObject, error) {
	t, err := s.bTreeAt(ctx, req.Params.Ref)
	if err != nil {
		return nil, err
	}
	ld := s.bGather(ctx, &api.ListHistoryParams{Limit: 500}, false)
	return api.GetWorkBoard200JSONResponse(s.workBoard(ctx, t.Repo, ld.idx, ld.hist)), nil
}

// workStage orders change states for the work item status.
func workStage(st api.ChangeState) (api.WorkStatus, int) {
	switch st {
	case api.ChangeStateDraft, api.ChangeStateLab:
		return api.InProgress, 1
	case api.ChangeStateReview, api.ChangeStateApproved:
		return api.InReview, 2
	case api.ChangeStateMerged, api.ChangeStateDeployed, api.ChangeStateVerified:
		return api.Done, 3
	}
	return api.Planned, 0
}

// DeriveWorkStatus derives the status of a work item from its changes:
// planned without (non-closed) changes; otherwise the least advanced change
// decides (draft/lab in-progress, review/approved in-review, merged and
// later done).
func DeriveWorkStatus(links []api.ChangeLink) (api.WorkStatus, string) {
	status, from, best := api.Planned, "", 99
	for _, l := range links {
		st, rank := workStage(l.State)
		if rank == 0 {
			continue
		}
		if rank < best {
			status, from, best = st, l.Id, rank
		}
	}
	return status, from
}

func (s *Server) workBoard(ctx context.Context, r *repo.Repo, idx map[string]change.Summary, hist []api.HistoryEntry) api.WorkBoard {
	wb := api.WorkBoard{Items: []api.WorkItem{}, Milestones: []api.Milestone{}}
	linked := map[string]*bChangeLinks{}
	add := func(item string, l api.ChangeLink) {
		if linked[item] == nil {
			linked[item] = &bChangeLinks{}
		}
		linked[item].add(l)
	}
	ids := make([]string, 0, len(idx))
	for id := range idx {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := idx[id]
		for _, w := range c.WorkItems {
			add(w, bLinkOfSummary(c))
		}
	}
	for _, h := range hist {
		for _, w := range h.WorkItems {
			add(w, bLinkOfHistory(h))
		}
	}
	for _, d := range r.ByKind("WorkItem") {
		id := kindutil.Str(d.Data, "id")
		if id == "" {
			id = d.Name
		}
		it := api.WorkItem{Id: id, Title: kindutil.Str(d.Data, "title"), Milestone: kindutil.Str(d.Data, "milestone"), Owner: kindutil.Str(d.Data, "owner"),
			Path: d.Path, Assets: kindutil.Strings(d.Data, "assets"), Kb: kindutil.Strings(d.Data, "kb")}
		for _, cid := range kindutil.Strings(d.Data, "changes") {
			l := api.ChangeLink{Id: cid, Title: cid}
			if c, ok := idx[cid]; ok {
				l = bLinkOfSummary(c)
			}
			add(id, l)
		}
		if lk := linked[id]; lk != nil {
			it.Changes = lk.get()
		} else {
			it.Changes = []api.ChangeLink{}
		}
		it.Status, it.StatusFrom = DeriveWorkStatus(it.Changes)
		wb.Items = append(wb.Items, it)
	}
	sort.SliceStable(wb.Items, func(i, j int) bool { return wb.Items[i].Id < wb.Items[j].Id })
	for _, d := range r.ByKind("MilestoneList") {
		ms, _ := d.Data["milestones"].([]any)
		for _, m := range ms {
			mm, _ := m.(map[string]any)
			ml := api.Milestone{Id: kindutil.Str(mm, "id"), Title: kindutil.Str(mm, "title"), Description: kindutil.Str(mm, "description")}
			if due := kindutil.Str(mm, "due"); due != "" {
				if tm, err := time.Parse("2006-01-02", due); err == nil {
					ml.Due = openapi_types.Date{Time: tm}
				}
			}
			for _, it := range wb.Items {
				if it.Milestone == ml.Id {
					ml.Items++
					if it.Status == api.Done {
						ml.Done++
					}
				}
			}
			wb.Milestones = append(wb.Milestones, ml)
		}
	}
	return wb
}

// ---------------------------------------------------------------- file helpers (used by area A)

// fileContent describes a file for the editor: masked content, kind,
// problems (schema and cross-file in the tree at dir) and secret fields.
// The caller sets exists, readOnly, status, sha and ref.
func (s *Server) fileContent(ctx context.Context, dir, path string, content []byte) api.FileContent {
	st := s.b()
	fc := api.FileContent{Path: path, Content: string(content), Language: api.FileContentLanguage(repo.Language(path)), Problems: []api.Problem{}, Secrets: []api.SecretField{}}
	if fc.Language == "yaml" || fc.Language == "json" {
		masked, secrets := schema.Mask(content, repo.SchemaLookup(st.reg, path))
		fc.Content = string(masked)
		for _, sc := range secrets {
			fc.Secrets = append(fc.Secrets, api.SecretField{Field: sc.Field, Key: sc.Key, Kind: api.SecretFieldKind(sc.Kind), Line: sc.Line})
		}
	}
	f := repo.ParseFile(path, content)
	if len(f.Docs) > 0 {
		d := f.Docs[0]
		if k, ok := st.reg.Match(path, d); ok {
			fc.Kind = api.KindRef{Name: k.Name, ApiVersion: k.APIVersion(), ObjectName: d.Name, Own: k.Own, Version: bVersionOr(k, d)}
			fc.TruthClass = api.TruthClass(k.Truth)
			fc.ApplyPath = api.ApplyPath(k.ApplyPath)
			if k.Schema != nil {
				fc.SchemaUrl = schemaURL(k, bVersionOr(k, d))
			}
			if d.APIVersion != "" && d.APIVersion != k.APIVersion() {
				fc.StoredVersion = d.APIVersion
			}
		}
	}
	fc.Problems = s.problemsWith(dir, path, content)
	return fc
}

// problemsWith validates path with content inside the tree at dir.
func (s *Server) problemsWith(dir, path string, content []byte) []api.Problem {
	st := s.b()
	var r *repo.Repo
	if dir != "" {
		if base, err := st.eng.Loader().Load(dir); err == nil {
			r = base
			if f := base.File(path); f == nil || !bytes.Equal(f.Content, content) {
				r = base.WithFile(path, content)
			}
		}
	}
	if r == nil {
		r = repo.FromFiles(map[string][]byte{path: content}, st.reg)
	}
	return bAPIProblems(r.ProblemsOf(path))
}

// unmaskContent puts the stored values back into masked secret fields.
func (s *Server) unmaskContent(path string, content, stored []byte) ([]byte, error) {
	if !bytes.Contains(content, []byte(schema.MaskText)) {
		return content, nil
	}
	out, err := schema.Unmask(content, stored)
	if err != nil {
		return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Msg: "masked secret", Detail: err.Error()}
	}
	return out, nil
}

// setFields sets fields by dotted path (spec.location.unit, ports[1].peer.port)
// in stored and keeps comments and order. In Markdown files it sets front
// matter fields.
func (s *Server) setFields(path string, stored []byte, set map[string]any) ([]byte, error) {
	if len(set) == 0 {
		return stored, nil
	}
	if repo.Language(path) == "markdown" {
		fm, body, _, ok := repo.FrontMatter(stored)
		if !ok {
			fm = nil
		}
		nfm, err := schema.SetFields(fm, set)
		if err != nil {
			return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Msg: "cannot set fields", Detail: err.Error()}
		}
		var b bytes.Buffer
		b.WriteString("---\n")
		b.Write(nfm)
		if !bytes.HasSuffix(nfm, []byte("\n")) {
			b.WriteString("\n")
		}
		b.WriteString("---\n")
		b.WriteString(body)
		return b.Bytes(), nil
	}
	out, err := schema.SetFieldsWith(stored, set, repo.SchemaRawLookup(s.b().reg, path))
	if err != nil {
		return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Msg: "cannot set fields", Detail: err.Error()}
	}
	return out, nil
}

var objectNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// newFileSkeleton returns the default path and content of a new object.
func (s *Server) newFileSkeleton(kind, name string) (string, []byte, error) {
	k, ok := s.b().reg.Get(kind)
	if !ok {
		return "", nil, bBadRequest("unknown kind %s", kind)
	}
	if !objectNameRe.MatchString(name) {
		return "", nil, bBadRequest("name %q: use letters, digits, '.', '_' and '-'", name)
	}
	p := ""
	if k.NewPath != nil {
		p = k.NewPath(name)
	}
	if k.Skeleton != nil {
		b, err := k.Skeleton(name)
		return p, b, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n", k.APIVersion(), k.Name, name)
	if k.Name != "Namespace" {
		b.WriteString("spec: {}\n")
	}
	return p, b.Bytes(), nil
}

// problemsFor returns the problems of the tree at dir (only of path when set).
func (s *Server) problemsFor(ctx context.Context, dir, path string) []api.Problem {
	r, err := s.b().eng.Loader().Load(dir)
	if err != nil {
		return []api.Problem{{Path: path, Severity: api.ProblemSeverity(kinds.SevError), Source: api.ProblemSource(kinds.SourceYAML), Message: "cannot read the tree: " + err.Error()}}
	}
	if path != "" {
		return bAPIProblems(r.ProblemsOf(path))
	}
	return bAPIProblems(r.Problems())
}
