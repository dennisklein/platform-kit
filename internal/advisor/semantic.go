package advisor

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

// describeFiles returns the semantic diff of the uncommitted files (HEAD to
// worktree) with the kind hooks. The impact of the change compares with the
// base of the change, so it also names what earlier commits did.
func describeFiles(reg kinds.Registry, files []fileChange) []api.SemanticLine {
	var out []api.SemanticLine
	for _, f := range files {
		var bdocs, hdocs []*kinds.Doc
		if f.Old != "" {
			bdocs = repo.ParseFile(f.Path, []byte(f.Old)).Docs
		}
		if f.New != "" {
			hdocs = repo.ParseFile(f.Path, []byte(f.New)).Docs
		}
		if len(bdocs) == 0 && len(hdocs) == 0 {
			continue
		}
		key := func(d *kinds.Doc) string {
			if d.Name == "" {
				return fmt.Sprintf("%s#%d", d.Kind, d.Index)
			}
			return d.Key()
		}
		old := map[string]*kinds.Doc{}
		for _, d := range bdocs {
			old[key(d)] = d
		}
		var lines []kinds.SemanticLine
		for _, d := range hdocs {
			o := old[key(d)]
			delete(old, key(d))
			lines = append(lines, describeDoc(reg, f.Path, o, d)...)
		}
		for _, d := range bdocs {
			if _, ok := old[key(d)]; ok {
				lines = append(lines, describeDoc(reg, f.Path, d, nil)...)
			}
		}
		for _, l := range lines {
			p := l.Path
			if p == "" {
				p = f.Path
			}
			out = append(out, api.SemanticLine{Subject: l.Subject, Text: l.Text, Op: api.Op(l.Op), Path: p, Line: l.Line, Kind: l.Kind})
		}
	}
	return out
}

func describeDoc(reg kinds.Registry, p string, old, new *kinds.Doc) []kinds.SemanticLine {
	if old != nil && new != nil && sameData(old.Data, new.Data) && old.Body == new.Body {
		return nil
	}
	d := new
	if d == nil {
		d = old
	}
	k, ok := reg.Match(p, d)
	if ok && k.Describe != nil {
		return k.Describe(old, new)
	}
	name := "document"
	if ok {
		name = k.Name
	}
	subject := p
	if d.Kind != "" && d.Name != "" {
		subject = d.Kind + "/" + d.Name
	}
	lines := kindutil.Describer(name, strings.ToLower(name))(old, new)
	for i := range lines {
		lines[i].Subject = subject
	}
	return lines
}

func sameData(a, b map[string]any) bool {
	ab, err1 := yaml.Marshal(a)
	bb, err2 := yaml.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}
