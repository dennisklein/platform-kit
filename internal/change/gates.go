package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// GateData is what a gate evaluation reads. The bench fills it from a
// worktree, pk ci from a pull request.
type GateData struct {
	ID      string
	Author  identity.Identity
	Writers []identity.Identity
	// Paths changed against the merge base (with saved files).
	Paths   []string
	Reviews []forge.Review
	Lab     policy.LabState
	Behind  int
	Target  string
	// Revert: the change reverts a merged change (policy fast path).
	Revert bool
	// Base and Head render the impact objects (risk rules).
	Base, Head render.Source
}

// EvaluateGates runs the policy evaluation. Without a renderer the impact
// objects stay empty (risk then comes from path rules only).
func EvaluateGates(ctx context.Context, eng policy.Engine, p *policy.Policy, rd render.Renderer, g GateData) (*policy.Gates, error) {
	gates, _, err := evaluateGates(ctx, eng, p, rd, g)
	return gates, err
}

// evaluateGates also returns the changed objects of the impact (overlap).
func evaluateGates(ctx context.Context, eng policy.Engine, p *policy.Policy, rd render.Renderer, g GateData) (*policy.Gates, []render.ObjectRef, error) {
	if eng == nil || p == nil {
		return nil, nil, fmt.Errorf("no policy")
	}
	in := policy.GateInput{
		ChangeID: g.ID, Author: g.Author, Writers: g.Writers, Lab: g.Lab,
		Behind: g.Behind, Target: g.Target, Revert: g.Revert,
	}
	for _, path := range g.Paths {
		in.Paths = append(in.Paths, path)
	}
	for _, rv := range g.Reviews {
		if rv.State == forge.ReviewApproved {
			in.Reviews = append(in.Reviews, policy.Review{User: rv.User, SHA: rv.SHA, Stale: rv.Stale})
		}
	}
	if rd != nil && g.Head.Dir != "" && g.Base.Dir != "" {
		imp, err := rd.Impact(ctx, g.Base, g.Head)
		if err != nil {
			return nil, nil, fmt.Errorf("render: %w", err)
		}
		in.Objects = imp.Objects()
	}
	gates := eng.Evaluate(p, in)
	return &gates, in.Objects, nil
}

func gateKey(g GateData) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%d|%s|%s|%s|%v|%v\n", g.ID, g.Author.User, g.Base.Key, g.Head.Key, g.Behind, g.Target,
		g.Lab.Status, g.Lab.Run, g.Lab.CoversHead, g.Revert)
	var rs []string
	for _, r := range g.Reviews {
		rs = append(rs, fmt.Sprintf("%s@%s:%s:%v", r.User, r.SHA, r.State, r.Stale))
	}
	sort.Strings(rs)
	fmt.Fprint(h, rs)
	for _, w := range g.Writers {
		fmt.Fprint(h, w.User, ",")
	}
	fmt.Fprint(h, g.Paths)
	return hex.EncodeToString(h.Sum(nil))
}

// gates evaluates a derived change (cached by its inputs).
func (s *Svc) gates(ctx context.Context, d *derived, author identity.Identity) (*policy.Gates, error) {
	p := s.policy(ctx)
	if p == nil {
		return nil, fmt.Errorf("no policy")
	}
	r := d.rec
	g := GateData{
		ID: r.id, Author: author, Writers: d.writers, Paths: d.paths,
		Reviews: s.markStale(ctx, r.id, s.targetRef(ctx, r), r.headSHA(), d.reviews),
		Behind:  d.ch.BehindTarget, Target: d.ch.Target,
		Lab: policy.LabState{Status: d.ch.LabState, Run: d.ch.LabRun,
			CoversHead: d.ch.LabState == "pass" || d.ch.LabState == "fail"},
	}
	if d.labRun != nil {
		g.Lab.SHA = d.labRun.SHA
	}
	d.reviews = g.Reviews // with Stale from git (approval list, rail group)
	g.Revert = s.isRevert(ctx, r.id, d.ch.Intent, d.paths, d.ch.BaseSHA, r.headSHA(), r.wt)
	if len(g.Writers) == 0 && author.User != "" {
		g.Writers = []identity.Identity{author}
	}
	var err error
	if r.wt != "" {
		g.Head, err = s.liveSource(ctx, r.id, r.wt)
	} else {
		g.Head, err = s.shaSource(ctx, r.id, r.headSHA())
	}
	if err != nil {
		return nil, err
	}
	if d.ch.BaseSHA != "" {
		if g.Base, err = s.shaSource(ctx, "main", d.ch.BaseSHA); err != nil {
			return nil, err
		}
	}
	key := gateKey(g)
	s.cmu.Lock()
	if cached, ok := s.gatesCache[key]; ok {
		s.cmu.Unlock()
		s.setObjects(d, cached.objects)
		return cached.gates, nil
	}
	s.cmu.Unlock()
	gates, objs, err := evaluateGates(ctx, s.o.Policy, p, s.o.Renderer, g)
	if err != nil {
		return nil, err
	}
	s.cmu.Lock()
	if len(s.gatesCache) > 256 {
		s.gatesCache = map[string]gatesEntry{}
	}
	s.gatesCache[key] = gatesEntry{gates: gates, objects: objs}
	s.cmu.Unlock()
	s.setObjects(d, objs)
	return gates, nil
}

