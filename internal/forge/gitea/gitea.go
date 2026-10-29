// Package gitea is the forge adapter for Gitea (tested with 1.24). It holds
// one access token per forge user and acts as the user that a call names.
// Admin calls (users, tokens, org, repo, branch protection) use the admin
// account with basic auth.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/forge"
)

// Options of a client.
type Options struct {
	// URL is the base URL, for example http://gitea:3000.
	URL   string
	Owner string
	Repo  string
	// Tokens maps forge users to access tokens.
	Tokens map[string]string
	// AdminUser and AdminPassword authorize admin calls (forge-init).
	AdminUser     string
	AdminPassword string
	// ReadAs is the user for read calls (default the first of pk-ci, dennis, any).
	ReadAs string
	// HTTP is the HTTP client (default with a 20 s timeout).
	HTTP *http.Client
}

// Client is the Gitea adapter. It implements forge.Forge and forge.Admin.
type Client struct {
	base   string
	owner  string
	repo   string
	admin  string
	pass   string
	readAs string
	http   *http.Client

	mu     sync.RWMutex
	tokens map[string]string
}

var (
	_ forge.Forge = (*Client)(nil)
	_ forge.Admin = (*Client)(nil)
)

// New returns a client.
func New(o Options) *Client {
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	c := &Client{
		base: strings.TrimRight(o.URL, "/"), owner: o.Owner, repo: o.Repo,
		admin: o.AdminUser, pass: o.AdminPassword, readAs: o.ReadAs, http: hc,
		tokens: map[string]string{},
	}
	for u, t := range o.Tokens {
		c.tokens[u] = t
	}
	return c
}

// FromCredentials returns a client for a credentials file.
func FromCredentials(cr *forge.Credentials) *Client {
	return New(Options{URL: cr.URL, Owner: cr.Owner, Repo: cr.Repo, Tokens: cr.Tokens})
}

// SetToken sets the token of a user.
func (c *Client) SetToken(user, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[user] = token
}

// Token returns the token of a user.
func (c *Client) Token(user string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tokens[user]
	return t, ok && t != ""
}

func (c *Client) Name() string { return "Gitea" }

func (c *Client) Repo() forge.Repo {
	return forge.Repo{
		Owner: c.owner, Name: c.repo, DefaultBranch: "main",
		CloneURL: c.base + "/" + c.owner + "/" + c.repo + ".git",
		HTMLURL:  c.base + "/" + c.owner + "/" + c.repo,
	}
}

func (c *Client) WebURL(number int) string {
	return fmt.Sprintf("%s/%s/%s/pulls/%d", c.base, c.owner, c.repo, number)
}

// ---------------------------------------------------------------- transport

// auth selects how a request authenticates.
type auth struct {
	user  string // token of this user
	basic [2]string
	admin bool
}

func as(user string) auth { return auth{user: user} }

// APIError is a non-2xx answer from Gitea.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
	kind    error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitea %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func (e *APIError) Unwrap() error { return e.kind }

func (c *Client) repoPath(format string, args ...any) string {
	return "/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repo) + fmt.Sprintf(format, args...)
}

