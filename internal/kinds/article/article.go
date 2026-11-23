// Package article is the kind Article (kb.pk.io/v1): Markdown files under kb/
// with YAML front matter. Entity and change links are validated like references.
package article

import (
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

const (
	Group = "kb.pk.io"
	Name  = "Article"
)

// Type of an article.
type Type string

const (
	Runbook   Type = "runbook"
	Design    Type = "design"
	Reference Type = "reference"
	Incident  Type = "incident"
	LabReport Type = "lab-report"
)

// FrontMatter is the YAML front matter of an article.
type FrontMatter struct {
	Title    string   `json:"title" yaml:"title" jsonschema:"title=Title,minLength=3" pk:"truth=intent,label=Title,order=1"`
	Type     Type     `json:"type" yaml:"type" jsonschema:"title=Type,enum=runbook,enum=design,enum=reference,enum=incident,enum=lab-report" pk:"truth=intent,widget=select,label=Type,order=2"`
	Entities []string `json:"entities,omitempty" yaml:"entities,omitempty" jsonschema:"title=Entities" pk:"truth=intent,ref=Entity,label=Entities,order=3"`
	Changes  []string `json:"changes,omitempty" yaml:"changes,omitempty" jsonschema:"title=Changes" pk:"truth=intent,ref=Change,label=Changes,order=4"`
	Tags     []string `json:"tags,omitempty" yaml:"tags,omitempty" pk:"truth=intent,label=Tags,order=5"`
	// Lab reports only.
	Run    string `json:"run,omitempty" yaml:"run,omitempty" jsonschema:"title=Lab run" pk:"truth=generated,label=Lab run,order=6"`
	Result string `json:"result,omitempty" yaml:"result,omitempty" jsonschema:"enum=pass,enum=fail" pk:"truth=generated,label=Result,order=7"`
	SHA    string `json:"sha,omitempty" yaml:"sha,omitempty" pk:"truth=generated,label=Tested sha,order=8"`
}

var versions = []kindutil.Version{{Name: "v1", Type: FrontMatter{}}}

// Kind returns the Article kind. Articles have no apiVersion in the file; they
// match by path.
func Kind() kinds.Kind {
	return kinds.Kind{
		Name:      Name,
		Group:     Group,
		Version:   "v1",
		Versions:  kindutil.Names(versions...),
		Own:       true,
		Paths:     []string{"kb/**/*.md"},
		NewPath:   func(name string) string { return "kb/" + name + ".md" },
		Truth:     kinds.Intent,
		ApplyPath: kinds.ApplyNone,
		Views:     []string{"article"},
		Match: func(p string, doc *kinds.Doc) bool {
			return len(p) > 6 && p[:3] == "kb/" && p[len(p)-3:] == ".md"
		},
		Schema:   kindutil.SchemaFunc(Group, Name, "Front matter of a knowledge base article.", versions...),
		Validate: validate,
		Describe: describe,
		Skeleton: Skeleton,
	}
}
