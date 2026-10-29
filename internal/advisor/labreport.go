package advisor

import (
	"regexp"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// The failed checks of a lab run, from the run itself or from its KB report
// (a seeded run has only the report). Chat reads the same reports for its
// lab lines, so both surfaces name the same failures.

var (
	reReportFail  = regexp.MustCompile(`(?im)^\s*[-*]\s*FAIL:\s*(.+)$`)
	reReportFront = regexp.MustCompile(`(?s)\A---\n.*?\n---\n?`)
	reParens      = regexp.MustCompile(`\s*\([^)]*\)`)
	reMDLinkText  = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
)

func srcReport(p string) llm.Source { return llm.Source{Label: shortPath(p), Ref: p, Kind: "kb"} }

// labFailures returns the failed checks of a run, a sentence that explains
// them (the last paragraph of the report) and the report path. A read
// report is recorded as a source.
func (w *workspace) labFailures(run *api.LabRun, ref *api.LabRunRef) (fails []string, why, report string) {
	id := ""
	switch {
	case run != nil:
		id = run.Id
	case ref != nil:
		id, report = ref.Run, ref.Report
	}
	if report == "" && w.ch != nil {
		for _, r := range w.ch.LabRuns {
			if r.Run == id {
				report = r.Report
			}
		}
	}
	if run != nil {
		fails = append(fails, run.Failures...)
	}
	if report == "" {
		return fails, "", ""
	}
	content, ok := w.file(w.id, report)
	if !ok {
		if content, ok = w.file("main", report); !ok {
			return fails, "", ""
		}
	}
	w.read(srcReport(report))
	if len(fails) == 0 {
		for _, m := range reReportFail.FindAllStringSubmatch(content, -1) {
			fails = append(fails, strings.TrimSpace(m[1]))
		}
	}
	return fails, reportWhy(content), report
}

// reportWhy is the first sentence of the last paragraph of a report, when
// that paragraph is prose a person wrote (a generated report ends with
// where the raw results stay, which explains nothing).
func reportWhy(content string) string {
	body := reReportFront.ReplaceAllString(content, "")
	paras := strings.Split(strings.TrimSpace(body), "\n\n")
	if len(paras) == 0 {
		return ""
	}
	last := strings.TrimSpace(paras[len(paras)-1])
	switch {
	case last == "", strings.HasPrefix(last, "Raw results"), strings.HasPrefix(last, "#"), strings.HasPrefix(last, "|"), strings.HasPrefix(last, "-"),
		strings.HasPrefix(last, "*"), strings.HasPrefix(last, "```"):
		return ""
	}
	last = strings.Join(strings.Fields(reMDLinkText.ReplaceAllString(last, "$1")), " ")
	if i := strings.Index(last, ". "); i >= 0 {
		last = last[:i+1]
	}
	return last
}

// failureText joins failed checks for one sentence: the check family
// ("bgp: ") goes, checks that differ in one value merge ("sessions to
// 10.6.0.3 and 10.6.0.4 not established"), and a subject said once is not
// repeated ("cn-040: mgmt has …; data-b has …").
func failureText(fails []string) string {
	type group struct {
		words []string
		at    int
		vals  []string
	}
	var groups []*group
	for _, f := range fails {
		f = strings.TrimSpace(f)
		if family, rest, ok := strings.Cut(f, ": "); ok && isLowerWord(family) {
			f = rest
		}
		ws := strings.Fields(f)
		merged := false
		for _, g := range groups {
			if len(g.words) != len(ws) {
				continue
			}
			at, n := -1, 0
			for i := range ws {
				if ws[i] != g.words[i] {
					at, n = i, n+1
				}
			}
			// Only values merge (addresses, numbers): "mgmt and data-b has"
			// would break the verb.
			if n == 1 && (g.at < 0 || g.at == at) && hasDigit(ws[at]) && hasDigit(g.words[at]) {
				g.at = at
				g.vals = append(g.vals, ws[at])
				merged = true
				break
			}
		}
		if !merged {
			groups = append(groups, &group{words: ws, at: -1})
		}
	}
	var out []string
	subject := ""
	for _, g := range groups {
		if g.at < 0 {
			out = append(out, strings.Join(g.words, " "))
			continue
		}
		ws := append([]string(nil), g.words...)
		ws[g.at] = joinAnd(append([]string{g.words[g.at]}, g.vals...))
		// "session to A and B": the noun before the merged word is plural.
		if g.at > 0 && isLowerWord(ws[0]) && !strings.HasSuffix(ws[0], "s") {
			ws[0] += "s"
		}
		out = append(out, strings.Join(ws, " "))
	}
	for i, o := range out {
		s, rest, ok := strings.Cut(o, ": ")
		if !ok || strings.Contains(s, " ") {
			subject = ""
			continue
		}
		if s == subject {
			out[i] = rest
		}
		subject = s
	}
	return strings.Join(out, "; ")
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

func isLowerWord(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

// failClause makes the report's explanation a clause: "The lab emulates
// only the router 10.6.0.1 (see …)." → "the lab emulates only the router
// 10.6.0.1".
func failClause(why string) string {
	why = strings.TrimSpace(reParens.ReplaceAllString(why, ""))
	why = strings.TrimSuffix(why, ".")
	return lowerFirst(why)
}

// labFailedText is the text of the "Lab failed" point.
func (w *workspace) labFailedText(runLabel string, run *api.LabRun, ref *api.LabRunRef) (string, []llm.Source) {
	srcs := []llm.Source{}
	if run != nil {
		srcs = append(srcs, srcLab(run.Id))
	} else if ref != nil {
		srcs = append(srcs, srcLab(ref.Run))
	}
	fails, why, report := w.labFailures(run, ref)
	if report != "" {
		srcs = append(srcs, srcReport(report))
	}
	detail := failureText(fails)
	if detail == "" && run != nil {
		detail = failDetail(*run)
	}
	if detail == "" {
		detail = "see the report"
	}
	text := runLabel + " failed: " + detail
	if c := failClause(why); c != "" && len(fails) > 0 {
		text += "; " + c
	}
	return text + ".", srcs
}