func (c *Client) do(ctx context.Context, a auth, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case a.basic[0] != "":
		req.SetBasicAuth(a.basic[0], a.basic[1])
	case a.admin:
		if c.admin == "" {
			return fmt.Errorf("gitea: %w: no admin credentials", forge.ErrForbidden)
		}
		req.SetBasicAuth(c.admin, c.pass)
	default:
		user := a.user
		if user == "" {
			user = c.reader()
		}
		if t, ok := c.Token(user); ok {
			req.Header.Set("Authorization", "token "+t)
		} else if a.user != "" {
			return fmt.Errorf("gitea: %w: no token for %s", forge.ErrForbidden, a.user)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("%w: %v", forge.ErrUnreachable, err)
		}
		return fmt.Errorf("%w: %v", forge.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: apiMessage(data)}
		switch resp.StatusCode {
		case http.StatusNotFound:
			ae.kind = forge.ErrNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			ae.kind = forge.ErrForbidden
		case http.StatusConflict, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
			ae.kind = forge.ErrConflict
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			ae.kind = forge.ErrUnreachable
		}
		return ae
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("gitea %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func apiMessage(b []byte) string {
	var m struct {
		Message string   `json:"message"`
		Errors  []string `json:"errors"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		if len(m.Errors) > 0 {
			return m.Message + ": " + strings.Join(m.Errors, "; ")
		}
		return m.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// reader is the user for read calls.
func (c *Client) reader() string {
	if c.readAs != "" {
		return c.readAs
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, u := range []string{forge.CIUser, "dennis"} {
		if c.tokens[u] != "" {
			return u
		}
	}
	users := make([]string, 0, len(c.tokens))
	for u := range c.tokens {
		users = append(users, u)
	}
	sort.Strings(users)
	if len(users) > 0 {
		return users[0]
	}
	return ""
}

// ---------------------------------------------------------------- forge

func (c *Client) Ping(ctx context.Context) error {
	var v struct {
		Version string `json:"version"`
	}
	return c.do(ctx, auth{}, http.MethodGet, "/version", nil, &v)
}

type apiUser struct {
	Login    string `json:"login"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"is_admin"`
}

type apiPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	HTMLURL        string    `json:"html_url"`
	Mergeable      bool      `json:"mergeable"`
	Merged         bool      `json:"merged"`
	MergedAt       time.Time `json:"merged_at"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	MergedBy       *apiUser  `json:"merged_by"`
	User           *apiUser  `json:"user"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p *apiPR) toPR() forge.PR {
	pr := forge.PR{
		Number: p.Number, Body: p.Body, Head: p.Head.Ref, HeadSHA: p.Head.SHA, Base: p.Base.Ref,
		URL: p.HTMLURL, Mergeable: p.Mergeable, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	pr.Title, pr.Draft = SplitDraft(p.Title)
	pr.Draft = pr.Draft || p.Draft
	switch {
	case p.Merged:
		pr.State = forge.PRMerged
		pr.MergedSHA = p.MergeCommitSHA
		pr.MergedAt = p.MergedAt
		if p.MergedBy != nil {
			pr.MergedBy = p.MergedBy.Login
		}
	case p.State == "closed":
		pr.State = forge.PRClosed
	default:
		pr.State = forge.PROpen
	}
	if p.User != nil {
		pr.Author = p.User.Login
	}
	return pr
}

// SplitDraft removes a draft prefix (WIP: or [WIP]) from a title.
func SplitDraft(title string) (string, bool) {
	t := strings.TrimSpace(title)
	for _, p := range []string{forge.DraftPrefix, "WIP:", "[WIP]"} {
		if rest, ok := strings.CutPrefix(t, p); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return t, false
}

func draftTitle(title string, draft bool) string {
	t, _ := SplitDraft(title)
	if draft {
		return forge.DraftPrefix + t
	}
	return t
}

func (c *Client) ListPRs(ctx context.Context, state forge.PRState) ([]forge.PR, error) {
	st := "open"
	switch state {
	case forge.PRClosed, forge.PRMerged:
		st = "closed"
	case forge.PRAll:
		st = "all"
	}
	var out []forge.PR
	for page := 1; page < 100; page++ {
		var prs []apiPR
		path := c.repoPath("/pulls?state=%s&sort=recentupdate&page=%d&limit=50", st, page)
		if err := c.do(ctx, auth{}, http.MethodGet, path, nil, &prs); err != nil {
			return nil, err
		}
		for i := range prs {
			pr := prs[i].toPR()
			if state == forge.PRMerged && pr.State != forge.PRMerged {
				continue
			}
			if state == forge.PRClosed && pr.State != forge.PRClosed {
				continue
			}
			out = append(out, pr)
		}
		if len(prs) < 50 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out, nil
}

func (c *Client) GetPR(ctx context.Context, number int) (*forge.PR, error) {
	var p apiPR
	if err := c.do(ctx, auth{}, http.MethodGet, c.repoPath("/pulls/%d", number), nil, &p); err != nil {
		return nil, err
	}
	pr := p.toPR()
	return &pr, nil
}

func (c *Client) FindPR(ctx context.Context, branch string) (*forge.PR, error) {
	prs, err := c.ListPRs(ctx, forge.PRAll)
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

func (c *Client) CreatePR(ctx context.Context, user string, req forge.PRCreate) (*forge.PR, error) {
	in := map[string]any{
		"title": draftTitle(req.Title, req.Draft), "body": req.Body,
		"head": req.Head, "base": req.Base,
	}
	var p apiPR
	// Gitea registers pushed branches asynchronously: retry a missing head.
	err := retryNotFound(ctx, func() error {
		return c.do(ctx, as(user), http.MethodPost, c.repoPath("/pulls"), in, &p)
	})
	if err != nil {
		return nil, err
	}
	pr := p.toPR()
	return &pr, nil
}

// retryNotFound retries fn for a few seconds while it fails with ErrNotFound.
func retryNotFound(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < 25; i++ {
		if err = fn(); !errors.Is(err, forge.ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(300 * time.Millisecond):
		}
	}
	return err
}

func (c *Client) UpdatePR(ctx context.Context, user string, number int, req forge.PRUpdate) (*forge.PR, error) {
	in := map[string]any{}
	if req.Title != nil || req.Draft != nil {
		cur, err := c.GetPR(ctx, number)
		if err != nil {
			return nil, err
		}
		title, draft := cur.Title, cur.Draft
		if req.Title != nil {
			title = *req.Title
		}
		if req.Draft != nil {
			draft = *req.Draft
		}
		in["title"] = draftTitle(title, draft)
	}
	if req.Body != nil {
		in["body"] = *req.Body
	}
	if req.Base != nil {
		in["base"] = *req.Base
	}
	if req.State != nil {
		in["state"] = string(*req.State)
	}
	var p apiPR
	if err := c.do(ctx, as(user), http.MethodPatch, c.repoPath("/pulls/%d", number), in, &p); err != nil {
		return nil, err
	}
	pr := p.toPR()
	return &pr, nil
}

type apiReview struct {
	ID          int64     `json:"id"`
	User        *apiUser  `json:"user"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	Body        string    `json:"body"`
	SubmittedAt time.Time `json:"submitted_at"`
	Stale       bool      `json:"stale"`
	Dismissed   bool      `json:"dismissed"`
}

func (r *apiReview) toReview() forge.Review {
	rv := forge.Review{ID: r.ID, SHA: r.CommitID, Body: r.Body, At: r.SubmittedAt, Stale: r.Stale}
	if r.User != nil {
		rv.User = r.User.Login
	}
	switch strings.ToUpper(r.State) {
	case "APPROVED":
		rv.State = forge.ReviewApproved
	case "REQUEST_CHANGES":
		rv.State = forge.ReviewChangesRequested
	default:
		rv.State = forge.ReviewComment
	}
	if r.Dismissed {
		rv.State = forge.ReviewDismissed
	}
	return rv
}

func (c *Client) ListReviews(ctx context.Context, number int) ([]forge.Review, error) {
	var rs []apiReview
	if err := c.do(ctx, auth{}, http.MethodGet, c.repoPath("/pulls/%d/reviews?limit=50", number), nil, &rs); err != nil {
		return nil, err
	}
	out := make([]forge.Review, 0, len(rs))
	for i := range rs {
		if strings.EqualFold(rs[i].State, "PENDING") || strings.EqualFold(rs[i].State, "REQUEST_REVIEW") {
			continue
		}
		out = append(out, rs[i].toReview())
	}
	return out, nil
}

func (c *Client) Approve(ctx context.Context, user string, number int, body string) (*forge.Review, error) {
	pr, err := c.GetPR(ctx, number)
	if err != nil {
		return nil, err
	}
	in := map[string]any{"event": "APPROVED", "body": body, "commit_id": pr.HeadSHA}
	var r apiReview
	if err := c.do(ctx, as(user), http.MethodPost, c.repoPath("/pulls/%d/reviews", number), in, &r); err != nil {
		return nil, err
	}
	rv := r.toReview()
	if rv.User == "" {
		rv.User = user
	}
	return &rv, nil
}

type apiComment struct {
	ID        int64     `json:"id"`
	User      *apiUser  `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Path      string    `json:"path"`
	Line      int       `json:"position"`
}

func (cm *apiComment) toComment() forge.Comment {
	out := forge.Comment{ID: cm.ID, Body: cm.Body, At: cm.CreatedAt, Path: cm.Path, Line: cm.Line}
	if cm.User != nil {
		out.User = cm.User.Login
	}
	return out
}

func (c *Client) ListComments(ctx context.Context, number int) ([]forge.Comment, error) {
	var cs []apiComment
	if err := c.do(ctx, auth{}, http.MethodGet, c.repoPath("/issues/%d/comments", number), nil, &cs); err != nil {
		return nil, err
	}
	out := make([]forge.Comment, 0, len(cs))
	for i := range cs {
		out = append(out, cs[i].toComment())
	}
	return out, nil
}

func (c *Client) Comment(ctx context.Context, user string, number int, body string) (*forge.Comment, error) {
	var cm apiComment
	if err := c.do(ctx, as(user), http.MethodPost, c.repoPath("/issues/%d/comments", number), map[string]any{"body": body}, &cm); err != nil {
		return nil, err
	}
	out := cm.toComment()
	return &out, nil
}

type apiStatus struct {
	ID          int64     `json:"id"`
	Context     string    `json:"context"`
	Status      string    `json:"status"`
	State       string    `json:"state"`
	Description string    `json:"description"`
	TargetURL   string    `json:"target_url"`
	Creator     *apiUser  `json:"creator"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (c *Client) SetStatus(ctx context.Context, user, sha string, st forge.CommitStatus) error {
	desc := st.Description
	if len(desc) > 250 {
		desc = desc[:250]
	}
	in := map[string]any{"state": string(st.State), "context": st.Context, "description": desc, "target_url": st.TargetURL}
	return retryNotFound(ctx, func() error {
		return c.do(ctx, as(user), http.MethodPost, c.repoPath("/statuses/%s", url.PathEscape(sha)), in, nil)
	})
}

func (c *Client) Statuses(ctx context.Context, sha string) ([]forge.CommitStatus, error) {
	var all []apiStatus
	path := c.repoPath("/commits/%s/statuses?limit=50", url.PathEscape(sha))
	if err := c.do(ctx, auth{}, http.MethodGet, path, nil, &all); err != nil {
		return nil, err
	}
	// Timestamps have second resolution; the id breaks ties (higher is newer).
	newest := map[string]forge.CommitStatus{}
	newestID := map[string]int64{}
	for _, s := range all {
		state := s.Status
		if state == "" {
			state = s.State
		}
		at := s.UpdatedAt
		if at.IsZero() {
			at = s.CreatedAt
		}
		cs := forge.CommitStatus{Context: s.Context, State: forge.StatusState(state), Description: s.Description,
			TargetURL: s.TargetURL, At: at}
		if s.Creator != nil {
			cs.Creator = s.Creator.Login
		}
		cur, ok := newest[s.Context]
		if !ok || cs.At.After(cur.At) || (cs.At.Equal(cur.At) && s.ID > newestID[s.Context]) {
			newest[s.Context] = cs
			newestID[s.Context] = s.ID
		}
	}
	out := make([]forge.CommitStatus, 0, len(newest))
	for _, s := range newest {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out, nil
}

func (c *Client) Merge(ctx context.Context, user string, number int, o forge.MergeOptions) (*forge.MergeResult, error) {
	in := map[string]any{
		"Do":                        "merge",
		"MergeTitleField":           o.Title,
		"MergeMessageField":         o.Message,
		"delete_branch_after_merge": o.DeleteBranch,
	}
	if o.HeadSHA != "" {
		in["head_commit_id"] = o.HeadSHA
	}
	// Gitea answers 405 "try again later" while it checks mergeability.
	var err error
	for i := 0; i < 40; i++ {
		err = c.do(ctx, as(user), http.MethodPost, c.repoPath("/pulls/%d/merge", number), in, nil)
		var ae *APIError
		if err == nil || !errors.As(err, &ae) || !strings.Contains(strings.ToLower(ae.Message), "try again later") {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}
	// The merge commit sha is on the PR after the merge.
	for i := 0; i < 20; i++ {
		pr, err := c.GetPR(ctx, number)
		if err != nil {
			return nil, err
		}
		if pr.State == forge.PRMerged && pr.MergedSHA != "" {
			return &forge.MergeResult{SHA: pr.MergedSHA}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("gitea: pull request %d: merged, but no merge commit sha", number)
}

func (c *Client) ListBranches(ctx context.Context, prefix string) ([]forge.Branch, error) {
	var out []forge.Branch
	for page := 1; page < 100; page++ {
		var bs []struct {
			Name   string `json:"name"`
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		if err := c.do(ctx, auth{}, http.MethodGet, c.repoPath("/branches?page=%d&limit=50", page), nil, &bs); err != nil {
			return nil, err
		}
		for _, b := range bs {
			if strings.HasPrefix(b.Name, prefix) {
				out = append(out, forge.Branch{Name: b.Name, SHA: b.Commit.ID})
			}
		}
		if len(bs) < 50 {
			break
		}
	}
	return out, nil
}

func escapeBranch(b string) string {
	parts := strings.Split(b, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) DeleteBranch(ctx context.Context, user, branch string) error {
	err := c.do(ctx, as(user), http.MethodDelete, c.repoPath("/branches/%s", escapeBranch(branch)), nil, nil)
	if err == nil || errors.Is(err, forge.ErrNotFound) {
		return err
	}
	// Gitea answers 500 for a branch that was deleted already.
	if gerr := c.do(ctx, auth{}, http.MethodGet, c.repoPath("/branches/%s", escapeBranch(branch)), nil, nil); errors.Is(gerr, forge.ErrNotFound) {
		return fmt.Errorf("branch %s: %w", branch, forge.ErrNotFound)
	}
	return err
}

// ---------------------------------------------------------------- admin

func (c *Client) EnsureUser(ctx context.Context, u forge.User) error {
	email := u.Email
	if email == "" {
		email = u.Login + "@pk.local"
	}
	var cur apiUser
	err := c.do(ctx, auth{admin: true}, http.MethodGet, "/users/"+url.PathEscape(u.Login), nil, &cur)
	if errors.Is(err, forge.ErrNotFound) {
		in := map[string]any{
			"username": u.Login, "email": email, "password": u.Password, "full_name": u.FullName,
			"must_change_password": false, "visibility": "public",
		}
		if err := c.do(ctx, auth{admin: true}, http.MethodPost, "/admin/users", in, nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Set password, name and flags again (idempotent; the password may have changed).
	in := map[string]any{
		"login_name": u.Login, "source_id": 0, "email": email, "full_name": u.FullName,
		"must_change_password": false, "admin": u.Admin, "active": true,
	}
	if u.Password != "" {
		in["password"] = u.Password
	}
	return c.do(ctx, auth{admin: true}, http.MethodPatch, "/admin/users/"+url.PathEscape(u.Login), in, nil)
}

// tokenScopes are the scopes of the tokens forge-init makes.
var tokenScopes = []string{"write:repository", "write:issue", "write:user", "write:organization", "read:misc", "read:notification"}

func (c *Client) CreateToken(ctx context.Context, user, password, name string) (string, error) {
	a := auth{basic: [2]string{user, password}}
	// Token names are unique per user: delete an old one first.
	err := c.do(ctx, a, http.MethodDelete, "/users/"+url.PathEscape(user)+"/tokens/"+url.PathEscape(name), nil, nil)
	if err != nil && !errors.Is(err, forge.ErrNotFound) {
		return "", err
	}
	var out struct {
		SHA1 string `json:"sha1"`
	}
	in := map[string]any{"name": name, "scopes": tokenScopes}
	if err := c.do(ctx, a, http.MethodPost, "/users/"+url.PathEscape(user)+"/tokens", in, &out); err != nil {
		return "", err
	}
	if out.SHA1 == "" {
		return "", fmt.Errorf("gitea: token for %s: empty answer", user)
	}
	c.SetToken(user, out.SHA1)
	return out.SHA1, nil
}

func (c *Client) EnsureOrg(ctx context.Context, org string) error {
	err := c.do(ctx, auth{admin: true}, http.MethodGet, "/orgs/"+url.PathEscape(org), nil, nil)
	if !errors.Is(err, forge.ErrNotFound) {
		return err
	}
	in := map[string]any{"username": org, "visibility": "public", "repo_admin_change_team_access": true}
	return c.do(ctx, auth{admin: true}, http.MethodPost, "/orgs", in, nil)
}

type apiRepo struct {
	Name          string `json:"name"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// RepoExists reports whether the repo exists and whether it has commits.
func (c *Client) RepoExists(ctx context.Context, owner, name string) (exists, empty bool, err error) {
	var r apiRepo
	err = c.do(ctx, auth{admin: true}, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &r)
	if errors.Is(err, forge.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, r.Empty, nil
}

func (c *Client) EnsureRepo(ctx context.Context, owner, name string, private bool) (*forge.Repo, error) {
	var r apiRepo
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	err := c.do(ctx, auth{admin: true}, http.MethodGet, path, nil, &r)
	if errors.Is(err, forge.ErrNotFound) {
		in := map[string]any{"name": name, "private": private, "default_branch": "main", "auto_init": false,
			"description": "platform-kit state repo"}
		if err := c.do(ctx, auth{admin: true}, http.MethodPost, "/orgs/"+url.PathEscape(owner)+"/repos", in, &r); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	// Merge commits only; detect merges pushed to main (seed history).
	settings := map[string]any{
		"allow_merge_commits": true, "allow_rebase": false, "allow_rebase_explicit": false,
		"allow_squash_merge": false, "allow_rebase_update": false, "default_merge_style": "merge",
		"autodetect_manual_merge": true, "default_delete_branch_after_merge": true,
		"has_pull_requests": true, "has_issues": true, "has_wiki": false, "default_branch": "main",
	}
	if err := c.do(ctx, auth{admin: true}, http.MethodPatch, path, settings, &r); err != nil {
		return nil, err
	}
	c.owner, c.repo = owner, name
	repo := c.Repo()
	return &repo, nil
}

func (c *Client) DeleteRepo(ctx context.Context, owner, name string) error {
	err := c.do(ctx, auth{admin: true}, http.MethodDelete, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) AddCollaborator(ctx context.Context, user, permission string) error {
	return c.do(ctx, auth{admin: true}, http.MethodPut, c.repoPath("/collaborators/%s", url.PathEscape(user)),
		map[string]any{"permission": permission}, nil)
}

type apiProtection struct {
	RuleName                    string   `json:"rule_name"`
	BranchName                  string   `json:"branch_name"`
	EnablePush                  bool     `json:"enable_push"`
	EnableStatusCheck           bool     `json:"enable_status_check"`
	StatusCheckContexts         []string `json:"status_check_contexts"`
	EnableMergeWhitelist        bool     `json:"enable_merge_whitelist"`
	MergeWhitelistUsernames     []string `json:"merge_whitelist_usernames"`
	EnableApprovalsWhitelist    bool     `json:"enable_approvals_whitelist"`
	ApprovalsWhitelistUsernames []string `json:"approvals_whitelist_username"`
	RequiredApprovals           int      `json:"required_approvals"`
	BlockOnOutdatedBranch       bool     `json:"block_on_outdated_branch"`
	DismissStaleApprovals       bool     `json:"dismiss_stale_approvals"`
	BlockOnRejectedReviews      bool     `json:"block_on_rejected_reviews"`
}

func (c *Client) SetBranchProtection(ctx context.Context, bp forge.BranchProtection) error {
	in := apiProtection{
		RuleName: bp.Branch, BranchName: bp.Branch, EnablePush: bp.EnablePush,
		EnableStatusCheck: len(bp.RequiredChecks) > 0, StatusCheckContexts: nonNil(bp.RequiredChecks),
		EnableMergeWhitelist: len(bp.MergeWhitelist) > 0, MergeWhitelistUsernames: nonNil(bp.MergeWhitelist),
		EnableApprovalsWhitelist: len(bp.ApprovalsWhitelist) > 0, ApprovalsWhitelistUsernames: nonNil(bp.ApprovalsWhitelist),
		RequiredApprovals: bp.RequiredApprovals, BlockOnOutdatedBranch: bp.BlockOnOutdated,
		DismissStaleApprovals: bp.DismissStale, BlockOnRejectedReviews: true,
	}
	path := c.repoPath("/branch_protections/%s", url.PathEscape(bp.Branch))
	err := c.do(ctx, auth{admin: true}, http.MethodGet, path, nil, nil)
	if errors.Is(err, forge.ErrNotFound) {
		return c.do(ctx, auth{admin: true}, http.MethodPost, c.repoPath("/branch_protections"), in, nil)
	}
	if err != nil {
		return err
	}
	return c.do(ctx, auth{admin: true}, http.MethodPatch, path, in, nil)
}

func (c *Client) GetBranchProtection(ctx context.Context, branch string) (*forge.BranchProtection, error) {
	var p apiProtection
	if err := c.do(ctx, auth{admin: true}, http.MethodGet, c.repoPath("/branch_protections/%s", url.PathEscape(branch)), nil, &p); err != nil {
		return nil, err
	}
	return &forge.BranchProtection{
		Branch: branch, EnablePush: p.EnablePush, RequiredChecks: p.StatusCheckContexts,
		MergeWhitelist: p.MergeWhitelistUsernames, ApprovalsWhitelist: p.ApprovalsWhitelistUsernames,
		RequiredApprovals: p.RequiredApprovals, BlockOnOutdated: p.BlockOnOutdatedBranch, DismissStale: p.DismissStaleApprovals,
	}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ParseNumber parses "!12", "#12" or "12".
func ParseNumber(s string) (int, bool) {
	s = strings.TrimLeft(strings.TrimSpace(s), "!#")
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}
