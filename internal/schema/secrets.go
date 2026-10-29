package schema

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// MaskText replaces secret values in the API, the UI and LLM prompts.
const MaskText = "••••••••"

// Secret kinds.
const (
	SecretSOPS   = "sops"
	SecretSchema = "schema"
)

// Secret is a secret field of a document.
type Secret struct {
	// Doc is the index of the document in the file.
	Doc int
	// Field is the dotted path, for example spec.values.hubble.ui.auth.token.
	Field    string
	Location []string
	// Kind is sops (an ENC[...] value) or schema (x-pk-secret).
	Kind string
	Line int
	Col  int
	// Key names the key that encrypts a SOPS value.
	Key string
}

// SchemaLookup returns the parsed JSON Schema (as a map) for a document, or
// nil. It lets Mask find x-pk-secret fields.
type SchemaLookup func(doc *yaml.Node) map[string]any

// IsSOPS reports whether a value is SOPS ciphertext.
func IsSOPS(v string) bool { return strings.HasPrefix(v, "ENC[") }

// IsMasked reports whether a value consists only of mask characters.
func IsMasked(v string) bool {
	t := strings.TrimSpace(strings.NewReplacer("•", "", "\n", "", " ", "").Replace(v))
	return t == "" && strings.Contains(v, "•")
}

// FindSecrets returns the secret fields of a parsed YAML document.
func FindSecrets(doc *yaml.Node, index int, sch map[string]any) []Secret {
	key := sopsKey(doc)
	var out []Secret
	Walk(doc, func(loc []string, _ *yaml.Node, v *yaml.Node) {
		kind := ""
		switch {
		case IsSOPS(v.Value):
			kind = SecretSOPS
		case sch != nil && isSecretField(sch, loc):
			kind = SecretSchema
		default:
			return
		}
		s := Secret{Doc: index, Field: FieldPath(loc), Location: loc, Kind: kind, Line: v.Line, Col: v.Column}
		if kind == SecretSOPS {
			s.Key = key
		}
		out = append(out, s)
	})
	return out
}

// sopsKey describes the key of a SOPS file from its metadata block.
func sopsKey(doc *yaml.Node) string {
	n, _, ok := Lookup(doc, []string{"sops"})
	if !ok || n == nil {
		return ""
	}
	if age, _, ok := Lookup(doc, []string{"sops", "age", "0", "recipient"}); ok && age.Value != "" {
		r := age.Value
		if len(r) > 16 {
			r = r[:8] + "…" + r[len(r)-4:]
		}
		return "admin key, local (age " + r + ")"
	}
	return "admin key, local"
}

// isSecretField reports whether the schema marks the field at loc with
// x-pk-secret.
func isSecretField(sch map[string]any, loc []string) bool {
	n := SubSchema(sch, loc)
	if n == nil {
		return false
	}
	b, _ := n[XSecret].(bool)
	return b
}

// SubSchema walks a JSON Schema along an instance location (properties,
// items, additionalProperties). It returns nil when the schema has no node.
func SubSchema(sch map[string]any, loc []string) map[string]any {
	n := sch
	for _, tok := range loc {
		if n == nil {
			return nil
		}
		if props, ok := n["properties"].(map[string]any); ok {
			if p, ok := props[tok].(map[string]any); ok {
				n = p
				continue
			}
		}
		if items, ok := n["items"].(map[string]any); ok && isIndex(tok) {
			n = items
			continue
		}
		if ap, ok := n["additionalProperties"].(map[string]any); ok {
			n = ap
			continue
		}
		return nil
	}
	return n
}

