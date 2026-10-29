// Package repo parses a state repo tree into documents (kinds.Repo) and runs
// the validation: YAML syntax, JSON Schema per kind version and the
// cross-file checks of the kinds.
package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// File is one parsed file of the tree.
type File struct {
	Path    string
	Content []byte
	// Language: yaml, markdown, json or text.
	Language string
	Docs     []*kinds.Doc
	// Syntax is the YAML syntax error, if any.
	Syntax *schema.Error
}

// Repo is a parsed tree. It implements kinds.Repo.
type Repo struct {
	reg   kinds.Registry
	fsys  fs.FS
	files map[string]*File
	paths []string
	docs  []*kinds.Doc

	once     sync.Once
	problems []kinds.Problem

	hashOnce sync.Once
	hash     string
}

var _ kinds.Repo = (*Repo)(nil)

// skipDir reports directories that are never part of the state.
func skipDir(name string) bool {
	return name == ".git" || name == "node_modules"
}

// skipFile reports files that are never part of the state: the .git file
// of a linked worktree and the marker of a tree export (change service).
func skipFile(rel string) bool {
	return rel == ".git" || rel == ".pk-export" || path.Base(rel) == ".DS_Store"
}

// Load reads and parses all files below dir.
func Load(dir string, reg kinds.Registry) (*Repo, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if skipFile(filepath.ToSlash(rel)) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return FromFiles(files, reg), nil
}

// FromFiles builds a repo from file contents (path to content).
func FromFiles(files map[string][]byte, reg kinds.Registry) *Repo {
	r := &Repo{reg: reg, files: map[string]*File{}}
	mfs := fstest.MapFS{}
	for p, b := range files {
		p = path.Clean(p)
		mfs[p] = &fstest.MapFile{Data: b, Mode: 0o644}
		f := ParseFile(p, b)
		r.files[p] = f
		r.paths = append(r.paths, p)
	}
	sort.Strings(r.paths)
	for _, p := range r.paths {
		r.docs = append(r.docs, r.files[p].Docs...)
	}
	r.fsys = mfs
	return r
}

// WithFile returns a copy of the repo with one file replaced (content nil
// deletes it). It validates unsaved editor content against the tree.
func (r *Repo) WithFile(p string, content []byte) *Repo {
	files := make(map[string][]byte, len(r.files)+1)
	for k, f := range r.files {
		files[k] = f.Content
	}
	if content == nil {
		delete(files, path.Clean(p))
	} else {
		files[path.Clean(p)] = content
	}
	return FromFiles(files, r.reg)
}

// Registry returns the kind registry.
func (r *Repo) Registry() kinds.Registry { return r.reg }

// FS returns the file tree.
func (r *Repo) FS() fs.FS { return r.fsys }

// Paths returns all file paths, sorted.
func (r *Repo) Paths() []string { return r.paths }

// File returns a parsed file or nil.
func (r *Repo) File(p string) *File { return r.files[path.Clean(p)] }

// Docs returns all documents in path order.
func (r *Repo) Docs() []*kinds.Doc { return r.docs }

// ByKind returns the documents of one kind (case-insensitive).
func (r *Repo) ByKind(kind string) []*kinds.Doc {
	var out []*kinds.Doc
	for _, d := range r.docs {
		if strings.EqualFold(d.Kind, kind) {
			out = append(out, d)
		}
	}
	return out
}

// Find returns the document of a kind with a name. Names match exactly
// first, then case-insensitively (rack gc5a-6z finds GC5A-6Z).
func (r *Repo) Find(kind, name string) *kinds.Doc {
	var fold *kinds.Doc
	for _, d := range r.docs {
		if !strings.EqualFold(d.Kind, kind) {
			continue
		}
		if d.Name == name {
			return d
		}
		if fold == nil && strings.EqualFold(d.Name, name) {
			fold = d
		}
	}
	return fold
}

// KindOf returns the kind of a document.
func (r *Repo) KindOf(d *kinds.Doc) *kinds.Kind {
	if r.reg == nil || d == nil {
		return nil
	}
	k, ok := r.reg.Match(d.Path, d)
	if !ok {
		return nil
	}
	return k
}

// Language returns the editor language of a path.
func Language(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".yaml", ".yml":
		return "yaml"
	case ".md", ".markdown":
		return "markdown"
	case ".json":
		return "json"
	}
	return "text"
}

