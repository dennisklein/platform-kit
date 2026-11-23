package change

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// record is what the bench knows about one change id before derivation.
type record struct {
	id        string
	branch    string
	wt        string // local worktree
	localSHA  string // refs/heads/<branch>
	remoteSHA string // refs/remotes/origin/<branch>
	pr        *forge.PR
	merge     *gitx.LogEntry // merge commit on main with Pk-Change
	target    string
}

func (r *record) headRev() string {
	switch {
	case r.wt != "" && r.localSHA != "":
		return localRef(r.branch)
	case r.remoteSHA != "":
		return remoteRef(r.branch)
	case r.localSHA != "":
		return localRef(r.branch)
	}
	return ""
}

func (r *record) headSHA() string {
	switch {
	case r.wt != "" && r.localSHA != "":
		return r.localSHA
	case r.remoteSHA != "":
		return r.remoteSHA
	}
	return r.localSHA
}

const mergeLogDepth = 50

// records collects all change ids: local worktrees, remote change branches,
// pull requests and the last merges on main.
func (s *Svc) records(ctx context.Context) (map[string]*record, error) {
	recs := map[string]*record{}
	get := func(id string) *record {
		r, ok := recs[id]
		if !ok {
			r = &record{id: id}
			recs[id] = r
		}
		return r
	}
	wts, err := s.repo.WorktreeList(ctx)
	if err != nil {
		return nil, err
	}
	wtRoot := filepath.Join(s.o.DataDir, "worktrees") + string(filepath.Separator)
	for _, w := range wts {
		id, _, ok := ParseBranch(w.Branch)
		if !ok || !strings.HasPrefix(w.Path, wtRoot) {
			continue
		}
		r := get(id)
		r.wt, r.branch, r.localSHA = w.Path, w.Branch, w.Head
	}
	refs, err := s.repo.Run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", remotePrefix+"change/", "refs/heads/change/")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(refs, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if b, ok := strings.CutPrefix(ref, remotePrefix); ok {
			if id, _, ok := ParseBranch(b); ok {
				r := get(id)
				if r.branch == "" || r.branch == b {
					r.branch, r.remoteSHA = b, sha
				}
			}
		} else if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			if id, _, ok := ParseBranch(b); ok {
				if r, exists := recs[id]; exists && r.branch == b {
					r.localSHA = sha
				}
			}
		}
	}
	s.fmu.RLock()
	for i := range s.fs.prs {
		pr := s.fs.prs[i]
		id, _, ok := ParseBranch(pr.Head)
		if !ok {
			continue
		}
		r := get(id)
		if r.pr == nil || pr.Number > r.pr.Number {
			cp := pr
			r.pr = &cp
		}
		if r.branch == "" {
			r.branch = pr.Head
		}
	}
	s.fmu.RUnlock()
	// By sha: the log of a commit never changes (gitx caches it).
	mainRev := mainRef
	if sha, err := s.repo.RevParse(ctx, mainRef); err == nil {
		mainRev = sha
	}
	merges, err := s.repo.Log(ctx, mainRev, "--merges", "--first-parent", "-n", fmt.Sprint(mergeLogDepth))
	if err != nil && !gitx.IsNotFound(err) {
		return nil, err
	}
	for i := range merges {
		id := Trailer(merges[i].Trailers, TrailerChange)
		if !ValidID(id) {
			continue
		}
		r := get(id)
		if r.merge == nil {
			m := merges[i]
			r.merge = &m
		}
	}
	// A merged PR whose branch is deleted has no change branch as head;
	// find it by the Reviewed-in trailer of the merge.
	s.fmu.RLock()
	for _, r := range recs {
		if r.pr != nil || r.merge == nil {
			continue
		}
		n := mrNumber(Trailer(r.merge.Trailers, TrailerReviewedIn))
		for i := range s.fs.prs {
			if n > 0 && s.fs.prs[i].Number == n {
				cp := s.fs.prs[i]
				r.pr = &cp
				break
			}
		}
	}
	s.fmu.RUnlock()
	return recs, nil
}

