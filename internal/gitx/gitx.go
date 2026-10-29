// Package gitx wraps the git CLI. Worktrees, rebase and push need the real
// git, so the bench execs it instead of using a Go git library.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is a git repository or worktree directory.
type Repo struct {
	// Dir is the working directory for git commands.
	Dir string
	// Env is added to the environment of each command (for example GIT_AUTHOR_DATE).
	Env []string
}

// Open returns a Repo for dir. It does not check that dir is a repository.
func Open(dir string) *Repo { return &Repo{Dir: dir} }

// Error is a failed git command.
type Error struct {
	Args   []string
	Stderr string
	Code   int
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// IsNotFound reports whether err is a git error about a missing ref or path.
func IsNotFound(err error) bool {
	var ge *Error
	if !errors.As(err, &ge) {
		return false
	}
	s := ge.Stderr
	return strings.Contains(s, "does not exist") || strings.Contains(s, "not a valid") ||
		strings.Contains(s, "unknown revision") || strings.Contains(s, "bad revision") ||
		strings.Contains(s, "Not a valid object name") || strings.Contains(s, "exists on disk, but not in")
}

// baseEnv keeps git independent of the user's global config and prompts.
var baseEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_CONFIG_NOSYSTEM=1",
	"LC_ALL=C",
	// Read-only commands (git status refreshes the index) never take
	// index.lock, so they cannot make a concurrent commit fail.
	"GIT_OPTIONAL_LOCKS=0",
}

// checkRevs refuses revisions that git would parse as options (for example
// "--output=<file>" from a request path). The error reads like git's own
// "bad revision", so IsNotFound treats it as a missing commit.
func checkRevs(revs ...string) error {
	for _, r := range revs {
		if strings.HasPrefix(r, "-") {
			return &Error{Args: []string{"rev"}, Stderr: fmt.Sprintf("bad revision '%s'", r), Code: 128}
		}
	}
	return nil
}

// Run runs git with args and returns stdout.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	out, err := r.RunBytes(ctx, nil, args...)
	return string(out), err
}

// RunInput runs git with stdin.
func (r *Repo) RunInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	out, err := r.RunBytes(ctx, stdin, args...)
	return string(out), err
}

// RunBytes runs git and returns raw stdout. Read-only commands whose revs
// are all full shas answer from a cache: their output never changes.
func (r *Repo) RunBytes(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	if stdin == nil && len(r.Env) == 0 && immutableArgs(args) {
		key := r.Dir + "\x00" + strings.Join(args, "\x00")
		if out, ok := immCache.get(key); ok {
			return out, nil
		}
		if err := immErrs.get(key); err != nil {
			return nil, err
		}
		out, err := r.run(ctx, nil, args...)
		if err == nil {
			immCache.put(key, out)
		} else if missingPath(args, err) {
			immErrs.put(key, err)
		}
		return out, err
	}
	return r.run(ctx, stdin, args...)
}

func (r *Repo) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(append(os.Environ(), baseEnv...), r.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return stdout.Bytes(), &Error{Args: redactArgs(args), Stderr: redact(stderr.String()), Code: code}
	}
	return stdout.Bytes(), nil
}

// Line runs git and returns stdout without the trailing newline.
func (r *Repo) Line(ctx context.Context, args ...string) (string, error) {
	out, err := r.Run(ctx, args...)
	return strings.TrimRight(out, "\n"), err
}

// With returns a copy of r with extra environment variables.
func (r *Repo) With(env ...string) *Repo {
	return &Repo{Dir: r.Dir, Env: append(append([]string{}, r.Env...), env...)}
}

// WithDates returns a copy of r that sets author and committer dates.
func (r *Repo) WithDates(t time.Time) *Repo {
	d := t.Format(time.RFC3339)
	return r.With("GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
}

// Init creates a repository with main as the initial branch.
func Init(ctx context.Context, dir string, bare bool) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := Open(dir)
	args := []string{"init", "-q", "--initial-branch=main"}
	if bare {
		args = append(args, "--bare")
	}
	if _, err := r.Run(ctx, args...); err != nil {
		return nil, err
	}
	return r, nil
}

