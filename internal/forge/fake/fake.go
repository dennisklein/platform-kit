// Package fake is an in-memory forge for tests. It keeps pull requests,
// reviews, comments, statuses and branches; a merge calls the optional
// OnMerge hook so tests can make the merge commit in a real git repo.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/gitx"
)

// Forge is the fake. The zero value is not usable; use New.
type Forge struct {
	mu       sync.Mutex
	repo     forge.Repo
	prs      map[int]*forge.PR
	reviews  map[int][]forge.Review
	comments map[int][]forge.Comment
	statuses map[string][]forge.CommitStatus
	branches map[string]string
	next     int
	nextID   int64

	// Users lists users that may act; empty allows everyone.
	Users map[string]bool
	// MergeWhitelist lists users that may merge; empty allows everyone.
	MergeWhitelist map[string]bool
	// OnMerge makes the merge commit and returns its sha.
	OnMerge func(pr forge.PR, o forge.MergeOptions) (string, error)
	// Now is the clock.
	Now func() time.Time
	// Unreachable makes every call fail with forge.ErrUnreachable.
	Unreachable bool
	// Git is the bare repo behind the fake (optional). With it, branch
	// heads come from git and a merge makes a real merge commit.
	Git *gitx.Repo
}

var _ forge.Forge = (*Forge)(nil)

// New returns an empty fake for owner/name.
func New(owner, name string) *Forge {
	return &Forge{
		repo: forge.Repo{Owner: owner, Name: name, DefaultBranch: "main",
			CloneURL: "file:///fake/" + owner + "/" + name + ".git", HTMLURL: "http://forge.test/" + owner + "/" + name},
		prs: map[int]*forge.PR{}, reviews: map[int][]forge.Review{}, comments: map[int][]forge.Comment{},
		statuses: map[string][]forge.CommitStatus{}, branches: map[string]string{},
		Now: time.Now,
	}
}

// SetCloneURL sets the clone URL (for example a local bare repo).
func (f *Forge) SetCloneURL(u string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo.CloneURL = u
}

// SetBranch records a branch head (tests call it after a push).
func (f *Forge) SetBranch(name, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches[name] = sha
	for _, pr := range f.prs {
		if pr.Head == name && pr.State == forge.PROpen {
			pr.HeadSHA = sha
		}
	}
}

// head returns the sha of a branch (from git when set).
func (f *Forge) head(name string) string {
	if f.Git != nil {
		sha, err := f.Git.RevParse(context.Background(), "refs/heads/"+name)
		if err != nil {
			return ""
		}
		return sha
	}
	return f.branches[name]
}

// syncHeads updates the head sha of open pull requests from git.
func (f *Forge) syncHeads() {
	if f.Git == nil {
		return
	}
	for _, pr := range f.prs {
		if pr.State == forge.PROpen {
			if sha := f.head(pr.Head); sha != "" {
				pr.HeadSHA = sha
			}
		}
	}
}

func (f *Forge) check(user string) error {
	if f.Unreachable {
		return forge.ErrUnreachable
	}
	if user != "" && len(f.Users) > 0 && !f.Users[user] {
		return fmt.Errorf("%w: unknown user %s", forge.ErrForbidden, user)
	}
	return nil
}

func (f *Forge) Name() string { return "Fake" }

func (f *Forge) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.check("")
}

func (f *Forge) Repo() forge.Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repo
}

func (f *Forge) WebURL(number int) string {
	return fmt.Sprintf("%s/pulls/%d", f.repo.HTMLURL, number)
}

