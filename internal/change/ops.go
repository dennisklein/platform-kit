package change

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/policy"
)

// BehindError is a merge refusal of a branch that is not rebased.
type BehindError struct {
	Target string
	N      int
}

func (e *BehindError) Error() string {
	return fmt.Sprintf("behind %s by %d, rebase first", e.Target, e.N)
}

func (e *BehindError) Unwrap() error { return ErrBehind }

// ---------------------------------------------------------------- create

func (s *Svc) Create(ctx context.Context, req CreateRequest) (*Change, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Title)
	if len(title) < 3 {
		return nil, fmt.Errorf("%w: a change needs a title", ErrConflict)
	}
	who := s.actor(ctx)
	if who.Kind == identity.Bot {
		if err := s.may(ctx, who, policy.RightPush); err != nil {
			return nil, err
		}
	}
	target, parent := strings.TrimSpace(req.Target), strings.TrimSpace(req.Parent)
	if target == "" {
		target = "main"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.fetchLocked(ctx)
	recs, err := s.records(ctx)
	if err != nil {
		return nil, err
	}
	// A target can be a change id or the branch of an open change (stack).
	if ValidID(target) {
		r, ok := recs[target]
		if !ok || r.branch == "" {
			return nil, fmt.Errorf("%w: target %s", ErrNotFound, target)
		}
		parent, target = target, r.branch
	} else if id, _, ok := ParseBranch(target); ok && parent == "" {
		parent = id
	}
	base := mainRef
	if target != "main" {
		switch {
		case s.repo.Exists(ctx, localRef(target)):
			base = localRef(target)
		case s.repo.Exists(ctx, remoteRef(target)):
			base = remoteRef(target)
		default:
			return nil, fmt.Errorf("%w: target branch %s", ErrNotFound, target)
		}
	}
	var id string
	for i := 0; i < 20; i++ {
		id = NewID()
		if _, taken := recs[id]; !taken {
			break
		}
	}
	branch := Branch(id, title)
	dir := s.worktreeDir(id)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if err := s.repo.WorktreeAdd(ctx, dir, branch, base, true); err != nil {
		return nil, err
	}
	m := Meta{ID: id, Title: title, Intent: strings.TrimSpace(req.Intent), Author: who.Principal(),
		Target: target, Parent: parent, WorkItems: req.WorkItems}
	if who.Kind == identity.Agent {
		m.Agent = who.User
	}
	wt := gitx.Open(dir)
	if err := writeFile(dir, MetaPath(id), MarshalMeta(m)); err != nil {
		return nil, err
	}
	paths := []string{MetaPath(id)}
	// A work item lists its changes: the opening commit adds the change to
	// work/items/<item>.yaml (same review and history as all state).
	for _, w := range req.WorkItems {
		if p, ok := linkWorkItem(dir, w, id); ok {
			paths = append(paths, p)
		}
	}
	if err := wt.Add(ctx, paths...); err != nil {
		return nil, err
	}
	if _, err := wt.Commit(ctx, gitx.CommitOptions{
		Message: "chore(change): open " + id, Author: signature(who), Trailers: agentTrailers(who),
		Paths: paths,
	}); err != nil {
		return nil, err
	}
	ch, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s.o.Events.Publish(events.ChangeCreated, changeEvent(ch.Summary, "create"))
	return ch, nil
}

var (
	reWorkItemID  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	reFlowChanges = regexp.MustCompile(`(?m)^changes:[ \t]*\[([^\]\n]*)\][ \t]*(#.*)?$`)
	reBlockChange = regexp.MustCompile(`(?m)^changes:[ \t]*(#.*)?\n((?:[ \t]+-[^\n]*\n)*)`)
)

// linkWorkItem adds a change id to the changes: list of a work item file in
// a worktree. It keeps the file as written (flow or block list) and returns
// the path when it changed the file.
func linkWorkItem(dir, item, id string) (string, bool) {
	if !reWorkItemID.MatchString(item) {
		return "", false
	}
	p := "work/items/" + item + ".yaml"
	full := filepath.Join(dir, filepath.FromSlash(p))
	b, err := os.ReadFile(full)
	if err != nil {
		return "", false
	}
	text := string(b)
	switch {
	case reFlowChanges.MatchString(text):
		m := reFlowChanges.FindStringSubmatchIndex(text)
		inner := strings.TrimSpace(text[m[2]:m[3]])
		for _, c := range strings.Split(inner, ",") {
			if strings.Trim(strings.TrimSpace(c), `"'`) == id {
				return "", false
			}
		}
		if inner == "" {
			inner = id
		} else {
			inner += ", " + id
		}
		text = text[:m[2]] + inner + text[m[3]:]
	case reBlockChange.MatchString(text):
		m := reBlockChange.FindStringSubmatchIndex(text)
		items := text[m[4]:m[5]]
		indent := "  "
		for _, line := range strings.Split(strings.TrimRight(items, "\n"), "\n") {
			if line == "" {
				continue
			}
			indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			if strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-")), `"'`) == id {
				return "", false
			}
		}
		text = text[:m[5]] + indent + "- " + id + "\n" + text[m[5]:]
	case !strings.Contains(text, "\nchanges:") && !strings.HasPrefix(text, "changes:"):
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "changes: [" + id + "]\n"
	default:
		return "", false
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		return "", false
	}
	return p, true
}