// Clone clones url into dir.
func Clone(ctx context.Context, url, dir string, extra ...string) (*Repo, error) {
	r := Open(".")
	args := append([]string{"clone", "-q"}, extra...)
	args = append(args, url, dir)
	if _, err := r.Run(ctx, args...); err != nil {
		return nil, err
	}
	return Open(dir), nil
}

// Config sets a local config value.
func (r *Repo) Config(ctx context.Context, key, value string) error {
	_, err := r.Run(ctx, "config", key, value)
	return err
}

// RevParse resolves rev to a full sha.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	if err := checkRevs(rev); err != nil {
		return "", err
	}
	return r.Line(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// Exists reports whether rev resolves to a commit.
func (r *Repo) Exists(ctx context.Context, rev string) bool {
	_, err := r.RevParse(ctx, rev)
	return err == nil
}

// Head returns the sha of HEAD.
func (r *Repo) Head(ctx context.Context) (string, error) { return r.RevParse(ctx, "HEAD") }

// CurrentBranch returns the checked-out branch name ("" when detached).
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.Line(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.Code == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// Fetch fetches from remote with prune.
func (r *Repo) Fetch(ctx context.Context, remote string, refspecs ...string) error {
	args := append([]string{"fetch", "-q", "--prune", remote}, refspecs...)
	_, err := r.Run(ctx, args...)
	return err
}

// MergeBase returns the merge base of a and b.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	if err := checkRevs(a, b); err != nil {
		return "", err
	}
	return r.Line(ctx, "merge-base", a, b)
}

// IsAncestor reports whether a is an ancestor of b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if err := checkRevs(a, b); err != nil {
		return false, err
	}
	_, err := r.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ge *Error
	if errors.As(err, &ge) && ge.Code == 1 {
		return false, nil
	}
	return false, err
}

// AheadBehind returns how many commits head has that base lacks (ahead) and
// the other way round (behind).
func (r *Repo) AheadBehind(ctx context.Context, base, head string) (ahead, behind int, err error) {
	if err := checkRevs(base, head); err != nil {
		return 0, 0, err
	}
	out, err := r.Line(ctx, "rev-list", "--left-right", "--count", base+"..."+head)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("rev-list: unexpected output %q", out)
	}
	behind, _ = strconv.Atoi(f[0])
	ahead, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// Behind counts the first-parent commits of target that head lacks: the
// merges on main a branch must rebase over ("behind main by 1").
func (r *Repo) Behind(ctx context.Context, head, target string) (int, error) {
	if err := checkRevs(head, target); err != nil {
		return 0, err
	}
	out, err := r.Line(ctx, "rev-list", "--count", "--first-parent", head+".."+target)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// ShowFile returns the content of path at rev.
func (r *Repo) ShowFile(ctx context.Context, rev, path string) ([]byte, error) {
	if err := checkRevs(rev); err != nil {
		return nil, err
	}
	return r.RunBytes(ctx, nil, "show", rev+":"+path)
}

// TreeEntry is one file in a tree listing.
type TreeEntry struct {
	Mode string
	Type string // blob, tree, commit
	SHA  string
	Path string
}

// LsTree lists all files below dir ("" for all) at rev, recursively.
func (r *Repo) LsTree(ctx context.Context, rev, dir string) ([]TreeEntry, error) {
	if err := checkRevs(rev); err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "-r", "-z", "--full-tree", rev}
	if dir != "" {
		args = append(args, "--", dir)
	}
	out, err := r.RunBytes(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			continue
		}
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], SHA: f[2], Path: string(path)})
	}
	return entries, nil
}

// TreeHash returns the tree sha of rev (a cache key for renders).
func (r *Repo) TreeHash(ctx context.Context, rev string) (string, error) {
	if err := checkRevs(rev); err != nil {
		return "", err
	}
	return r.Line(ctx, "rev-parse", rev+"^{tree}")
}

// Export writes the tree of rev into dir (which must not exist or be empty).
func (r *Repo) Export(ctx context.Context, rev, dir string) error {
	if err := checkRevs(rev); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive, err := r.RunBytes(ctx, nil, "archive", "--format=tar", rev)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "tar", "-x", "-C", dir)
	cmd.Stdin = bytes.NewReader(archive)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar %s: %v: %s", rev, err, out)
	}
	return nil
}

