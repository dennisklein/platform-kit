package llm

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dennisklein/platform-kit/internal/gitx"
)

// RepoDocs reads the documents of a tree (main): Markdown under kb/ (lab
// reports under kb/labs/ as kind lab) and YAML files. Change metadata
// (.pk/changes) is skipped. Contents pass through the redactor.
func RepoDocs(dir string, r Redactor) ([]Document, error) {
	var docs []Document
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".git") || rel == ".pk/changes" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		isMD := ext == ".md" && strings.HasPrefix(rel, "kb/")
		isYAML := ext == ".yaml" || ext == ".yml"
		if !isMD && !isYAML {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 512<<10 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(b, 0) >= 0 {
			return nil
		}
		text, red := string(b), 0
		if r != nil {
			text, red = r.Redact(text)
		}
		doc := Document{Kind: "file", Ref: rel, Path: rel, Title: rel, Text: text, Redactions: red}
		if isMD {
			doc.Kind = "kb"
			if strings.HasPrefix(rel, "kb/labs/") {
				doc.Kind = "lab"
			}
			if t := frontMatterTitle(text); t != "" {
				doc.Title = t
			}
		}
		docs = append(docs, doc)
		return nil
	})
	return docs, err
}

func frontMatterTitle(md string) string {
	if !strings.HasPrefix(md, "---") {
		for _, l := range strings.Split(md, "\n") {
			if strings.HasPrefix(l, "# ") {
				return strings.TrimSpace(l[2:])
			}
		}
		return ""
	}
	end := strings.Index(md[3:], "\n---")
	if end < 0 {
		return ""
	}
	for _, l := range strings.Split(md[3:3+end], "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "title:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// TrailerDocs makes one document per merge commit with a Pk-Change or
// Pk-Break-Glass trailer: subject, intent and the trailer block.
func TrailerDocs(entries []gitx.LogEntry, r Redactor) []Document {
	var docs []Document
	for _, e := range entries {
		var change, bg, mr string
		for _, t := range e.Trailers {
			switch t.Key {
			case "Pk-Change":
				change = t.Value
			case "Pk-Break-Glass":
				bg = t.Value
			case "Reviewed-in":
				mr = t.Value
			}
		}
		if change == "" && bg == "" {
			continue
		}
		title := e.Subject
		if change != "" {
			title = "trailer " + change + ": " + strings.TrimPrefix(e.Subject, "Merge change "+change+": ")
		}
		text := fmt.Sprintf("%s\n\n%s\n\nmerged %s by %s, commit %s", e.Subject, e.Body, e.CommitDate.Format("2006-01-02"), e.Committer, short7(e.SHA))
		red := 0
		if r != nil {
			text, red = r.Redact(text)
		}
		ref := change
		if ref == "" {
			ref = e.SHA
		}
		docs = append(docs, Document{Kind: "trailer", Ref: ref, Title: title, Text: text, Change: change, Path: mr, Redactions: red})
	}
	return docs
}

// MRDoc makes the document of one merge request discussion.
func MRDoc(number int, changeID, title, body string, comments []string, r Redactor) Document {
	var b strings.Builder
	fmt.Fprintf(&b, "MR !%d %s\n\n%s\n", number, title, body)
	for _, c := range comments {
		b.WriteString("\n" + c + "\n")
	}
	text, red := b.String(), 0
	if r != nil {
		text, red = r.Redact(text)
	}
	return Document{Kind: "mr", Ref: fmt.Sprintf("!%d", number), Title: fmt.Sprintf("MR !%d %s", number, title), Text: text, Change: changeID, Redactions: red}
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
