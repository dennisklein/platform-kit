package schema

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	xmessage "golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

// XMessage replaces the validation message of a schema node, for example
// "must be a duration, for example 5m".
const XMessage = "x-pk-message"

// Error is one validation error with its position in the YAML file.
type Error struct {
	// Field is the dotted path, for example spec.ports[2].peer.port.
	Field string
	// Location is the instance location (object keys and array indexes).
	Location []string
	Line     int
	Col      int
	Message  string
	// Keyword is the failed JSON Schema keyword (required, pattern, ...) or
	// syntax.
	Keyword string
}

// Validator validates documents against one compiled schema.
type Validator struct {
	sch *jsonschema.Schema
	raw any
}

var (
	compileMu    sync.Mutex
	compileCache = map[string]*Validator{}
)

// Compile compiles a JSON Schema. Validators are cached by schema content.
func Compile(raw json.RawMessage) (*Validator, error) {
	key := string(raw)
	compileMu.Lock()
	defer compileMu.Unlock()
	if v, ok := compileCache[key]; ok {
		return v, nil
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(key))
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	// The x-pk-* keywords are annotations; drop $id so many schemas share
	// one compiler URL space without clashes.
	if m, ok := doc.(map[string]any); ok {
		delete(m, "$id")
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	url := fmt.Sprintf("mem:///schema-%d.json", len(compileCache))
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	v := &Validator{sch: sch, raw: doc}
	compileCache[key] = v
	return v, nil
}

// Validate validates a parsed YAML document (a DocumentNode or its content)
// and returns errors with lines, sorted by position.
func (v *Validator) Validate(doc *yaml.Node) []Error {
	err := v.sch.Validate(toJSON(ToValue(doc)))
	if err == nil {
		return nil
	}
	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Error{{Line: 1, Col: 1, Message: err.Error(), Keyword: "schema"}}
	}
	var out []Error
	seen := map[string]bool{}
	for _, leaf := range leaves(verr) {
		e := v.convert(doc, leaf)
		k := e.Field + "\x00" + e.Message
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// toJSON converts int64 values into json.Number-free values the validator
// accepts (it accepts Go integer and float types directly).
func toJSON(v any) any { return v }

// leaves returns the innermost errors. For oneOf and anyOf it keeps the
// branch error itself, because the branch errors confuse people.
func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	switch e.ErrorKind.(type) {
	case *kind.OneOf, *kind.AnyOf:
		return []*jsonschema.ValidationError{e}
	}
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func (v *Validator) convert(doc *yaml.Node, e *jsonschema.ValidationError) Error {
	loc := e.InstanceLocation
	out := Error{Location: loc, Field: FieldPath(loc), Keyword: keyword(e.ErrorKind)}
	node, key, _ := Lookup(doc, loc)
	pos := node
	if key != nil {
		pos = key
	}
	text := message(e.ErrorKind)
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		field := joinField(out.Field, k.Missing[0])
		if len(k.Missing) == 1 {
			text = fmt.Sprintf("%s is missing", field)
		} else {
			text = fmt.Sprintf("fields %s are missing", strings.Join(prefixAll(out.Field, k.Missing), ", "))
		}
		out.Field = field
		out.Message = text
	case *kind.AdditionalProperties:
		// Point at the first unknown key.
		if node != nil && node.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == k.Properties[0] {
					pos = node.Content[i]
					break
				}
			}
		}
		field := joinField(out.Field, k.Properties[0])
		out.Field = field
		if len(k.Properties) == 1 {
			out.Message = fmt.Sprintf("%s is not a known field", field)
		} else {
			out.Message = fmt.Sprintf("%s are not known fields", strings.Join(prefixAll(FieldPath(loc), k.Properties), ", "))
		}
	default:
		if custom := v.customMessage(e.SchemaURL); custom != "" {
			text = custom
		}
		if out.Field == "" {
			out.Message = text
		} else {
			out.Message = out.Field + ": " + text
		}
	}
	if pos != nil {
		out.Line, out.Col = pos.Line, pos.Column
	}
	if out.Line == 0 {
		out.Line, out.Col = 1, 1
	}
	return out
}

func joinField(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func prefixAll(parent string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = joinField(parent, n)
	}
	return out
}

// customMessage returns x-pk-message of the schema node at the URL fragment.
func (v *Validator) customMessage(schemaURL string) string {
	_, frag, ok := strings.Cut(schemaURL, "#")
	if !ok {
		return ""
	}
	n := pointer(v.raw, frag)
	// The keyword location ends at the failed keyword's schema; also try the
	// parent (for items and nested keywords).
	for i := 0; i < 2 && n != nil; i++ {
		if m, ok := n.(map[string]any); ok {
			if s, ok := m[XMessage].(string); ok {
				return s
			}
		}
		frag = frag[:max(strings.LastIndex(frag, "/"), 0)]
		n = pointer(v.raw, frag)
	}
	return ""
}

func pointer(doc any, frag string) any {
	if frag == "" || frag == "/" {
		return doc
	}
	n := doc
	for _, tok := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := n.(type) {
		case map[string]any:
			n = c[tok]
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			n = c[i]
		default:
			return nil
		}
	}
	return n
}

func keyword(k jsonschema.ErrorKind) string {
	if k == nil {
		return ""
	}
	p := k.KeywordPath()
	if len(p) == 0 {
		return ""
	}
	return p[0]
}

func rat(r *big.Rat) string {
	if r == nil {
		return "?"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// message words validation errors for people.
func message(k jsonschema.ErrorKind) string {
	switch k := k.(type) {
	case *kind.Type:
		return fmt.Sprintf("must be %s, not %s", article(strings.Join(k.Want, " or ")), k.Got)
	case *kind.Enum:
		var want []string
		for _, w := range k.Want {
			want = append(want, fmt.Sprint(w))
		}
		if len(want) == 1 {
			return "must be " + want[0]
		}
		return "must be one of " + strings.Join(want, ", ")
	case *kind.Const:
		return fmt.Sprintf("must be %v", k.Want)
	case *kind.Pattern:
		if k.Got == "" {
			return "is empty"
		}
		return fmt.Sprintf("%q does not match %s", k.Got, k.Want)
	case *kind.Format:
		return fmt.Sprintf("must be a valid %s", k.Want)
	case *kind.Minimum:
		return "must be at least " + rat(k.Want)
	case *kind.Maximum:
		return "must be at most " + rat(k.Want)
	case *kind.ExclusiveMinimum:
		return "must be more than " + rat(k.Want)
	case *kind.ExclusiveMaximum:
		return "must be less than " + rat(k.Want)
	case *kind.MinLength:
		if k.Want == 1 {
			return "must not be empty"
		}
		return fmt.Sprintf("must have at least %d characters", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("must have at most %d characters", k.Want)
	case *kind.MinItems:
		if k.Want == 1 {
			return "must have at least one item"
		}
		return fmt.Sprintf("must have at least %d items", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("must have at most %d items", k.Want)
	case *kind.UniqueItems:
		return fmt.Sprintf("items %d and %d are the same", k.Duplicates[0], k.Duplicates[1])
	case *kind.OneOf, *kind.AnyOf:
		return "does not match any allowed form"
	case *kind.FalseSchema:
		return "is not allowed"
	}
	if k == nil {
		return "is not valid"
	}
	return k.LocalizedString(printer)
}

var printer = xmessage.NewPrinter(language.English)

func article(t string) string {
	if t == "" {
		return "a value"
	}
	if strings.HasPrefix(t, "integer") || strings.HasPrefix(t, "object") || strings.HasPrefix(t, "array") {
		return "an " + t
	}
	return "a " + t
}
