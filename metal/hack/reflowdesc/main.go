// reflowdesc post-processes controller-gen CRD output: comment text is copied into
// description fields verbatim, source line breaks included, and `kubectl explain`
// re-wraps that pre-broken text into ragged orphan lines. This joins the lines inside
// each paragraph (blank lines and bullet items survive) so descriptions flow at
// whatever width the reader's terminal has. Runs as part of `mise run generate`.
//
// usage: go run ./hack/reflowdesc <crd.yaml>...
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v4"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: reflowdesc <crd.yaml>...")
		os.Exit(2)
	}
	for _, path := range os.Args[1:] {
		if err := reflowFile(path); err != nil {
			fmt.Fprintf(os.Stderr, "reflowdesc: %s: %v\n", path, err)
			os.Exit(1)
		}
	}
}

func reflowFile(path string) error {
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// One CRD per file is controller-gen's layout, but decode a stream to be safe.
	dec := yaml.NewDecoder(bytes.NewReader(in))
	var out bytes.Buffer
	// No line width: the point of the reflow is one logical line per paragraph,
	// and an emitter that folds at 80 columns would put the breaks back.
	enc, err := yaml.NewDumper(&out, yaml.WithV3Defaults(), yaml.WithIndent(2), yaml.WithLineWidth(-1))
	if err != nil {
		return err
	}
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return err
		}
		walk(&doc)
		// controller-gen opens every document with an explicit `---`; the encoder
		// only separates, so emit the first one ourselves.
		out.WriteString("---\n")
		if err := enc.Dump(&doc); err != nil {
			return err
		}
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func walk(n *yaml.Node) {
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "description" && v.Kind == yaml.ScalarNode && v.Tag == "!!str" {
				v.Value = reflow(v.Value)
				v.Style = 0
			}
			walk(v)
		}
		return
	}
	for _, c := range n.Content {
		walk(c)
	}
}

// reflow joins hard-wrapped lines into logical ones: paragraphs (blank-line
// separated) are preserved, bullet items keep their own line with continuations
// folded in, everything else inside a paragraph becomes one line.
func reflow(s string) string {
	paras := strings.Split(s, "\n\n")
	for i, p := range paras {
		var joined []string
		cur := ""
		for ln := range strings.SplitSeq(p, "\n") {
			t := strings.TrimSpace(ln)
			switch {
			case t == "":
				continue
			case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
				if cur != "" {
					joined = append(joined, cur)
				}
				cur = t
			case cur == "":
				cur = t
			default:
				cur += " " + t
			}
		}
		if cur != "" {
			joined = append(joined, cur)
		}
		paras[i] = strings.Join(joined, "\n")
	}
	return strings.Join(paras, "\n\n")
}
