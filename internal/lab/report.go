package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ReportPath is the report article of a run in the change.
func ReportPath(changeID string, number int) string {
	return fmt.Sprintf("kb/labs/%s-run-%d.md", changeID, number)
}

// Trailer is the Pk-Lab-Run trailer value of a run.
func (r *Run) Trailer() string {
	s := r.ID + " " + string(r.Result)
	if r.Report != "" {
		s += " " + r.Report
	}
	return s
}

// Count sets ChecksPassed, ChecksTotal and Failures from the steps.
func (r *Run) Count() {
	r.ChecksPassed, r.ChecksTotal = 0, 0
	r.Failures = nil
	seen := map[string]bool{}
	for _, s := range r.Steps {
		for _, c := range s.Checks {
			r.ChecksTotal++
			if c.OK {
				r.ChecksPassed++
				continue
			}
			if !seen[c.Detail] {
				seen[c.Detail] = true
				r.Failures = append(r.Failures, c.Detail)
			}
		}
	}
}

// SummaryText is "12 of 12 checks pass" plus the first failure.
func (r *Run) SummaryText() string {
	s := fmt.Sprintf("%d of %d checks pass", r.ChecksPassed, r.ChecksTotal)
	switch {
	case r.Result == ResultCancelled:
		return "cancelled, " + s
	case r.ChecksTotal == 0 && r.Result == ResultPass:
		return "no checks"
	case len(r.Failures) > 0:
		s += " · " + r.Failures[0]
		if len(r.Failures) > 1 {
			s += fmt.Sprintf(" (+%d more)", len(r.Failures)-1)
		}
	}
	return s
}

// ReportInfo is what the report needs beyond the run.
type ReportInfo struct {
	Title    string
	Branch   string
	BaseSHA  string
	Topology string // summary line
}

// Report renders the lab report article (Markdown with front matter, type
// lab-report).
func Report(r *Run, info ReportInfo) []byte {
	var b strings.Builder
	entities := reportEntities(r)
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: Lab report %s\n", r.ID)
	b.WriteString("type: lab-report\n")
	if len(entities) > 0 {
		fmt.Fprintf(&b, "entities: [%s]\n", strings.Join(entities, ", "))
	}
	if r.ChangeID != "" {
		fmt.Fprintf(&b, "changes: [%s]\n", r.ChangeID)
	}
	fmt.Fprintf(&b, "run: %s\n", r.ID)
	if r.Result == ResultPass || r.Result == ResultFail {
		fmt.Fprintf(&b, "result: %s\n", r.Result)
	}
	if r.SHA != "" {
		fmt.Fprintf(&b, "sha: %s\n", r.SHA)
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "# Lab report %s\n\n", r.ID)

	fresh := "twin snapshot"
	if r.Fresh {
		fresh = "fresh machines"
	}
	fmt.Fprintf(&b, "Result: **%s**. %s.\n\n", r.Result, sentence(r.SummaryText()))
	fmt.Fprintf(&b, "Driver %s, host %s, provisioning %s, %s, fidelity %s.", r.Driver, r.Host, r.Provision, fresh, r.Fidelity)
	if r.SimulatedMs > 0 {
		fmt.Fprintf(&b, " Lab time %s", Human(time.Duration(r.SimulatedMs)*time.Millisecond))
		if !r.FinishedAt.IsZero() {
			fmt.Fprintf(&b, " (%s on the sim driver)", Human(r.FinishedAt.Sub(r.StartedAt)))
		}
		b.WriteString(".")
	}
	b.WriteString("\n\n")

	b.WriteString("| | |\n| --- | --- |\n")
	if r.ChangeID != "" {
		title := r.ChangeID
		if info.Title != "" {
			title += " " + info.Title
		}
		fmt.Fprintf(&b, "| Change | %s |\n", mdCell(title))
	}
	if r.SHA != "" {
		ref := short(r.SHA)
		if info.Branch != "" {
			ref += " on " + info.Branch
		}
		fmt.Fprintf(&b, "| Tested commit | `%s` |\n", ref)
	}
	if info.BaseSHA != "" {
		fmt.Fprintf(&b, "| Base | main `%s` |\n", short(info.BaseSHA))
	}
	fmt.Fprintf(&b, "| Lab | %s, scenario %s |\n", r.Lab, r.Scenario)
	if info.Topology != "" {
		fmt.Fprintf(&b, "| Topology | %s |\n", mdCell(info.Topology))
	}
	if len(r.Nodes) > 0 {
		fmt.Fprintf(&b, "| Machines under test | %s |\n", strings.Join(r.Nodes, ", "))
	}
	if len(r.GatePaths) > 0 {
		state := "met"
		if r.Result != ResultPass {
			state = "not met"
		}
		fmt.Fprintf(&b, "| Lab gate | %s %s |\n", mdCell(strings.Join(r.GatePaths, ", ")), state)
	}
	fmt.Fprintf(&b, "| Trailer | `Pk-Lab-Run: %s` |\n\n", r.Trailer())

	b.WriteString("## Steps\n\n| Step | Detail | Result | Lab time |\n| --- | --- | --- | --- |\n")
	for _, s := range r.Steps {
		if s.Type == StepReport {
			continue
		}
		title := s.Title
		if s.Scenario {
			title = string(s.Type) + ": " + s.Title
			if s.Title == "checks" {
				title += " (" + s.Name + ")"
			}
		}
		lt := ""
		if s.SimulatedMs > 0 {
			lt = Human(time.Duration(s.SimulatedMs) * time.Millisecond)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", mdCell(title), mdCell(s.Detail), s.Status, lt)
	}

	b.WriteString("\n## Checks\n\n")
	n := 0
	for _, s := range r.Steps {
		for _, c := range s.Checks {
			n++
			mark := "pass"
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "- %s: %s: %s\n", mark, c.Name, c.Detail)
		}
	}
	if n == 0 {
		b.WriteString("No checks ran.\n")
	}
	if len(r.Failures) > 0 {
		b.WriteString("\n## Failures\n\n")
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if r.LabObject != nil {
		if y, err := OrderedYAML(r.LabObject); err == nil {
			b.WriteString("\n## Lab\n\nGenerated for this run (not stored in `.pk/labs`):\n\n```yaml\n")
			b.Write(y)
			b.WriteString("```\n")
		}
	}
	fmt.Fprintf(&b, "\nRaw results (steps, checks, topology) stay on the bench in `labs/%s/`.\n", r.ID)
	return []byte(b.String())
}

func reportEntities(r *Run) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(r.Cluster)
	nodes := append([]string(nil), r.Nodes...)
	sort.Strings(nodes)
	for _, n := range nodes {
		add(n)
	}
	return out
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// OrderedYAML marshals v as YAML in the field order of its JSON form.
func OrderedYAML(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(b, &n); err != nil {
		return nil, err
	}
	blockStyle(&n)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

func blockStyle(n *yaml.Node) {
	n.Style &^= yaml.FlowStyle | yaml.DoubleQuotedStyle
	for _, c := range n.Content {
		blockStyle(c)
	}
}
