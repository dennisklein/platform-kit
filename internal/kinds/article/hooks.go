package article

import (
	"fmt"
	"regexp"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

var changeID = regexp.MustCompile(`^c-[a-z0-9]{6}$`)

func validate(r kinds.Repo) []kinds.Problem {
	var out []kinds.Problem
	for _, d := range r.ByKind(Name) {
		for i, e := range kindutil.Strings(d.Data, "entities") {
			if kindutil.FindEntity(r, e) == nil {
				out = append(out, kindutil.RefProblem(d, fmt.Sprintf("entities[%d]", i), kinds.SevError, "unknown entity %s", e))
			}
		}
		for i, c := range kindutil.Strings(d.Data, "changes") {
			if !changeID.MatchString(c) {
				out = append(out, kindutil.RefProblem(d, fmt.Sprintf("changes[%d]", i), kinds.SevError, "%s is not a change id (c- and 6 characters)", c))
			}
		}
	}
	return out
}

func describe(old, new *kinds.Doc) []kinds.SemanticLine {
	switch {
	case old == nil && new != nil:
		t := kindutil.Str(new.Data, "type")
		if t == "" {
			t = "article"
		}
		return []kinds.SemanticLine{{Subject: new.Path, Text: fmt.Sprintf("new %s: %s", t, kindutil.Str(new.Data, "title")), Op: kinds.OpAdd, Path: new.Path, Line: 1, Kind: Name}}
	case new == nil && old != nil:
		return []kinds.SemanticLine{{Subject: old.Path, Text: "article removed", Op: kinds.OpDelete, Path: old.Path, Kind: Name}}
	case old != nil && new != nil:
		var out []kinds.SemanticLine
		for _, t := range kindutil.FieldLines(old.Data, new.Data, 3) {
			out = append(out, kinds.SemanticLine{Subject: new.Path, Text: t, Op: kinds.OpChange, Path: new.Path, Line: 1, Kind: Name})
		}
		if old.Body != new.Body {
			out = append(out, kinds.SemanticLine{Subject: new.Path, Text: "text changed", Op: kinds.OpChange, Path: new.Path, Kind: Name})
		}
		return out
	}
	return nil
}

// Skeleton is a new article.
func Skeleton(name string) ([]byte, error) {
	return []byte(fmt.Sprintf("---\ntitle: %s\ntype: runbook\nentities: []\nchanges: []\n---\n# %s\n\n", name, name)), nil
}