func writeFile(dir, p string, b []byte) error {
	full := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, b, 0o644)
}

// ---------------------------------------------------------------- checkout, close

func (s *Svc) Checkout(ctx context.Context, id string) (*Summary, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_ = s.fetchLocked(ctx)
	r, err := s.record(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if r.wt == "" {
		if r.remoteSHA == "" {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %s has no branch on the forge", ErrNotFound, id)
		}
		dir := s.worktreeDir(id)
		_ = os.RemoveAll(dir)
		_, _ = s.repo.Run(ctx, "worktree", "prune")
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if s.repo.Exists(ctx, localRef(r.branch)) {
			// Reset a stale local branch to the forge.
			if _, err := s.repo.Run(ctx, "branch", "-f", r.branch, remoteRef(r.branch)); err != nil {
				s.mu.Unlock()
				return nil, err
			}
			err = s.repo.WorktreeAdd(ctx, dir, r.branch, "", false)
		} else {
			err = s.repo.WorktreeAdd(ctx, dir, r.branch, remoteRef(r.branch), true)
		}
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()
	ch, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s.o.Events.Publish(events.ChangeUpdated, changeEvent(ch.Summary, "checkout"))
	return &ch.Summary, nil
}

func (s *Svc) Close(ctx context.Context, id string) (*Summary, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	who := s.actor(ctx)
	s.mu.Lock()
	r, err := s.record(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if r.merge != nil || (r.pr != nil && r.pr.State == forge.PRMerged) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is merged", ErrConflict, id)
	}
	ch, _ := s.Get(ctx, id)
	own, err := s.mayClose(ctx, who, ch)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !own && (r.pr == nil || r.pr.State != forge.PROpen) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: only the author closes a change without an open merge request", ErrForbidden)
	}
	if r.pr != nil && r.pr.State == forge.PROpen && s.o.Forge != nil {
		closed := forge.PRClosed
		if _, err := s.o.Forge.UpdatePR(ctx, s.forgeUser(who), r.pr.Number, forge.PRUpdate{State: &closed}); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.refreshPR(ctx, r.pr.Number)
	}
	if !own {
		// Someone else's change: the closed MR records it; the author's
		// worktree and branches stay.
		s.mu.Unlock()
		var out Summary
		if ch != nil {
			out = ch.Summary
		}
		out.State = Closed
		s.o.Events.Publish(events.ChangeUpdated, changeEvent(out, "close"))
		return &out, nil
	}
	// Closing abandons the change: uncommitted edits go with the worktree
	// (the UI asks first), else the change would come back as a draft.
	if r.wt != "" {
		if err := s.repo.WorktreeRemove(ctx, r.wt, true); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("remove worktree: %w", err)
		}
		_, _ = s.repo.Run(ctx, "worktree", "prune")
	}
	if r.branch != "" && r.localSHA != "" {
		_, _ = s.repo.Run(ctx, "branch", "-D", r.branch)
	}
	// Without an MR nothing records the close: a pushed branch alone would
	// list the change as a draft again, so it goes too.
	if r.pr == nil && r.remoteSHA != "" {
		if err := s.deleteRemoteBranch(ctx, who, r.branch); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()
	var out Summary
	if ch != nil {
		out = ch.Summary
	}
	out.State = Closed
	out.Local = false
	s.o.Events.Publish(events.ChangeUpdated, changeEvent(out, "close"))
	return &out, nil
}

// mayClose checks who may close a change. Agents and bots need the push
// right and close only changes of their own or of the person they act for.
// People close their own changes (and those of their agents) fully; own is
// false for someone else's change, which they may only close on the forge
// (the merge request), never by deleting the author's branch.
func (s *Svc) mayClose(ctx context.Context, who identity.Identity, ch *Change) (own bool, err error) {
	author := ""
	if ch != nil {
		author = ch.Author
	}
	var actsFor string
	if author != "" {
		actsFor = s.identify(ctx, author).ActsFor
	}
	mine := author == "" || author == who.User || (actsFor != "" && actsFor == who.User)
	if who.Kind != identity.Human {
		if err := s.may(ctx, who, policy.RightPush); err != nil {
			return false, err
		}
		if author == "" || !mine && !(who.ActsFor != "" && (author == who.ActsFor || actsFor == who.ActsFor)) {
			return false, fmt.Errorf("%w: %s may close only changes of %s", ErrForbidden, who.User, firstNonEmpty(who.ActsFor, who.User))
		}
		return true, nil
	}
	return mine, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// deleteRemoteBranch deletes a change branch on the forge and its
// remote-tracking ref.
func (s *Svc) deleteRemoteBranch(ctx context.Context, who identity.Identity, branch string) error {
	tok, err := s.tokenFor(who)
	if err != nil {
		return err
	}
	po := gitx.PushOptions{Refspecs: []string{":refs/heads/" + branch}, User: s.forgeUser(who), Token: tok}
	if err := s.repo.Push(ctx, po); err != nil {
		return fmt.Errorf("%w: delete branch %s: %v", forge.ErrUnreachable, branch, err)
	}
	_, _ = s.repo.Run(ctx, "update-ref", "-d", remoteRef(branch))
	return nil
}

// forgeUser is the forge account of an identity (people and agents have own accounts).
func (s *Svc) forgeUser(id identity.Identity) string {
	if _, ok := s.o.Token(id.User); ok {
		return id.User
	}
	if _, ok := s.o.Token(id.Principal()); ok && id.Kind != identity.Agent {
		return id.Principal()
	}
	return id.User
}

// ---------------------------------------------------------------- status, commits

func (s *Svc) Status(ctx context.Context, id string) ([]FileStatus, error) {
	dir, err := s.Worktree(ctx, id)
	if err != nil {
		return nil, err
	}
	st, err := gitx.Open(dir).Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]FileStatus, 0, len(st))
	for _, f := range st {
		out = append(out, FileStatus{Path: f.Path, OldPath: f.OldPath, Status: statusWord(f.Code), Staged: f.Staged})
	}
	return out, nil
}