// FileStatus is one entry of git status or a name-status diff.
type FileStatus struct {
	// Code is the status letter: A, M, D, R, ? (untracked).
	Code    string
	Path    string
	OldPath string
	// Staged is set for index changes in Status.
	Staged bool
}

// Status returns the uncommitted changes of a worktree (index and work tree).
func (r *Repo) Status(ctx context.Context) ([]FileStatus, error) {
	out, err := r.RunBytes(ctx, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var res []FileStatus
	recs := bytes.Split(out, []byte{0})
	for i := 0; i < len(recs); i++ {
		rec := string(recs[i])
		if len(rec) < 4 {
			continue
		}
		x, y, path := rec[0], rec[1], rec[3:]
		fs := FileStatus{Path: path}
		switch {
		case x == '?' && y == '?':
			fs.Code = "?"
		case x == 'R' || y == 'R':
			fs.Code = "R"
			if i+1 < len(recs) {
				fs.OldPath = string(recs[i+1])
				i++
			}
		case x == 'A' || y == 'A':
			fs.Code = "A"
		case x == 'D' || y == 'D':
			fs.Code = "D"
		default:
			fs.Code = "M"
		}
		fs.Staged = x != ' ' && x != '?'
		res = append(res, fs)
	}
	return res, nil
}

// DiffNames returns the name-status diff between two revs (or rev and the
// work tree when b is "").
func (r *Repo) DiffNames(ctx context.Context, a, b string) ([]FileStatus, error) {
	if err := checkRevs(a, b); err != nil {
		return nil, err
	}
	args := []string{"diff", "--name-status", "-z", "--find-renames", a}
	if b != "" {
		args = append(args, b)
	}
	out, err := r.RunBytes(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	recs := bytes.Split(out, []byte{0})
	var res []FileStatus
	for i := 0; i < len(recs); i++ {
		code := string(recs[i])
		if code == "" {
			continue
		}
		fs := FileStatus{Code: code[:1]}
		if fs.Code == "R" || fs.Code == "C" {
			if i+2 >= len(recs) {
				break
			}
			fs.OldPath, fs.Path = string(recs[i+1]), string(recs[i+2])
			i += 2
		} else {
			if i+1 >= len(recs) {
				break
			}
			fs.Path = string(recs[i+1])
			i++
		}
		res = append(res, fs)
	}
	return res, nil
}

// Diff returns a unified diff between two revs (b "" means the work tree),
// optionally limited to paths.
func (r *Repo) Diff(ctx context.Context, a, b string, paths ...string) (string, error) {
	if err := checkRevs(a, b); err != nil {
		return "", err
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", a}
	if b != "" {
		args = append(args, b)
	}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	return r.Run(ctx, args...)
}

// Add stages paths ("-A" stages everything).
func (r *Repo) Add(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		paths = []string{"-A"}
	}
	args := append([]string{"add", "--"}, paths...)
	if paths[0] == "-A" {
		args = []string{"add", "-A"}
	}
	_, err := r.Run(ctx, args...)
	return err
}

// Signature is a git author or committer.
type Signature struct {
	Name  string
	Email string
	When  time.Time // zero means now
}

// CommitOptions describe a commit.
type CommitOptions struct {
	Message   string
	Author    Signature
	Committer Signature // zero means the author
	// Trailers are appended to the message as "Key: value" lines.
	Trailers []Trailer
	// AllowEmpty allows a commit without changes.
	AllowEmpty bool
	// Paths limits the commit to these paths (they must be staged or tracked).
	Paths []string
}

// Commit creates a commit from the index and returns its sha.
func (r *Repo) Commit(ctx context.Context, o CommitOptions) (string, error) {
	msg := strings.TrimRight(o.Message, "\n")
	if len(o.Trailers) > 0 {
		msg += "\n\n" + FormatTrailers(o.Trailers)
	}
	c := o.Committer
	if c.Name == "" {
		c = o.Author
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + o.Author.Name, "GIT_AUTHOR_EMAIL=" + o.Author.Email,
		"GIT_COMMITTER_NAME=" + c.Name, "GIT_COMMITTER_EMAIL=" + c.Email,
	}
	if !o.Author.When.IsZero() {
		env = append(env, "GIT_AUTHOR_DATE="+o.Author.When.Format(time.RFC3339))
	}
	if !c.When.IsZero() {
		env = append(env, "GIT_COMMITTER_DATE="+c.When.Format(time.RFC3339))
	}
	args := []string{"commit", "-q", "--no-verify", "-F", "-"}
	if o.AllowEmpty {
		args = append(args, "--allow-empty")
	}
	if len(o.Paths) > 0 {
		args = append(append(args, "--"), o.Paths...)
	}
	if _, err := r.With(env...).RunInput(ctx, []byte(msg+"\n"), args...); err != nil {
		return "", err
	}
	return r.Head(ctx)
}

// Trailer is a git trailer line.
type Trailer struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// FormatTrailers renders trailers as "Key: value" lines.
func FormatTrailers(ts []Trailer) string {
	var b strings.Builder
	for i, t := range ts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(t.Key + ": " + t.Value)
	}
	return b.String()
}

// ParseTrailers returns the trailers of the last paragraph of msg. A
// paragraph counts as trailers when every non-empty line is "Key: value"
// with a key of letters, digits and dashes.
func ParseTrailers(msg string) []Trailer {
	paras := strings.Split(strings.TrimSpace(strings.ReplaceAll(msg, "\r\n", "\n")), "\n\n")
	if len(paras) < 2 {
		return nil
	}
	last := paras[len(paras)-1]
	var ts []Trailer
	for _, line := range strings.Split(last, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !isTrailerKey(k) {
			return nil
		}
		ts = append(ts, Trailer{Key: k, Value: strings.TrimSpace(v)})
	}
	return ts
}

func isTrailerKey(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if !(c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// LogEntry is one commit from Log.
type LogEntry struct {
	SHA         string
	Parents     []string
	AuthorName  string
	AuthorEmail string
	AuthorDate  time.Time
	Committer   string
	CommitDate  time.Time
	Subject     string
	Body        string
	Trailers    []Trailer
}

const logSep = "\x1e"

// Log returns commits for the rev range (for example "main..HEAD" or "main")
// with extra git log args (for example "--merges", "--first-parent", "-n", "50", "--", "path").
func (r *Repo) Log(ctx context.Context, revRange string, extra ...string) ([]LogEntry, error) {
	if err := checkRevs(revRange); err != nil {
		return nil, err
	}
	format := strings.Join([]string{"%H", "%P", "%an", "%ae", "%aI", "%cn", "%cI", "%s", "%b"}, "%x1f") + logSep
	args := []string{"log", "--format=" + format, revRange}
	args = append(args, extra...)
	out, err := r.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var res []LogEntry
	for _, rec := range strings.Split(out, logSep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 9)
		if len(f) != 9 {
			continue
		}
		e := LogEntry{
			SHA: f[0], Parents: strings.Fields(f[1]),
			AuthorName: f[2], AuthorEmail: f[3], Committer: f[5],
			Subject: f[7], Body: strings.TrimRight(f[8], "\n"),
		}
		e.AuthorDate, _ = time.Parse(time.RFC3339, f[4])
		e.CommitDate, _ = time.Parse(time.RFC3339, f[6])
		e.Trailers = ParseTrailers(e.Subject + "\n\n" + e.Body)
		res = append(res, e)
	}
	return res, nil
}

// ChangedFiles returns the files a commit changed against its first parent.
func (r *Repo) ChangedFiles(ctx context.Context, sha string) ([]FileStatus, error) {
	if err := checkRevs(sha); err != nil {
		return nil, err
	}
	parent := sha + "^1"
	if !r.Exists(ctx, parent) {
		parent = emptyTree
	}
	return r.DiffNames(ctx, parent, sha)
}

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Worktree is one entry of git worktree list.
type Worktree struct {
	Path   string
	Head   string
	Branch string // short name, "" when detached
	Bare   bool
}

// WorktreeAdd adds a worktree at path. With newBranch it creates the branch at base.
func (r *Repo) WorktreeAdd(ctx context.Context, path, branch, base string, newBranch bool) error {
	args := []string{"worktree", "add", "-q"}
	if newBranch {
		args = append(args, "-b", branch, path, base)
	} else if branch == "" {
		args = append(args, "--detach", path, base)
	} else {
		args = append(args, path, branch)
	}
	_, err := r.Run(ctx, args...)
	return err
}

// WorktreeRemove removes a worktree. Without force it fails on a dirty worktree.
func (r *Repo) WorktreeRemove(ctx context.Context, path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = []string{"worktree", "remove", "--force", path}
	}
	_, err := r.Run(ctx, args...)
	return err
}

// WorktreeList lists the worktrees of the repository.
func (r *Repo) WorktreeList(ctx context.Context) ([]Worktree, error) {
	out, err := r.Run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var res []Worktree
	var cur *Worktree
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			res = append(res, Worktree{Path: strings.TrimPrefix(line, "worktree ")})
			cur = &res[len(res)-1]
		case cur == nil:
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "bare":
			cur.Bare = true
		}
	}
	return res, nil
}

