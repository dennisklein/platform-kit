package advisor

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// ---------------------------------------------------------------- risks

func (e *Engine) risks(w *workspace) ([]finding, string, error) {
	ch := w.ch
	var fs []finding
	fs = append(fs, e.overlapFindings(w)...)
	fs = append(fs, w.bgpGapFindings()...)
	fs = append(fs, e.portHistory(w)...)
	gaps := e.labGaps(w)
	// The risks recipe keeps the two most important lab gaps.
	sortFindings(gaps)
	if len(gaps) > 2 {
		gaps = gaps[:2]
	}
	fs = append(fs, gaps...)
	if probs, err := w.t.Problems(w.ctx, w.id); err == nil {
		var errs []api.Problem
		for _, p := range probs {
			if p.Severity == api.ProblemSeverityError {
				errs = append(errs, p)
			}
		}
		if len(errs) > 0 {
			p := errs[0]
			text := fmt.Sprintf("%s: %s", shortPath(p.Path), p.Message)
			if len(errs) > 1 {
				text += fmt.Sprintf(" (and %s more)", plural(len(errs)-1, "problem", "problems"))
			}
			fs = append(fs, finding{Point: Point{Title: "Problems", Text: text + ". The render and the lab use the files as they are.",
				Severity: SevRisk, Sources: []llm.Source{srcFile(p.Path, p.Line)}}})
		}
	}
	sortFindings(fs)
	if len(fs) == 0 {
		fs = append(fs, finding{Point: Point{Title: "No findings", Severity: SevOK,
			Text:    "No overlap with open changes, no BGP gaps, no port history and no lab gaps found.",
			Sources: []llm.Source{srcImpact(w.id)}}})
	}
	level := string(ch.Gates.Risk.Level)
	note := "Advisory only. The policy rating and the gates do not change."
	if level != "" {
		note = fmt.Sprintf("Advisory only. The rating stays %q from the policy.", level)
		w.read(srcPolicy())
	}
	return fs, note, nil
}

// overlapFindings: open changes that touch the same rendered objects.
func (e *Engine) overlapFindings(w *workspace) []finding {
	var out []finding
	ch := w.ch
	overlaps := ch.OverlapsDetail
	if len(overlaps) == 0 {
		if imp, err := w.impact(); err == nil {
			overlaps = imp.Overlaps
		}
	}
	for _, o := range overlaps {
		objs := objectNames(o.Objects)
		srcs := []llm.Source{srcChange(o.Change)}
		state := string(o.State)
		sum, ok := w.summary(o.Change)
		if ok && sum.State != "" {
			// The list derives the state of every change; an overlap entry
			// can carry an older one.
			state = string(sum.State)
		}
		if ok && sum.Mr.Number > 0 {
			srcs = append(srcs, srcMR(sum.Mr.Number, sum.Mr.Url))
			w.read(srcMR(sum.Mr.Number, sum.Mr.Url))
		}
		by := o.By
		if by == "" && ok {
			by = sum.Author
		}
		text := fmt.Sprintf("%s (%s) also changes %s.", o.Change, by, objs)
		// What the other change does to the shared objects.
		addsPeers := map[string]bool{}
		if oimp, err := w.t.Impact(w.ctx, o.Change); err == nil {
			shared := map[string]bool{}
			for _, ob := range o.Objects {
				shared[ob.Name] = true
			}
			var lines []string
			for _, s := range oimp.Semantic {
				if shared[s.Subject] {
					lines = append(lines, s.Text)
					if strings.Contains(strings.ToLower(s.Text), "peer") {
						addsPeers[s.Subject] = true
					}
				}
			}
			if len(lines) > 0 {
				text = fmt.Sprintf("%s (%s) also changes %s: %s.", o.Change, by, objs, strings.Join(lines, "; "))
			}
		}
		// Peers of a shared BGP policy: until the other change merges, the
		// nodes of this change peer with the current peers only. A node the
		// policy does not select peers with nobody (a BGP gap says so).
		for _, pol := range w.bgpPolicies() {
			if containsObj(o.Objects, "CiliumBGPPeeringPolicy", pol.Name) && len(pol.Peers) > 0 && addsPeers[pol.Name] {
				var newNodes []string
				for _, n := range w.changeNodes() {
					if n.New && strings.EqualFold(n.Rack, pol.Rack) && pol.Selects(n.Name) {
						newNodes = append(newNodes, n.Name)
					}
				}
				if len(newNodes) > 0 {
					verb := "peer"
					if len(newNodes) == 1 {
						verb = "peers"
					}
					text += fmt.Sprintf(" Until %s merges, %s %s only with %s.", o.Change, joinAnd(newNodes), verb, joinAnd(pol.Peers))
				}
			}
		}
		if runs, err := w.t.LabRuns(w.ctx, o.Change); err == nil && len(runs) > 0 {
			r := newestRun(runs)
			if r.Result == api.LabResultFail {
				text += fmt.Sprintf(" Its last lab run failed: %s.", failDetail(r))
				srcs = append(srcs, srcLab(r.Id))
				w.read(srcLab(r.Id))
			}
		} else if ok && sum.LabState == api.LabStateFail {
			text += " Its last lab run failed."
		}
		text += " Merge order matters: the change that merges second must rebase and run the lab again."
		sev := SevWarn
		if state == string(api.ChangeStateApproved) || state == string(api.ChangeStateReview) {
			sev = SevRisk
		}
		out = append(out, finding{Point: Point{Title: "Overlap", Text: text, Severity: sev, Sources: srcs}})
	}
	return out
}

