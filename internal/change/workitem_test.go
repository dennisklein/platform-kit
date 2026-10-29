package change

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkWorkItem(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"flow empty": {"id: w-114\nchanges: []\nkb: []\n", "id: w-114\nchanges: [c-abc234]\nkb: []\n"},
		"flow one":   {"changes: [c-31d0aa]\n", "changes: [c-31d0aa, c-abc234]\n"},
		"flow dup":   {"changes: [c-abc234]\n", ""},
		"block":      {"changes:\n  - c-31d0aa\nkb: []\n", "changes:\n  - c-31d0aa\n  - c-abc234\nkb: []\n"},
		"block none": {"changes:\nkb: []\n", "changes:\n  - c-abc234\nkb: []\n"},
		"missing":    {"id: w-1\n", "id: w-1\nchanges: [c-abc234]\n"},
	} {
		dir := t.TempDir()
		writeT(t, dir, "work/items/w-114.yaml", tc.in)
		p, ok := linkWorkItem(dir, "w-114", "c-abc234")
		b, _ := os.ReadFile(filepath.Join(dir, "work/items/w-114.yaml"))
		if tc.want == "" {
			if ok || string(b) != tc.in {
				t.Errorf("%s: changed to %q", name, b)
			}
			continue
		}
		if !ok || p != "work/items/w-114.yaml" || string(b) != tc.want {
			t.Errorf("%s: %v %q, want %q", name, ok, b, tc.want)
		}
	}
	if _, ok := linkWorkItem(t.TempDir(), "../../etc/passwd", "c-abc234"); ok {
		t.Error("path traversal")
	}
	if _, ok := linkWorkItem(t.TempDir(), "w-999", "c-abc234"); ok {
		t.Error("missing work item file")
	}
}

// A change made for a work item adds itself to the work item file in its
// opening commit.
func TestCreateLinksWorkItem(t *testing.T) {
	h := newHarnessWith(t, map[string]string{"work/items/w-114.yaml": "id: w-114\nchanges: []\n"}, false)
	ch, err := h.svc.Create(h.dennis, CreateRequest{Title: "Add cn-035", Intent: "x", WorkItems: []string{"w-114"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(ch.Worktree, "work/items/w-114.yaml"))
	if string(b) != "id: w-114\nchanges: ["+ch.ID+"]\n" {
		t.Fatalf("work item %q", b)
	}
	if ch.Dirty || len(ch.ChangedFiles) != 2 {
		t.Fatalf("dirty %v files %+v", ch.Dirty, ch.ChangedFiles)
	}
}