// ErrConflict is returned by Rebase when the rebase stopped on conflicts.
type ErrConflict struct {
	Onto  string
	Paths []string
}

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("rebase onto %s: conflict in %s", e.Onto, strings.Join(e.Paths, ", "))
}

// Rebase rebases the current branch onto onto. On conflict it aborts the
// rebase and returns *ErrConflict with the conflicting paths.
func (r *Repo) Rebase(ctx context.Context, onto string, committer Signature) error {
	env := []string{"GIT_COMMITTER_NAME=" + committer.Name, "GIT_COMMITTER_EMAIL=" + committer.Email}
	_, err := r.With(env...).Run(ctx, "rebase", "-q", onto)
	if err == nil {
		return nil
	}
	out, _ := r.Run(ctx, "diff", "--name-only", "--diff-filter=U")
	_, _ = r.Run(ctx, "rebase", "--abort")
	var paths []string
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return err
	}
	return &ErrConflict{Onto: onto, Paths: paths}
}

// PushOptions describe a push.
type PushOptions struct {
	Remote string // default origin
	// Refspecs, for example "change/c-k7m2qz-x:change/c-k7m2qz-x".
	Refspecs []string
	// ForceWithLease forces with lease on the given ref (expected remote sha; "" means any).
	ForceWithLease bool
	Lease          string
	// Token is sent as HTTP basic auth password via http.extraHeader.
	User  string
	Token string
	// Delete deletes the remote refs.
	Delete bool
}

