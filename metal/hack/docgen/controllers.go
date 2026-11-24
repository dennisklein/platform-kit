package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// controllersDoc lists the named controllers of the manager: each
// `Named("...")` call in the controller package. The site counts and lists them
// from this file, so a new controller shows up without a hand-written number.
type controllersDoc struct {
	Source      string       `json:"source"`
	Controllers []controller `json:"controllers"`
}

type controller struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

var namedCall = regexp.MustCompile(`\bNamed\("([a-z0-9-]+)"\)`)

func readControllers(cfg config) (controllersDoc, error) {
	doc := controllersDoc{Source: relPath(cfg.root, cfg.controllerDir), Controllers: []controller{}}
	files, err := filepath.Glob(filepath.Join(cfg.controllerDir, "*.go"))
	if err != nil {
		return doc, err
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return doc, err
		}
		for _, m := range namedCall.FindAllStringSubmatch(string(b), -1) {
			doc.Controllers = append(doc.Controllers, controller{Name: m[1], Source: relPath(cfg.root, f)})
		}
	}
	slices.SortFunc(doc.Controllers, func(a, b controller) int { return strings.Compare(a.Name, b.Name) })
	return doc, nil
}
