package seed

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/forge"
	"github.com/dennisklein/platform-kit/internal/forge/gitea"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/policy"
	seedfs "github.com/dennisklein/platform-kit/seed"
)

// Options of pk forge-init.
type Options struct {
	// ForgeURL is the forge base URL, AdminUser and AdminPassword its admin.
	ForgeURL      string
	AdminUser     string
	AdminPassword string
	Owner, Repo   string
	// CredentialsPath is where the tokens go (shared volume).
	CredentialsPath string
	// CredentialsURL is the forge URL other containers use (written into the credentials).
	CredentialsURL string
	// Reset deletes and rebuilds the repo.
	Reset bool
	// WorkDir is a scratch directory for the build.
	WorkDir string
	// Password is the password of the demo users (default pk-demo-2026).
	Password string
	// Wait is how long to wait for the forge (default 2 min).
	Wait time.Duration
	Log  *slog.Logger
}

// DefaultPassword of the demo users in the forge web UI.
const DefaultPassword = "pk-demo-2026"

// Change is seed/changes/NN-<id>/change.yaml.
type Change struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Intent     string            `json:"intent"`
	Author     string            `json:"author"`
	Agent      string            `json:"agent,omitempty"`
	Date       time.Time         `json:"date"`
	Merged     bool              `json:"merged"`
	MergedAt   time.Time         `json:"mergedAt,omitempty"`
	MergedBy   string            `json:"mergedBy,omitempty"`
	ApprovedBy []string          `json:"approvedBy,omitempty"`
	LabRun     *change.LabRunRef `json:"labRun,omitempty"`
	WorkItems  []string          `json:"workItems,omitempty"`
	Trailers   []gitx.Trailer    `json:"trailers,omitempty"`
	Commits    []SeedCommit      `json:"commits,omitempty"`
	Open       *OpenSpec         `json:"open,omitempty"`
	dir        string
	before     map[string][]byte
	after      map[string][]byte
}

// SeedCommit is one commit of a seed change.
type SeedCommit struct {
	Message string   `json:"message"`
	Files   []string `json:"files"`
}

// OpenSpec describes an open change.
type OpenSpec struct {
	Ready     bool               `json:"ready"`
	Approvals []string           `json:"approvals,omitempty"`
	LabRuns   []change.LabRunRef `json:"labRuns,omitempty"`
}

// Load reads the seed tree: the final state and the changes in order.
func Load(fsys fs.FS) (state map[string][]byte, changes []*Change, err error) {
	if state, err = readTree(fsys, "state"); err != nil {
		return nil, nil, err
	}
	dirs, err := fs.ReadDir(fsys, "changes")
	if err != nil {
		return nil, nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := path.Join("changes", d.Name())
		b, err := fs.ReadFile(fsys, dir+"/change.yaml")
		if err != nil {
			return nil, nil, err
		}
		c := &Change{dir: dir}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", dir, err)
		}
		if !change.ValidID(c.ID) || c.Title == "" || c.Author == "" {
			return nil, nil, fmt.Errorf("%s: id, title and author are required", dir)
		}
		if c.before, err = readTree(fsys, dir+"/before"); err != nil {
			return nil, nil, err
		}
		if c.after, err = readTree(fsys, dir+"/after"); err != nil {
			return nil, nil, err
		}
		changes = append(changes, c)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].dir < changes[j].dir })
	return state, changes, nil
}

func readTree(fsys fs.FS, root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if _, err := fs.Stat(fsys, root); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		out[strings.TrimPrefix(p, root+"/")] = b
		return err
	})
	return out, err
}