// Push pushes refs. The token never appears in the remote URL or in errors.
func (r *Repo) Push(ctx context.Context, o PushOptions) error {
	remote := o.Remote
	if remote == "" {
		remote = "origin"
	}
	var args []string
	if o.Token != "" {
		args = append(args, "-c", "http.extraHeader="+AuthHeader(o.User, o.Token))
	}
	args = append(args, "push", "-q", "--porcelain")
	if o.Delete {
		args = append(args, "--delete")
	}
	if o.ForceWithLease {
		if o.Lease != "" && len(o.Refspecs) == 1 {
			dst := o.Refspecs[0]
			if _, d, ok := strings.Cut(dst, ":"); ok {
				dst = d
			}
			args = append(args, "--force-with-lease="+dst+":"+o.Lease)
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, remote)
	args = append(args, o.Refspecs...)
	_, err := r.Run(ctx, args...)
	return err
}

// FetchAuth fetches with a token header.
func (r *Repo) FetchAuth(ctx context.Context, user, token, remote string, refspecs ...string) error {
	args := []string{}
	if token != "" {
		args = append(args, "-c", "http.extraHeader="+AuthHeader(user, token))
	}
	args = append(args, "fetch", "-q", "--prune", remote)
	args = append(args, refspecs...)
	_, err := r.Run(ctx, args...)
	return err
}

// RemoteBranches lists branches on remote that match pattern (for example "change/*")
// as a map of branch name to sha.
func (r *Repo) RemoteBranches(ctx context.Context, user, token, remote, pattern string) (map[string]string, error) {
	args := []string{}
	if token != "" {
		args = append(args, "-c", "http.extraHeader="+AuthHeader(user, token))
	}
	args = append(args, "ls-remote", "--heads", remote)
	if pattern != "" {
		args = append(args, "refs/heads/"+pattern)
	}
	out, err := r.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			res[strings.TrimPrefix(f[1], "refs/heads/")] = f[0]
		}
	}
	return res, nil
}

