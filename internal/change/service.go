package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/fluxsim"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// Options of the change service.
type Options struct {
	// DataDir holds repo/ (bare clone), main/ (worktree of origin/main),
	// worktrees/<id> and cache/.
	DataDir string
	// RemoteURL is the clone URL of the state repo on the forge.
	RemoteURL string
	Forge     forge.Forge
	// Token returns the forge token of a user (git push and fetch).
	Token func(user string) (string, bool)
	// User is the person the bench runs for (PK_USER).
	User string
	// Policy, Renderer: optional; without them gates are not evaluated.
	Policy   policy.Engine
	Renderer render.Renderer
	// Labs and Flux are late-bound (wired after the change service).
	Labs func() lab.Service
	Flux func() fluxsim.Client
	// Events receives change.* and forge.updated events.
	Events events.Publisher
	// CIWait is how long a merge waits for the pk/policy status (default 20 s).
	CIWait time.Duration
	// OnForgeReset runs after a forge reset dropped the local state of
	// changes opened before since (the bench drops old lab runs).
	OnForgeReset func(ctx context.Context, since time.Time)
	Log          *slog.Logger
	Now          func() time.Time
}

// Svc implements Service with git worktrees and a forge.
type Svc struct {
	o    Options
	repo *gitx.Repo
	log  *slog.Logger

	// mu serializes git writes (fetch, worktrees, commits, pushes).
	mu sync.Mutex
	// initMu guards init.
	initMu sync.Mutex
	inited bool
	// resets counts forge.reset events (guarded by mu).
	resets int

	fmu sync.RWMutex
	fs  forgeState
	// fetchErr is the error of the last fetch (nil when it worked).
	fetchErr error
	fsSaved  []byte // last saved forge cache
	seenMu   sync.Mutex
	seen     map[string]fp // fingerprints for events
	mainH    string

	cmu        sync.Mutex
	gatesCache map[string]gatesEntry
	// objects are the changed objects of each open change (overlap).
	objects   map[string]objEntry
	rollCache map[string][]string   // merge sha -> impacted clusters
	recCache  map[string]bool       // change@sha -> commit only touches records
	revCache  map[string]*mergeInfo // merged change -> its merge on main
	exportMu  sync.Mutex
}

var _ Service = (*Svc)(nil)

