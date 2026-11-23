package seedfs

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
)

type changeFile struct {
	ID      string `json:"id"`
	Merged  bool   `json:"merged"`
	Commits []struct {
		Message string   `json:"message"`
		Files   []string `json:"files"`
	} `json:"commits"`
}

func readTree(t *testing.T, fsys fs.FS, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		out[strings.TrimPrefix(p, root+"/")] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type step struct {
	dir           string
	meta          changeFile
	before, after map[string]string
}

func steps(t *testing.T) []step {
	dirs, err := fs.ReadDir(files, "changes")
	if err != nil {
		t.Fatal(err)
	}
	var out []step
	for _, d := range dirs {
		dir := path.Join("changes", d.Name())
		var s step
		s.dir = dir
		b, err := fs.ReadFile(files, path.Join(dir, "change.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &s.meta); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		s.before, s.after = map[string]string{}, map[string]string{}
		if _, err := fs.Stat(files, dir+"/before"); err == nil {
			s.before = readTree(t, files, dir+"/before")
		}
		if _, err := fs.Stat(files, dir+"/after"); err == nil {
			s.after = readTree(t, files, dir+"/after")
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

func touched(s step) []string {
	set := map[string]bool{}
	for p := range s.before {
		set[p] = true
	}
	for p := range s.after {
		set[p] = true
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// TestHistoryReplays checks the documented build: undo the merged changes
// from the final state to get the initial tree, then apply them forward.
func TestHistoryReplays(t *testing.T) {
	final := readTree(t, files, "state")
	all := steps(t)
	tree := map[string]string{}
	for k, v := range final {
		tree[k] = v
	}
	for i := len(all) - 1; i >= 0; i-- {
		s := all[i]
		if !s.meta.Merged {
			continue
		}
		for _, p := range touched(s) {
			if tree[p] != s.after[p] {
				t.Fatalf("%s: %s does not match after/ when undoing", s.meta.ID, p)
			}
			if v, ok := s.before[p]; ok {
				tree[p] = v
			} else {
				delete(tree, p)
			}
		}
	}
	for _, s := range all {
		for _, p := range touched(s) {
			cur, ok := tree[p]
			if want, had := s.before[p]; had != ok || cur != want {
				t.Fatalf("%s: %s does not match before/", s.meta.ID, p)
			}
		}
		listed := map[string]bool{}
		for _, c := range s.meta.Commits {
			for _, f := range c.Files {
				listed[f] = true
			}
		}
		if len(s.meta.Commits) > 0 && len(listed) != len(touched(s)) {
			t.Fatalf("%s: commits list %d files, overlays touch %d", s.meta.ID, len(listed), len(touched(s)))
		}
		if !s.meta.Merged {
			continue // open changes branch off the final main
		}
		for _, p := range touched(s) {
			if v, ok := s.after[p]; ok {
				tree[p] = v
			} else {
				delete(tree, p)
			}
		}
	}
	if len(tree) != len(final) {
		t.Fatalf("replay has %d files, state has %d", len(tree), len(final))
	}
	for p, v := range final {
		if tree[p] != v {
			t.Fatalf("replay differs from state at %s", p)
		}
	}
}

// TestOwnKindsValid validates every own-kind document of the state and the
// overlays against the generated schemas.
func TestOwnKindsValid(t *testing.T) {
	reg := all.Registry()
	trees := map[string]map[string]string{"state": readTree(t, files, "state")}
	for _, s := range steps(t) {
		trees[s.dir+"/before"] = s.before
		trees[s.dir+"/after"] = s.after
	}
	compiled := map[string]*jsonschema.Schema{}
	n := 0
	for name, tree := range trees {
		for p, content := range tree {
			var doc map[string]any
			raw := content
			if strings.HasSuffix(p, ".md") {
				parts := strings.SplitN(content, "---\n", 3)
				if len(parts) < 3 {
					continue
				}
				raw = parts[1]
			} else if !strings.HasSuffix(p, ".yaml") || strings.Contains(content, "\n---\n") {
				continue
			}
			if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
				t.Fatalf("%s/%s: %v", name, p, err)
			}
			d := &kinds.Doc{Path: p, Data: doc}
			d.APIVersion, _ = doc["apiVersion"].(string)
			d.Kind, _ = doc["kind"].(string)
			k, ok := reg.Match(p, d)
			if !ok || !k.Own {
				continue
			}
			version := k.Version
			if d.APIVersion != "" {
				version = d.Version()
			}
			key := k.Name + "/" + version
			sch, ok := compiled[key]
			if !ok {
				b, err := k.Schema(version)
				if err != nil {
					t.Fatalf("%s: %v", key, err)
				}
				var sdoc any
				if err := json.Unmarshal(b, &sdoc); err != nil {
					t.Fatal(err)
				}
				c := jsonschema.NewCompiler()
				if err := c.AddResource(key+".json", sdoc); err != nil {
					t.Fatal(err)
				}
				sch, err = c.Compile(key + ".json")
				if err != nil {
					t.Fatalf("%s: %v", key, err)
				}
				compiled[key] = sch
			}
			jb, _ := json.Marshal(doc)
			inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(jb))
			if err := sch.Validate(inst); err != nil {
				t.Errorf("%s/%s (%s): %v", name, p, key, err)
			}
			n++
		}
	}
	if n < 60 {
		t.Fatalf("validated only %d documents", n)
	}
}