// AuthHeader returns an HTTP Authorization header value for basic auth with a token.
func AuthHeader(user, token string) string {
	if user == "" {
		user = "pk"
	}
	return "Authorization: Basic " + basic(user+":"+token)
}

// redactArgs hides auth headers in error messages.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "http.extraHeader=") {
			a = "http.extraHeader=<redacted>"
		}
		out[i] = a
	}
	return out
}

// redact hides credentials in URLs (https://user:token@host).
func redact(s string) string {
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			return s
		}
		rest := s[i+3:]
		at := strings.IndexByte(rest, '@')
		sp := strings.IndexAny(rest, " \n/'\"")
		if at < 0 || (sp >= 0 && sp < at) {
			return s[:i+3] + redact(rest)
		}
		return s[:i+3] + "<redacted>" + redact(rest[at:])
	}
}

// missingPath reports "git show <sha>:<path>" of a path the commit does
// not have: as final as the commit.
func missingPath(args []string, err error) bool {
	var ge *Error
	if args[0] != "show" || !errors.As(err, &ge) {
		return false
	}
	return strings.Contains(ge.Stderr, "does not exist in '") || strings.Contains(ge.Stderr, "exists on disk, but not in '")
}

// immErrs holds final errors of immutable commands (bounded).
var immErrs = &errCache{m: map[string]error{}}

type errCache struct {
	mu sync.Mutex
	m  map[string]error
}

func (c *errCache) get(k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *errCache) put(k string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= immCacheMax {
		c.m = map[string]error{}
	}
	c.m[k] = err
}

// immCache holds the output of git commands on content-addressed revisions
// (bounded; only successful runs).
var immCache = &outCache{m: map[string][]byte{}}

const immCacheMax = 8192

type outCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *outCache) get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

func (c *outCache) put(k string, v []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= immCacheMax {
		c.m = map[string][]byte{}
	}
	c.m[k] = append([]byte(nil), v...)
}

var (
	reSHARev  = regexp.MustCompile(`^[0-9a-f]{40}(\^[0-9]?|~[0-9]*)*(\^\{(commit|tree)\})?$`)
	reNumber  = regexp.MustCompile(`^[0-9]+$`)
	immutable = map[string]bool{"rev-parse": true, "show": true, "diff": true, "log": true,
		"merge-base": true, "rev-list": true, "ls-tree": true}
)

// immutableArgs reports a read-only command whose revisions are all full
// shas (or sha ranges, sha:path): its output depends only on the objects.
func immutableArgs(args []string) bool {
	if len(args) == 0 || !immutable[args[0]] {
		return false
	}
	revs, ranges := 0, 0
	enough := func() bool {
		// diff with one rev compares against the work tree or the index.
		if args[0] == "diff" {
			return revs >= 2 || ranges > 0
		}
		return revs > 0
	}
	for i, a := range args[1:] {
		switch {
		case a == "--":
			return enough() // paths follow
		case strings.HasPrefix(a, "-"):
			if a == "--grep" || a == "--author" || a == "-S" || a == "-G" || a == "--output" || a == "--all" ||
				a == "--branches" || a == "--remotes" || a == "--not" || strings.HasPrefix(a, "--remotes=") ||
				strings.HasPrefix(a, "--output=") || a == "--no-index" || a == "--cached" {
				return false
			}
		case reNumber.MatchString(a) && i > 0 && strings.HasPrefix(args[i], "-"):
			// value of an option such as -n 50
		default:
			rev := a
			if args[0] == "show" {
				if j := strings.IndexByte(a, ':'); j > 0 {
					rev = a[:j]
				}
			}
			parts := strings.Split(strings.ReplaceAll(rev, "...", ".."), "..")
			for _, part := range parts {
				if !reSHARev.MatchString(part) {
					return false
				}
			}
			if len(parts) > 1 {
				ranges++
			}
			revs++
		}
	}
	return enough()
}