func (s *Svc) Gates(ctx context.Context, id string) (*policy.Gates, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	if s.o.Policy == nil {
		return nil, fmt.Errorf("policy engine is not available")
	}
	r, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	d, err := s.derive(ctx, r, s.me(ctx), false)
	if err != nil {
		return nil, err
	}
	if d.ch.Gates != nil {
		return d.ch.Gates, nil
	}
	if r.merge != nil && len(r.merge.Parents) == 2 {
		return s.mergedGates(ctx, d)
	}
	if d.ch.State == Closed {
		return nil, fmt.Errorf("%w: %s is closed; gates apply to open changes", ErrConflict, id)
	}
	if r.headRev() == "" {
		return nil, fmt.Errorf("%w: %s has no commits yet", ErrConflict, id)
	}
	return s.gates(ctx, d, s.identify(ctx, d.ch.Author))
}

// mergedGates evaluates a merged change as it was merged: the change head
// (second parent of the merge) against its base, the approvals and the lab
// run of the merge trailers, with the policy on main.
func (s *Svc) mergedGates(ctx context.Context, d *derived) (*policy.Gates, error) {
	p := s.policy(ctx)
	if p == nil {
		return nil, fmt.Errorf("no policy")
	}
	ch, m := d.ch, d.rec.merge
	author := s.identify(ctx, ch.Author)
	g := GateData{ID: ch.ID, Author: author, Paths: d.paths, Target: ch.Target}
	if commits, err := s.commitRange(ctx, ch.BaseSHA, ch.HeadSHA, nil, false); err == nil {
		g.Writers = s.writersOf(ctx, ch.ID, commits)
	}
	for _, u := range TrailerList(m.Trailers, TrailerApprovedBy) {
		g.Reviews = append(g.Reviews, forge.Review{User: u, State: forge.ReviewApproved, SHA: ch.HeadSHA})
	}
	runs := append([]LabRunRef(nil), ch.LabRuns...)
	sort.SliceStable(runs, func(i, j int) bool { return runNumber(runs[i].Run) > runNumber(runs[j].Run) })
	for _, r := range runs {
		if r.Result == LabPass {
			g.Lab = policy.LabState{Status: "pass", Run: r.Run, SHA: ch.HeadSHA, CoversHead: true}
			break
		}
	}
	g.Revert = s.isRevert(ctx, ch.ID, ch.Intent, d.paths, ch.BaseSHA, ch.HeadSHA, "")
	var err error
	if g.Head, err = s.shaSource(ctx, ch.ID, ch.HeadSHA); err != nil {
		return nil, err
	}
	if g.Base, err = s.shaSource(ctx, "main", ch.BaseSHA); err != nil {
		return nil, err
	}
	gates, _, err := evaluateGates(ctx, s.o.Policy, p, s.o.Renderer, g)
	return gates, err
}

// RevertOf returns the change id an intent says it reverts: the revert
// action writes "Revert c-xxxxxx (merge <sha>): <title>".
func RevertOf(intent string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(intent), "Revert ")
	if !ok || len(rest) < 8 {
		return ""
	}
	if id := rest[:8]; ValidID(id) && (len(rest) == 8 || rest[8] == ' ' || rest[8] == ':') {
		return id
	}
	return ""
}

// isRevert reports whether a change gets the revert fast path: its intent
// names a change that is merged on main, it changes only paths that merge
// changed (records of the change itself aside), and its content undoes the
// merge on those paths: the same patch as merge..merge^1, or the content of
// main before the merge. An intent alone does not open the fast path, and
// neither do other edits to the same files. dir is the worktree (uncommitted
// edits count) or "" to compare head.
func (s *Svc) isRevert(ctx context.Context, id, intent string, paths []string, base, head, dir string) bool {
	orig := RevertOf(intent)
	if orig == "" || orig == id || base == "" || (head == "" && dir == "") {
		return false
	}
	mi := s.mergeOf(ctx, orig)
	if mi == nil {
		return false
	}
	var content []string
	for _, p := range paths {
		if RecordOnly(id, []string{p}) {
			continue
		}
		if !mi.paths[p] {
			return false
		}
		content = append(content, p)
	}
	if len(content) == 0 {
		return false
	}
	g := s.repo
	if dir != "" {
		g, head = gitx.Open(dir), ""
	}
	// Content of main before the merge.
	args := append([]string{"diff", "--quiet", "--no-ext-diff", mi.parent}, head)
	if head == "" {
		args = args[:len(args)-1]
	}
	if _, err := g.Run(ctx, append(append(args, "--"), content...)...); err == nil {
		return true
	}
	// The inverse patch of the merge (later edits elsewhere in the files kept).
	want, err := patchID(ctx, s.repo, s.repo.Diff, mi.sha, mi.parent, content)
	if err != nil || want == "" {
		return false
	}
	got, err := patchID(ctx, s.repo, g.Diff, base, head, content)
	return err == nil && got == want
}