// touched returns the paths a change touches, sorted.
func (c *Change) touched() []string {
	set := map[string]bool{}
	for p := range c.before {
		set[p] = true
	}
	for p := range c.after {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// commits returns the commits of a change; without a list one commit with all files.
func (c *Change) commits() []SeedCommit {
	if len(c.Commits) > 0 {
		return c.Commits
	}
	return []SeedCommit{{Message: c.Title, Files: c.touched()}}
}

// InitialTree undoes the merged changes from the final state.
func InitialTree(state map[string][]byte, changes []*Change) (map[string][]byte, error) {
	tree := map[string][]byte{}
	for k, v := range state {
		tree[k] = v
	}
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if !c.Merged {
			continue
		}
		for _, p := range c.touched() {
			if string(tree[p]) != string(c.after[p]) {
				return nil, fmt.Errorf("%s: %s does not match after/", c.ID, p)
			}
			if v, ok := c.before[p]; ok {
				tree[p] = v
			} else {
				delete(tree, p)
			}
		}
	}
	return tree, nil
}

// ---------------------------------------------------------------- people

type people struct {
	p *policy.Policy
}

func (pp people) sig(user string, when time.Time) gitx.Signature {
	for _, x := range pp.p.People {
		if x.ID == user {
			return gitx.Signature{Name: x.Name, Email: x.Email, When: when}
		}
	}
	return gitx.Signature{Name: user, Email: user + "@pk.local", When: when}
}

func (pp people) actsFor(agent string) string {
	for _, a := range pp.p.Agents {
		if a.ID == agent {
			return a.ActsFor
		}
	}
	return ""
}

// users returns the forge accounts: people, agents, bots and pk-ci.
func (pp people) users() []forge.User {
	var out []forge.User
	for _, x := range pp.p.People {
		out = append(out, forge.User{Login: x.ID, FullName: x.Name, Email: x.Email})
	}
	for _, a := range pp.p.Agents {
		out = append(out, forge.User{Login: a.ID, FullName: a.ID + " (agent of " + a.ActsFor + ")", Bot: true})
	}
	for _, b := range pp.p.Bots {
		out = append(out, forge.User{Login: b.ID, FullName: b.ID, Bot: true})
	}
	return append(out, forge.User{Login: forge.CIUser, FullName: "platform-kit CI", Bot: true})
}

// Protection is the branch protection of main generated from the policy:
// no direct push, required check pk/policy, block outdated branches, merge
// whitelist = people with role platform-admin, approvals whitelist = all
// people who approve in some rule, 1 required approval (pk/policy enforces
// the per-path count).
func Protection(p *policy.Policy) forge.BranchProtection {
	bp := forge.BranchProtection{
		Branch: "main", RequiredChecks: []string{forge.ContextPolicy}, RequiredApprovals: 1, BlockOnOutdated: true,
	}
	approver := map[string]bool{}
	for _, r := range p.Rules {
		for _, a := range r.Approvers {
			approver[a] = true
		}
	}
	for _, x := range p.People {
		for _, role := range x.Roles {
			if role == "platform-admin" && !contains(bp.MergeWhitelist, x.ID) {
				bp.MergeWhitelist = append(bp.MergeWhitelist, x.ID)
			}
		}
		ok := false
		for _, role := range x.Roles {
			ok = ok || approver[role]
		}
		for _, t := range x.Teams {
			ok = ok || approver["team:"+t]
		}
		if ok {
			bp.ApprovalsWhitelist = append(bp.ApprovalsWhitelist, x.ID)
		}
	}
	return bp
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- forge-init

// ForgeInit configures the forge (users, tokens, repo, branch protection)
// and pushes the seed history and open changes. It does nothing to the repo
// when it exists, unless Reset is set; tokens and credentials are made again
// when the credentials file is missing or stale.
func ForgeInit(ctx context.Context, o Options) error {
	if o.Log == nil {
		o.Log = slog.Default().With("component", "forge-init")
	}
	if o.Password == "" {
		o.Password = DefaultPassword
	}
	if o.Wait == 0 {
		o.Wait = 2 * time.Minute
	}
	if o.CredentialsURL == "" {
		o.CredentialsURL = o.ForgeURL
	}
	state, changes, err := Load(seedfs.FS())
	if err != nil {
		return err
	}
	var pol policy.Policy
	if err := yaml.Unmarshal(state[policy.Path], &pol); err != nil {
		return fmt.Errorf("seed policy: %w", err)
	}
	pp := people{&pol}
	g := gitea.New(gitea.Options{URL: o.ForgeURL, Owner: o.Owner, Repo: o.Repo, AdminUser: o.AdminUser, AdminPassword: o.AdminPassword})
	if err := waitForge(ctx, g, o.Wait, o.Log); err != nil {
		return err
	}
	exists, empty, err := g.RepoExists(ctx, o.Owner, o.Repo)
	if err != nil {
		return fmt.Errorf("forge admin %s: %w", o.AdminUser, err)
	}
	if exists && !empty && !o.Reset {
		if cr, err := forge.LoadCredentials(o.CredentialsPath); err == nil && credsWork(ctx, o, cr) {
			o.Log.Info("state repo exists, nothing to do", "repo", o.Owner+"/"+o.Repo)
			return nil
		}
		o.Log.Info("state repo exists; making tokens again")
		cr, err := setupUsers(ctx, g, pp, o)
		if err != nil {
			return err
		}
		if old, err := forge.LoadCredentials(o.CredentialsPath); err == nil {
			cr.Generation = old.Generation
		}
		return cr.Save(o.CredentialsPath)
	}
	if exists {
		o.Log.Info("deleting the state repo", "repo", o.Owner+"/"+o.Repo)
		if err := g.DeleteRepo(ctx, o.Owner, o.Repo); err != nil {
			return err
		}
	}
	cr, err := setupUsers(ctx, g, pp, o)
	if err != nil {
		return err
	}
	if err := g.EnsureOrg(ctx, o.Owner); err != nil {
		return err
	}
	if _, err := g.EnsureRepo(ctx, o.Owner, o.Repo, false); err != nil {
		return err
	}
	for _, u := range pp.users() {
		perm := "write"
		if u.Login == "vera" {
			perm = "read"
		}
		if err := g.AddCollaborator(ctx, u.Login, perm); err != nil {
			return fmt.Errorf("collaborator %s: %w", u.Login, err)
		}
	}
	b := &builder{g: g, pp: pp, o: o, log: o.Log}
	if err := b.build(ctx, state, changes); err != nil {
		return err
	}
	if err := g.SetBranchProtection(ctx, Protection(&pol)); err != nil {
		return fmt.Errorf("branch protection: %w", err)
	}
	if err := b.openChanges(ctx, changes); err != nil {
		return err
	}
	cr.Generation = time.Now().UTC().Format(time.RFC3339Nano)
	if err := cr.Save(o.CredentialsPath); err != nil {
		return err
	}
	o.Log.Info("forge ready", "repo", g.Repo().HTMLURL, "credentials", o.CredentialsPath)
	return nil
}

func waitForge(ctx context.Context, g *gitea.Client, wait time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(wait)
	for {
		err := g.Ping(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("forge not reachable: %w", err)
		}
		log.Info("waiting for the forge", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// credsWork checks that the stored tokens still work.
func credsWork(ctx context.Context, o Options, cr *forge.Credentials) bool {
	tok, ok := cr.Token(forge.CIUser)
	if !ok || cr.Owner != o.Owner || cr.Repo != o.Repo {
		return false
	}
	c := gitea.New(gitea.Options{URL: o.ForgeURL, Owner: o.Owner, Repo: o.Repo, Tokens: map[string]string{forge.CIUser: tok}})
	_, err := c.ListBranches(ctx, "main")
	return err == nil
}

func setupUsers(ctx context.Context, g *gitea.Client, pp people, o Options) (*forge.Credentials, error) {
	cr := &forge.Credentials{URL: o.CredentialsURL, Owner: o.Owner, Repo: o.Repo,
		Tokens: map[string]string{}, Passwords: map[string]string{}}
	for _, u := range pp.users() {
		u.Password = o.Password
		if err := g.EnsureUser(ctx, u); err != nil {
			return nil, fmt.Errorf("user %s: %w", u.Login, err)
		}
		tok, err := g.CreateToken(ctx, u.Login, u.Password, "platform-kit")
		if err != nil {
			return nil, fmt.Errorf("token %s: %w", u.Login, err)
		}
		cr.Tokens[u.Login] = tok
		if !u.Bot {
			cr.Passwords[u.Login] = u.Password
		}
	}
	return cr, nil
}

// ---------------------------------------------------------------- builder

type builder struct {
	g    *gitea.Client
	pp   people
	o    Options
	log  *slog.Logger
	repo *gitx.Repo
}

func (b *builder) token(user string) string {
	t, _ := b.g.Token(user)
	return t
}

func (b *builder) push(ctx context.Context, user string, refspecs ...string) error {
	return b.repo.Push(ctx, gitx.PushOptions{Refspecs: refspecs, User: user, Token: b.token(user)})
}

// writeTree makes the work tree equal to tree.
func writeTree(dir string, tree map[string][]byte) error {
	for p, v := range tree {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, v, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// apply puts the after/ version of files into the work tree (or deletes them).
func (b *builder) apply(c *Change, files []string) error {
	for _, p := range files {
		full := filepath.Join(b.repo.Dir, filepath.FromSlash(p))
		if v, ok := c.after[p]; ok {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, v, 0o644); err != nil {
				return err
			}
		} else if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (b *builder) commit(ctx context.Context, msg string, author gitx.Signature, trailers []gitx.Trailer) (string, error) {
	if err := b.repo.Add(ctx); err != nil {
		return "", err
	}
	return b.repo.Commit(ctx, gitx.CommitOptions{Message: msg, Author: author, Trailers: trailers, AllowEmpty: true})
}

// meta is the metadata file of a seed change.
func (c *Change) meta() change.Meta {
	m := change.Meta{ID: c.ID, Title: c.Title, Intent: c.Intent, Author: c.Author, Agent: c.Agent,
		Target: "main", WorkItems: c.WorkItems}
	if m.Agent != "" {
		m.Author = c.Author
	}
	return m
}

// writer is the identity that makes the commits of a change.
func (b *builder) writer(c *Change) (user string, trailers []gitx.Trailer) {
	if c.Agent != "" {
		return c.Agent, []gitx.Trailer{{Key: change.TrailerAgent, Value: c.Agent}, {Key: change.TrailerOnBehalfOf, Value: c.Author}}
	}
	return c.Author, nil
}

// branchChange makes the branch of a change with its commits. It returns
// the tested sha of each lab run (the head before its report commit).
func (b *builder) branchChange(ctx context.Context, c *Change, runs []change.LabRunRef, keepMeta bool) (map[string]string, error) {
	branch := change.Branch(c.ID, c.Title)
	if _, err := b.repo.Run(ctx, "checkout", "-q", "-B", branch, "main"); err != nil {
		return nil, err
	}
	user, trailers := b.writer(c)
	when := c.Date
	next := func() gitx.Signature {
		s := b.pp.sig(user, when)
		when = when.Add(7 * time.Minute)
		return s
	}
	m := c.meta()
	mp := filepath.Join(b.repo.Dir, filepath.FromSlash(change.MetaPath(c.ID)))
	if err := writeTree(b.repo.Dir, map[string][]byte{change.MetaPath(c.ID): change.MarshalMeta(m)}); err != nil {
		return nil, err
	}
	if _, err := b.commit(ctx, "chore(change): open "+c.ID, next(), trailers); err != nil {
		return nil, err
	}
	tested := map[string]string{}
	for _, sc := range c.commits() {
		if strings.HasPrefix(sc.Message, "docs(lab):") {
			head, _ := b.repo.Head(ctx)
			for i := range runs {
				if strings.Contains(sc.Message, runs[i].Run) || len(runs) == 1 {
					tested[runs[i].Run] = head
					runs[i].SHA = head
				}
			}
			if keepMeta {
				m.LabRuns = runs
				if err := os.WriteFile(mp, change.MarshalMeta(m), 0o644); err != nil {
					return nil, err
				}
			}
		}
		if err := b.apply(c, sc.Files); err != nil {
			return nil, err
		}
		if _, err := b.commit(ctx, sc.Message, next(), trailers); err != nil {
			return nil, err
		}
	}
	if keepMeta && len(runs) > 0 && len(tested) == 0 {
		m.LabRuns = runs
		if err := os.WriteFile(mp, change.MarshalMeta(m), 0o644); err != nil {
			return nil, err
		}
		if _, err := b.commit(ctx, "chore(change): record lab runs", next(), trailers); err != nil {
			return nil, err
		}
	}
	if !keepMeta {
		if err := os.Remove(mp); err != nil {
			return nil, err
		}
		if _, err := b.commit(ctx, "chore(change): move metadata to the merge request", next(), trailers); err != nil {
			return nil, err
		}
	}
	return tested, nil
}

// build pushes main with the merged history.
func (b *builder) build(ctx context.Context, state map[string][]byte, changes []*Change) error {
	dir := filepath.Join(b.o.WorkDir, "state")
	_ = os.RemoveAll(dir)
	r, err := gitx.Init(ctx, dir, false)
	if err != nil {
		return err
	}
	b.repo = r
	_ = r.Config(ctx, "remote.origin.url", b.g.Repo().CloneURL)
	_ = r.Config(ctx, "commit.gpgsign", "false")
	initial, err := InitialTree(state, changes)
	if err != nil {
		return err
	}
	if err := writeTree(dir, initial); err != nil {
		return err
	}
	start := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	if _, err := b.commit(ctx, "chore: initial state", b.pp.sig("dennis", start), nil); err != nil {
		return err
	}
	if err := b.push(ctx, "dennis", "main:main"); err != nil {
		return fmt.Errorf("push main: %w", err)
	}
	for _, c := range changes {
		if !c.Merged {
			continue
		}
		if err := b.mergedChange(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	// main must equal state/.
	got, err := r.LsTree(ctx, "main", "")
	if err != nil {
		return err
	}
	if len(got) != len(state) {
		return fmt.Errorf("seed: main has %d files, state has %d", len(got), len(state))
	}
	for _, e := range got {
		v, err := r.ShowFile(ctx, "main", e.Path)
		if err != nil || string(v) != string(state[e.Path]) {
			return fmt.Errorf("seed: main differs from state at %s", e.Path)
		}
	}
	return nil
}

func (b *builder) mergedChange(ctx context.Context, c *Change) error {
	b.log.Info("merged change", "id", c.ID, "title", c.Title)
	var runs []change.LabRunRef
	if c.LabRun != nil {
		runs = []change.LabRunRef{*c.LabRun}
	}
	branch := change.Branch(c.ID, c.Title)
	tested, err := b.branchChange(ctx, c, runs, false)
	if err != nil {
		return err
	}
	user, _ := b.writer(c)
	if err := b.push(ctx, user, branch+":"+branch); err != nil {
		return err
	}
	m := c.meta()
	m.LabRuns = runs
	pr, err := b.g.CreatePR(ctx, user, forge.PRCreate{Title: c.Title, Body: change.Description(m), Head: branch, Base: "main"})
	if err != nil {
		return err
	}
	head, _ := b.repo.Head(ctx)
	if sha := tested[c.labRunName()]; sha != "" && c.LabRun != nil {
		_ = b.g.SetStatus(ctx, user, sha, forge.CommitStatus{Context: forge.ContextLab, State: labState(c.LabRun.Result), Description: c.LabRun.Run + " " + string(c.LabRun.Result)})
		_ = b.g.SetStatus(ctx, user, head, forge.CommitStatus{Context: forge.ContextLab, State: labState(c.LabRun.Result), Description: c.LabRun.Run + " " + string(c.LabRun.Result)})
	}
	_ = b.g.SetStatus(ctx, forge.CIUser, head, forge.CommitStatus{Context: forge.ContextPolicy, State: forge.StatusSuccess, Description: "policy gates met"})
	for _, a := range c.ApprovedBy {
		if _, err := b.g.Approve(ctx, a, pr.Number, ""); err != nil {
			return fmt.Errorf("approve as %s: %w", a, err)
		}
	}
	// The merge commit, dated, with trailers; main is not protected yet.
	if _, err := b.repo.Run(ctx, "checkout", "-q", "main"); err != nil {
		return err
	}
	var labRun *change.LabRunRef
	if c.LabRun != nil {
		lr := *c.LabRun
		labRun = &lr
	}
	title, msg := change.MergeMessage(m, change.MergeInfo{MR: pr.Number, ApprovedBy: c.ApprovedBy, LabRun: labRun, Extra: c.Trailers})
	mergedBy := c.MergedBy
	if mergedBy == "" {
		mergedBy = "dennis"
	}
	sig := b.pp.sig(mergedBy, c.MergedAt)
	d := c.MergedAt.Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=" + sig.Name, "GIT_AUTHOR_EMAIL=" + sig.Email, "GIT_AUTHOR_DATE=" + d,
		"GIT_COMMITTER_NAME=" + sig.Name, "GIT_COMMITTER_EMAIL=" + sig.Email, "GIT_COMMITTER_DATE=" + d}
	msgFile := filepath.Join(b.o.WorkDir, "merge-msg")
	if err := os.WriteFile(msgFile, []byte(title+"\n\n"+msg+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := b.repo.With(env...).Run(ctx, "merge", "-q", "--no-ff", "--no-edit", "--cleanup=verbatim", "-F", msgFile, branch); err != nil {
		return err
	}
	if err := b.push(ctx, mergedBy, "main:main"); err != nil {
		return fmt.Errorf("push main: %w", err)
	}
	// Gitea marks the PR merged when it sees the head in main (autodetect manual merge).
	if err := b.waitMerged(ctx, pr.Number); err != nil {
		b.log.Warn("pull request not marked merged", "id", c.ID, "mr", pr.Number, "err", err)
	}
	if err := b.g.DeleteBranch(ctx, user, branch); err != nil && !errors.Is(err, forge.ErrNotFound) {
		b.log.Warn("delete branch", "branch", branch, "err", err)
	}
	_, _ = b.repo.Run(ctx, "branch", "-q", "-D", branch)
	return nil
}

func (c *Change) labRunName() string {
	if c.LabRun == nil {
		return ""
	}
	return c.LabRun.Run
}

func labState(r change.LabResult) forge.StatusState {
	if r == change.LabPass {
		return forge.StatusSuccess
	}
	return forge.StatusFailure
}

func (b *builder) waitMerged(ctx context.Context, number int) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		pr, err := b.g.GetPR(ctx, number)
		if err == nil && pr.State == forge.PRMerged {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("state %s", pr.State)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// openChanges pushes the open changes with their pull requests.
func (b *builder) openChanges(ctx context.Context, changes []*Change) error {
	if _, err := b.repo.Run(ctx, "checkout", "-q", "main"); err != nil {
		return err
	}
	for _, c := range changes {
		if c.Merged {
			continue
		}
		if err := b.openChange(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
	}
	_, err := b.repo.Run(ctx, "checkout", "-q", "main")
	return err
}

func (b *builder) openChange(ctx context.Context, c *Change) error {
	b.log.Info("open change", "id", c.ID, "title", c.Title)
	spec := c.Open
	if spec == nil {
		spec = &OpenSpec{}
	}
	runs := append([]change.LabRunRef(nil), spec.LabRuns...)
	tested, err := b.branchChange(ctx, c, runs, !spec.Ready)
	if err != nil {
		return err
	}
	for i := range runs {
		if sha := tested[runs[i].Run]; sha != "" {
			runs[i].SHA = sha
		}
	}
	branch := change.Branch(c.ID, c.Title)
	user, _ := b.writer(c)
	if err := b.push(ctx, user, branch+":"+branch); err != nil {
		return err
	}
	head, _ := b.repo.Head(ctx)
	m := c.meta()
	m.LabRuns = runs
	pr, err := b.g.CreatePR(ctx, user, forge.PRCreate{Title: c.Title, Body: change.Description(m), Head: branch, Base: "main", Draft: !spec.Ready})
	if err != nil {
		return err
	}
	for _, r := range runs {
		st := labState(r.Result)
		desc := r.Run + " " + string(r.Result)
		if r.SHA != "" {
			if err := b.g.SetStatus(ctx, user, r.SHA, forge.CommitStatus{Context: forge.ContextLab, State: st, Description: desc}); err != nil {
				return err
			}
		}
		if err := b.g.SetStatus(ctx, user, head, forge.CommitStatus{Context: forge.ContextLab, State: st, Description: desc}); err != nil {
			return err
		}
	}
	for _, a := range spec.Approvals {
		if _, err := b.g.Approve(ctx, a, pr.Number, ""); err != nil {
			return fmt.Errorf("approve as %s: %w", a, err)
		}
	}
	return nil
}
