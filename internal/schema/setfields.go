package schema

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetFields sets fields of the first document by dotted path
// (spec.location.unit, drawers[1].slots[17].state). Existing scalars are
// replaced in the text, so comments, order and style stay. New fields or
// non-scalar values re-encode the file. A string value for an existing
// number or boolean field is converted.
func SetFields(content []byte, set map[string]any) ([]byte, error) {
	return SetFieldsWith(content, set, nil)
}

// SetFieldsWith is SetFields with the raw JSON Schema of the document (nil
// for none): new keys go to their place in the schema's property order and
// new objects get their keys in that order, so a port added from JSON reads
// "name, cable, peer, network" like the hand-written ones.
func SetFieldsWith(content []byte, set map[string]any, schemaRaw func(doc *yaml.Node) []byte) ([]byte, error) {
	if len(set) == 0 {
		return content, nil
	}
	docs, perr := ParseYAML(content)
	if perr != nil {
		return nil, fmt.Errorf("line %d: %s", perr.Line, perr.Message)
	}
	if len(docs) == 0 {
		docs = []*yaml.Node{{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}}
	}
	var ord *orderNode
	if schemaRaw != nil {
		ord = parseOrder(schemaRaw(docs[0]))
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	type splice struct {
		sp   tokenSpan
		text string
		loc  []string
		val  any
	}
	var splices []splice
	structural := false
	lines := splitLines(string(content))
	for _, p := range paths {
		loc, err := ParseFieldPath(p)
		if err != nil {
			return nil, err
		}
		val := set[p]
		loc = specRelative(docs[0], loc)
		v, _, found := Lookup(docs[0], loc)
		if found && v != nil && v.Kind == yaml.ScalarNode && isScalar(val) && len(lines) > 0 {
			val = coerce(val, v)
			text, err := scalarText(val)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if sp, ok := spanOf(lines, v); ok && !sp.block {
				splices = append(splices, splice{sp, text, loc, val})
				continue
			}
		}
		structural = true
		if err := setNode(docs[0], loc, val, ord); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if structural {
		// The file is re-encoded: put the scalar values into the tree too.
		for _, s := range splices {
			if err := setNode(docs[0], s.loc, s.val, ord); err != nil {
				return nil, fmt.Errorf("%s: %w", FieldPath(s.loc), err)
			}
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		for _, d := range docs {
			if err := enc.Encode(d); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	sort.Slice(splices, func(i, j int) bool {
		if splices[i].sp.line != splices[j].sp.line {
			return splices[i].sp.line > splices[j].sp.line
		}
		return splices[i].sp.col > splices[j].sp.col
	})
	out := lines
	for _, s := range splices {
		out = replaceSpanLines(out, s.sp, []string{s.text})
	}
	return []byte(strings.Join(out, "")), nil
}

// specRelative lets a path name a field below spec without the prefix
// (drawers[1].slots[17].state for spec.drawers[...]) when the top-level key
// does not exist but spec has it.
func specRelative(doc *yaml.Node, loc []string) []string {
	if len(loc) == 0 || loc[0] == "spec" {
		return loc
	}
	if _, _, found := Lookup(doc, loc[:1]); found {
		return loc
	}
	spec := append([]string{"spec"}, loc...)
	if _, _, found := Lookup(doc, spec[:2]); found {
		return spec
	}
	return loc
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

// coerce converts a string value to the type of the existing node.
func coerce(val any, n *yaml.Node) any {
	if f, ok := val.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1e15 {
		val = int64(f)
	}
	s, ok := val.(string)
	if !ok {
		return val
	}
	switch n.ShortTag() {
	case "!!int":
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	case "!!float":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	case "!!bool":
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	return val
}

// scalarText encodes a scalar as YAML text (quoted when needed).
func scalarText(v any) (string, error) {
	if v == nil {
		return "null", nil
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(b), "\n")
	if strings.Contains(s, "\n") {
		// Multi-line strings: use a double-quoted scalar on one line.
		return strconv.Quote(fmt.Sprint(v)), nil
	}
	return s, nil
}

// setNode sets the value at loc, creating mappings on the way.
func setNode(doc *yaml.Node, loc []string, val any, ord *orderNode) error {
	var vn yaml.Node
	if err := vn.Encode(normalize(val)); err != nil {
		return err
	}
	n := Content(doc)
	for i, tok := range loc {
		last := i == len(loc)-1
		parent := ord
		ord = ord.child(n.Kind, tok)
		if last {
			sortKeys(&vn, ord)
		}
		switch n.Kind {
		case yaml.MappingNode:
			var next *yaml.Node
			for j := 0; j+1 < len(n.Content); j += 2 {
				if n.Content[j].Value == tok {
					next = n.Content[j+1]
					if last {
						// Keep the quoting style, except the quotes of an empty value.
						if next.Kind == yaml.ScalarNode && vn.Kind == yaml.ScalarNode && next.Value != "" {
							vn.Style = next.Style &^ (yaml.LiteralStyle | yaml.FoldedStyle)
						}
						n.Content[j+1] = &vn
						return nil
					}
					break
				}
			}
			if next == nil {
				k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tok}
				if last {
					insertKey(n, k, &vn, parent)
					return nil
				}
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				if isIndex(loc[i+1]) {
					next = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				}
				insertKey(n, k, next, parent)
			}
			n = next
		case yaml.SequenceNode:
			idx, err := strconv.Atoi(tok)
			if err != nil {
				return fmt.Errorf("%q is not an index", tok)
			}
			if idx == len(n.Content) {
				var add *yaml.Node
				switch {
				case last:
					add = &vn
				case isIndex(loc[i+1]):
					add = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				default:
					add = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				}
				n.Content = append(n.Content, add)
				if last {
					return nil
				}
				n = add
				continue
			}
			if idx < 0 || idx > len(n.Content) {
				return fmt.Errorf("index %d out of range (%d items)", idx, len(n.Content))
			}
			if last {
				n.Content[idx] = &vn
				return nil
			}
			n = n.Content[idx]
		default:
			return fmt.Errorf("%s is not an object or a list", FieldPath(loc[:i]))
		}
	}
	return nil
}

// orderNode is the property order of a schema node.
type orderNode struct {
	keys  map[string]int
	props map[string]*orderNode
	items *orderNode
	addl  *orderNode
}

// parseOrder reads the property order of a raw JSON Schema (JSON keeps the
// order of the Go struct fields it was generated from).
func parseOrder(raw []byte) *orderNode {
	if len(raw) == 0 {
		return nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return orderOf(Content(&doc))
}

func orderOf(n *yaml.Node) *orderNode {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	o := &orderNode{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "properties":
			if v.Kind != yaml.MappingNode {
				continue
			}
			o.keys, o.props = map[string]int{}, map[string]*orderNode{}
			for j := 0; j+1 < len(v.Content); j += 2 {
				name := v.Content[j].Value
				o.keys[name] = j / 2
				o.props[name] = orderOf(v.Content[j+1])
			}
		case "items":
			o.items = orderOf(v)
		case "additionalProperties":
			o.addl = orderOf(v)
		}
	}
	return o
}

// child is the order node below a mapping key or a sequence index.
func (o *orderNode) child(kind yaml.Kind, tok string) *orderNode {
	if o == nil {
		return nil
	}
	if kind == yaml.SequenceNode {
		return o.items
	}
	if c, ok := o.props[tok]; ok {
		return c
	}
	return o.addl
}

// rank is the place of a key in the schema order; unknown keys go last.
func (o *orderNode) rank(key string) int {
	if o != nil {
		if r, ok := o.keys[key]; ok {
			return r
		}
	}
	return 1 << 20
}

// insertKey adds key: value to a mapping before the first key that comes
// later in the schema order (at the end without a schema).
func insertKey(m, k, v *yaml.Node, o *orderNode) {
	at := len(m.Content)
	if o != nil && o.keys != nil {
		if r := o.rank(k.Value); r < 1<<20 {
			for j := 0; j+1 < len(m.Content); j += 2 {
				if o.rank(m.Content[j].Value) > r {
					at = j
					break
				}
			}
		}
	}
	m.Content = append(m.Content[:at], append([]*yaml.Node{k, v}, m.Content[at:]...)...)
}

// sortKeys orders the keys of new mappings (recursively) like the schema.
func sortKeys(n *yaml.Node, o *orderNode) {
	if n == nil || o == nil {
		return
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			sortKeys(c, o.items)
		}
	case yaml.MappingNode:
		type pair struct{ k, v *yaml.Node }
		ps := make([]pair, 0, len(n.Content)/2)
		for j := 0; j+1 < len(n.Content); j += 2 {
			ps = append(ps, pair{n.Content[j], n.Content[j+1]})
		}
		sort.SliceStable(ps, func(a, b int) bool { return o.rank(ps[a].k.Value) < o.rank(ps[b].k.Value) })
		n.Content = n.Content[:0]
		for _, p := range ps {
			n.Content = append(n.Content, p.k, p.v)
			sortKeys(p.v, o.child(yaml.MappingNode, p.k.Value))
		}
	}
}

// normalize turns integral float64 values (from JSON) into integers.
func normalize(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return int64(x)
		}
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[k] = normalize(vv)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, vv := range x {
			s[i] = normalize(vv)
		}
		return s
	}
	return v
}