// patchID returns the stable patch id of a diff ("" for an empty diff).
func patchID(ctx context.Context, r *gitx.Repo, diff func(context.Context, string, string, ...string) (string, error), a, b string, paths []string) (string, error) {
	d, err := diff(ctx, a, b, paths...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(d) == "" {
		return "", nil
	}
	out, err := r.RunInput(ctx, []byte(d), "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0], nil
	}
	return "", nil
}

// mergeInfo is the merge of a change on main.
type mergeInfo struct {
	sha, parent string
	paths       map[string]bool
}

// mergeOf returns the merge of a change on main and the paths it changed
// (nil when the change is not merged on main). Cached per change id.
func (s *Svc) mergeOf(ctx context.Context, id string) *mergeInfo {
	s.cmu.Lock()
	m, ok := s.revCache[id]
	s.cmu.Unlock()
	if ok {
		return m
	}
	ms, err := s.repo.Log(ctx, mainRef, "--merges", "--first-parent", "--grep", TrailerChange+": "+id, "-n", "1")
	if err != nil || len(ms) == 0 || len(ms[0].Parents) != 2 || Trailer(ms[0].Trailers, TrailerChange) != id {
		return nil
	}
	names, err := s.repo.DiffNames(ctx, ms[0].Parents[0], ms[0].SHA)
	if err != nil {
		return nil
	}
	m = &mergeInfo{sha: ms[0].SHA, parent: ms[0].Parents[0], paths: map[string]bool{}}
	for _, f := range names {
		m.paths[f.Path] = true
		if f.OldPath != "" {
			m.paths[f.OldPath] = true
		}
	}
	s.cmu.Lock()
	s.revCache[id] = m
	s.cmu.Unlock()
	return m
}

// markStale returns the reviews with Stale set from git, not from the forge
// (Gitea marks every review on an older commit stale, also after a rebase).
// An approval stays fresh when it is on the head, or when the content the
// reviewer saw is the content of the head: the same patch against the
// target once records of the change (.pk/changes/**, the change's lab
// reports) are left out. A rebase and a lab report commit keep approvals;
// any other new content makes them stale.
func (s *Svc) markStale(ctx context.Context, id, tref, head string, reviews []forge.Review) []forge.Review {
	out := make([]forge.Review, len(reviews))
	for i, rv := range reviews {
		rv.Stale = false
		if rv.State == forge.ReviewApproved && head != "" && rv.SHA != head {
			rv.Stale = !s.sameContent(ctx, id, tref, rv.SHA, head)
		}
		out[i] = rv
	}
	return out
}

// sameContent reports whether two commits of a change carry the same patch
// against the target, records of the change aside (cached: commits do not
// change, the target sha is part of the key).
func (s *Svc) sameContent(ctx context.Context, id, tref, a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	tsha, _ := s.repo.RevParse(ctx, tref)
	key := "same|" + id + "|" + tsha + "|" + a + "|" + b
	s.cmu.Lock()
	v, ok := s.recCache[key]
	s.cmu.Unlock()
	if ok {
		return v
	}
	pa, errA := s.contentPatchID(ctx, id, tref, a)
	pb, errB := s.contentPatchID(ctx, id, tref, b)
	v = errA == nil && errB == nil && pa == pb
	if errA == nil && errB == nil {
		s.cmu.Lock()
		if len(s.recCache) > 4096 {
			s.recCache = map[string]bool{}
		}
		s.recCache[key] = v
		s.cmu.Unlock()
	}
	return v
}

// contentPatchID is the stable patch id of a commit against its merge base
// with the target, without the records of the change ("" for no content).
func (s *Svc) contentPatchID(ctx context.Context, id, tref, sha string) (string, error) {
	if !s.repo.Exists(ctx, sha) {
		return "", fmt.Errorf("unknown commit %s", sha)
	}
	base, err := s.repo.MergeBase(ctx, tref, sha)
	if err != nil {
		return "", err
	}
	return patchID(ctx, s.repo, s.repo.Diff, base, sha,
		[]string{".", ":(exclude,glob)" + policy.MetaGlob, ":(exclude,glob)kb/labs/" + id + "-run-*.md"})
}