func (s *Svc) Commits(ctx context.Context, id string) ([]Commit, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	r, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	m, _ := s.meta(ctx, r)
	r.target = m.Target
	c, err := s.commits(ctx, r, s.targetRef(ctx, r), true)
	return nonNilCommits(c), err
}

func (s *Svc) Commit(ctx context.Context, id, message string, paths []string) (*Commit, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, fmt.Errorf("%w: empty commit message", ErrConflict)
	}
	who := s.actor(ctx)
	if who.Kind != identity.Human {
		if err := s.may(ctx, who, policy.RightPush); err != nil {
			return nil, err
		}
	}
	dir, err := s.Worktree(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wt := gitx.Open(dir)
	st, err := wt.Status(ctx)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, f := range st {
		if len(paths) > 0 && !inPaths(f.Path, paths) && !inPaths(f.OldPath, paths) {
			continue
		}
		changed = append(changed, f.Path)
		if f.OldPath != "" {
			changed = append(changed, f.OldPath)
		}
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("%w: nothing to commit", ErrConflict)
	}
	// Path rights of the writer (the forge and pk ci check them again).
	if p := s.policy(ctx); p != nil {
		var check []string
		for _, c := range changed {
			if !strings.HasPrefix(c, MetaDir+"/") {
				check = append(check, c)
			}
		}
		for _, pr := range s.o.Policy.CanWrite(p, who, check) {
			if !pr.OK {
				msg := pr.Message
				if msg == "" {
					msg = fmt.Sprintf("%s may not write %s (rule %s)", who.User, pr.Path, pr.Rule)
				}
				return nil, fmt.Errorf("%w: %s", ErrForbidden, msg)
			}
		}
	}
	if _, err := wt.Run(ctx, append([]string{"add", "-A", "--"}, changed...)...); err != nil {
		return nil, err
	}
	sha, err := wt.Commit(ctx, gitx.CommitOptions{
		Message: message, Author: signature(who), Trailers: agentTrailers(who), Paths: changed,
	})
	if err != nil {
		return nil, err
	}
	log, err := wt.Log(ctx, sha, "-n", "1")
	if err != nil || len(log) == 0 {
		return nil, fmt.Errorf("commit %s: %v", sha, err)
	}
	e := log[0]
	c := &Commit{SHA: e.SHA, Subject: e.Subject, Body: e.Body, Author: userOfEmail(e.AuthorEmail),
		AuthorName: e.AuthorName, AuthorEmail: e.AuthorEmail, Date: e.AuthorDate, Trailers: e.Trailers,
		Agent: Trailer(e.Trailers, TrailerAgent), OnBehalfOf: Trailer(e.Trailers, TrailerOnBehalfOf)}
	if fs, err := s.repo.ChangedFiles(ctx, sha); err == nil {
		for _, f := range fs {
			c.Files = append(c.Files, f.Path)
		}
	}
	s.notify(ctx, events.ChangeUpdated, id, "commit")
	return c, nil
}

