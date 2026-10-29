package bench

// Files of a change worktree: tree, status, read, write, delete, new, problems (area A; uses kinds and schema for problems and secret masking).

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/gitx"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

// cleanPath checks a repo-relative path: no absolute paths, no "..", no .git.
func repoRelPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", Errorf(http.StatusBadRequest, "path is required")
	}
	c := path.Clean(p)
	if path.IsAbs(c) || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", Errorf(http.StatusBadRequest, "bad path %q", p)
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".git" {
			return "", Errorf(http.StatusBadRequest, "bad path %q", p)
		}
	}
	return c, nil
}

// blobSHA is the git blob sha of content (the same as git hash-object).
func fileBlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// skipName reports files that are not part of the tree (git, export marker).
func treeSkipName(name string) bool {
	return name == ".git" || name == ".pk-export" || name == "node_modules"
}

// treeKindCache remembers the kind of a file by path, size and mtime.
var treeKindCache sync.Map // key string -> string

func (s *Server) kindOf(full, rel string, fi fs.FileInfo) string {
	if s.d.Kinds == nil {
		return ""
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".yaml", ".yml", ".md", ".json":
	default:
		return ""
	}
	key := fmt.Sprintf("%s|%d|%d", full, fi.Size(), fi.ModTime().UnixNano())
	if v, ok := treeKindCache.Load(key); ok {
		return v.(string)
	}
	k := ""
	if b, err := os.ReadFile(full); err == nil {
		for _, d := range repo.ParseFile(rel, b).Docs {
			if kd, ok := s.d.Kinds.Match(rel, d); ok {
				k = kd.Name
				break
			}
		}
	}
	treeKindCache.Store(key, k)
	return k
}

// buildTree lists the files below dir as nested nodes. status maps paths to
// their status against the target (deleted files are added as nodes);
// problems counts problems per path.
func (s *Server) buildTree(dir string, status map[string]api.FileChange, problems map[string]int) ([]api.FileNode, error) {
	type fileRec struct {
		rel  string
		kind string
	}
	var files []fileRec
	err := filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if full == dir {
			return nil
		}
		if treeSkipName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, full)
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, fileRec{rel: rel, kind: s.kindOf(full, rel, fi)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.rel] = true
	}
	for p, st := range status {
		if st == api.FileChangeDeleted && !seen[p] {
			files = append(files, fileRec{rel: p})
		}
	}
	// Build with maps first and convert to sorted slices at the end.
	type node struct {
		n        api.FileNode
		children map[string]*node
	}
	top := &node{children: map[string]*node{}}
	for _, f := range files {
		parts := strings.Split(f.rel, "/")
		cur := top
		for i, part := range parts {
			p := strings.Join(parts[:i+1], "/")
			last := i == len(parts)-1
			c, ok := cur.children[part]
			if !ok {
				c = &node{n: api.FileNode{Name: part, Path: p, Type: api.FileNodeTypeDir}, children: map[string]*node{}}
				if last {
					c.n.Type = api.FileNodeTypeFile
					c.n.Kind = f.kind
					c.n.Status = status[p]
					c.n.Problems = problems[p]
				}
				cur.children[part] = c
			}
			cur = c
		}
	}
	var conv func(n *node) []api.FileNode
	conv = func(n *node) []api.FileNode {
		out := make([]api.FileNode, 0, len(n.children))
		for _, c := range n.children {
			fn := c.n
			if fn.Type == api.FileNodeTypeDir {
				fn.Children = conv(c)
				for _, ch := range fn.Children {
					if ch.Type == api.FileNodeTypeDir {
						fn.Changed += ch.Changed
						fn.Problems += ch.Problems
					} else {
						if ch.Status != "" {
							fn.Changed++
						}
						fn.Problems += ch.Problems
					}
				}
			}
			out = append(out, fn)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Type != out[j].Type {
				return out[i].Type == api.FileNodeTypeDir
			}
			return out[i].Name < out[j].Name
		})
		return out
	}
	return conv(top), nil
}

// changeStatus maps the changed files of a change (against the target,
// with saved files) to their status.
func (s *Server) changeStatus(ctx context.Context, svc change.Service, id string) map[string]api.FileChange {
	out := map[string]api.FileChange{}
	ch, err := svc.Get(ctx, id)
	if err != nil {
		return out
	}
	for _, f := range ch.ChangedFiles {
		out[f.Path] = api.FileChange(f.Status)
	}
	return out
}

