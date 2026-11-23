package bench

import (
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/kinds/repo"
)

func TestHistEncodeKeepsOrderAndStyle(t *testing.T) {
	src := `apiVersion: inventory.pk.io/v1alpha1
kind: Node
metadata:
  name: cn-003
spec:
  cluster: defiant
  rack: GC5A-6Y
  unit: 30
  model: X
  ports:
    - {name: p2, peer: {switch: data-a, port: 37}}
`
	pf := repo.ParseFile("control/inventory/cn-003.yaml", []byte(src))
	if len(pf.Docs) != 1 {
		t.Fatalf("docs: %d", len(pf.Docs))
	}
	d := pf.Docs[0]
	data := d.Data
	data["apiVersion"] = "inventory.pk.io/v1beta1"
	spec := data["spec"].(map[string]any)
	delete(spec, "rack")
	delete(spec, "unit")
	spec["location"] = map[string]any{"rack": "GC5A-6Y", "unit": 30}
	out, err := histEncode(data, d.Node)
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: cn-003
spec:
  cluster: defiant
  location:
    rack: GC5A-6Y
    unit: 30
  model: X
  ports:
    - {name: p2, peer: {switch: data-a, port: 37}}
`
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

func TestHistNodeRefs(t *testing.T) {
	got := histNodeRefs(map[string]any{"spec": map[string]any{"cluster": "defiant", "location": map[string]any{"rack": "GC5A-6Z"}}})
	if strings.Join(got, ",") != "defiant,GC5A-6Z" {
		t.Errorf("v1beta1: %v", got)
	}
	got = histNodeRefs(map[string]any{"spec": map[string]any{"rack": "GC5A-6Y"}})
	if strings.Join(got, ",") != "GC5A-6Y" {
		t.Errorf("v1alpha1: %v", got)
	}
}

func TestFinalNewline(t *testing.T) {
	cases := []struct {
		next, stored string
		existed      bool
		want         string
	}{
		{"a: 1", "a: 0\n", true, "a: 1\n"},
		{"a: 1", "", false, "a: 1\n"},
		{"a: 1", "a: 0", true, "a: 1"},
		{"a: 1\n", "a: 0", true, "a: 1\n"},
		{"", "a: 0\n", true, ""},
	}
	for _, c := range cases {
		if got := string(finalNewline([]byte(c.next), []byte(c.stored), c.existed)); got != c.want {
			t.Errorf("finalNewline(%q, %q) = %q, want %q", c.next, c.stored, got, c.want)
		}
	}
}