// New returns the service. Call Init (or Refresh) before use; the bench
// calls Run for the background poll.
func New(o Options) *Svc {
	if o.Log == nil {
		o.Log = slog.Default().With("component", "change")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Events == nil {
		o.Events = events.Nop{}
	}
	if o.CIWait == 0 {
		o.CIWait = 20 * time.Second
	}
	if o.Token == nil {
		o.Token = func(string) (string, bool) { return "", false }
	}
	return &Svc{
		o: o, repo: gitx.Open(filepath.Join(o.DataDir, "repo")), log: o.Log,
		seen: map[string]fp{}, gatesCache: map[string]gatesEntry{}, objects: map[string]objEntry{}, rollCache: map[string][]string{},
		recCache: map[string]bool{}, revCache: map[string]*mergeInfo{},
	}
}

// Paths.
func (s *Svc) repoDir() string              { return filepath.Join(s.o.DataDir, "repo") }
func (s *Svc) mainDir() string              { return filepath.Join(s.o.DataDir, "main") }
func (s *Svc) worktreeDir(id string) string { return filepath.Join(s.o.DataDir, "worktrees", id) }
func (s *Svc) treeCache(sha string) string  { return filepath.Join(s.o.DataDir, "cache", "trees", sha) }

const (
	remotePrefix = "refs/remotes/origin/"
	mainRef      = remotePrefix + "main"
)

func remoteRef(branch string) string { return remotePrefix + branch }
func localRef(branch string) string  { return "refs/heads/" + branch }

// fetchUser is the user whose token reads the forge.
func (s *Svc) fetchUser() string {
	for _, u := range []string{s.o.User, forge.CIUser} {
		if _, ok := s.o.Token(u); ok {
			return u
		}
	}
	return s.o.User
}

// Init clones the state repo (bare) and checks out main. It is idempotent.
func (s *Svc) Init(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.inited {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Join(s.repoDir(), "HEAD")); err != nil {
		if _, err := gitx.Init(ctx, s.repoDir(), true); err != nil {
			return err
		}
	}
	for k, v := range map[string]string{
		"remote.origin.url":     s.o.RemoteURL,
		"remote.origin.fetch":   "+refs/heads/*:refs/remotes/origin/*",
		"core.logAllRefUpdates": "true",
		"gc.auto":               "0",
		"user.name":             "platform-kit",
		"user.email":            "bench@pk.local",
		"rebase.autoStash":      "false",
		"merge.conflictStyle":   "merge",
	} {
		if err := s.repo.Config(ctx, k, v); err != nil {
			return err
		}
	}
	if err := s.fetchLocked(ctx); err != nil {
		// A restart while the forge is down: work from the local clone and
		// the last forge poll (edit, commit and labs work; push and merge
		// wait for the forge). The refresh loop fetches when it is back.
		if !s.repo.Exists(ctx, mainRef) {
			return err
		}
		s.log.Warn("forge unreachable at start: using the local clone", "err", err)
		s.loadForgeCache()
		s.fmu.Lock()
		s.fetchErr = err
		s.fmu.Unlock()
	}
	if !s.repo.Exists(ctx, mainRef) {
		return fmt.Errorf("state repo %s has no main branch yet (run pk forge-init)", s.o.RemoteURL)
	}
	_, _ = s.repo.Run(ctx, "worktree", "prune")
	if err := s.updateMainLocked(ctx); err != nil {
		return err
	}
	s.inited = true
	return nil
}

func (s *Svc) ensure(ctx context.Context) error {
	s.initMu.Lock()
	ok := s.inited
	s.initMu.Unlock()
	if ok {
		return nil
	}
	return s.Init(ctx)
}

func (s *Svc) fetchLocked(ctx context.Context) error {
	user := s.fetchUser()
	tok, _ := s.o.Token(user)
	old, _ := s.repo.RevParse(ctx, mainRef)
	if err := s.repo.FetchAuth(ctx, user, tok, "origin"); err != nil {
		return fmt.Errorf("%w: fetch: %v", forge.ErrUnreachable, err)
	}
	if cur, err := s.repo.RevParse(ctx, mainRef); err == nil && old != "" && cur != old {
		if ok, err := s.repo.IsAncestor(ctx, old, cur); err == nil && !ok {
			since := time.Now().Add(time.Second)
			s.forgeResetLocked(ctx, since, old, cur)
			s.publishReset(since, cur)
		}
	}
	return nil
}

// ForgeReset drops the local worktrees and change branches: the forge state
// was rebuilt (pk forge-init --reset), so they belong to an old state.
// Changes opened at or after "since" (the rebuild time) are kept.
func (s *Svc) ForgeReset(ctx context.Context, since time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.repoDir()); err != nil {
		return
	}
	s.forgeResetLocked(ctx, since, "", "")
	n := s.resets
	_ = s.fetchLocked(ctx)
	_ = s.updateMainLocked(ctx)
	if s.resets == n { // the fetch did not announce it already
		cur, _ := s.repo.RevParse(ctx, mainRef)
		s.publishReset(since, cur)
	}
}

// publishReset tells clients (forge.reset) that the local state was rebuilt.
func (s *Svc) publishReset(since time.Time, main string) {
	s.resets++
	s.o.Events.Publish(events.ForgeReset, ForgeResetEvent{Since: since.UTC(), Main: main})
}

// forgeResetLocked drops the local worktrees and change branches after the
// history of main was rewritten (pk forge-init --reset): they belong to the
// old forge state. Main never moves this way in normal use (protected).
func (s *Svc) forgeResetLocked(ctx context.Context, since time.Time, old, cur string) {
	s.log.Warn("forge state rebuilt: dropping local worktrees", "oldMain", short(old), "newMain", short(cur))
	wts, err := s.repo.WorktreeList(ctx)
	if err != nil {
		return
	}
	root := filepath.Join(s.o.DataDir, "worktrees") + string(filepath.Separator)
	for _, w := range wts {
		id, _, ok := ParseBranch(w.Branch)
		if !ok || !strings.HasPrefix(w.Path, root) {
			continue
		}
		if opened := s.openedAt(ctx, id, w.Branch); !opened.IsZero() && !opened.Before(since.Truncate(time.Second)) {
			continue
		}
		if err := s.repo.WorktreeRemove(ctx, w.Path, true); err != nil {
			s.log.Warn("forge reset: remove worktree", "change", id, "err", err)
			continue
		}
		s.forgetObjects(id)
	}
	_, _ = s.repo.Run(ctx, "worktree", "prune")
	kept := map[string]bool{}
	if wts, err := s.repo.WorktreeList(ctx); err == nil {
		for _, w := range wts {
			kept[w.Branch] = true
		}
	}
	if out, err := s.repo.Run(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads/change/"); err == nil {
		for _, b := range strings.Fields(out) {
			if !kept[b] {
				_, _ = s.repo.Run(ctx, "branch", "-D", b)
			}
		}
	}
	if s.o.OnForgeReset != nil {
		s.o.OnForgeReset(ctx, since)
	}
}

