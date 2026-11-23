package schema

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const durationSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["apiVersion", "kind"],
  "properties": {
    "apiVersion": {"type": "string"},
    "kind": {"type": "string"},
    "spec": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "interval": {"type": "string", "pattern": "^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$", "x-pk-message": "must be a duration, for example 5m"},
        "replicas": {"type": "integer", "minimum": 1},
        "role": {"type": "string", "enum": ["a", "b"]},
        "items": {"type": "array", "items": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "port": {"type": "integer", "maximum": 48}}}}
      }
    }
  }
}`

func TestValidateLines(t *testing.T) {
	v, err := Compile([]byte(durationSchema))
	if err != nil {
		t.Fatal(err)
	}
	doc := `apiVersion: x/v1
kind: Thing
metadata:
  name: x
spec:
  replicas: 0
  interval: 5 minutes
  role: c
  extra: 1
  items:
    - {name: a, port: 49}
    - port: 1
`
	docs, perr := ParseYAML([]byte(doc))
	if perr != nil {
		t.Fatal(perr)
	}
	errs := v.Validate(docs[0])
	got := map[string]Error{}
	for _, e := range errs {
		got[e.Field] = e
	}
	check := func(field string, line int, msg string) {
		t.Helper()
		e, ok := got[field]
		if !ok {
			t.Errorf("no error for %s; got %+v", field, errs)
			return
		}
		if e.Line != line || !strings.Contains(e.Message, msg) {
			t.Errorf("%s: line %d %q, want line %d containing %q", field, e.Line, e.Message, line, msg)
		}
	}
	check("spec.replicas", 6, "spec.replicas: must be at least 1")
	check("spec.interval", 7, "spec.interval: must be a duration, for example 5m")
	check("spec.role", 8, "must be one of a, b")
	check("spec.extra", 9, "spec.extra is not a known field")
	check("spec.items[0].port", 11, "must be at most 48")
	check("spec.items[1].name", 12, "spec.items[1].name is missing")
}

func TestSyntaxError(t *testing.T) {
	_, perr := ParseYAML([]byte("a: 1\nb: [1, 2\nc: 3\n"))
	if perr == nil || perr.Line == 0 {
		t.Fatalf("want syntax error with line, got %+v", perr)
	}
}

func TestMaskUnmaskSOPS(t *testing.T) {
	stored, err := os.ReadFile("../../seed/state/clusters/defiant/infra/cilium/helmrelease.yaml")
	if err != nil {
		t.Fatal(err)
	}
	masked, secrets := Mask(stored, nil)
	if strings.Contains(string(masked), "ENC[") {
		t.Fatalf("ENC value left:\n%s", masked)
	}
	if len(secrets) != 2 { // the token and sops.mac
		t.Fatalf("secrets: %+v", secrets)
	}
	if secrets[0].Field != "spec.values.hubble.ui.auth.token" || secrets[0].Kind != SecretSOPS || secrets[0].Line != 27 {
		t.Errorf("first secret: %+v", secrets[0])
	}
	if strings.Count(string(masked), "\n") != strings.Count(string(stored), "\n") {
		t.Error("masking changed the line count")
	}
	// The user edits the interval and saves the masked content.
	edited := strings.Replace(string(masked), "interval: 5m", "interval: 10m", 1)
	restored, err := Unmask([]byte(edited), stored)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(stored), "interval: 5m", "interval: 10m", 1)
	if string(restored) != want {
		t.Fatalf("restored differs:\n%s", restored)
	}
	// A new masked field has no stored value.
	bad := strings.Replace(edited, "kubeProxyReplacement: true", "kubeProxyReplacement: true\n    newToken: ••••••••", 1)
	if _, err := Unmask([]byte(bad), stored); err == nil {
		t.Error("want error for a masked field without stored value")
	}
}

func TestMaskSchemaSecretAndBlock(t *testing.T) {
	sch := map[string]any{"properties": map[string]any{"spec": map[string]any{"properties": map[string]any{
		"password": map[string]any{"type": "string", XSecret: true},
		"key":      map[string]any{"type": "string", XSecret: true},
	}}}}
	in := "spec:\n  password: \"hunter2\" # comment\n  key: |\n    line1\n    line2\n  other: x\n"
	masked, secrets := Mask([]byte(in), func(*yaml.Node) map[string]any { return sch })
	if len(secrets) != 2 {
		t.Fatalf("secrets %+v", secrets)
	}
	if strings.Contains(string(masked), "hunter2") || strings.Contains(string(masked), "line1") {
		t.Fatalf("not masked:\n%s", masked)
	}
	if !strings.Contains(string(masked), "# comment") || !strings.Contains(string(masked), "other: x") {
		t.Fatalf("lost text:\n%s", masked)
	}
	back, err := Unmask(masked, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != in {
		t.Fatalf("roundtrip:\n%q\n%q", back, in)
	}
}

func TestSetFields(t *testing.T) {
	in := "# head\napiVersion: v1\nspec:\n  location: {rack: GC5A-6Z, unit: 6, size: 2} # here\n  drawers:\n    - id: 0\n      slots:\n        - {slot: 0, state: ok}\n        - {slot: 1, state: ok}\n"
	out, err := SetFields([]byte(in), map[string]any{
		"spec.location.unit":             "8",
		"spec.drawers[0].slots[1].state": "replace",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(strings.Replace(in, "unit: 6", "unit: 8", 1), "{slot: 1, state: ok}", "{slot: 1, state: replace}", 1)
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	out, err = SetFields([]byte(in), map[string]any{"spec.model": "SYS 220", "metadata.name": "cn-035"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "model: SYS 220") || !strings.Contains(s, "name: cn-035") || !strings.Contains(s, "# head") {
		t.Fatalf("structural set:\n%s", s)
	}
	// Scalar and structural sets in one call: the scalar must survive the re-encode.
	out, err = SetFields([]byte("spec:\n  cluster: \"\"\n  role: worker\n"), map[string]any{
		"spec.cluster": "defiant",
		"spec.ports":   []any{map[string]any{"name": "p1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, "cluster: defiant") || !strings.Contains(s, "name: p1") {
		t.Fatalf("mixed set:\n%s", s)
	}
	// A path below spec may leave out the prefix (the runbook's pk edit line).
	jb := "kind: Jbod\nspec:\n  drawers:\n    - id: 0\n      slots:\n        - {slot: 0, state: ok}\n"
	out, err = SetFields([]byte(jb), map[string]any{"drawers[0].slots[0].state": "replace"})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(jb, "state: ok", "state: replace", 1); string(out) != want {
		t.Fatalf("spec-relative set:\n%s", out)
	}
	if _, err := ParseFieldPath("a..b"); err == nil {
		t.Error("want error")
	}
	loc, _ := ParseFieldPath("drawers[1].slots[17].state")
	if FieldPath(loc) != "drawers[1].slots[17].state" {
		t.Errorf("roundtrip %v", loc)
	}
}
