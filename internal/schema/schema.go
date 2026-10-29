// Package schema generates JSON Schemas with platform-kit extensions from Go
// types, and validates YAML documents against schemas with positions.
//
// Go struct fields use `json` tags for names, `jsonschema` tags (invopop) for
// standard keywords (enum, pattern, minimum, default, description, title) and
// a `pk` tag for the extensions:
//
//	pk:"truth=intent,owner=asset-db,ref=Node,secret,widget=rack-picker,label=Location,group=Intent,order=3"
//
// becomes
//
//	x-pk-truth: intent
//	x-pk-owner: asset-db
//	x-pk-ref: {kind: Node}
//	x-pk-secret: true
//	x-pk-ui: {widget: rack-picker, label: Location, group: Intent, order: 3}
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

// Extension keys.
const (
	XTruth  = "x-pk-truth"
	XOwner  = "x-pk-owner"
	XSecret = "x-pk-secret"
	XRef    = "x-pk-ref"
	XUI     = "x-pk-ui"
	XKind   = "x-pk-kind"
)

// Options for Generate.
type Options struct {
	// Title and Description of the root schema.
	Title       string
	Description string
	// APIVersion and Kind are set as x-pk-kind on the root.
	APIVersion string
	Kind       string
}

// Generate reflects v (a struct value or pointer) into a JSON Schema with
// x-pk extensions. Nested structs are inlined (no $defs).
func Generate(v any, o Options) (*jsonschema.Schema, error) {
	r := &jsonschema.Reflector{
		DoNotReference:            true,
		ExpandedStruct:            true,
		AllowAdditionalProperties: false,
	}
	s := r.Reflect(v)
	if s == nil {
		return nil, fmt.Errorf("schema: cannot reflect %T", v)
	}
	s.ID = ""
	if o.Title != "" {
		s.Title = o.Title
	}
	if o.Description != "" {
		s.Description = o.Description
	}
	if o.Kind != "" {
		setExtra(s, XKind, map[string]any{"apiVersion": o.APIVersion, "kind": o.Kind})
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if err := annotate(t, s); err != nil {
		return nil, err
	}
	return s, nil
}

// JSON is Generate marshalled with indentation.
func JSON(v any, o Options) (json.RawMessage, error) {
	s, err := Generate(v, o)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(s, "", "  ")
}

var timeType = reflect.TypeOf(time.Time{})

// annotate walks the Go type and the schema together and applies pk tags.
func annotate(t reflect.Type, s *jsonschema.Schema) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if s == nil {
		return nil
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return annotate(t.Elem(), s.Items)
	case reflect.Map:
		return annotate(t.Elem(), s.AdditionalProperties)
	case reflect.Struct:
		if t == timeType || s.Properties == nil {
			return nil
		}
	default:
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, inline := jsonName(f)
		if name == "-" {
			continue
		}
		if inline {
			if err := annotate(f.Type, s); err != nil {
				return err
			}
			continue
		}
		prop, ok := s.Properties.Get(name)
		if !ok || prop == nil {
			continue
		}
		if tag, ok := f.Tag.Lookup("pk"); ok {
			if err := applyTag(prop, tag); err != nil {
				return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
			}
		}
		if err := annotate(f.Type, prop); err != nil {
			return err
		}
	}
	return nil
}

func jsonName(f reflect.StructField) (name string, inline bool) {
	tag := f.Tag.Get("json")
	n, _, _ := strings.Cut(tag, ",")
	if n == "" && f.Anonymous {
		return "", true
	}
	if n == "" {
		return f.Name, false
	}
	return n, false
}

// Tag is a parsed pk tag.
type Tag struct {
	Truth  string
	Owner  string
	Ref    string
	Secret bool
	UI     map[string]any
}

// ParseTag parses a pk struct tag.
func ParseTag(tag string) (Tag, error) {
	var t Tag
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "truth":
			switch v {
			case "intent", "generated", "asserted", "derived", "imperative-only":
			default:
				return t, fmt.Errorf("unknown truth class %q", v)
			}
			t.Truth = v
		case "owner":
			t.Owner = v
		case "ref":
			t.Ref = v
		case "secret":
			t.Secret = true
		case "widget", "label", "group", "unit":
			if t.UI == nil {
				t.UI = map[string]any{}
			}
			t.UI[k] = v
		case "order":
			n, err := strconv.Atoi(v)
			if err != nil {
				return t, fmt.Errorf("order: %w", err)
			}
			if t.UI == nil {
				t.UI = map[string]any{}
			}
			t.UI["order"] = n
		default:
			return t, fmt.Errorf("unknown pk tag key %q", k)
		}
	}
	return t, nil
}

func applyTag(s *jsonschema.Schema, tag string) error {
	t, err := ParseTag(tag)
	if err != nil {
		return err
	}
	if t.Truth != "" {
		setExtra(s, XTruth, t.Truth)
	}
	if t.Owner != "" {
		setExtra(s, XOwner, t.Owner)
	}
	if t.Ref != "" {
		setExtra(s, XRef, map[string]any{"kind": t.Ref})
	}
	if t.Secret {
		setExtra(s, XSecret, true)
	}
	if t.UI != nil {
		setExtra(s, XUI, t.UI)
	}
	return nil
}

func setExtra(s *jsonschema.Schema, k string, v any) {
	if s.Extras == nil {
		s.Extras = map[string]any{}
	}
	s.Extras[k] = v
}