// openedAt is the commit time of the "open" commit of a change branch.
func (s *Svc) openedAt(ctx context.Context, id, branch string) time.Time {
	out, err := s.repo.Run(ctx, "log", "-1", "--format=%ct", "--fixed-strings", "--grep", "chore(change): open "+id, "refs/heads/"+branch)
	if err != nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// updateMainLocked moves the main worktree to origin/main.
func (s *Svc) updateMainLocked(ctx context.Context) error {
	sha, err := s.repo.RevParse(ctx, mainRef)
	if err != nil {
		return err
	}
	dir := s.mainDir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		_ = os.RemoveAll(dir)
		_, _ = s.repo.Run(ctx, "worktree", "prune")
		if err := s.repo.WorktreeAdd(ctx, dir, "", sha, false); err != nil {
			return err
		}
	} else {
		m := gitx.Open(dir)
		if head, _ := m.Head(ctx); head != sha {
			if _, err := m.Run(ctx, "checkout", "-q", "-f", "--detach", sha); err != nil {
				return err
			}
			_, _ = m.Run(ctx, "clean", "-q", "-fdx")
		}
	}
	s.mainH = sha
	return nil
}

// ---------------------------------------------------------------- identity

// actor returns the acting identity with name and email.
func (s *Svc) actor(ctx context.Context) identity.Identity {
	id, ok := identity.From(ctx)
	if !ok {
		id = identity.Identity{User: s.o.User, Kind: identity.Human}
	}
	return s.complete(ctx, id)
}

func (s *Svc) complete(ctx context.Context, id identity.Identity) identity.Identity {
	if id.Name != "" && id.Email != "" && id.Name != id.User {
		return id
	}
	if p := s.policy(ctx); p != nil && s.o.Policy != nil {
		if known, ok := s.o.Policy.Identify(p, id.User); ok {
			if id.Name == "" || id.Name == id.User {
				id.Name = known.Name
			}
			if id.Email == "" {
				id.Email = known.Email
			}
			if id.ActsFor == "" {
				id.ActsFor = known.ActsFor
			}
		}
	}
	return id
}

// identify returns the identity of a user id (person, agent or bot).
func (s *Svc) identify(ctx context.Context, user string) identity.Identity {
	if user == "" {
		return identity.Identity{}
	}
	if p := s.policy(ctx); p != nil && s.o.Policy != nil {
		if id, ok := s.o.Policy.Identify(p, user); ok {
			return id
		}
	}
	id := identity.Identity{User: user, Kind: identity.Human}
	switch {
	case strings.HasSuffix(user, "-agent"):
		id.Kind, id.ActsFor = identity.Agent, strings.TrimSuffix(user, "-agent")
	case strings.HasSuffix(user, "-bot"), user == forge.CIUser:
		id.Kind = identity.Bot
	}
	return id
}

func (s *Svc) policy(ctx context.Context) *policy.Policy {
	if s.o.Policy == nil {
		return nil
	}
	p, err := s.o.Policy.Current(ctx)
	if err != nil {
		return nil
	}
	return p
}

// signature is the git author of an identity.
func signature(id identity.Identity) gitx.Signature {
	name, email := id.CommitAuthor()
	return gitx.Signature{Name: name, Email: email}
}

// agentTrailers are the trailers of commits an agent makes.
func agentTrailers(id identity.Identity) []gitx.Trailer {
	if id.Kind != identity.Agent {
		return nil
	}
	ts := []gitx.Trailer{{Key: TrailerAgent, Value: id.User}}
	if id.ActsFor != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerOnBehalfOf, Value: id.ActsFor})
	}
	return ts
}