func (s *Server) GetTree(ctx context.Context, req api.GetTreeRequestObject) (api.GetTreeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	dir, _, err := svc.Resolve(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	status := s.changeStatus(ctx, svc, req.Id)
	// The tree counts what needs a fix: errors and warnings. Notes (info,
	// for example "cn-035 does not exist yet") show only in the editor.
	problems := map[string]int{}
	for _, p := range s.problemsFor(ctx, dir, "") {
		if p.Severity == api.ProblemSeverityInfo {
			continue
		}
		problems[p.Path]++
	}
	entries, err := s.buildTree(dir, status, problems)
	if err != nil {
		return nil, err
	}
	out := api.FileTree{Ref: req.Id, Entries: entries}
	if src, err := svc.Source(ctx, req.Id); err == nil {
		out.Sha = src.SHA
	}
	return api.GetTree200JSONResponse(out), nil
}

func (s *Server) WorktreeStatus(ctx context.Context, req api.WorktreeStatusRequestObject) (api.WorktreeStatusResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	st, err := svc.Status(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := api.WorktreeStatus{Files: []api.FileStatus{}, Clean: len(st) == 0}
	for _, f := range st {
		out.Files = append(out.Files, change.FileStatusToAPI(f))
	}
	if wt, err := svc.Worktree(ctx, req.Id); err == nil {
		out.Head, _ = gitx.Open(wt).Head(ctx)
	}
	return api.WorktreeStatus200JSONResponse(out), nil
}

// fileAt reads a file of a change and describes it for the editor.
func (s *Server) fileAt(ctx context.Context, svc change.Service, id, dir string, live bool, p string) (api.FileContent, error) {
	content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	exists := true
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return api.FileContent{}, err
		}
		exists, content = false, nil
	}
	fc := s.fileContent(ctx, dir, p, content)
	fc.Exists, fc.ReadOnly, fc.Ref = exists, !live, id
	if fc.Problems == nil {
		fc.Problems = []api.Problem{}
	}
	if fc.Secrets == nil {
		fc.Secrets = []api.SecretField{}
	}
	if fc.Language == "" {
		fc.Language = api.FileContentLanguage(repo.Language(p))
	}
	if exists {
		fc.Sha = fileBlobSHA(content)
	}
	if st, ok := s.changeStatus(ctx, svc, id)[p]; ok {
		fc.Status = st
	}
	return fc, nil
}

func (s *Server) ReadFile(ctx context.Context, req api.ReadFileRequestObject) (api.ReadFileResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	p, err := repoRelPath(string(req.Params.Path))
	if err != nil {
		return nil, err
	}
	dir, live, err := svc.Resolve(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	fc, err := s.fileAt(ctx, svc, req.Id, dir, live, p)
	if err != nil {
		return nil, err
	}
	return api.ReadFile200JSONResponse(fc), nil
}

// worktree returns the live worktree of a change for writes.
func (s *Server) changeWorktree(ctx context.Context, id string) (change.Service, string, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, "", err
	}
	wt, err := svc.Worktree(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return svc, wt, nil
}

// afterSave publishes the change update and renders the change.
func (s *Server) afterSave(ctx context.Context, svc change.Service, id string) api.ImpactSummary {
	if n, ok := svc.(interface {
		Notify(ctx context.Context, id, reason string)
	}); ok {
		n.Notify(ctx, id, "saved")
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if imp := s.renderAfterSave(rctx, id); imp != nil {
		return *imp
	}
	return api.ImpactSummary{}
}

func (s *Server) WriteFile(ctx context.Context, req api.WriteFileRequestObject) (api.WriteFileResponseObject, error) {
	svc, wt, err := s.changeWorktree(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	p, err := repoRelPath(string(req.Params.Path))
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, Errorf(http.StatusBadRequest, "content or set is required")
	}
	full := filepath.Join(wt, filepath.FromSlash(p))
	stored, rerr := os.ReadFile(full)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return nil, rerr
	}
	if req.Body.BaseSha != "" && fileBlobSHA(stored) != req.Body.BaseSha && !(rerr != nil && req.Body.BaseSha == fileBlobSHA(nil)) {
		return nil, &HTTPError{Status: http.StatusConflict, Msg: "the file changed since it was read, reload it", Paths: []string{p}}
	}
	var next []byte
	switch {
	case len(req.Body.Set) > 0:
		if rerr != nil {
			return nil, Errorf(http.StatusNotFound, "%s does not exist", p)
		}
		next, err = s.setFields(p, stored, req.Body.Set)
	default:
		next, err = s.unmaskContent(p, []byte(req.Body.Content), stored)
	}
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			return nil, he
		}
		return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Msg: "cannot save", Detail: err.Error()}
	}
	next = finalNewline(next, stored, rerr == nil)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, next, 0o644); err != nil {
		return nil, err
	}
	out := api.WriteFileResult{Impact: s.afterSave(ctx, svc, req.Id)}
	if out.File, err = s.fileAt(ctx, svc, req.Id, wt, true, p); err != nil {
		return nil, err
	}
	return api.WriteFile200JSONResponse(out), nil
}