func inPaths(p string, paths []string) bool {
	if p == "" {
		return false
	}
	for _, x := range paths {
		x = strings.TrimSuffix(x, "/")
		if p == x || strings.HasPrefix(p, x+"/") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- push

// pushLocked pushes the branch of a worktree change and opens or updates
// the merge request. ready opens a ready MR.
func (s *Svc) pushLocked(ctx context.Context, who identity.Identity, id string, force bool, lease string) (*PushResult, error) {
	res, _, err := s.pushID(ctx, who, id, force, lease)
	return res, err
}

// pushID is pushLocked that also returns the id (new after a rename).
func (s *Svc) pushID(ctx context.Context, who identity.Identity, id string, force bool, lease string) (*PushResult, string, error) {
	r, err := s.record(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if r.wt == "" {
		return nil, "", fmt.Errorf("%w: %s", ErrNoWorktree, id)
	}
	tok, err := s.tokenFor(who)
	if err != nil {
		return nil, "", err
	}
	user := s.forgeUser(who)
	res := &PushResult{}
	// First push: the id must be free on the forge.
	if r.remoteSHA == "" && r.pr == nil {
		remote, err := s.repo.RemoteBranches(ctx, user, tok, "origin", "change/"+id+"*")
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", forge.ErrUnreachable, err)
		}
		for b := range remote {
			if b != r.branch {
				newID, err := s.renameLocked(ctx, who, r)
				if err != nil {
					return nil, "", err
				}
				res.RenamedFrom, id = id, newID
				if r, err = s.record(ctx, id); err != nil {
					return nil, "", err
				}
				break
			}
		}
	}
	local, err := s.repo.RevParse(ctx, localRef(r.branch))
	if err != nil {
		return nil, "", err
	}
	if r.remoteSHA != "" && !force {
		if ok, _ := s.repo.IsAncestor(ctx, r.remoteSHA, local); !ok {
			return nil, "", fmt.Errorf("%w: the branch on the forge has commits this worktree lacks; rebase or check out again", ErrConflict)
		}
	}
	var pushed []string
	if r.remoteSHA != local {
		if out, err := s.repo.Run(ctx, "rev-list", "--abbrev-commit", "--abbrev=7", local, "--not", "--remotes=origin"); err == nil {
			pushed = strings.Fields(out)
		}
		po := gitx.PushOptions{Refspecs: []string{r.branch + ":" + r.branch}, User: user, Token: tok}
		if force {
			po.ForceWithLease, po.Lease = true, lease
		}
		if err := gitx.Open(r.wt).Push(ctx, po); err != nil {
			if strings.Contains(err.Error(), "stale info") || strings.Contains(err.Error(), "rejected") {
				return nil, "", fmt.Errorf("%w: push rejected: %v", ErrConflict, err)
			}
			return nil, "", fmt.Errorf("%w: push: %v", forge.ErrUnreachable, err)
		}
		_ = s.repo.FetchAuth(ctx, user, tok, "origin", "+refs/heads/"+r.branch+":"+remoteRef(r.branch))
		r.remoteSHA = local
	}
	res.Pushed = pushed
	if s.o.Forge != nil {
		m, fromFile := s.meta(ctx, r)
		if r.pr == nil || r.pr.State != forge.PROpen {
			if err := s.may(ctx, who, policy.RightOpenMR); err != nil {
				return nil, "", err
			}
			base := m.Target
			if base == "" {
				base = "main"
			}
			pr, err := s.o.Forge.CreatePR(ctx, user, forge.PRCreate{Title: m.Title, Body: Description(m), Head: r.branch, Base: base, Draft: fromFile})
			if err != nil {
				return nil, "", err
			}
			res.Created = true
			r.pr = pr
		} else if fromFile && r.pr.Body != Description(m) {
			body := Description(m)
			if pr, err := s.o.Forge.UpdatePR(ctx, user, r.pr.Number, forge.PRUpdate{Body: &body}); err == nil {
				r.pr = pr
			}
		}
		s.refreshPR(ctx, r.pr.Number)
		res.MR = &MR{Number: r.pr.Number, URL: r.pr.URL, Draft: r.pr.Draft, State: string(r.pr.State), Title: r.pr.Title}
		s.carryLabStatus(ctx, r, user)
	}
	return res, id, nil
}

// carryLabStatus posts pk/lab on the pushed head when the newest lab run covers it.
func (s *Svc) carryLabStatus(ctx context.Context, r *record, user string) {
	if s.o.Forge == nil || r.remoteSHA == "" {
		return
	}
	d, err := s.derive(ctx, r, s.o.User, false)
	if err != nil || d.ch.HeadSHA != r.remoteSHA {
		return
	}
	var state forge.StatusState
	switch d.ch.LabState {
	case "pass":
		state = forge.StatusSuccess
	case "fail":
		state = forge.StatusFailure
	default:
		return
	}
	for _, st := range s.cachedStatuses(r.remoteSHA) {
		if st.Context == forge.ContextLab && st.State == state {
			return
		}
	}
	desc := d.ch.LabRun + " " + d.ch.LabState
	if err := s.o.Forge.SetStatus(ctx, user, r.remoteSHA, forge.CommitStatus{Context: forge.ContextLab, State: state, Description: desc}); err != nil {
		s.log.Warn("pk/lab status", "change", r.id, "err", err)
	}
}

// renameLocked gives a change a new id after an id collision on the forge.
func (s *Svc) renameLocked(ctx context.Context, who identity.Identity, r *record) (string, error) {
	m, _ := s.meta(ctx, r)
	newID := NewID()
	newBranch := Branch(newID, m.Title)
	wt := gitx.Open(r.wt)
	if _, err := wt.Run(ctx, "branch", "-m", r.branch, newBranch); err != nil {
		return "", err
	}
	newDir := s.worktreeDir(newID)
	if _, err := s.repo.Run(ctx, "worktree", "move", r.wt, newDir); err != nil {
		return "", err
	}
	wt = gitx.Open(newDir)
	m.ID = newID
	if _, err := wt.Run(ctx, "rm", "-q", "--ignore-unmatch", MetaPath(r.id)); err != nil {
		return "", err
	}
	if err := writeFile(newDir, MetaPath(newID), MarshalMeta(m)); err != nil {
		return "", err
	}
	if err := wt.Add(ctx, MetaPath(newID)); err != nil {
		return "", err
	}
	if _, err := wt.Commit(ctx, gitx.CommitOptions{Message: fmt.Sprintf("chore(change): rename %s to %s", r.id, newID),
		Author: signature(who), Trailers: agentTrailers(who)}); err != nil {
		return "", err
	}
	return newID, nil
}

func (s *Svc) Push(ctx context.Context, id string) (*PushResult, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	who := s.actor(ctx)
	if err := s.may(ctx, who, policy.RightPush); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_ = s.fetchLocked(ctx)
	res, newID, err := s.pushID(ctx, who, id, false, "")
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	id = newID
	if ch, err := s.Get(ctx, id); err == nil {
		res.Change = ch.Summary
	}
	reason := "push"
	if res.Created {
		reason = "mr"
	}
	s.o.Events.Publish(events.ChangeUpdated, changeEvent(res.Change, reason))
	if res.MR != nil {
		s.o.Events.Publish(events.ForgeUpdated, forgeEvent("pr", res.Change.ID, res.MR, ""))
	}
	return res, nil
}

// ---------------------------------------------------------------- ready

func (s *Svc) MarkReady(ctx context.Context, id string) (*Change, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	who := s.actor(ctx)
	if err := s.may(ctx, who, policy.RightMarkReady); err != nil {
		return nil, err
	}
	dir, err := s.Worktree(ctx, id)
	if err != nil {
		return nil, err
	}
	if st, _ := gitx.Open(dir).Status(ctx); len(st) > 0 {
		return nil, fmt.Errorf("%w: commit or discard the saved files first", ErrDirty)
	}
	ch, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ch.State.Open() {
		return nil, fmt.Errorf("%w: %s is %s", ErrConflict, id, ch.State)
	}
	if g := ch.Gates; g != nil && g.LabGate.Required && g.LabGate.Status != "met" {
		detail := g.LabGate.Detail
		if detail == "" {
			detail = "the policy needs a green lab run on the head"
		}
		return nil, fmt.Errorf("%w: lab gate %s: %s", ErrGates, g.LabGate.Status, detail)
	}
	s.mu.Lock()
	err = s.readyLocked(ctx, who, id, dir, ch)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	s.o.Events.Publish(events.ChangeUpdated, changeEvent(out.Summary, "ready"))
	if out.MR != nil {
		s.o.Events.Publish(events.ForgeUpdated, forgeEvent("pr", id, out.MR, out.HeadSHA))
	}
	return out, nil
}

func (s *Svc) readyLocked(ctx context.Context, who identity.Identity, id, dir string, ch *Change) error {
	_ = s.fetchLocked(ctx)
	r, err := s.record(ctx, id)
	if err != nil {
		return err
	}
	m, fromFile := s.meta(ctx, r)
	// Lab runs and the agent go into the description.
	for _, lr := range ch.LabRuns {
		m.LabRuns = appendRun(m.LabRuns, lr)
	}
	if m.Agent == "" {
		m.Agent = ch.Agent
	}
	wt := gitx.Open(dir)
	if fromFile {
		if _, err := wt.Run(ctx, "rm", "-q", MetaPath(id)); err != nil {
			return err
		}
		if _, err := wt.Commit(ctx, gitx.CommitOptions{Message: "chore(change): move metadata to the merge request",
			Author: signature(who), Trailers: agentTrailers(who)}); err != nil {
			return err
		}
	}
	res, err := s.pushLocked(ctx, who, id, false, "")
	if err != nil {
		return err
	}
	if s.o.Forge != nil && res.MR != nil {
		draft := false
		body := Description(m)
		title := m.Title
		if _, err := s.o.Forge.UpdatePR(ctx, s.forgeUser(who), res.MR.Number, forge.PRUpdate{Title: &title, Body: &body, Draft: &draft}); err != nil {
			return err
		}
		s.refreshPR(ctx, res.MR.Number)
		if r, err := s.record(ctx, id); err == nil {
			s.carryLabStatus(ctx, r, s.forgeUser(who))
			s.refreshPR(ctx, res.MR.Number)
		}
	}
	return nil
}

// ---------------------------------------------------------------- rebase

func (s *Svc) Rebase(ctx context.Context, id string) (*RebaseResult, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	who := s.actor(ctx)
	if who.Kind != identity.Human {
		if err := s.may(ctx, who, policy.RightPush); err != nil {
			return nil, err
		}
	}
	dir, err := s.Worktree(ctx, id)
	if err != nil {
		return nil, err
	}
	wt := gitx.Open(dir)
	if st, _ := wt.Status(ctx); len(st) > 0 {
		return nil, fmt.Errorf("%w: commit or discard the saved files first", ErrDirty)
	}
	s.mu.Lock()
	if err := s.fetchLocked(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	_ = s.updateMainLocked(ctx)
	r, err := s.record(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	m, _ := s.meta(ctx, r)
	r.target = m.Target
	tref := s.targetRef(ctx, r)
	onto, err := s.repo.RevParse(ctx, tref)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	from, _ := wt.Head(ctx)
	res := &RebaseResult{OK: true, From: from, To: from, Onto: onto}
	if _, behind, _ := s.repo.AheadBehind(ctx, tref, localRef(r.branch)); behind > 0 {
		if err := wt.Rebase(ctx, onto, signature(who)); err != nil {
			s.mu.Unlock()
			var ce *gitx.ErrConflict
			if errors.As(err, &ce) {
				return nil, err
			}
			return nil, fmt.Errorf("rebase: %w", err)
		}
		res.To, _ = wt.Head(ctx)
		if r.remoteSHA != "" {
			if _, err := s.pushLocked(ctx, who, id, true, r.remoteSHA); err != nil {
				s.mu.Unlock()
				return nil, err
			}
		}
	}
	s.mu.Unlock()
	if ch, err := s.Get(ctx, id); err == nil {
		res.Change = ch.Summary
	}
	s.o.Events.Publish(events.ChangeUpdated, changeEvent(res.Change, "rebase"))
	return res, nil
}

// ---------------------------------------------------------------- approve

func (s *Svc) Approve(ctx context.Context, id, comment string) (*policy.Approval, error) {
	who := s.actor(ctx)
	if who.Kind == identity.Agent {
		return nil, ErrAgentDeny
	}
	if who.Kind == identity.Bot {
		return nil, fmt.Errorf("%w: bots never approve or merge", ErrForbidden)
	}
	return s.approveAs(ctx, who.User, id, comment)
}

// ApproveAs approves in the forge as another person (demo helper). The
// caller checks that this is allowed.
func (s *Svc) ApproveAs(ctx context.Context, user, id, comment string) (*policy.Approval, error) {
	if s.identify(ctx, user).Kind != identity.Human {
		return nil, ErrAgentDeny
	}
	return s.approveAs(ctx, user, id, comment)
}

func (s *Svc) approveAs(ctx context.Context, user, id, comment string) (*policy.Approval, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	if s.o.Forge == nil {
		return nil, fmt.Errorf("%w: no forge", forge.ErrUnreachable)
	}
	r, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.pr == nil || r.pr.State != forge.PROpen {
		return nil, fmt.Errorf("%w: %s has no open merge request", ErrConflict, id)
	}
	if r.pr.Draft {
		return nil, fmt.Errorf("%w: the merge request is a draft; mark it ready first", ErrConflict)
	}
	m, _ := s.meta(ctx, r)
	if m.Author == user {
		return nil, fmt.Errorf("%w: %s cannot approve an own change", ErrForbidden, user)
	}
	rv, err := s.o.Forge.Approve(ctx, user, r.pr.Number, comment)
	if err != nil {
		return nil, err
	}
	s.refreshPR(ctx, r.pr.Number)
	a := &policy.Approval{User: user, SHA: rv.SHA, Counts: true}
	if g, err := s.Gates(ctx, id); err == nil && g != nil {
		a.Counts = contains(g.Approvals.By, user)
		for _, ig := range g.Approvals.Ignored {
			if ig.User == user && !a.Counts {
				a.Reason = ig.Reason
			}
		}
	}
	s.notify(ctx, events.ChangeUpdated, id, "review")
	s.o.Events.Publish(events.ForgeUpdated, forgeEvent("review", id, &MR{Number: r.pr.Number, URL: r.pr.URL, State: "open", Title: r.pr.Title}, r.pr.HeadSHA))
	return a, nil
}

// ---------------------------------------------------------------- merge

func (s *Svc) Merge(ctx context.Context, id string) (*MergeResult, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	who := s.actor(ctx)
	if who.Kind == identity.Agent {
		return nil, ErrAgentDeny
	}
	if who.Kind == identity.Bot {
		return nil, fmt.Errorf("%w: bots never approve or merge", ErrForbidden)
	}
	if s.o.Forge == nil {
		return nil, fmt.Errorf("%w: no forge", forge.ErrUnreachable)
	}
	s.mu.Lock()
	if err := s.fetchLocked(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	_ = s.updateMainLocked(ctx)
	s.mu.Unlock()
	if err := s.pollForge(ctx); err != nil {
		return nil, err
	}
	r, err := s.record(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.merge != nil {
		return nil, fmt.Errorf("%w: %s is merged already", ErrConflict, id)
	}
	if r.pr == nil || r.pr.State != forge.PROpen {
		return nil, fmt.Errorf("%w: %s has no open merge request; push and mark it ready first", ErrConflict, id)
	}
	if r.pr.Draft {
		return nil, fmt.Errorf("%w: the merge request is a draft; mark it ready first", ErrConflict)
	}
	if r.wt != "" && r.localSHA != r.remoteSHA {
		if ok, _ := s.repo.IsAncestor(ctx, r.localSHA, r.remoteSHA); !ok {
			return nil, fmt.Errorf("%w: the worktree has commits that are not pushed; push first", ErrConflict)
		}
	}
	d, err := s.derive(ctx, &record{id: r.id, branch: r.branch, remoteSHA: r.remoteSHA, pr: r.pr}, who.Principal(), false)
	if err != nil {
		return nil, err
	}
	ch := d.ch
	if ch.BehindTarget > 0 {
		return nil, &BehindError{Target: ch.Target, N: ch.BehindTarget}
	}
	if ch.Gates == nil && s.o.Policy != nil {
		return nil, fmt.Errorf("%w: the gates could not be evaluated", ErrGates)
	}
	if g := ch.Gates; g != nil && !g.Mergeable {
		return nil, fmt.Errorf("%w: %s", ErrGates, strings.Join(g.Blockers, "; "))
	}
	if err := s.waitPolicyStatus(ctx, r.remoteSHA); err != nil {
		return nil, err
	}
	m := ch.Meta
	if m.Title == "" {
		m.Title = ch.Title
	}
	if m.Author == "" {
		m.Author = ch.Author
	}
	if m.Agent == "" {
		m.Agent = ch.Agent
	}
	info := MergeInfo{MR: r.pr.Number, LabRun: d.labRun}
	if ch.Gates != nil {
		info.ApprovedBy = ch.Gates.Approvals.By
	} else {
		// No policy engine: every approval on the head counts.
		for _, a := range ch.Approvals {
			if a.Counts {
				info.ApprovedBy = append(info.ApprovedBy, a.User)
			}
		}
	}
	title, msg := MergeMessage(m, info)
	res, err := s.o.Forge.Merge(ctx, s.forgeUser(who), r.pr.Number, forge.MergeOptions{
		Title: title, Message: msg, HeadSHA: r.remoteSHA, DeleteBranch: true,
	})
	if err != nil {
		return nil, err
	}
	out := &MergeResult{SHA: res.SHA, Message: title + "\n\n" + msg}
	s.mu.Lock()
	_ = s.fetchLocked(ctx)
	_ = s.updateMainLocked(ctx)
	if r.wt != "" {
		if st, _ := gitx.Open(r.wt).Status(ctx); len(st) == 0 {
			if err := s.repo.WorktreeRemove(ctx, r.wt, true); err == nil {
				_, _ = s.repo.Run(ctx, "branch", "-D", r.branch)
			}
		}
	}
	s.mu.Unlock()
	s.refreshPR(ctx, r.pr.Number)
	s.retargetChildren(ctx, who, r.branch)
	if c, err := s.Get(ctx, id); err == nil {
		out.Change = c.Summary
		out.MR = c.MR
	}
	s.o.Events.Publish(events.ChangeUpdated, changeEvent(out.Change, "merge"))
	s.o.Events.Publish(events.ForgeUpdated, forgeEvent("merge", id, &MR{Number: r.pr.Number, URL: r.pr.URL, State: "merged", Title: r.pr.Title}, res.SHA))
	go func() {
		// Other changes are now behind: tell the rail.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.publishChanges(rctx)
	}()
	return out, nil
}

// waitPolicyStatus waits for pk ci to post pk/policy success on sha.
func (s *Svc) waitPolicyStatus(ctx context.Context, sha string) error {
	deadline := time.Now().Add(s.o.CIWait)
	for {
		sts, err := s.o.Forge.Statuses(ctx, sha)
		if err != nil {
			return err
		}
		for _, st := range sts {
			if st.Context != forge.ContextPolicy {
				continue
			}
			switch st.State {
			case forge.StatusSuccess:
				return nil
			case forge.StatusFailure, forge.StatusError:
				if time.Now().After(deadline) {
					return fmt.Errorf("%w: CI check pk/policy failed: %s", ErrGates, st.Description)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: CI check pk/policy has not passed on %s yet (is pk ci running?)", ErrGates, short(sha))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// retargetChildren moves stacked changes of a merged branch to main.
func (s *Svc) retargetChildren(ctx context.Context, who identity.Identity, branch string) {
	recs, err := s.records(ctx)
	if err != nil {
		return
	}
	main := "main"
	for _, r := range recs {
		if r.merge != nil {
			continue
		}
		m, fromFile := s.meta(ctx, r)
		if m.Target != branch {
			continue
		}
		if r.pr != nil && r.pr.State == forge.PROpen && s.o.Forge != nil {
			if _, err := s.o.Forge.UpdatePR(ctx, s.forgeUser(who), r.pr.Number, forge.PRUpdate{Base: &main}); err != nil {
				s.log.Warn("retarget", "change", r.id, "err", err)
			}
			if !fromFile {
				m.Target = "main"
				body := Description(m)
				_, _ = s.o.Forge.UpdatePR(ctx, s.forgeUser(who), r.pr.Number, forge.PRUpdate{Body: &body})
			}
		}
		if fromFile && r.wt != "" {
			m.Target = "main"
			s.mu.Lock()
			if err := writeFile(r.wt, MetaPath(r.id), MarshalMeta(m)); err == nil {
				wt := gitx.Open(r.wt)
				_ = wt.Add(ctx, MetaPath(r.id))
				_, _ = wt.Commit(ctx, gitx.CommitOptions{Message: "chore(change): retarget to main", Author: signature(who),
					Trailers: agentTrailers(who), Paths: []string{MetaPath(r.id)}})
			}
			s.mu.Unlock()
		}
		s.notify(ctx, events.ChangeUpdated, r.id, "retarget")
	}
}

// ---------------------------------------------------------------- lab runs

func (s *Svc) RecordLabRun(ctx context.Context, id string, run LabRunRef, report []byte) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	who := s.actor(ctx)
	dir, err := s.Worktree(ctx, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.record(ctx, id)
	if err != nil {
		return err
	}
	wt := gitx.Open(dir)
	var paths []string
	if len(report) > 0 && run.Report != "" {
		if err := writeFile(dir, run.Report, report); err != nil {
			return err
		}
		paths = append(paths, run.Report)
	}
	m, fromFile := s.meta(ctx, r)
	m.LabRuns = replaceRun(m.LabRuns, run)
	if fromFile {
		if err := writeFile(dir, MetaPath(id), MarshalMeta(m)); err != nil {
			return err
		}
		paths = append(paths, MetaPath(id))
	}
	if len(paths) > 0 {
		if _, err := wt.Run(ctx, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
			return err
		}
		if _, err := wt.Commit(ctx, gitx.CommitOptions{Message: "docs(lab): add report for " + run.Run,
			Author: signature(who), Trailers: agentTrailers(who), Paths: paths}); err != nil {
			return err
		}
	}
	user := s.forgeUser(who)
	if s.o.Forge != nil && run.SHA != "" && r.remoteSHA != "" && s.isAncestor(ctx, run.SHA, r.remoteSHA) {
		st := forge.StatusSuccess
		if run.Result != LabPass {
			st = forge.StatusFailure
		}
		_ = s.o.Forge.SetStatus(ctx, user, run.SHA, forge.CommitStatus{Context: forge.ContextLab, State: st,
			Description: run.Run + " " + string(run.Result)})
	}
	if r.pr != nil && r.pr.State == forge.PROpen && s.o.Forge != nil {
		if _, err := s.pushLocked(ctx, who, id, false, ""); err != nil {
			s.log.Warn("push lab report", "change", id, "err", err)
		}
		if !fromFile {
			body := Description(m)
			if _, err := s.o.Forge.UpdatePR(ctx, user, r.pr.Number, forge.PRUpdate{Body: &body}); err != nil {
				s.log.Warn("lab run in description", "change", id, "err", err)
			}
			s.refreshPR(ctx, r.pr.Number)
		}
	}
	go s.notify(context.WithoutCancel(ctx), events.ChangeUpdated, id, "lab")
	return nil
}

func replaceRun(runs []LabRunRef, r LabRunRef) []LabRunRef {
	for i := range runs {
		if runs[i].Run == r.Run {
			runs[i] = r
			return runs
		}
	}
	return append(runs, r)
}