func containsObj(objs []api.ObjectRef, kind, name string) bool {
	for _, o := range objs {
		if o.Kind == kind && o.Name == name {
			return true
		}
	}
	return false
}

func objectNames(objs []api.ObjectRef) string {
	seen := map[string]bool{}
	var names []string
	for _, o := range objs {
		n := o.Kind + "/" + o.Name
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if len(names) > 3 {
		return strings.Join(names[:3], ", ") + fmt.Sprintf(" and %d more", len(names)-3)
	}
	return joinAnd(names)
}

func newestRun(runs []api.LabRun) api.LabRun {
	best := runs[0]
	for _, r := range runs[1:] {
		if r.Number > best.Number || (r.Number == best.Number && r.StartedAt.After(best.StartedAt)) {
			best = r
		}
	}
	return best
}

// failDetail returns the first failed check or step of a run.
func failDetail(r api.LabRun) string {
	for _, s := range r.Steps {
		for _, c := range s.Checks {
			if !c.Ok {
				if c.Detail != "" {
					return c.Detail
				}
				return c.Name
			}
		}
	}
	for _, s := range r.Steps {
		if s.Status == api.LabStepStatusFail {
			if s.Detail != "" {
				return s.Title + ": " + s.Detail
			}
			return s.Title
		}
	}
	if r.Summary != "" {
		return r.Summary
	}
	return "see the report"
}

// portHistory: merged changes that moved ports next to (or onto) the ports
// this change uses.
func (e *Engine) portHistory(w *workspace) []finding {
	type use struct {
		node string
		sp   SwitchPort
	}
	var uses []use
	for _, n := range w.changeNodes() {
		for _, p := range n.Ports {
			uses = append(uses, use{n.Name, SwitchPort{p.Peer.Switch, p.Peer.Port}})
		}
	}
	if len(uses) == 0 {
		return nil
	}
	mine := map[string]bool{}
	for _, n := range w.changeNodes() {
		mine[n.Name] = true
	}
	entries, err := w.t.History(w.ctx, api.ListHistoryParams{Path: "control/inventory/**", Limit: 40})
	if err != nil {
		return nil
	}
	var out []finding
	for i, h := range entries {
		if i >= 20 || len(out) >= 2 {
			break
		}
		lines := []string{}
		if d, err := w.t.HistoryEntry(w.ctx, h.Sha); err == nil {
			for _, s := range d.Semantic {
				if len(PortsInText(s.Text)) > 0 && !mine[strings.Fields(s.Subject + " x")[0]] {
					lines = append(lines, strings.TrimSpace(s.Subject+": "+s.Text))
				}
			}
		}
		if len(lines) == 0 {
			continue
		}
		var hits []string
		seen := map[string]bool{}
		for _, l := range lines {
			moved := PortsInText(l)
			for k, sp := range moved {
				for _, u := range uses {
					if u.sp.Switch != sp.Switch {
						continue
					}
					var why string
					switch d := u.sp.Port - sp.Port; {
					case d == 0 && k == 0 && len(moved) > 1:
						why = fmt.Sprintf("%s (freed then)", u.sp)
					case d == 0:
						why = fmt.Sprintf("%s (the same port)", u.sp)
					case d == -1 || d == 1:
						why = fmt.Sprintf("%s (next to %s)", u.sp, sp)
					}
					if why != "" && !seen[u.sp.String()] {
						seen[u.sp.String()] = true
						hits = append(hits, why)
					}
				}
			}
		}
		if len(hits) == 0 {
			continue
		}
		change := h.Change
		if change == "" {
			change = short7(h.Sha)
		}
		what := strings.Join(lines, "; ")
		text := fmt.Sprintf("%s (%s, %s) changed %s", change, h.Author, h.Date.Format("2006-01-02"), what)
		if h.Intent != "" {
			text += fmt.Sprintf(", to %s", lowerFirst(strings.TrimSuffix(h.Intent, ".")))
		}
		text += fmt.Sprintf(". This change uses %s.", joinAnd(hits))
		srcs := []llm.Source{srcTrailer(change)}
		w.read(srcTrailer(change))
		if n := mrNumber(h.Mr); n > 0 {
			srcs = append(srcs, srcMR(n, ""))
		}
		out = append(out, finding{Point: Point{Title: "Port history", Text: text, Severity: SevInfo, Sources: srcs}})
	}
	return out
}

func lowerFirst(s string) string {
	if s == "" || (len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z') {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

var reMR = regexp.MustCompile(`!(\d+)`)

func mrNumber(s string) int {
	if m := reMR.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// ---------------------------------------------------------------- lab gaps

// labGaps compares the last lab run with what the change needs tested.
func (e *Engine) labGaps(w *workspace) []finding {
	ch := w.ch
	run, ref := w.lastRun()
	gate := ch.Gates.LabGate
	var out []finding
	if run == nil && ref == nil {
		if gate.Required {
			out = append(out, finding{Point: Point{Title: "No lab run", Severity: SevRisk,
				Text:    "No lab run yet. The policy needs a lab run on the head of the change (lab gate).",
				Sources: []llm.Source{srcPolicy()}}})
		}
		return out
	}
	runID := ""
	if run != nil {
		runID = run.Id
	} else {
		runID = ref.Run
	}
	runLabel := runID
	if _, n, ok := strings.Cut(runID, "/"); ok {
		runLabel = n
	}
	src := srcLab(runID)
	// Stale run.
	if gate.Required && gate.Status != api.LabGateStatusMet && gate.Status != api.LabGateStatusRunning {
		sha := ""
		if run != nil {
			sha = run.Sha
		} else {
			sha = ref.Sha
		}
		res := ""
		if run != nil {
			res = string(run.Result)
		} else {
			res = string(ref.Result)
		}
		if res == string(api.LabResultFail) {
			text, srcs := w.labFailedText(runLabel, run, ref)
			out = append(out, finding{Point: Point{Title: "Lab failed", Severity: SevRisk, Text: text, Sources: srcs}})
		} else if sha != "" && ch.HeadSha != "" && !strings.HasPrefix(ch.HeadSha, sha) && !strings.HasPrefix(sha, ch.HeadSha) {
			out = append(out, finding{Point: Point{Title: "Lab is old", Severity: SevWarn,
				Text: fmt.Sprintf("%s tested %s, the head is %s. The lab gate needs a new run.", runLabel, short7(sha), short7(ch.HeadSha)), Sources: []llm.Source{src, srcPolicy()}}})
		}
	}
	if run == nil {
		return out
	}
	imp, _ := w.impact()
	kindsHit := map[string]bool{}
	if imp != nil {
		for _, c := range imp.Clusters {
			for _, k := range c.Kustomizations {
				for _, o := range k.Objects {
					kindsHit[o.Kind] = true
					if o.Kind == "HelmRelease" && o.Name == "cilium" {
						kindsHit["cilium"] = true
					}
				}
			}
		}
	}
	faults, bmc := "", ""
	bothDown := false
	all := ""
	for _, s := range run.Steps {
		txt := strings.ToLower(s.Name + " " + s.Title + " " + s.Detail + " " + strings.Join(s.Output, " "))
		for _, c := range s.Checks {
			txt += " " + strings.ToLower(c.Name+" "+c.Detail+" "+c.Subject)
		}
		all += " " + txt
		switch s.Type {
		case api.LabStepTypeFault:
			faults += " " + txt
			if strings.Contains(txt, "both") || strings.Contains(txt, "all data") ||
				(strings.Contains(txt, "data-a") && strings.Contains(txt, "data-b") && !strings.Contains(txt, "via")) {
				bothDown = true
			}
		case api.LabStepTypeBmc:
			bmc += " " + txt
		}
	}
	nodes := w.changeNodes()
	var dual []string
	var newNodes []string
	for _, n := range nodes {
		nets := map[string]bool{}
		for _, p := range n.DataPorts() {
			nets[p.Peer.Switch] = true
		}
		if len(nets) >= 2 {
			dual = append(dual, n.Name)
		}
		if n.New {
			newNodes = append(newNodes, n.Name)
		}
	}
	if (kindsHit["Machine"] || len(nodes) > 0) && len(dual) > 0 {
		if !bothDown {
			text := fmt.Sprintf("%s had no test with both data links down.", runLabel)
			if strings.Contains(faults, "link down") {
				text = fmt.Sprintf("%s tested one data link down, not both. %s %s two data links.", runLabel, joinAnd(dual), hasHave(len(dual)))
			}
			out = append(out, finding{Point: Point{Title: "Lab gap", Text: text, Severity: SevWarn, Sources: []llm.Source{src}}})
		}
	}
	if len(newNodes) > 0 {
		var missing []string
		for _, n := range newNodes {
			if !strings.Contains(bmc, n) {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 && len(missing) < len(newNodes) {
			out = append(out, finding{Point: Point{Title: "Lab gap", Severity: SevInfo,
				Text: fmt.Sprintf("The BMC power cycle in %s covers %s, not %s.", runLabel, joinAnd(diff(newNodes, missing)), joinAnd(missing)), Sources: []llm.Source{src}}})
		} else if len(missing) == len(newNodes) && bmc == "" {
			out = append(out, finding{Point: Point{Title: "Lab gap", Severity: SevInfo,
				Text: fmt.Sprintf("%s did no BMC power cycle of %s.", runLabel, joinAnd(newNodes)), Sources: []llm.Source{src}}})
		}
	}
	if kindsHit["CiliumBGPPeeringPolicy"] && !strings.Contains(faults, "bgp") && !strings.Contains(faults, "peer") && !strings.Contains(faults, "router") {
		text := fmt.Sprintf("%s had no test with a BGP peer down.", runLabel)
		for _, pol := range w.bgpPolicies() {
			if len(pol.Peers) == 1 {
				text += fmt.Sprintf(" %s has one peer (%s): no failover.", pol.Name, pol.Peers[0])
				break
			}
		}
		out = append(out, finding{Point: Point{Title: "Lab gap", Text: text, Severity: SevWarn, Sources: []llm.Source{src}}})
	}
	if kindsHit["cilium"] && !strings.Contains(all, "rollback") {
		out = append(out, finding{Point: Point{Title: "Lab gap", Severity: SevWarn,
			Text: fmt.Sprintf("%s did not test a rollback of the cilium HelmRelease.", runLabel), Sources: []llm.Source{src}}})
	}
	if run.Driver == "sim" || run.Driver == "" {
		out = append(out, finding{Point: Point{Title: "Simulated lab", Severity: SevInfo,
			Text:    "The sim driver computes install, join and BGP from the desired state. It boots no machines and opens no real BGP sessions.",
			Sources: []llm.Source{src}}})
	}
	if run.Fidelity == "scaled" {
		out = append(out, finding{Point: Point{Title: "Scaled lab", Severity: SevInfo,
			Text: "The lab is scaled: it emulates fewer machines than production.", Sources: []llm.Source{src}}})
	}
	return out
}

func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func diff(a, b []string) []string {
	m := map[string]bool{}
	for _, s := range b {
		m[s] = true
	}
	var out []string
	for _, s := range a {
		if !m[s] {
			out = append(out, s)
		}
	}
	return out
}

func (e *Engine) labGapsRecipe(w *workspace) ([]finding, error) {
	var fs []finding
	run, ref := w.lastRun()
	if run != nil {
		var tested []string
		for _, s := range run.Steps {
			if s.Type == api.LabStepTypeCheck || s.Type == api.LabStepTypeFault || s.Type == api.LabStepTypeBmc {
				t := s.Title
				if s.Detail != "" {
					t += ": " + s.Detail
				}
				tested = append(tested, t)
			}
		}
		text := fmt.Sprintf("%s %s", run.Id, run.Result)
		if run.Summary != "" {
			text += ", " + run.Summary
		}
		if len(tested) > 0 {
			text += ". It tested " + strings.Join(tested, "; ")
		}
		fs = append(fs, finding{Point: Point{Title: "What the lab tests", Text: text + ".", Severity: SevOK, Sources: []llm.Source{srcLab(run.Id)}}})
	} else if ref != nil {
		// A recorded run: what it tested comes from its report.
		text, srcs := w.recordedRunText(ref)
		fs = append(fs, finding{Point: Point{Title: "What the lab tests", Severity: SevInfo, Text: text, Sources: srcs}})
	} else {
		fs = append(fs, e.plannedLab(w)...)
	}
	gaps := e.labGaps(w)
	if g, ok := w.emulationGap(run, ref); ok {
		// The gap names what the lab emulates: the "Lab failed" point
		// need not say it again.
		if _, why, _ := w.labFailures(run, ref); failClause(why) != "" {
			for i := range gaps {
				if gaps[i].Title == "Lab failed" {
					gaps[i].Text = strings.Replace(gaps[i].Text, "; "+failClause(why), "", 1)
				}
			}
		}
		gaps = append(gaps, g)
	}
	sortFindings(gaps)
	fs = append(fs, gaps...)
	if len(fs) == 0 {
		fs = append(fs, finding{Point: Point{Title: "No lab needed", Severity: SevOK,
			Text: "The policy needs no lab for this change, and it has no lab run.", Sources: []llm.Source{srcPolicy()}}})
	}
	return fs, nil
}

// ---------------------------------------------------------------- merge order

func (e *Engine) mergeOrder(w *workspace) ([]finding, error) {
	ch := w.ch
	type cand struct {
		id, title, by string
		score         int
		why           string
		mr            int
		mrURL         string
		overlap       bool
	}
	score := func(s api.ChangeSummary) (int, string) {
		switch {
		case s.State == api.ChangeStateApproved:
			return 4, fmt.Sprintf("approved %d of %d", s.Approvals.Have, s.Approvals.Need)
		case s.State == api.ChangeStateReview:
			return 3, fmt.Sprintf("in review, %d of %d approvals", s.Approvals.Have, s.Approvals.Need)
		case s.LabState == api.LabStateFail:
			return 0, "last lab run failed"
		case s.State == api.ChangeStateLab:
			return 1, "lab"
		default:
			return 2, string(s.State)
		}
	}
	overlapping := map[string]bool{}
	for _, o := range ch.OverlapsDetail {
		overlapping[o.Change] = true
	}
	var cands []cand
	me := cand{id: ch.Id, title: ch.Title, by: ch.Author, mr: ch.Mr.Number, mrURL: ch.Mr.Url}
	if sum, ok := w.summary(ch.Id); ok {
		me.score, me.why = score(sum)
	} else {
		me.score, me.why = 2, string(ch.State)
	}
	cands = append(cands, me)
	for _, s := range w.summaries() {
		if s.Id == ch.Id || s.Target != ch.Target {
			continue
		}
		if !overlapping[s.Id] && s.State != api.ChangeStateApproved {
			continue
		}
		c := cand{id: s.Id, title: s.Title, by: s.Author, mr: s.Mr.Number, mrURL: s.Mr.Url, overlap: overlapping[s.Id]}
		c.score, c.why = score(s)
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	// What the advisor reads: the other changes and their MRs, and the policy.
	for _, c := range cands {
		if c.id == ch.Id {
			continue
		}
		w.read(srcChange(c.id))
		if c.mr > 0 {
			w.read(srcMR(c.mr, c.mrURL))
		}
	}
	w.read(srcPolicy())
	var fs []finding
	if len(cands) == 1 {
		fs = append(fs, finding{Point: Point{Title: "No conflicts", Severity: SevOK,
			Text: "No open change overlaps this change, and no other change is approved. The order does not matter.", Sources: []llm.Source{srcImpact(ch.Id)}}})
		return fs, nil
	}
	var order []string
	for _, c := range cands {
		order = append(order, c.id)
	}
	labNeeded := ch.Gates.LabGate.Required
	for i, c := range cands {
		if c.id == ch.Id {
			continue
		}
		srcs := []llm.Source{srcChange(c.id)}
		if c.mr > 0 {
			srcs = append(srcs, srcMR(c.mr, c.mrURL))
		}
		var text string
		before := i < indexOf(order, ch.Id)
		if before {
			text = fmt.Sprintf("Merge %s (%s, %s) first.", c.id, c.by, c.why)
			text += fmt.Sprintf(" After it merges, %s is behind main by 1: rebase", ch.Id)
			if labNeeded {
				text += ", render and run the lab again"
			}
			text += "."
		} else {
			text = fmt.Sprintf("Merge %s (%s, %s) after %s.", c.id, c.by, c.why, ch.Id)
		}
		if c.overlap {
			text += " " + e.textConflict(w, c.id)
		}
		sev := SevInfo
		if c.overlap {
			sev = SevWarn
		}
		fs = append(fs, finding{Point: Point{Title: "Order", Text: strings.TrimSpace(text), Severity: sev, Sources: srcs}})
	}
	fs = append(fs, finding{Point: Point{Title: "Suggested order", Severity: SevInfo, Text: strings.Join(order, " → ") + ".",
		Sources: []llm.Source{srcChange(ch.Id), srcPolicy()}}})
	return fs, nil
}

func indexOf(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}

var reHunk = regexp.MustCompile(`(?m)^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// changedRegions returns the base line ranges [start, end) a unified diff
// changes. An insertion before base line b is the empty range [b, b).
func changedRegions(u string) [][2]int {
	var out [][2]int
	base, in := 0, false
	open := -1
	closeRegion := func() {
		if open >= 0 {
			out = append(out, [2]int{open, base})
			open = -1
		}
	}
	for _, line := range strings.Split(u, "\n") {
		if m := reHunk.FindStringSubmatch(line); m != nil {
			closeRegion()
			base, _ = strconv.Atoi(m[1])
			if m[2] == "0" {
				base++ // "-12,0": the insertion follows line 12
			}
			in = true
			continue
		}
		if !in || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "-"):
			if open < 0 {
				open = base
			}
			base++
		case strings.HasPrefix(line, "+"):
			if open < 0 {
				open = base
			}
		case strings.HasPrefix(line, " "), line == "":
			closeRegion()
			base++
		}
	}
	closeRegion()
	return out
}

// textConflict compares the base line ranges both changes edit in shared files.
func (e *Engine) textConflict(w *workspace, other string) string {
	mine, err1 := w.t.Diff(w.ctx, w.id)
	theirs, err2 := w.t.Diff(w.ctx, other)
	if err1 != nil || err2 != nil {
		return ""
	}
	their := map[string]string{}
	for _, f := range theirs.Files {
		their[f.Path] = f.Unified
	}
	var parts []string
	for _, f := range mine.Files {
		u, ok := their[f.Path]
		if !ok {
			continue
		}
		a, b := changedRegions(f.Unified), changedRegions(u)
		minGap := 1 << 30
		for _, x := range a {
			for _, y := range b {
				// Regions that overlap or touch conflict (as in git).
				gap := max(y[0]-x[1], x[0]-y[1])
				minGap = min(minGap, gap)
			}
		}
		switch {
		case minGap == 1<<30:
		case minGap > 0:
			parts = append(parts, fmt.Sprintf("Both edit %s, %d lines apart: git can rebase without a conflict.", shortPath(f.Path), minGap))
		default:
			parts = append(parts, fmt.Sprintf("Both edit the same lines of %s: expect a rebase conflict.", shortPath(f.Path)))
		}
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- check commit

func (e *Engine) checkCommit(w *workspace) ([]finding, error) {
	ch := w.ch
	var fs []finding
	fs = append(fs, w.bgpGapFindings()...)

	// Problems in changed files.
	if probs, err := w.t.Problems(w.ctx, w.id); err == nil {
		changed := map[string]bool{}
		for _, f := range w.changedFiles() {
			changed[f.Path] = true
		}
		n := 0
		for _, p := range probs {
			if p.Severity != api.ProblemSeverityError || !changed[p.Path] || n >= 3 {
				continue
			}
			n++
			loc := shortPath(p.Path)
			if p.Line > 0 {
				loc += fmt.Sprintf(" line %d", p.Line)
			}
			fs = append(fs, finding{Point: Point{Title: "Problem", Text: loc + ": " + p.Message + ".", Severity: SevRisk,
				Sources: []llm.Source{srcFile(p.Path, p.Line)}}})
		}
	}

	// Intent coverage: names and units in the title and intent.
	intent := ch.Title + "\n" + ch.Intent
	nodes := w.changeNodes()
	have := map[string]bool{}
	units := map[int]string{}
	for _, n := range nodes {
		have[n.Name] = true
		units[n.Unit] = n.Name
	}
	var contents strings.Builder
	for _, f := range w.changedFiles() {
		if c, ok := w.file(w.id, f.Path); ok && f.Status != api.FileChangeDeleted {
			contents.WriteString(c + "\n")
		}
	}
	var missing []string
	for _, name := range EntityNames(intent) {
		if !have[name] && !strings.Contains(contents.String(), name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fs = append(fs, finding{Point: Point{Title: "Intent", Severity: SevWarn,
			Text:    fmt.Sprintf("The intent names %s, but no file of the change has %s.", joinAnd(missing), itThem(len(missing))),
			Sources: []llm.Source{srcChange(ch.Id)}}})
	}
	if us := Units(intent); len(us) > 0 && len(nodes) > 0 {
		want := map[int]bool{}
		for _, u := range us {
			want[u] = true
		}
		var off []string
		for _, n := range nodes {
			if n.New && !want[n.Unit] {
				off = append(off, fmt.Sprintf("%s is in U%d", n.Name, n.Unit))
			}
		}
		if len(off) > 0 {
			fs = append(fs, finding{Point: Point{Title: "Intent", Severity: SevWarn,
				Text:    fmt.Sprintf("The intent says %s, but %s.", unitList(us), joinAnd(off)),
				Sources: []llm.Source{srcChange(ch.Id), srcFile(nodes[0].Path, 0)}}})
		}
	}

	// Commit hygiene: last commit subject and saved files.
	if len(ch.Commits) > 0 {
		last := ch.Commits[len(ch.Commits)-1]
		for _, c := range ch.Commits {
			if c.Date.After(last.Date) {
				last = c
			}
		}
		if !strings.HasPrefix(last.Subject, "chore(change):") && !reConventional.MatchString(last.Subject) {
			fs = append(fs, finding{Point: Point{Title: "Commit message", Severity: SevInfo,
				Text:    fmt.Sprintf("The last commit %s (%q) is not a Conventional Commit (type(scope): subject).", short7(last.Sha), last.Subject),
				Sources: []llm.Source{{Label: "commit " + short7(last.Sha), Ref: last.Sha, Kind: "change"}}}})
		}
	}
	if st, err := w.t.Status(w.ctx, w.id); err == nil && len(st.Files) > 0 {
		var paths []string
		for _, f := range st.Files {
			paths = append(paths, shortPath(f.Path))
		}
		fs = append(fs, finding{Point: Point{Title: "Not committed", Severity: SevInfo,
			Text:    fmt.Sprintf("%s saved but not committed: %s.", plural(len(paths), "file is", "files are"), strings.Join(paths, ", ")),
			Sources: []llm.Source{srcChange(ch.Id)}}})
	}
	sortFindings(fs)
	if len(fs) == 0 {
		text := "The commit matches the intent."
		if len(nodes) > 0 {
			var names []string
			for _, n := range nodes {
				names = append(names, n.Name)
			}
			text = fmt.Sprintf("The commit matches the intent: %s, with ports and BGP policy.", joinAnd(names))
		}
		fs = append(fs, finding{Point: Point{Title: "Looks good", Text: text, Severity: SevOK, Sources: []llm.Source{srcChange(ch.Id)}}})
	}
	return fs, nil
}

var reConventional = regexp.MustCompile(`^(feat|fix|chore|docs|refactor|test|ci|build|perf|style|revert)(\([a-zA-Z0-9._/ -]+\))?!?: \S`)

func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func unitList(us []int) string {
	var s []string
	for _, u := range us {
		s = append(s, fmt.Sprintf("U%d", u))
	}
	return joinAnd(s)
}