// userOfEmail maps a commit email (dennis@pk.local) to a user id.
func userOfEmail(email string) string {
	if u, ok := strings.CutSuffix(email, "@pk.local"); ok {
		return u
	}
	return email
}

// tokenFor returns the forge token of an identity.
func (s *Svc) tokenFor(id identity.Identity) (string, error) {
	if tok, ok := s.o.Token(id.User); ok {
		return tok, nil
	}
	return "", fmt.Errorf("%w: no forge token for %s", ErrForbidden, id.User)
}

// may checks a right of an agent or bot (people have all rights here; path
// rights are checked on commit).
func (s *Svc) may(ctx context.Context, id identity.Identity, right policy.Right) error {
	if id.Kind == identity.Human {
		return nil
	}
	p := s.policy(ctx)
	if p == nil {
		return nil
	}
	for _, a := range p.Agents {
		if a.ID == id.User {
			for _, r := range a.Can {
				if r == right {
					return nil
				}
			}
			return fmt.Errorf("%w: %s may not %s", ErrForbidden, id.User, right)
		}
	}
	for _, b := range p.Bots {
		if b.ID == id.User {
			for _, r := range b.Can {
				if r == right {
					return nil
				}
			}
			return fmt.Errorf("%w: %s may not %s", ErrForbidden, id.User, right)
		}
	}
	return fmt.Errorf("%w: %s is not in the policy", ErrForbidden, id.User)
}

// ---------------------------------------------------------------- trees

// export writes the tree of a sha into the tree cache once and returns the dir.
func (s *Svc) export(ctx context.Context, sha string) (string, error) {
	dir := s.treeCache(sha)
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, ".pk-export")); err == nil {
		return dir, nil
	}
	_ = os.RemoveAll(dir)
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := s.repo.Export(ctx, sha, tmp); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".pk-export"), []byte(sha+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	s.pruneExports(sha)
	return dir, nil
}