func (f *Forge) ListPRs(ctx context.Context, state forge.PRState) ([]forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(""); err != nil {
		return nil, err
	}
	f.syncHeads()
	var out []forge.PR
	for _, pr := range f.prs {
		if state == forge.PRAll || pr.State == state {
			out = append(out, *pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out, nil
}

func (f *Forge) GetPR(ctx context.Context, number int) (*forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(""); err != nil {
		return nil, err
	}
	f.syncHeads()
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request %d: %w", number, forge.ErrNotFound)
	}
	cp := *pr
	return &cp, nil
}

func (f *Forge) FindPR(ctx context.Context, branch string) (*forge.PR, error) {
	prs, err := f.ListPRs(ctx, forge.PRAll)
	if err != nil {
		return nil, err
	}
	for i := range prs {
		if prs[i].Head == branch {
			return &prs[i], nil
		}
	}
	return nil, fmt.Errorf("pull request for %s: %w", branch, forge.ErrNotFound)
}

func (f *Forge) CreatePR(ctx context.Context, as string, req forge.PRCreate) (*forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(as); err != nil {
		return nil, err
	}
	for _, pr := range f.prs {
		if pr.Head == req.Head && pr.State == forge.PROpen {
			return nil, fmt.Errorf("%w: pull request for %s exists", forge.ErrConflict, req.Head)
		}
	}
	f.next++
	now := f.Now()
	title := strings.TrimPrefix(req.Title, forge.DraftPrefix)
	pr := &forge.PR{
		Number: f.next, Title: title, Body: req.Body, Draft: req.Draft || title != req.Title,
		State: forge.PROpen, Head: req.Head, HeadSHA: f.head(req.Head), Base: req.Base,
		Author: as, Mergeable: true, CreatedAt: now, UpdatedAt: now,
	}
	pr.URL = fmt.Sprintf("%s/pulls/%d", f.repo.HTMLURL, pr.Number)
	f.prs[pr.Number] = pr
	cp := *pr
	return &cp, nil
}

func (f *Forge) UpdatePR(ctx context.Context, as string, number int, req forge.PRUpdate) (*forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(as); err != nil {
		return nil, err
	}
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request %d: %w", number, forge.ErrNotFound)
	}
	if req.Title != nil {
		pr.Title = *req.Title
	}
	if req.Body != nil {
		pr.Body = *req.Body
	}
	if req.Draft != nil {
		pr.Draft = *req.Draft
	}
	if req.Base != nil {
		pr.Base = *req.Base
	}
	if req.State != nil {
		pr.State = *req.State
	}
	pr.UpdatedAt = f.Now()
	cp := *pr
	return &cp, nil
}

func (f *Forge) ListReviews(ctx context.Context, number int) ([]forge.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(""); err != nil {
		return nil, err
	}
	f.syncHeads()
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request %d: %w", number, forge.ErrNotFound)
	}
	out := append([]forge.Review(nil), f.reviews[number]...)
	for i := range out {
		out[i].Stale = out[i].SHA != pr.HeadSHA
	}
	return out, nil
}

func (f *Forge) Approve(ctx context.Context, as string, number int, body string) (*forge.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(as); err != nil {
		return nil, err
	}
	f.syncHeads()
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("pull request %d: %w", number, forge.ErrNotFound)
	}
	if pr.Author == as {
		return nil, fmt.Errorf("%w: approve your own pull is not allowed", forge.ErrConflict)
	}
	f.nextID++
	rv := forge.Review{ID: f.nextID, User: as, State: forge.ReviewApproved, SHA: pr.HeadSHA, Body: body, At: f.Now()}
	f.reviews[number] = append(f.reviews[number], rv)
	return &rv, nil
}

func (f *Forge) ListComments(ctx context.Context, number int) ([]forge.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(""); err != nil {
		return nil, err
	}
	return append([]forge.Comment(nil), f.comments[number]...), nil
}

func (f *Forge) Comment(ctx context.Context, as string, number int, body string) (*forge.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(as); err != nil {
		return nil, err
	}
	f.nextID++
	c := forge.Comment{ID: f.nextID, User: as, Body: body, At: f.Now()}
	f.comments[number] = append(f.comments[number], c)
	return &c, nil
}

func (f *Forge) SetStatus(ctx context.Context, as, sha string, st forge.CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(as); err != nil {
		return err
	}
	st.Creator, st.At = as, f.Now()
	f.statuses[sha] = append(f.statuses[sha], st)
	return nil
}