func isIndex(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Mask replaces the values of secret fields in content with MaskText. Line
// numbers stay the same. lookup may be nil (then only SOPS values are found).
func Mask(content []byte, lookup SchemaLookup) ([]byte, []Secret) {
	docs, _ := ParseYAML(content)
	if len(docs) == 0 {
		return content, []Secret{}
	}
	lines := splitLines(string(content))
	secrets := []Secret{}
	var spans []tokenSpan
	for i, d := range docs {
		var sch map[string]any
		if lookup != nil {
			sch = lookup(d)
		}
		for _, s := range FindSecrets(d, i, sch) {
			v, _, ok := Lookup(d, s.Location)
			if !ok || v == nil {
				continue
			}
			sp, ok := spanOf(lines, v)
			if !ok {
				continue
			}
			secrets = append(secrets, s)
			spans = append(spans, sp)
		}
	}
	if len(spans) == 0 {
		return content, secrets
	}
	out := lines
	// Replace from the end so earlier spans keep their positions.
	for i := len(spans) - 1; i >= 0; i-- {
		out = replaceSpan(out, spans[i], maskFor(out, spans[i]))
	}
	return []byte(strings.Join(out, "")), secrets
}

// Unmask puts the stored values back into fields of content whose value is
// masked. The field is found by document index and field path. It fails when
// a masked field has no stored value.
func Unmask(content, stored []byte) ([]byte, error) {
	if !strings.Contains(string(content), "•") {
		return content, nil
	}
	newDocs, perr := ParseYAML(content)
	if perr != nil {
		return nil, fmt.Errorf("line %d: %s", perr.Line, perr.Message)
	}
	oldDocs, _ := ParseYAML(stored)
	newLines := splitLines(string(content))
	oldLines := splitLines(string(stored))
	type repl struct {
		at   tokenSpan
		text []string
	}
	var repls []repl
	for i, d := range newDocs {
		var missing error
		Walk(d, func(loc []string, _ *yaml.Node, v *yaml.Node) {
			if missing != nil || !IsMasked(v.Value) {
				return
			}
			field := FieldPath(loc)
			if i >= len(oldDocs) {
				missing = fmt.Errorf("%s: secret value is masked, but there is no stored value", field)
				return
			}
			ov, _, ok := Lookup(oldDocs[i], loc)
			if !ok || ov == nil || ov.Kind != yaml.ScalarNode {
				missing = fmt.Errorf("%s: secret value is masked, but there is no stored value", field)
				return
			}
			nsp, ok1 := spanOf(newLines, v)
			osp, ok2 := spanOf(oldLines, ov)
			if !ok1 || !ok2 {
				missing = fmt.Errorf("%s: cannot restore the secret value", field)
				return
			}
			repls = append(repls, repl{at: nsp, text: spanText(oldLines, osp)})
		})
		if missing != nil {
			return nil, missing
		}
	}
	out := newLines
	for i := len(repls) - 1; i >= 0; i-- {
		out = replaceSpanLines(out, repls[i].at, repls[i].text)
	}
	return []byte(strings.Join(out, "")), nil
}

// tokenSpan is a range of a scalar in the source: from (line, col) to
// (endLine, endCol), 0-based lines and byte offsets, end exclusive.
type tokenSpan struct {
	line, col       int
	endLine, endCol int
	block           bool
}

// splitLines splits keeping the line endings.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// byteCol converts a 1-based rune column into a byte offset in line.
func byteCol(line string, col int) int {
	off := 0
	for i := 1; i < col && off < len(line); i++ {
		_, n := utf8.DecodeRuneInString(line[off:])
		off += n
	}
	return off
}

func trimEOL(s string) string { return strings.TrimRight(s, "\r\n") }

// spanOf finds the source range of a scalar node.
func spanOf(lines []string, v *yaml.Node) (tokenSpan, bool) {
	li := v.Line - 1
	if li < 0 || li >= len(lines) {
		return tokenSpan{}, false
	}
	line := trimEOL(lines[li])
	c := byteCol(line, v.Column)
	if c > len(line) {
		return tokenSpan{}, false
	}
	rest := line[c:]
	switch v.Style {
	case yaml.LiteralStyle, yaml.FoldedStyle:
		// Header up to the end of the line, then the more-indented lines.
		end := li
		indent := -1
		for j := li + 1; j < len(lines); j++ {
			t := trimEOL(lines[j])
			if strings.TrimSpace(t) == "" {
				continue
			}
			ind := len(t) - len(strings.TrimLeft(t, " "))
			if indent < 0 {
				indent = ind
			}
			if ind < indent || indent <= indentOf(line) {
				break
			}
			end = j
		}
		return tokenSpan{line: li, col: c, endLine: end, endCol: len(trimEOL(lines[end])), block: true}, true
	case yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle:
		q := rest[0]
		for i := 1; i < len(rest); i++ {
			if q == '"' && rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == q {
				if q == '\'' && i+1 < len(rest) && rest[i+1] == '\'' {
					i++
					continue
				}
				return tokenSpan{line: li, col: c, endLine: li, endCol: c + i + 1}, true
			}
		}
		return tokenSpan{line: li, col: c, endLine: li, endCol: len(line)}, true
	default:
		if strings.HasPrefix(rest, v.Value) {
			return tokenSpan{line: li, col: c, endLine: li, endCol: c + len(v.Value)}, true
		}
		// Multi-line plain scalar: up to the end of the line.
		return tokenSpan{line: li, col: c, endLine: li, endCol: len(line)}, true
	}
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// maskFor returns the replacement lines for a span: one mask for a scalar,
// and for a block scalar a header plus one masked line per content line, so
// line numbers stay the same.
func maskFor(lines []string, sp tokenSpan) []string {
	if !sp.block {
		return []string{MaskText}
	}
	out := []string{"|"}
	for j := sp.line + 1; j <= sp.endLine; j++ {
		t := trimEOL(lines[j])
		out = append(out, strings.Repeat(" ", indentOf(t))+MaskText)
	}
	return out
}

func spanText(lines []string, sp tokenSpan) []string {
	if sp.line == sp.endLine {
		return []string{trimEOL(lines[sp.line])[sp.col:sp.endCol]}
	}
	out := []string{trimEOL(lines[sp.line])[sp.col:]}
	for j := sp.line + 1; j < sp.endLine; j++ {
		out = append(out, trimEOL(lines[j]))
	}
	out = append(out, trimEOL(lines[sp.endLine])[:sp.endCol])
	return out
}

func replaceSpan(lines []string, sp tokenSpan, repl []string) []string {
	return replaceSpanLines(lines, sp, repl)
}

// replaceSpanLines replaces the span with text lines (the first goes at the
// span start, the last keeps the rest of the end line).
func replaceSpanLines(lines []string, sp tokenSpan, text []string) []string {
	first := lines[sp.line]
	last := lines[sp.endLine]
	eol := last[len(trimEOL(last)):]
	prefix := trimEOL(first)[:sp.col]
	suffix := trimEOL(last)[sp.endCol:]
	var mid []string
	for i, t := range text {
		l := t
		if i == 0 {
			l = prefix + l
		}
		if i == len(text)-1 {
			l = l + suffix + eol
		} else {
			l += "\n"
		}
		mid = append(mid, l)
	}
	out := append([]string{}, lines[:sp.line]...)
	out = append(out, mid...)
	return append(out, lines[sp.endLine+1:]...)
}