// ParseFile parses one file into documents.
func ParseFile(p string, content []byte) *File {
	f := &File{Path: p, Content: content, Language: Language(p)}
	switch f.Language {
	case "yaml", "json":
		nodes, serr := schema.ParseYAML(content)
		f.Syntax = serr
		for i, n := range nodes {
			if d := newDoc(p, i, n, 0); d != nil {
				f.Docs = append(f.Docs, d)
			}
		}
	case "markdown":
		fm, body, offset, ok := FrontMatter(content)
		if !ok {
			return f
		}
		nodes, serr := schema.ParseYAML(fm)
		if serr != nil {
			serr.Line += offset
			f.Syntax = serr
		}
		if len(nodes) > 0 {
			d := newDoc(p, 0, nodes[0], offset)
			if d == nil {
				d = &kinds.Doc{Path: p, Node: nodes[0], Offset: offset, Data: map[string]any{}}
			}
			if d.Kind == "" && strings.HasPrefix(p, "kb/") {
				d.APIVersion, d.Kind = "kb.pk.io/v1", "Article"
			}
			d.Name = p
			d.Line = offset + 1
			d.Body = body
			f.Docs = append(f.Docs, d)
		}
	}
	return f
}

// FrontMatter splits a Markdown file into front matter and body. offset is
// the number of lines before the front matter text (the --- line).
func FrontMatter(content []byte) (fm []byte, body string, offset int, ok bool) {
	lines := strings.SplitAfter(string(content), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return nil, string(content), 0, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			fm = []byte(strings.Join(lines[1:i], ""))
			body = strings.TrimLeft(strings.Join(lines[i+1:], ""), "\r\n")
			return fm, body, 1, true
		}
	}
	return nil, string(content), 0, false
}

func newDoc(p string, index int, n *yaml.Node, offset int) *kinds.Doc {
	c := schema.Content(n)
	if c == nil || c.Kind == 0 {
		return nil
	}
	data, _ := schema.ToValue(n).(map[string]any)
	d := &kinds.Doc{Path: p, Index: index, Line: c.Line + offset, Node: n, Offset: offset, Data: data}
	if data == nil {
		d.Data = map[string]any{}
		return d
	}
	d.APIVersion, _ = data["apiVersion"].(string)
	d.Kind, _ = data["kind"].(string)
	if md, ok := data["metadata"].(map[string]any); ok {
		d.Name, _ = md["name"].(string)
		d.Namespace, _ = md["namespace"].(string)
	}
	if d.Name == "" {
		// Flat kinds (WorkItem) use id.
		if id, ok := data["id"].(string); ok {
			d.Name = id
		} else if cn, ok := data["clusterName"].(string); ok {
			d.Name = cn
		}
	}
	return d
}

// ---------------------------------------------------------------- cache

// Loader loads trees and caches them by a fingerprint of the files (path,
// size and modification time), so unchanged trees parse once.
type Loader struct {
	reg kinds.Registry
	mu  sync.Mutex
	c   map[string]cached
}

type cached struct {
	fp   string
	repo *Repo
	at   time.Time
}

// NewLoader returns a Loader.
func NewLoader(reg kinds.Registry) *Loader {
	return &Loader{reg: reg, c: map[string]cached{}}
}

// Registry returns the kind registry.
func (l *Loader) Registry() kinds.Registry { return l.reg }

// Load returns the parsed tree at dir.
func (l *Loader) Load(dir string) (*Repo, error) {
	fp, err := Fingerprint(dir)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if c, ok := l.c[dir]; ok && c.fp == fp {
		l.mu.Unlock()
		return c.repo, nil
	}
	l.mu.Unlock()
	r, err := Load(dir, l.reg)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.c[dir] = cached{fp: fp, repo: r, at: time.Now()}
	if len(l.c) > 64 {
		// Drop the oldest entry.
		var oldest string
		var t time.Time
		for k, v := range l.c {
			if oldest == "" || v.at.Before(t) {
				oldest, t = k, v.at
			}
		}
		delete(l.c, oldest)
	}
	return r, nil
}

// Fingerprint hashes the paths, sizes and modification times of all files
// below dir.
func Fingerprint(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, _ := filepath.Rel(dir, p); skipFile(filepath.ToSlash(rel)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", p, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ContentHash hashes the file contents of the repo (a stable key across
// directories with the same tree). It is computed once per Repo.
func (r *Repo) ContentHash() string {
	r.hashOnce.Do(func() { r.hash = r.contentHash() })
	return r.hash
}

func (r *Repo) contentHash() string {
	h := sha256.New()
	for _, p := range r.paths {
		sum := sha256.Sum256(r.files[p].Content)
		fmt.Fprintf(h, "%s\x00%x\n", p, sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SameContent reports whether a path has the same content in both repos.
func SameContent(a, b *Repo, p string) bool {
	fa, fb := a.File(p), b.File(p)
	if fa == nil || fb == nil {
		return fa == nil && fb == nil
	}
	return bytes.Equal(fa.Content, fb.Content)
}