func (f *Forge) Statuses(ctx context.Context, sha string) ([]forge.CommitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(""); err != nil {
		return nil, err
	}
	newest := map[string]forge.CommitStatus{}
	for _, s := range f.statuses[sha] {
		newest[s.Context] = s
	}
	out := make([]forge.CommitStatus, 0, len(newest))
	for _, s := range newest {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out, nil
}

func (f *Forge) Merge(ctx context.Context, as string, number int, o forge.MergeOptions) (*forge.MergeResult, error) {
	f.mu.Lock()
	if err := f.check(as); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.syncHeads()
	pr, ok := f.prs[number]
	if !ok {
		f.mu.Unlock()
		return nil, fmt.Errorf("pull request %d: %w", number, forge.ErrNotFound)
	}
	if len(f.MergeWhitelist) > 0 && !f.MergeWhitelist[as] {
		f.mu.Unlock()
		return nil, fmt.Errorf("%w: %s may not merge", forge.ErrForbidden, as)
	}
	if pr.State != forge.PROpen || pr.Draft {
		f.mu.Unlock()
		return nil, fmt.Errorf("%w: pull request %d is not mergeable", forge.ErrConflict, number)
	}
	if o.HeadSHA != "" && pr.HeadSHA != "" && o.HeadSHA != pr.HeadSHA {
		f.mu.Unlock()
		return nil, fmt.Errorf("%w: head changed", forge.ErrConflict)
	}
	cp := *pr
	hook := f.OnMerge
	f.mu.Unlock()

	sha := fmt.Sprintf("merge-%d", number)
	switch {
	case hook != nil:
		var err error
		if sha, err = hook(cp, o); err != nil {
			return nil, err
		}
	case f.Git != nil:
		var err error
		if sha, err = gitMerge(ctx, f.Git, cp, o, as); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pr.State, pr.MergedSHA, pr.MergedBy, pr.MergedAt = forge.PRMerged, sha, as, f.Now()
	f.branches[pr.Base] = sha
	if o.DeleteBranch {
		delete(f.branches, pr.Head)
		if f.Git != nil {
			_, _ = f.Git.Run(ctx, "update-ref", "-d", "refs/heads/"+pr.Head)
		}
	}
	return &forge.MergeResult{SHA: sha}, nil
}

func (f *Forge) ListBranches(ctx context.Context, prefix string) ([]forge.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(""); err != nil {
		return nil, err
	}
	if f.Git != nil {
		heads, err := f.Git.RemoteBranches(ctx, "", "", f.Git.Dir, prefix+"*")
		if err != nil {
			return nil, err
		}
		out := []forge.Branch{}
		for n, sha := range heads {
			out = append(out, forge.Branch{Name: n, SHA: sha})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	var out []forge.Branch
	for n, sha := range f.branches {
		if strings.HasPrefix(n, prefix) {
			out = append(out, forge.Branch{Name: n, SHA: sha})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Forge) DeleteBranch(ctx context.Context, as, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(as); err != nil {
		return err
	}
	if f.Git != nil {
		if f.head(branch) == "" {
			return fmt.Errorf("branch %s: %w", branch, forge.ErrNotFound)
		}
		_, err := f.Git.Run(ctx, "update-ref", "-d", "refs/heads/"+branch)
		return err
	}
	if _, ok := f.branches[branch]; !ok {
		return fmt.Errorf("branch %s: %w", branch, forge.ErrNotFound)
	}
	delete(f.branches, branch)
	return nil
}

// gitMerge makes a merge commit (style merge) of the PR head into the base
// in the bare repo, like the forge does.
func gitMerge(ctx context.Context, bare *gitx.Repo, pr forge.PR, o forge.MergeOptions, as string) (string, error) {
	base, err := bare.RevParse(ctx, "refs/heads/"+pr.Base)
	if err != nil {
		return "", err
	}
	head, err := bare.RevParse(ctx, "refs/heads/"+pr.Head)
	if err != nil {
		return "", err
	}
	tree, err := bare.Line(ctx, "merge-tree", "--write-tree", base, head)
	if err != nil {
		return "", fmt.Errorf("%w: merge conflict: %v", forge.ErrConflict, err)
	}
	msg := o.Title
	if o.Message != "" {
		msg += "\n\n" + o.Message
	}
	env := []string{"GIT_AUTHOR_NAME=" + as, "GIT_AUTHOR_EMAIL=" + as + "@pk.local",
		"GIT_COMMITTER_NAME=" + as, "GIT_COMMITTER_EMAIL=" + as + "@pk.local"}
	sha, err := bare.With(env...).RunInput(ctx, []byte(msg+"\n"), "commit-tree", strings.Fields(tree)[0], "-p", base, "-p", head, "-F", "-")
	if err != nil {
		return "", err
	}
	sha = strings.TrimSpace(sha)
	if _, err := bare.Run(ctx, "update-ref", "refs/heads/"+pr.Base, sha, base); err != nil {
		return "", err
	}
	return sha, nil
}
