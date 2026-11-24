package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// writeAll writes each file (path relative to out) as indented JSON and removes
// the JSON files in the directories it writes to that it did not write: a kind
// or a command that is gone must not leave its page data behind.
func writeAll(out string, files map[string]any) error {
	dirs := map[string]bool{out: true}
	for rel := range files {
		dirs[filepath.Join(out, filepath.Dir(rel))] = true
	}
	for dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			rel, err := filepath.Rel(out, filepath.Join(dir, e.Name()))
			if err != nil {
				return err
			}
			if _, keep := files[rel]; !keep {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	slices.Sort(names)
	for _, rel := range names {
		data, err := marshal(files[rel])
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err := os.WriteFile(filepath.Join(out, rel), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// marshal is json.MarshalIndent without HTML escaping: descriptions hold
// "<name>" and "->", and the site reads the file as data, not as HTML.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// relPath is path relative to the repository root, with forward slashes: the
// site turns it into a link to the file on GitHub.
func relPath(root, path string) string {
	absRoot, err1 := filepath.Abs(root)
	absPath, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func lowerKind(kind string) string { return strings.ToLower(kind) }