// mrNumber parses "!12" (or "12").
func mrNumber(v string) int {
	n := 0
	for _, c := range strings.TrimPrefix(strings.TrimSpace(v), "!") {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// record returns one change, also when it was merged before the merge log depth.
func (s *Svc) record(ctx context.Context, id string) (*record, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	recs, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	r, ok := recs[id]
	if !ok {
		ms, err := s.repo.Log(ctx, mainRef, "--merges", "--first-parent", "--grep", TrailerChange+": "+id, "-n", "1")
		if err == nil && len(ms) > 0 {
			return &record{id: id, merge: &ms[0]}, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return r, nil
}

// meta loads the metadata of a change from the worktree, the branch, the
// MR description or the merge commit.
func (s *Svc) meta(ctx context.Context, r *record) (m Meta, fromFile bool) {
	if r.wt != "" {
		if b, err := readWorktreeFile(r.wt, MetaPath(r.id)); err == nil {
			if m, err := ParseMeta(b); err == nil {
				return m, true
			}
		}
	} else if rev := r.headSHA(); rev != "" {
		if b, err := s.repo.ShowFile(ctx, rev, MetaPath(r.id)); err == nil {
			if m, err := ParseMeta(b); err == nil {
				return m, true
			}
		}
	}
	if r.pr != nil {
		if m, ok := MetaFromDescription(r.pr.Body); ok && m.ID == r.id {
			if m.Title == "" {
				m.Title = r.pr.Title
			}
			return m, false
		}
	}
	if r.merge != nil {
		m, _ := MetaFromTrailers(r.merge.Trailers)
		m.ID = r.id
		_, title, _ := TitleFromMergeSubject(r.merge.Subject)
		if m.Title == "" {
			m.Title = title
		}
		return m, false
	}
	m = Meta{ID: r.id, Target: "main"}
	if r.pr != nil {
		m.Title, m.Author = r.pr.Title, r.pr.Author
	}
	return m, false
}

// ---------------------------------------------------------------- derive

// derived is a change with everything the rail and the detail need.
type derived struct {
	rec      *record
	ch       *Change
	reviews  []forge.Review
	statuses []forge.CommitStatus
	labRun   *LabRunRef // newest finished run that covers the head
	writers  []identity.Identity
	paths    []string
}

// derive builds a change. detail adds commits and changed files.
func (s *Svc) derive(ctx context.Context, r *record, me string, detail bool) (*derived, error) {
	m, _ := s.meta(ctx, r)
	r.target = m.Target
	if r.target == "" {
		r.target = "main"
	}
	ch := &Change{}
	d := &derived{rec: r, ch: ch}
	ch.ID, ch.Title, ch.Intent = r.id, m.Title, m.Intent
	ch.Branch = r.branch
	if ch.Branch == "" {
		ch.Branch = Branch(r.id, m.Title)
	}
	ch.Target, ch.Parent, ch.Author, ch.Agent = r.target, m.Parent, m.Author, m.Agent
	ch.WorkItems = m.WorkItems
	ch.Meta = m
	ch.Local, ch.Worktree = r.wt != "", r.wt
	ch.Pushed = r.remoteSHA != ""
	ch.HeadSHA = r.headSHA()

	headRev := r.headRev()
	tref := s.targetRef(ctx, r)
	ch.TargetSHA, _ = s.repo.RevParse(ctx, tref)
	// Git below works on shas: results on commits are cached (gitx).
	if ch.TargetSHA != "" && ch.HeadSHA != "" && headRev != "" {
		tref, headRev = ch.TargetSHA, ch.HeadSHA
	}

	var st []gitx.FileStatus
	if r.wt != "" {
		st, _ = gitx.Open(r.wt).Status(ctx)
		ch.Dirty = len(st) > 0
	}
	if r.merge != nil && len(r.merge.Parents) == 2 {
		s.mergedDetail(ctx, d, detail)
	} else if headRev != "" && (r.merge == nil || r.wt != "" || r.remoteSHA != "") {
		ch.BaseSHA, _ = s.repo.MergeBase(ctx, tref, headRev)
		ch.AheadTarget, ch.BehindTarget, _ = s.repo.AheadBehind(ctx, tref, headRev)
		if ch.BehindTarget > 0 {
			ch.BehindTarget, _ = s.repo.Behind(ctx, headRev, tref)
		}
		ch.Summary.Commits = ch.AheadTarget
		if r.wt != "" && r.remoteSHA != "" {
			if r.localSHA != "" {
				ch.AheadRemote, ch.BehindRemote, _ = s.repo.AheadBehind(ctx, r.remoteSHA, r.localSHA)
			} else {
				ch.AheadRemote, ch.BehindRemote, _ = s.repo.AheadBehind(ctx, remoteRef(r.branch), localRef(r.branch))
			}
		}
		if ch.BaseSHA != "" {
			names, _ := s.repo.DiffNames(ctx, ch.BaseSHA, headRev)
			seen := map[string]bool{}
			for _, f := range names {
				seen[f.Path] = true
				ch.ChangedFiles = append(ch.ChangedFiles, FileStatus{Path: f.Path, OldPath: f.OldPath, Status: statusWord(f.Code), Committed: true})
			}
			for _, f := range st {
				if seen[f.Path] {
					for i := range ch.ChangedFiles {
						if ch.ChangedFiles[i].Path == f.Path {
							ch.ChangedFiles[i].Committed = false
						}
					}
					continue
				}
				seen[f.Path] = true
				ch.ChangedFiles = append(ch.ChangedFiles, FileStatus{Path: f.Path, OldPath: f.OldPath, Status: statusWord(f.Code), Staged: f.Staged})
			}
			for p := range seen {
				d.paths = append(d.paths, p)
			}
			sort.Strings(d.paths)
		}
		ch.Files = len(ch.ChangedFiles)
		if commits, err := s.commits(ctx, r, tref, detail); err == nil {
			ch.Summary.Commits = len(commits)
			if detail {
				ch.Commits = commits
			}
			d.writers = s.writersOf(ctx, r.id, commits)
			for _, c := range commits {
				if c.Agent != "" && ch.Agent == "" {
					ch.Agent = c.Agent
				}
			}
			if len(commits) > 0 {
				ch.UpdatedAt = commits[0].Date
				ch.CreatedAt = commits[len(commits)-1].Date
			}
		}
	}
	if ch.Author == "" && r.pr != nil {
		ch.Author = r.pr.Author
	}
	author := s.identify(ctx, ch.Author)
	ch.AuthorKind = author.Kind
	if ch.AuthorKind == "" {
		ch.AuthorKind = identity.Human
	}

	// MR, reviews and statuses from the forge cache.
	if r.pr != nil {
		ch.MR = &MR{Number: r.pr.Number, URL: r.pr.URL, Draft: r.pr.Draft, State: string(r.pr.State), Title: r.pr.Title, Body: r.pr.Body}
		ch.Description = r.pr.Body
		if ch.CreatedAt.IsZero() {
			ch.CreatedAt = r.pr.CreatedAt
		}
		if r.pr.UpdatedAt.After(ch.UpdatedAt) {
			ch.UpdatedAt = r.pr.UpdatedAt
		}
		if r.pr.State == forge.PROpen {
			d.reviews = s.cachedReviews(r.pr.Number)
		}
	}
	if ch.HeadSHA != "" && r.remoteSHA == ch.HeadSHA {
		d.statuses = s.cachedStatuses(ch.HeadSHA)
	}

	// Lab.
	s.labState(ctx, d, m)
	ch.LabRuns = m.LabRuns

	// Merge.
	merged := r.merge != nil || (r.pr != nil && r.pr.State == forge.PRMerged)
	if r.merge != nil {
		ch.MergeSHA, ch.MergedAt = r.merge.SHA, r.merge.CommitDate
		if ch.Title == "" {
			_, ch.Title, _ = TitleFromMergeSubject(r.merge.Subject)
		}
		approved := TrailerList(r.merge.Trailers, TrailerApprovedBy)
		ch.ApprovalsHave = len(approved)
		ch.ApprovalsNeed = len(approved)
		for _, u := range approved {
			ch.Approvals = append(ch.Approvals, policy.Approval{User: u, Counts: true})
		}
		if ch.CreatedAt.IsZero() {
			ch.CreatedAt = r.merge.AuthorDate
		}
		if ch.UpdatedAt.IsZero() || r.merge.CommitDate.After(ch.UpdatedAt) {
			ch.UpdatedAt = r.merge.CommitDate
		}
	} else if r.pr != nil && r.pr.State == forge.PRMerged {
		ch.MergeSHA, ch.MergedAt = r.pr.MergedSHA, r.pr.MergedAt
	}
	if merged && ch.MergeSHA != "" {
		ch.Rollout = s.rollout(ctx, ch.MergeSHA)
	}

	// Gates for open changes.
	open := !merged && !(r.pr != nil && r.pr.State == forge.PRClosed)
	if open && s.o.Policy != nil && headRev != "" {
		if g, err := s.gates(ctx, d, author); err == nil {
			ch.Gates = g
			ch.ApprovalsHave, ch.ApprovalsNeed = g.Approvals.Have, g.Approvals.Need
			ch.Risk = g.Risk.Level
		} else {
			s.log.Debug("gates", "change", r.id, "err", err)
		}
	}
	if ch.Gates == nil && open {
		for _, rv := range d.reviews {
			if rv.State == forge.ReviewApproved {
				ch.ApprovalsHave++
			}
		}
	}
	if open {
		freshBy := map[string]bool{}
		for _, rv := range d.reviews {
			if rv.State == forge.ReviewApproved && !rv.Stale {
				freshBy[rv.User] = true
			}
		}
		for _, rv := range d.reviews {
			if rv.State != forge.ReviewApproved || (ch.Gates != nil && rv.Stale && freshBy[rv.User]) {
				continue
			}
			a := policy.Approval{User: rv.User, SHA: rv.SHA, Counts: ch.Gates == nil}
			if ch.Gates != nil {
				for _, u := range ch.Gates.Approvals.By {
					if u == rv.User {
						a.Counts = true
					}
				}
				for _, ig := range ch.Gates.Approvals.Ignored {
					if ig.User == rv.User && !a.Counts {
						a.Reason = ig.Reason
					}
				}
			}
			ch.Approvals = append(ch.Approvals, a)
		}
	}

	ch.State = DeriveState(StateInput{
		Merged: merged, Closed: r.pr != nil && r.pr.State == forge.PRClosed,
		HasMR: r.pr != nil && r.pr.State == forge.PROpen, Draft: r.pr != nil && r.pr.Draft,
		Lab: ch.LabState, ApprovalsHave: ch.ApprovalsHave, ApprovalsNeed: ch.ApprovalsNeed,
		Rollout: ch.Rollout,
	})

	if ch.State.Open() {
		s.noteState(r.id, ch.State)
		ch.Summary.Overlaps = overlapIDs(s.overlaps(r.id))
	} else {
		s.forgetObjects(r.id)
	}

	approvedByMe, canApprove := false, true
	for _, rv := range d.reviews {
		if rv.User == me && rv.State == forge.ReviewApproved && !(ch.Gates != nil && rv.Stale) {
			approvedByMe = true
		}
	}
	if ch.Gates != nil {
		canApprove = contains(ch.Gates.Approvals.Eligible, me) || len(ch.Gates.Approvals.Eligible) == 0
	}
	ch.Group = GroupOf(GroupInput{
		Me: me, Author: ch.Author, AuthorKind: ch.AuthorKind, AuthorActsFor: author.ActsFor,
		Ready: ch.State == Review || ch.State == Approved, ApprovedByMe: approvedByMe, CanApprove: canApprove,
	})
	return d, nil
}

// writersOf returns the identities that wrote content commits. Record
// commits (only change metadata and the change's lab reports, as after a lab
// run on someone else's change) do not make a writer, so a reviewer who ran
// the lab still counts as an approver.
func (s *Svc) writersOf(ctx context.Context, id string, commits []Commit) []identity.Identity {
	var out []identity.Identity
	seen := map[string]bool{}
	for _, c := range commits {
		var who []string
		for _, u := range []string{c.Author, c.Agent} {
			if u != "" && !seen[u] {
				who = append(who, u)
			}
		}
		if len(who) == 0 {
			continue
		}
		if s.recordCommit(ctx, id, c) {
			continue
		}
		for _, u := range who {
			seen[u] = true
			out = append(out, s.identify(ctx, u))
		}
	}
	return out
}

// recordCommit reports whether a commit only touches records of the change
// (cached per sha: commits do not change).
func (s *Svc) recordCommit(ctx context.Context, id string, c Commit) bool {
	key := id + "@" + c.SHA
	s.cmu.Lock()
	v, ok := s.recCache[key]
	s.cmu.Unlock()
	if ok {
		return v
	}
	files := c.Files
	if files == nil {
		fs, err := s.repo.ChangedFiles(ctx, c.SHA)
		if err != nil {
			return false
		}
		files = []string{}
		for _, f := range fs {
			files = append(files, f.Path)
			if f.OldPath != "" {
				files = append(files, f.OldPath)
			}
		}
	}
	v = RecordOnly(id, files)
	s.cmu.Lock()
	if len(s.recCache) > 4096 {
		s.recCache = map[string]bool{}
	}
	s.recCache[key] = v
	s.cmu.Unlock()
	return v
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func statusWord(code string) string {
	switch code {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R", "C":
		return "renamed"
	case "?":
		return "untracked"
	}
	return "modified"
}

// mergedDetail fills files and commits of a merged change from its merge
// commit: first parent (main before) to second parent (the change head).
func (s *Svc) mergedDetail(ctx context.Context, d *derived, detail bool) {
	ch, m := d.ch, d.rec.merge
	base, head := m.Parents[0], m.Parents[1]
	if mb, err := s.repo.MergeBase(ctx, base, head); err == nil && mb != "" {
		base = mb
	}
	ch.BaseSHA = base
	if ch.HeadSHA == "" || d.rec.wt == "" {
		ch.HeadSHA = head
	}
	ch.AheadTarget, ch.BehindTarget = 0, 0
	// Its commits reached the forge, also when the branch is deleted now.
	ch.Pushed, ch.AheadRemote, ch.BehindRemote = true, 0, 0
	names, _ := s.repo.DiffNames(ctx, base, head)
	for _, f := range names {
		ch.ChangedFiles = append(ch.ChangedFiles, FileStatus{Path: f.Path, OldPath: f.OldPath, Status: statusWord(f.Code), Committed: true})
		d.paths = append(d.paths, f.Path)
	}
	sort.Strings(d.paths)
	ch.Files = len(ch.ChangedFiles)
	commits, err := s.commitRange(ctx, base, head, nil, detail)
	if err != nil {
		return
	}
	for i := range commits {
		commits[i].Pushed = true
	}
	ch.Summary.Commits = len(commits)
	if detail {
		ch.Commits = commits
	}
	for _, c := range commits {
		if c.Agent != "" && ch.Agent == "" {
			ch.Agent = c.Agent
		}
	}
	if len(commits) > 0 {
		ch.UpdatedAt = commits[0].Date
		ch.CreatedAt = commits[len(commits)-1].Date
	}
}

// commits lists target..head, newest first.
func (s *Svc) commits(ctx context.Context, r *record, tref string, files bool) ([]Commit, error) {
	head := r.headRev()
	if head == "" {
		return nil, nil
	}
	if sha := r.headSHA(); sha != "" {
		head = sha
	}
	var pushed map[string]bool
	if r.remoteSHA != "" {
		pushed = map[string]bool{}
		if out, err := s.repo.Run(ctx, "rev-list", tref+".."+r.remoteSHA); err == nil {
			for _, l := range strings.Fields(out) {
				pushed[l] = true
			}
		}
	}
	return s.commitRange(ctx, tref, head, pushed, files)
}

// commitRange lists from..to without merges, newest first.
func (s *Svc) commitRange(ctx context.Context, from, to string, pushed map[string]bool, files bool) ([]Commit, error) {
	log, err := s.repo.Log(ctx, from+".."+to, "--no-merges")
	if err != nil {
		return nil, err
	}
	out := make([]Commit, 0, len(log))
	for _, e := range log {
		c := Commit{
			SHA: e.SHA, Subject: e.Subject, Body: e.Body, Author: userOfEmail(e.AuthorEmail),
			AuthorName: e.AuthorName, AuthorEmail: e.AuthorEmail, Date: e.AuthorDate, Trailers: e.Trailers,
			Agent: Trailer(e.Trailers, TrailerAgent), OnBehalfOf: Trailer(e.Trailers, TrailerOnBehalfOf),
			Pushed: pushed[e.SHA],
		}
		if files {
			if fs, err := s.repo.ChangedFiles(ctx, e.SHA); err == nil {
				for _, f := range fs {
					c.Files = append(c.Files, f.Path)
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// ---------------------------------------------------------------- lab

// labState sets LabState and LabRun of a change head.
func (s *Svc) labState(ctx context.Context, d *derived, m Meta) {
	ch := d.ch
	ch.LabState = "none"
	runs := append([]LabRunRef(nil), m.LabRuns...)
	// listed are runs the change records but the lab store does not know
	// (seeded, another bench, or written into the metadata by hand). With a
	// lab store they never set the state: only the store or a pk/lab status
	// on the pushed head does, so a run written into .pk/changes/<id>.yaml
	// does not meet the lab gate.
	var listed []LabRunRef
	if s.o.Labs != nil {
		if ls := s.o.Labs(); ls != nil {
			if lr, err := ls.Runs(ctx, d.rec.id); err == nil {
				known := map[string]bool{}
				for _, r := range lr {
					known[r.ID] = true
				}
				listed = slices.DeleteFunc(runs, func(r LabRunRef) bool { return known[r.Run] })
				runs = nil
				for _, r := range lr {
					if r.Result == lab.ResultRunning {
						if r.SHA == "" || r.SHA == ch.HeadSHA || ch.LabState != "running" {
							ch.LabState, ch.LabRun = "running", r.ID
						}
						continue
					}
					// A cancelled run tested nothing: the run before it stays in effect.
					if r.Result == lab.ResultCancelled {
						continue
					}
					runs = appendRun(runs, LabRunRef{Run: r.ID, SHA: r.SHA, Result: LabResult(r.Result), Report: r.Report})
				}
			}
		}
	}
	runs = slices.DeleteFunc(runs, func(r LabRunRef) bool { return r.Result == LabCancelled })
	sort.SliceStable(runs, func(i, j int) bool { return runNumber(runs[i].Run) > runNumber(runs[j].Run) })
	if ch.LabState == "running" {
		return
	}
	if len(runs) > 0 && ch.LabRun == "" {
		ch.LabRun = runs[0].Run
	}
	defer func() {
		if len(runs) == 0 && len(listed) > 0 && ch.LabRun == "" {
			ch.LabRun = listed[len(listed)-1].Run
		}
	}()
	headRev := d.rec.headRev()
	for i := range runs {
		r := runs[i]
		if r.SHA == "" || r.Result == LabRunning || headRev == "" {
			continue
		}
		if !s.repo.Exists(ctx, r.SHA) {
			continue
		}
		names, err := s.repo.DiffNames(ctx, r.SHA, headRev)
		if err != nil {
			continue
		}
		var changed []string
		for _, f := range names {
			changed = append(changed, f.Path)
			if f.OldPath != "" {
				changed = append(changed, f.OldPath)
			}
		}
		if d.rec.wt != "" {
			if st, err := gitx.Open(d.rec.wt).Status(ctx); err == nil {
				for _, f := range st {
					changed = append(changed, f.Path)
				}
			}
		}
		ch.LabRun = r.Run
		if LabCoversHead(d.rec.id, changed) {
			ch.LabState = string(r.Result)
			if r.Result == LabPass {
				rr := r
				d.labRun = &rr
			}
		}
		return
	}
	// Seeded or remote runs: the pk/lab status on the pushed head.
	for _, st := range d.statuses {
		if st.Context != forge.ContextLab {
			continue
		}
		switch st.State {
		case forge.StatusSuccess:
			ch.LabState = "pass"
		case forge.StatusFailure, forge.StatusError:
			ch.LabState = "fail"
		case forge.StatusPending:
			ch.LabState = "running"
		}
		if ch.LabState == "pass" {
			cand := append(append([]LabRunRef(nil), runs...), listed...)
			sort.SliceStable(cand, func(i, j int) bool { return runNumber(cand[i].Run) > runNumber(cand[j].Run) })
			for i := range cand {
				if cand[i].Result == LabPass {
					rr := cand[i]
					rr.SHA = ch.HeadSHA
					d.labRun = &rr
					ch.LabRun = rr.Run
					break
				}
			}
		}
	}
}

func appendRun(runs []LabRunRef, r LabRunRef) []LabRunRef {
	for i := range runs {
		if runs[i].Run == r.Run {
			if runs[i].SHA == "" {
				runs[i].SHA = r.SHA
			}
			if r.Result != "" {
				runs[i].Result = r.Result
			}
			if r.Report != "" {
				runs[i].Report = r.Report
			}
			return runs
		}
	}
	return append(runs, r)
}

func runNumber(run string) int {
	_, n, ok := strings.Cut(run, "/run-")
	if !ok {
		return 0
	}
	var x int
	fmt.Sscanf(n, "%d", &x)
	return x
}

// ---------------------------------------------------------------- list

func (s *Svc) me(ctx context.Context) string {
	if id, ok := identity.From(ctx); ok {
		return id.Principal()
	}
	return s.o.User
}

func (s *Svc) List(ctx context.Context, all bool) ([]Summary, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	recs, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	me := s.me(ctx)
	out := []Summary{}
	for _, r := range recs {
		d, err := s.derive(ctx, r, me, false)
		if err != nil {
			s.log.Warn("change", "id", r.id, "err", err)
			continue
		}
		if !all && !d.ch.State.Open() {
			continue
		}
		out = append(out, d.ch.Summary)
	}
	// Second pass: overlap needs the objects of all open changes.
	for i := range out {
		if out[i].State.Open() {
			out[i].Overlaps = overlapIDs(s.overlaps(out[i].ID))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := out[i].State.Open(), out[j].State.Open()
		if oi != oj {
			return oi
		}
		ti, tj := out[i].UpdatedAt, out[j].UpdatedAt
		if !out[i].MergedAt.IsZero() {
			ti = out[i].MergedAt
		}
		if !out[j].MergedAt.IsZero() {
			tj = out[j].MergedAt
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Svc) Get(ctx context.Context, id string) (*Change, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	r, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	me := s.me(ctx)
	d, err := s.derive(ctx, r, me, true)
	if err != nil {
		return nil, err
	}
	ch := d.ch
	if ch.State.Open() {
		recs, err := s.records(ctx)
		if err == nil {
			s.warmObjects(ctx, recs, me)
		}
		ch.Overlaps = s.overlaps(id)
		if err == nil {
			ch.Overlaps = s.freshOverlaps(ctx, recs, me, ch.Overlaps)
		}
		ch.Summary.Overlaps = overlapIDs(ch.Overlaps)
	}
	ch.Commits = nonNilCommits(ch.Commits)
	ch.LabRuns = mergeLabRuns(ctx, s, id, ch.LabRuns)
	if ch.HeadSHA != "" {
		recs, _ := s.records(ctx)
		for _, o := range recs {
			if o.id == id {
				continue
			}
			om, _ := s.meta(ctx, o)
			if om.Target == ch.Branch && (o.merge == nil) {
				ch.Children = append(ch.Children, o.id)
			}
		}
	}
	return ch, nil
}

func nonNilCommits(c []Commit) []Commit {
	if c == nil {
		return []Commit{}
	}
	return c
}

// mergeLabRuns adds the runs of the lab service to the recorded runs.
func mergeLabRuns(ctx context.Context, s *Svc, id string, runs []LabRunRef) []LabRunRef {
	out := append([]LabRunRef(nil), runs...)
	if s.o.Labs != nil {
		if ls := s.o.Labs(); ls != nil {
			if lr, err := ls.Runs(ctx, id); err == nil {
				for _, r := range lr {
					out = appendRun(out, LabRunRef{Run: r.ID, SHA: r.SHA, Result: LabResult(r.Result), Report: r.Report})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return runNumber(out[i].Run) < runNumber(out[j].Run) })
	return out
}

func (s *Svc) Heads(ctx context.Context) ([]Head, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	recs, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	var out []Head
	for _, r := range recs {
		if r.merge != nil || (r.pr != nil && r.pr.State != forge.PROpen) || r.headRev() == "" {
			continue
		}
		m, _ := s.meta(ctx, r)
		h := Head{ID: r.id, Title: m.Title, Author: m.Author, Branch: r.branch, Target: m.Target,
			HeadSHA: r.headSHA(), Worktree: r.wt, State: Draft}
		if h.Author == "" && r.pr != nil {
			h.Author = r.pr.Author
		}
		if r.pr != nil && !r.pr.Draft {
			h.State = Review
		}
		if r.wt != "" {
			if st, err := gitx.Open(r.wt).Status(ctx); err == nil {
				h.Dirty = len(st) > 0
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// rollout is the Flux state of a merge commit on the clusters it impacts.
func (s *Svc) rollout(ctx context.Context, mergeSHA string) []Rollout {
	if s.o.Flux == nil {
		return nil
	}
	fc := s.o.Flux()
	if fc == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := fc.Status(ctx)
	if err != nil || st == nil {
		return nil
	}
	impacted := s.impactedClusters(ctx, mergeSHA)
	var out []Rollout
	for _, c := range st.Clusters {
		if impacted != nil && !contains(impacted, c.ID) {
			continue
		}
		r := Rollout{Cluster: c.ID, Ring: c.Ring}
		if c.Applied != nil && c.Applied.SHA != "" {
			r.SHA, r.At = c.Applied.SHA, c.Applied.At
			r.Deployed = c.Applied.SHA == mergeSHA || s.isAncestor(ctx, mergeSHA, c.Applied.SHA)
		}
		// Verified needs the cluster healthy now: Flux keeps the last
		// verified revision while health checks fail.
		if c.Verified != nil && c.Verified.SHA != "" && c.Healthy {
			r.Verified = r.Deployed && (c.Verified.SHA == mergeSHA || s.isAncestor(ctx, mergeSHA, c.Verified.SHA))
		}
		out = append(out, r)
	}
	return out
}

func (s *Svc) isAncestor(ctx context.Context, a, b string) bool {
	if !s.repo.Exists(ctx, b) {
		return false
	}
	ok, err := s.repo.IsAncestor(ctx, a, b)
	return err == nil && ok
}

// impactedClusters returns the clusters whose render a merge changed (nil
// when unknown: then all clusters count). Results are cached per sha.
func (s *Svc) impactedClusters(ctx context.Context, mergeSHA string) []string {
	s.cmu.Lock()
	if c, ok := s.rollCache[mergeSHA]; ok {
		s.cmu.Unlock()
		return c
	}
	s.cmu.Unlock()
	var clusters []string
	files, err := s.repo.ChangedFiles(ctx, mergeSHA)
	if err != nil {
		return nil
	}
	if s.o.Renderer != nil {
		if parent, err := s.repo.RevParse(ctx, mergeSHA+"^1"); err == nil {
			base, err1 := s.shaSource(ctx, parent, parent)
			head, err2 := s.shaSource(ctx, mergeSHA, mergeSHA)
			if err1 == nil && err2 == nil {
				if imp, err := s.o.Renderer.Impact(ctx, base, head); err == nil {
					clusters = []string{}
					for _, c := range imp.Clusters {
						if a, ch, d := c.Counts(); a+ch+d > 0 {
							clusters = append(clusters, c.Cluster)
						}
					}
				}
			}
		}
	}
	if clusters == nil {
		for _, f := range files {
			if rest, ok := strings.CutPrefix(f.Path, "clusters/"); ok {
				if c, _, ok := strings.Cut(rest, "/"); ok && !contains(clusters, c) {
					clusters = append(clusters, c)
				}
			}
		}
		if len(clusters) == 0 {
			clusters = nil
		}
	}
	if len(clusters) == 0 {
		clusters = nil
	}
	s.cmu.Lock()
	s.rollCache[mergeSHA] = clusters
	s.cmu.Unlock()
	return clusters
}
