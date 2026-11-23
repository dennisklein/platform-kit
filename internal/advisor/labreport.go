package advisor

import (
	"fmt"
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

// reportFile reads the KB report of a lab run (in the change, else on
// main) and records it as a source.
func (w *workspace) reportFile(report string) (string, bool) {
	if report == "" {
		return "", false
	}
	content, ok := w.file(w.id, report)
	if !ok {
		if content, ok = w.file("main", report); !ok {
			return "", false
		}
	}
	w.read(srcReport(report))
	return content, true
}

var reStepRow = regexp.MustCompile(`^\|\s*([^|]+?)\s*\|\s*([^|]*?)\s*\|\s*([a-z]+)\s*\|$`)

// reportSteps returns the steps of a report's step table by result (pass,
// fail, skipped), in table order. The report step itself is no test.
func reportSteps(content string) map[string][]string {
	out := map[string][]string{}
	for _, l := range strings.Split(content, "\n") {
		m := reStepRow.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil || strings.EqualFold(m[1], "step") || strings.HasPrefix(m[1], "-") || m[1] == "report" {
			continue
		}
		out[m[3]] = append(out[m[3]], m[1])
	}
	return out
}

func resultWord(r string) string {
	switch r {
	case string(api.LabResultPass):
		return "passed"
	case string(api.LabResultFail):
		return "failed"
	}
	return r
}

// recordedRunText says what a run recorded in the change tested, from its
// report: "run-1 failed. Its report lists topology, BMC, install and join as
// passed, BGP as failed and checks as skipped."
func (w *workspace) recordedRunText(ref *api.LabRunRef) (string, []llm.Source) {
	label := ref.Run
	if _, n, ok := strings.Cut(ref.Run, "/"); ok {
		label = n
	}
	srcs := []llm.Source{srcLab(ref.Run)}
	result := resultWord(string(ref.Result))
	report := ref.Report
	if report == "" && w.ch != nil {
		for _, r := range w.ch.LabRuns {
			if r.Run == ref.Run {
				report = r.Report
			}
		}
	}
	content, ok := w.reportFile(report)
	if !ok {
		return label + " " + result + ", as recorded in the change.", srcs
	}
	srcs = append(srcs, srcReport(report))
	steps := reportSteps(content)
	var parts []string
	for _, r := range [][2]string{{"pass", "passed"}, {"fail", "failed"}, {"skipped", "skipped"}} {
		if len(steps[r[0]]) > 0 {
			parts = append(parts, joinAnd(steps[r[0]])+" as "+r[1])
		}
	}
	text := label + " " + result + "."
	if len(parts) > 0 {
		text += " Its report lists " + joinAnd(parts) + "."
	}
	return text, srcs
}

var reIPv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// emulationGap is the gap the report of a failed run explains: "The lab
// emulates only the router 10.6.0.1, so the BGP peers 10.6.0.3 and 10.6.0.4
// are not tested."
func (w *workspace) emulationGap(run *api.LabRun, ref *api.LabRunRef) (finding, bool) {
	if run == nil && ref == nil {
		return finding{}, false
	}
	fails, why, report := w.labFailures(run, ref)
	clause := failClause(why)
	if len(fails) == 0 || clause == "" {
		return finding{}, false
	}
	noun := "the addresses"
	var ips []string
	seen := map[string]bool{}
	for _, f := range fails {
		if strings.HasPrefix(f, "bgp: ") {
			noun = "the BGP peers"
		}
		for _, ip := range reIPv4.FindAllString(f, -1) {
			if !seen[ip] && !strings.Contains(why, ip) {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	if len(ips) == 0 {
		return finding{}, false
	}
	id := ""
	if run != nil {
		id = run.Id
	} else {
		id = ref.Run
	}
	srcs := []llm.Source{srcLab(id)}
	if report != "" {
		srcs = append(srcs, srcReport(report))
	}
	text := fmt.Sprintf("%s%s, so %s %s %s not tested.", strings.ToUpper(clause[:1]), clause[1:], noun, joinAnd(ips), isAre(len(ips)))
	return finding{Point: Point{Title: "Lab gap", Text: text, Severity: SevWarn, Sources: srcs}}, true
}
