package schema

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseYAML parses all documents of a YAML file. It returns the document
// nodes (kind DocumentNode) and the first syntax error with its line.
func ParseYAML(content []byte) ([]*yaml.Node, *Error) {
	dec := yaml.NewDecoder(strings.NewReader(string(content)))
	var docs []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return docs, syntaxError(err)
		}
		docs = append(docs, &n)
	}
	return docs, nil
}

// syntaxError converts a yaml.v3 error ("yaml: line 7: ...") into an Error.
func syntaxError(err error) *Error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: ")
	line := 0
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if n, tail, ok := strings.Cut(rest, ":"); ok {
			if v, e := strconv.Atoi(n); e == nil {
				line = v
				msg = strings.TrimSpace(tail)
			}
		}
	}
	return &Error{Line: line, Col: 1, Message: "YAML syntax: " + msg, Keyword: "syntax"}
}

// Content returns the root content node of a document node (or n itself).
func Content(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// ToValue converts a YAML node into a JSON-like value (map[string]any,
// []any, string, int64, float64, bool, nil). Timestamps stay strings.
func ToValue(n *yaml.Node) any {
	n = resolve(Content(n))
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := resolve(n.Content[i])
			if k.Tag == "!!merge" {
				if mv, ok := ToValue(n.Content[i+1]).(map[string]any); ok {
					for kk, vv := range mv {
						if _, set := m[kk]; !set {
							m[kk] = vv
						}
					}
				}
				continue
			}
			m[k.Value] = ToValue(n.Content[i+1])
		}
		return m
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			s = append(s, ToValue(c))
		}
		return s
	case yaml.ScalarNode:
		return scalarValue(n)
	}
	return nil
}

func scalarValue(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err == nil {
			return b
		}
	case "!!int":
		var i int64
		if err := n.Decode(&i); err == nil {
			return i
		}
		var f float64
		if err := n.Decode(&f); err == nil {
			return f
		}
	case "!!float":
		var f float64
		if err := n.Decode(&f); err == nil {
			return f
		}
	}
	return n.Value
}

// Lookup finds the node at an instance location (object keys and array
// indexes). It returns the value node and, for mapping entries, the key node.
// When the full path does not exist it returns the deepest node found.
func Lookup(doc *yaml.Node, loc []string) (value, key *yaml.Node, found bool) {
	n := resolve(Content(doc))
	for _, tok := range loc {
		if n == nil {
			return nil, key, false
		}
		switch n.Kind {
		case yaml.MappingNode:
			var next *yaml.Node
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == tok {
					key, next = n.Content[i], resolve(n.Content[i+1])
					break
				}
			}
			if next == nil {
				return n, key, false
			}
			n = next
		case yaml.SequenceNode:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(n.Content) {
				return n, key, false
			}
			key = nil
			n = resolve(n.Content[i])
		default:
			return n, key, false
		}
	}
	return n, key, true
}

// FieldPath formats an instance location as a dotted path with indexes:
// spec.ports[2].peer.port.
func FieldPath(loc []string) string {
	var b strings.Builder
	for _, tok := range loc {
		if _, err := strconv.Atoi(tok); err == nil {
			fmt.Fprintf(&b, "[%s]", tok)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(tok)
	}
	return b.String()
}

// ParseFieldPath splits a dotted path with indexes (spec.ports[2].peer.port
// or spec.ports.2.peer.port) into tokens.
func ParseFieldPath(p string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(p, ".") {
		if part == "" {
			return nil, fmt.Errorf("bad field path %q", p)
		}
		name, rest, hasIdx := strings.Cut(part, "[")
		if name != "" {
			out = append(out, name)
		}
		for hasIdx {
			var idx string
			idx, rest, _ = strings.Cut(rest, "]")
			if _, err := strconv.Atoi(idx); err != nil {
				return nil, fmt.Errorf("bad index %q in %q", idx, p)
			}
			out = append(out, idx)
			if rest == "" {
				break
			}
			if !strings.HasPrefix(rest, "[") {
				return nil, fmt.Errorf("bad field path %q", p)
			}
			rest = rest[1:]
		}
	}
	return out, nil
}

// Walk calls fn for each scalar node with its instance location.
func Walk(doc *yaml.Node, fn func(loc []string, key, value *yaml.Node)) {
	var walk func(n, key *yaml.Node, loc []string)
	walk = func(n, key *yaml.Node, loc []string) {
		n = resolve(n)
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i+1], n.Content[i], append(append([]string(nil), loc...), n.Content[i].Value))
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(c, nil, append(append([]string(nil), loc...), strconv.Itoa(i)))
			}
		case yaml.ScalarNode:
			fn(loc, key, n)
		}
	}
	walk(Content(doc), nil, nil)
}