// pruneExports keeps the newest exports.
func (s *Svc) pruneExports(keep string) {
	root := filepath.Join(s.o.DataDir, "cache", "trees")
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) <= 64 {
		return
	}
	type e struct {
		name string
		at   time.Time
	}
	var all []e
	for _, d := range ents {
		if info, err := d.Info(); err == nil {
			all = append(all, e{d.Name(), info.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, x := range all[48:] {
		if x.name != keep {
			_ = os.RemoveAll(filepath.Join(root, x.name))
		}
	}
}

// dirtyKey is a content key of the saved, uncommitted files of a worktree.
func dirtyKey(dir string, st []gitx.FileStatus) string {
	if len(st) == 0 {
		return ""
	}
	h := sha256.New()
	for _, f := range st {
		fmt.Fprintf(h, "%s %s\n", f.Code, f.Path)
		if b, err := os.ReadFile(filepath.Join(dir, f.Path)); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// liveSource is the render source of a worktree with its saved files.
func (s *Svc) liveSource(ctx context.Context, ref, dir string) (render.Source, error) {
	wt := gitx.Open(dir)
	head, err := wt.Head(ctx)
	if err != nil {
		return render.Source{}, err
	}
	tree, err := wt.TreeHash(ctx, head)
	if err != nil {
		return render.Source{}, err
	}
	st, err := wt.Status(ctx)
	if err != nil {
		return render.Source{}, err
	}
	key := tree
	if dk := dirtyKey(dir, st); dk != "" {
		key += "+" + dk
	}
	return render.Source{Ref: ref, SHA: head, Dir: dir, Key: key, Dirty: len(st) > 0}, nil
}

// shaSource is the render source of a commit (main dir for the main head).
func (s *Svc) shaSource(ctx context.Context, ref, sha string) (render.Source, error) {
	tree, err := s.repo.TreeHash(ctx, sha)
	if err != nil {
		return render.Source{}, err
	}
	if sha == s.mainH {
		return render.Source{Ref: ref, SHA: sha, Dir: s.mainDir(), Key: tree}, nil
	}
	dir, err := s.export(ctx, sha)
	if err != nil {
		return render.Source{}, err
	}
	return render.Source{Ref: ref, SHA: sha, Dir: dir, Key: tree}, nil
}

func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveRev resolves main, a change id, a branch or a sha to a sha.
func (s *Svc) resolveRev(ctx context.Context, ref string) (string, error) {
	if ref == "" || ref == "main" || ref == "origin/main" {
		return s.repo.RevParse(ctx, mainRef)
	}
	cands := []string{remoteRef(ref), localRef(ref), ref}
	if isFullSHA(ref) {
		cands = []string{ref} // a sha (cached by gitx), not a branch name
	}
	for _, r := range cands {
		if sha, err := s.repo.RevParse(ctx, r); err == nil {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%w: unknown ref %s", ErrNotFound, ref)
}

func (s *Svc) MainDir(ctx context.Context) (string, error) {
	if err := s.ensure(ctx); err != nil {
		return "", err
	}
	return s.mainDir(), nil
}

func (s *Svc) Worktree(ctx context.Context, id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	dir := s.worktreeDir(id)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoWorktree, id)
	}
	return dir, nil
}

func (s *Svc) Resolve(ctx context.Context, ref string) (string, bool, error) {
	if err := s.ensure(ctx); err != nil {
		return "", false, err
	}
	if ref == "" || ref == "main" {
		return s.mainDir(), false, nil
	}
	if ValidID(ref) {
		if dir, err := s.Worktree(ctx, ref); err == nil {
			return dir, true, nil
		}
		rec, err := s.record(ctx, ref)
		if err != nil {
			return "", false, err
		}
		if rec.remoteSHA == "" {
			return "", false, fmt.Errorf("%w: %s has no branch", ErrNotFound, ref)
		}
		dir, err := s.export(ctx, rec.remoteSHA)
		return dir, false, err
	}
	sha, err := s.resolveRev(ctx, ref)
	if err != nil {
		return "", false, err
	}
	if sha == s.mainH {
		return s.mainDir(), false, nil
	}
	dir, err := s.export(ctx, sha)
	return dir, false, err
}

func (s *Svc) Source(ctx context.Context, ref string) (render.Source, error) {
	if err := s.ensure(ctx); err != nil {
		return render.Source{}, err
	}
	if ValidID(ref) {
		if dir, err := s.Worktree(ctx, ref); err == nil {
			return s.liveSource(ctx, ref, dir)
		}
		rec, err := s.record(ctx, ref)
		if err != nil {
			return render.Source{}, err
		}
		if rec.remoteSHA == "" {
			return render.Source{}, fmt.Errorf("%w: %s has no branch", ErrNotFound, ref)
		}
		return s.shaSource(ctx, ref, rec.remoteSHA)
	}
	sha, err := s.resolveRev(ctx, ref)
	if err != nil {
		return render.Source{}, err
	}
	if ref == "" {
		ref = "main"
	}
	return s.shaSource(ctx, ref, sha)
}

func (s *Svc) Sources(ctx context.Context, id string) (base, head render.Source, err error) {
	if err = s.ensure(ctx); err != nil {
		return
	}
	rec, err := s.record(ctx, id)
	if err != nil {
		return
	}
	// A merged change: main before the merge against the merge commit.
	if m := rec.merge; m != nil && len(m.Parents) == 2 {
		if base, err = s.shaSource(ctx, "main", m.Parents[0]); err != nil {
			return
		}
		head, err = s.shaSource(ctx, id, m.SHA)
		return
	}
	headRev := rec.headRev()
	if headRev == "" {
		err = fmt.Errorf("%w: %s has no branch", ErrNotFound, id)
		return
	}
	if rec.wt != "" {
		head, err = s.liveSource(ctx, id, rec.wt)
	} else {
		head, err = s.shaSource(ctx, id, rec.headSHA())
	}
	if err != nil {
		return
	}
	mb, err := s.repo.MergeBase(ctx, s.targetRef(ctx, rec), headRev)
	if err != nil {
		return
	}
	base, err = s.shaSource(ctx, "main", mb)
	return
}

// targetRef is the ref of the change target (falls back to main when a
// stacked parent is gone).
func (s *Svc) targetRef(ctx context.Context, rec *record) string {
	t := rec.target
	if t == "" || t == "main" {
		return mainRef
	}
	if s.repo.Exists(ctx, remoteRef(t)) {
		return remoteRef(t)
	}
	if s.repo.Exists(ctx, localRef(t)) {
		return localRef(t)
	}
	return mainRef
}

// readWorktreeFile reads a file of a worktree.
func readWorktreeFile(dir, p string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return b, err
}
