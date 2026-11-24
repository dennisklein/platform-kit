package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

// The glossary is hand-written (docs/src/data/glossary.yaml), but the site
// links pages to it by id and by form, so a broken entry breaks links on
// pages nobody edited. docgen checks its shape on every `mise run generate`;
// whether each `see` target exists is the site build's check, as only the
// site knows its pages.

type glossary struct {
	Domains map[string]glossaryDomain `yaml:"domains"`
	Terms   []glossaryTerm            `yaml:"terms"`
}

type glossaryDomain struct {
	Name string `yaml:"name"`
	Icon string `yaml:"icon"`
}

type glossaryTerm struct {
	ID      string            `yaml:"id"`
	Term    string            `yaml:"term"`
	Abbr    string            `yaml:"abbr"`
	Kind    string            `yaml:"kind"`
	Domain  string            `yaml:"domain"`
	Forms   []string          `yaml:"forms"`
	Guards  map[string]string `yaml:"guards"`
	Scope   string            `yaml:"scope"`
	See     string            `yaml:"see"`
	Related []string          `yaml:"related"`
	Def     string            `yaml:"def"`
}

var (
	glossaryID    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	glossarySee   = regexp.MustCompile(`^(kind|ctrl|act|cli|lab|ref|concept|flow|howto|rb|tut):[A-Za-z0-9/_.#-]+$`)
	glossaryKinds = []string{"noun", "verb", "api-kind", "abbr", "tool", "phase", "annotation"}
	glossaryGuard = []string{"not_before", "not_after", "only_after"}
)

// maxDefWords is the STE limit for a descriptive sentence; the Vale style
// checks the wording, this keeps the file from drifting past it unnoticed.
const maxDefWords = 25

// checkGlossary returns every problem in the file, not only the first, so a
// writer fixes them in one pass.
func checkGlossary(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var g glossary
	if err := dec.Decode(&g); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var errs []error
	bad := func(id, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: term %q: %s", path, id, fmt.Sprintf(format, args...)))
	}
	if len(g.Domains) == 0 || len(g.Terms) == 0 {
		return fmt.Errorf("%s: needs domains and terms", path)
	}
	ids := map[string]bool{}
	forms := map[string]string{}
	for _, t := range g.Terms {
		if !glossaryID.MatchString(t.ID) {
			bad(t.ID, "id must be lower case words joined with '-'")
		}
		if ids[t.ID] {
			bad(t.ID, "id is used twice")
		}
		ids[t.ID] = true
		if t.Term == "" || t.Def == "" || t.See == "" {
			bad(t.ID, "term, def and see are required")
		}
		if _, ok := g.Domains[t.Domain]; !ok {
			bad(t.ID, "unknown domain %q", t.Domain)
		}
		if !slices.Contains(glossaryKinds, t.Kind) {
			bad(t.ID, "kind %q is not one of %s", t.Kind, strings.Join(glossaryKinds, ", "))
		}
		if t.Kind == "abbr" && t.Abbr == "" {
			bad(t.ID, "an abbr needs its expansion in abbr")
		}
		if !glossarySee.MatchString(t.See) {
			bad(t.ID, "see %q is not a category link (kind:, ctrl:, act:, cli:, lab:, concept:, flow:, howto:, rb:, tut:)", t.See)
		}
		if t.Scope != "" && t.Scope != "page" {
			bad(t.ID, "scope %q is not page (leave it out to link once in each section)", t.Scope)
		}
		for k, v := range t.Guards {
			if !slices.Contains(glossaryGuard, k) {
				bad(t.ID, "unknown guard %q", k)
			}
			if _, err := regexp.Compile(v); err != nil {
				bad(t.ID, "guard %s: %v", k, err)
			}
		}
		for _, f := range t.Forms {
			if other, dup := forms[f]; dup {
				bad(t.ID, "form %q is also a form of %q", f, other)
			}
			forms[f] = t.ID
		}
		for _, s := range sentences(t.Def) {
			if n := len(strings.Fields(s)); n > maxDefWords {
				bad(t.ID, "a sentence of the definition has %d words (STE: at most %d): %q", n, maxDefWords, s)
			}
		}
	}
	for _, t := range g.Terms {
		for _, r := range t.Related {
			if !ids[r] {
				bad(t.ID, "related %q is no term id", r)
			}
		}
	}
	return errors.Join(errs...)
}

var sentenceEnd = regexp.MustCompile(`[.!?]\s+`)

func sentences(text string) []string {
	var out []string
	for s := range strings.SplitSeq(sentenceEnd.ReplaceAllString(strings.TrimSpace(text), "$0\x00"), "\x00") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