// finalNewline keeps the final newline of a text file: clients (agents,
// shell substitutions) often drop it, which shows as a diff on the last line
// and makes appends of other changes conflict on rebase. A stored file
// without a final newline stays as the client sent it.
func finalNewline(next, stored []byte, existed bool) []byte {
	if len(next) == 0 || next[len(next)-1] == '\n' {
		return next
	}
	if existed && len(stored) > 0 && stored[len(stored)-1] != '\n' {
		return next
	}
	return append(next, '\n')
}

func (s *Server) DeleteFile(ctx context.Context, req api.DeleteFileRequestObject) (api.DeleteFileResponseObject, error) {
	svc, wt, err := s.changeWorktree(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	p, err := repoRelPath(string(req.Params.Path))
	if err != nil {
		return nil, err
	}
	full := filepath.Join(wt, filepath.FromSlash(p))
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Errorf(http.StatusNotFound, "%s does not exist", p)
		}
		return nil, err
	}
	// Remove empty parent directories (git does not track them).
	for d := filepath.Dir(full); d != wt && strings.HasPrefix(d, wt); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
	out := api.WriteFileResult{Impact: s.afterSave(ctx, svc, req.Id)}
	if out.File, err = s.fileAt(ctx, svc, req.Id, wt, true, p); err != nil {
		return nil, err
	}
	return api.DeleteFile200JSONResponse(out), nil
}

func (s *Server) NewFile(ctx context.Context, req api.NewFileRequestObject) (api.NewFileResponseObject, error) {
	svc, wt, err := s.changeWorktree(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if req.Body == nil || req.Body.Kind == "" || req.Body.Name == "" {
		return nil, Errorf(http.StatusBadRequest, "kind and name are required")
	}
	p, content, err := s.newFileSkeleton(req.Body.Kind, req.Body.Name)
	if err != nil {
		return nil, err
	}
	if req.Body.Path != "" {
		p = req.Body.Path
	}
	if p, err = repoRelPath(p); err != nil {
		return nil, err
	}
	full := filepath.Join(wt, filepath.FromSlash(p))
	if _, err := os.Stat(full); err == nil {
		return nil, &HTTPError{Status: http.StatusConflict, Msg: p + " already exists", Paths: []string{p}}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return nil, err
	}
	s.afterSave(ctx, svc, req.Id)
	fc, err := s.fileAt(ctx, svc, req.Id, wt, true, p)
	if err != nil {
		return nil, err
	}
	return api.NewFile201JSONResponse(fc), nil
}

func (s *Server) ValidateChange(ctx context.Context, req api.ValidateChangeRequestObject) (api.ValidateChangeResponseObject, error) {
	svc, err := s.changeSvc()
	if err != nil {
		return nil, err
	}
	dir, _, err := svc.Resolve(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	p := ""
	if req.Params.Path != "" {
		if p, err = repoRelPath(req.Params.Path); err != nil {
			return nil, err
		}
	}
	out := api.ProblemList{Problems: []api.Problem{}}
	for _, pr := range s.problemsFor(ctx, dir, p) {
		out.Problems = append(out.Problems, pr)
		switch pr.Severity {
		case "error":
			out.Errors++
		case "warning":
			out.Warnings++
		}
	}
	return api.ValidateChange200JSONResponse(out), nil
}
