package chat

// Answers from the bench data (the built-in model, and the fallback when a
// model fails): a greeting, questions about a change (brief or an advisor
// recipe), open changes, recent merges, an entity, and the best passage of
// the search index. Each lookup is a step the asker sees.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// ---------------------------------------------------------------- greeting

var reGreeting = regexp.MustCompile(`(?i)^\s*(hi|hello|hey|hallo|moin|servus|good (morning|afternoon|evening)|greetings)\b|\bwho are you\b|\bwhat are you\b|\bwhat can you do\b|\bwhat do you (know|do)\b|\bhow do(es)? (you|this chat) work\b|^\s*help\s*[?!.]*\s*$`)

func isGreeting(q string) bool { return reGreeting.MatchString(q) }

// greet introduces the chat with example questions for the open change.
func (s *Svc) greet(req Request, a *Answer) {
	id := req.ChangeID
	if id == "" {
		id = "c-p4x8ma"
	}
	a.Text = "Hi, I am the chat of platform-kit. I read this project for you: the state repo, the KB, the merge history, MR discussions and lab reports. Each answer shows its sources.\n\n" +
		"Ask me, for example:\n" +
		"- Why does cn-003 use port 37 on data-a?\n" +
		"- What are the risks of " + id + "?\n" +
		"- Add cn-037 to the rack-6z BGP policy\n\n" +
		"I suggest edits as a diff in your change. I never commit, approve or merge, and I never see secret values."
}

// ---------------------------------------------------------------- changes

var (
	reChangeID = regexp.MustCompile(`\bc-[a-z0-9]{6}\b`)
	reMyChange = regexp.MustCompile(`(?i)\b(my|this|the open|the current|our) change\b`)
	reOrder    = regexp.MustCompile(`(?i)\bmerge[- ]order\b|\border\b|\bmerge (it |them )?(first|before|after|next)\b|\b(first|next) to merge\b|\bconflicts?\b`)
	reLabGaps  = regexp.MustCompile(`(?i)\blab\b.*\b(gaps?|miss(es|ing)?|cover(s|age)?|leaves? out|not test)|\b(not|n't) (test|cover)|\buntested\b|\bwhat does the lab\b|\bwhat is not tested\b`)
	reExplain  = regexp.MustCompile(`(?i)\b(rendered|render|diffs?|objects?)\b`)
	// "explain" alone is a general question ("Explain BGP in one
	// sentence"); with a change it asks for the rendered diff.
	reExplainWord = regexp.MustCompile(`(?i)\bexplain\b`)
	reExplainWhat = regexp.MustCompile(`(?i)\b(change|this|my)\b`)
	reRisk        = regexp.MustCompile(`(?i)\b(risks?|risky|safe|safety|dangerous|danger|go wrong|blast radius|worr(y|ied)|careful|watch out)\b|\b(will|would|could|can|does|might) it break\b|\bwhat (could|can|might|would) break\b`)
	reRuleWord    = regexp.MustCompile(`(?i)\b(rules?|polic(y|ies)|incidents?|break-glass)\b`)
	reLabWord     = regexp.MustCompile(`(?i)\blabs?\b`)
	recipeLeads   = map[string]string{
		advisor.MergeOrder:  "Merge order for %s:",
		advisor.LabGaps:     "What the lab of %s tests, and what it leaves out:",
		advisor.ExplainDiff: "What %s changes in the rendered objects:",
	}
)

// recipeFor returns the advisor recipe a question asks for, or "".
func recipeFor(q string) string {
	switch {
	case reOrder.MatchString(q):
		return advisor.MergeOrder
	case reLabGaps.MatchString(q):
		return advisor.LabGaps
	case reExplain.MatchString(q),
		reExplainWord.MatchString(q) && (reExplainWhat.MatchString(q) || reChangeID.MatchString(q)):
		return advisor.ExplainDiff
	case reRisk.MatchString(q) && !reRuleWord.MatchString(q):
		return advisor.Risks
	}
	return ""
}

// changeRef returns the change a question is about: an id in the question,
// "my change" (the open change), an id from the last turns, or the open
// change.
func changeRef(req Request, q string) (id string, named bool) {
	if m := reChangeID.FindString(q); m != "" {
		return m, true
	}
	if reMyChange.MatchString(q) {
		return req.ChangeID, false
	}
	// A follow-up ("and its lab?") is about the change of the last turns.
	msgs := req.Messages
	for i := len(msgs) - 2; i >= 0 && i >= len(msgs)-5; i-- {
		if m := reChangeID.FindString(msgs[i].Content); m != "" {
			return m, false
		}
	}
	return req.ChangeID, false
}

// changeQuestion answers a question about one change: an advisor recipe
// (risks, merge order, lab gaps, rendered diff) or a brief of the change.
func (s *Svc) changeQuestion(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	id, named := changeRef(req, q)
	recipe := recipeFor(q)
	approval := (reAskApproval.MatchString(q) || reAskMerged.MatchString(q)) && len(advisor.EntityNames(q)) == 0
	if !named && recipe == "" && !reMyChange.MatchString(q) && !(approval && id != "") {
		return false
	}
	if approval && id != "" {
		return s.approvals(ctx, p, req, id, q, a)
	}
	if id == "" {
		if recipe == "" {
			return false
		}
		a.Text = "Which change do you mean? Open one, or name it in the question."
		if list := s.openList(ctx, p); list != "" {
			a.Text += " These are open:\n\n" + list
		}
		return true
	}
	if recipe != "" && s.o.Advisor != nil {
		return s.advise(ctx, p, req, id, recipe, a)
	}
	// "Why did its lab fail?": the lab, not the whole brief again.
	if recipe == "" && reLabResultQ.MatchString(q) && !reBriefAsk.MatchString(q) && !reAction.MatchString(q) {
		return s.labAnswer(ctx, p, id, a)
	}
	ok := s.changeBrief(ctx, p, req, id, q, a)
	if m := reAction.FindStringSubmatch(q); m != nil && ok {
		a.Text = fmt.Sprintf("I cannot %s: I only read and suggest edits, people decide. This is where %s stands:\n\n", strings.ToLower(m[1]), id) + a.Text
	}
	return ok
}

// reBriefAsk asks for the whole change ("What does c-p4x8ma change and why
// did its lab fail?").
var reBriefAsk = regexp.MustCompile(`(?i)\bwhat (is|does)\b.*\b(change|do)\b|\btell me about\b|\bdescribe\b|\bsummar\w+`)

// reLabResultQ asks how the lab of a change went.
var reLabResultQ = regexp.MustCompile(`(?i)\blabs?\b.*\b(fail\w*|pass\w*|red|green|results?|broke\w*|status|state|go|went|say|says)\b|\b(fail\w*|pass\w*|broke\w*)\b.*\blabs?\b`)

// labAnswer says how the newest lab run of a change went: its failed
// checks and the explanation of its report.
func (s *Svc) labAnswer(ctx context.Context, p *progress, id string, a *Answer) bool {
	var ch *api.Change
	if err := p.do(ctx, "change", "change "+id, func() (string, error) {
		var err error
		ch, err = s.o.Tools.Change(ctx, id)
		if err != nil {
			return "not found", err
		}
		return stateWords(ch.State), nil
	}); err != nil || ch == nil {
		a.Text = fmt.Sprintf("I find no change %s. Check the id on the Changes page.", id)
		return true
	}
	addSource(a, llm.Source{Label: id, Ref: id, Kind: "change"})
	run := ch.LabRun
	if run == "" && len(ch.LabRuns) > 0 {
		run = ch.LabRuns[len(ch.LabRuns)-1].Run
	}
	if run == "" {
		a.Text = fmt.Sprintf("**%s** has no lab run yet.", id)
		return true
	}
	short := run
	if i := strings.LastIndex(run, "/"); i >= 0 {
		short = run[i+1:]
	}
	addSource(a, llm.Source{Label: "lab " + short, Ref: run, Kind: "lab"})
	who := fmt.Sprintf("Lab %s of **%s**", short, id)
	switch ch.LabState {
	case api.LabStatePass:
		a.Text = who + " passed on the head of the branch."
	case api.LabStateRunning:
		a.Text = who + " is running."
	case api.LabStateFail:
		fails, why, report := s.labFailures(ctx, p, ch)
		if report != "" {
			addSource(a, llm.Source{Label: shortPath(report), Ref: report, Kind: "kb"})
		}
		text := who + " failed"
		if len(fails) > 0 {
			text += ": " + strings.Join(fails, "; ")
		}
		text += "."
		if why != "" {
			text += " " + why
		}
		a.Text = text
	default:
		a.Text = fmt.Sprintf("%s: %s.", who, labStateWords(ch.LabState))
	}
	return true
}

func labStateWords(st api.LabState) string {
	if st == "" || st == api.LabStateNone {
		return "no result on record"
	}
	return string(st)
}

var reAction = regexp.MustCompile(`(?i)\b(?:can|could|would|will|please)\b.*\b(merge|approve|commit|push|close|deploy|revert|rebase|run the lab)\b`)

// advise runs an advisor recipe and renders its points.
func (s *Svc) advise(ctx context.Context, p *progress, req Request, id, recipe string, a *Answer) bool {
	n := p.begin("advise", fmt.Sprintf("advise %s %s", recipe, id))
	res, err := s.o.Advisor.Run(ctx, recipe, id)
	if err != nil {
		p.end(ctx, n, shortErr(err), true)
		a.Text = fmt.Sprintf("I cannot check %s: %s.", id, shortErr(err))
		return true
	}
	for i, src := range res.Reading {
		if i >= 5 {
			break
		}
		p.quick(ctx, src.Kind, "read "+src.Label, "")
	}
	p.end(ctx, n, plural(len(res.Points), "finding", "findings"), false)

	title := ""
	ch, _ := s.o.Tools.Change(ctx, id)
	if ch != nil {
		title = ch.Title
	}
	who := "**" + id + "**"
	if title != "" {
		who += " (" + title + ")"
	}
	if id == req.ChangeID && id != "" {
		who = "your change " + who
	}
	var b strings.Builder
	switch recipe {
	case advisor.Risks:
		// An OK point ("No findings") is neither a risk nor a note.
		risky, notes := 0, 0
		for _, pt := range res.Points {
			switch pt.Severity {
			case advisor.SevRisk, advisor.SevWarn:
				risky++
			case advisor.SevOK:
			default:
				notes++
			}
		}
		switch {
		case risky == 0 && notes == 0:
			fmt.Fprintf(&b, "I see no risks in %s.", who)
		case risky == 0:
			fmt.Fprintf(&b, "I see no blocking risk in %s, and %s to check:", who, plural(notes, "note", "notes"))
		case notes == 0:
			fmt.Fprintf(&b, "I see %s in %s:", plural(risky, "risk", "risks"), who)
		default:
			fmt.Fprintf(&b, "I see %s and %s in %s:", plural(risky, "risk", "risks"), plural(notes, "note", "notes"), who)
		}
	default:
		fmt.Fprintf(&b, recipeLeads[recipe], who)
	}
	// A failed lab: say what failed, from the report.
	var fails []string
	var why string
	if ch != nil && (recipe == advisor.Risks || recipe == advisor.LabGaps) && ch.LabState == api.LabStateFail {
		fails, why, _ = s.labFailures(ctx, p, ch)
	}
	for _, pt := range res.Points {
		text := pt.Text
		if len(fails) > 0 && strings.Contains(text, "see the report") {
			text = strings.Replace(text, "see the report", plural(len(fails), "check", "checks")+" failed ("+strings.Join(fails, "; ")+")", 1)
			if why != "" {
				text = strings.TrimSuffix(text, ".") + ". " + why
			}
		}
		fmt.Fprintf(&b, "\n- **%s.** %s", strings.TrimSuffix(pt.Title, "."), text)
		for _, src := range pt.Sources {
			addSource(a, src)
		}
	}
	if res.Note != "" {
		b.WriteString("\n\n" + res.Note)
	}
	for _, src := range res.Reading {
		addSource(a, src)
	}
	a.Text = b.String()
	a.Suggestions = append(a.Suggestions, res.Suggestions...)
	return true
}

// changeBrief describes a change: intent, files, lab, overlaps, blockers.
func (s *Svc) changeBrief(ctx context.Context, p *progress, req Request, id, q string, a *Answer) bool {
	var ch *api.Change
	if err := p.do(ctx, "change", "change "+id, func() (string, error) {
		var err error
		ch, err = s.o.Tools.Change(ctx, id)
		if err != nil {
			return "not found", err
		}
		return stateWords(ch.State), nil
	}); err != nil || ch == nil {
		a.Text = fmt.Sprintf("I find no change %s. Check the id on the Changes page.", id)
		return true
	}
	addSource(a, llm.Source{Label: id, Ref: id, Kind: "change"})
	if ch.Mr.Number > 0 {
		addSource(a, llm.Source{Label: fmt.Sprintf("MR !%d", ch.Mr.Number), Ref: fmt.Sprintf("!%d", ch.Mr.Number), Kind: "mr"})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** · %s · by %s · %s.", id, ch.Title, ch.Author, stateWords(ch.State))
	if ch.Intent != "" {
		fmt.Fprintf(&b, "\n- **Intent.** %s", endSentence(ch.Intent))
	}
	var files []string
	for _, f := range ch.ChangedFiles {
		if strings.HasPrefix(f.Path, ".pk/changes/") || strings.HasPrefix(f.Path, "kb/labs/") {
			continue
		}
		files = append(files, "`"+shortPath(f.Path)+"`")
	}
	if len(files) > 0 {
		more := ""
		if len(files) > 4 {
			more = fmt.Sprintf(" and %d more", len(files)-4)
			files = files[:4]
		}
		fmt.Fprintf(&b, "\n- **Changes.** %s%s", strings.Join(files, ", "), more)
		if ch.Impact.Text != "" {
			fmt.Fprintf(&b, "; rendered: %s", ch.Impact.Text)
		}
		if ch.Risk != "" {
			fmt.Fprintf(&b, ", risk %s", ch.Risk)
		}
		b.WriteString(".")
	}
	if lab := s.labLine(ctx, p, ch, a); lab != "" {
		fmt.Fprintf(&b, "\n- **Lab.** %s", lab)
	}
	for _, o := range ch.OverlapsDetail {
		var objs []string
		for _, ob := range o.Objects {
			objs = append(objs, ob.Kind+"/"+ob.Name)
		}
		other := "**" + o.Change + "**"
		if o.Change == req.ChangeID && req.ChangeID != "" {
			other = "your change " + other
		}
		fmt.Fprintf(&b, "\n- **Overlap.** %s (%s) also changes %s. The change that merges second must rebase and run the lab again.", other, o.By, strings.Join(objs, ", "))
		addSource(a, llm.Source{Label: o.Change, Ref: o.Change, Kind: "change"})
	}
	switch ch.State {
	case api.ChangeStateMerged, api.ChangeStateDeployed, api.ChangeStateVerified:
		var by []string
		for _, ap := range ch.ApprovalsBy {
			if ap.User != "" && ap.Counts {
				by = append(by, ap.User)
			}
		}
		line := ""
		if len(by) > 0 {
			line = "Approved by " + joinAnd(by)
		}
		if !ch.MergedAt.IsZero() {
			if line != "" {
				line += ", merged "
			} else {
				line = "Merged "
			}
			line += ch.MergedAt.Format("2006-01-02")
		}
		if line != "" {
			fmt.Fprintf(&b, "\n- **Merged.** %s.", line)
		}
	case api.ChangeStateClosed:
	default:
		if len(ch.Gates.Blockers) > 0 {
			fmt.Fprintf(&b, "\n- **Before merge.** %s.", upperFirst(strings.Join(ch.Gates.Blockers, "; ")))
		} else {
			b.WriteString("\n- **Before merge.** Nothing blocks it; it can merge.")
		}
	}
	a.Text = b.String()
	return true
}

// labLine describes the newest lab run of a change.
func (s *Svc) labLine(ctx context.Context, p *progress, ch *api.Change, a *Answer) string {
	if ch.LabRun == "" && len(ch.LabRuns) == 0 {
		return "no run yet."
	}
	run := ch.LabRun
	if run == "" {
		run = ch.LabRuns[len(ch.LabRuns)-1].Run
	}
	short := run
	if i := strings.LastIndex(run, "/"); i >= 0 {
		short = run[i+1:]
	}
	addSource(a, llm.Source{Label: "lab " + short, Ref: run, Kind: "lab"})
	switch ch.LabState {
	case api.LabStatePass:
		return short + " passed on the head of the branch."
	case api.LabStateRunning:
		return short + " is running."
	case api.LabStateFail:
		fails, why, report := s.labFailures(ctx, p, ch)
		if report != "" {
			addSource(a, llm.Source{Label: shortPath(report), Ref: report, Kind: "kb"})
		}
		text := short + " failed"
		if len(fails) > 0 {
			text += ": " + strings.Join(fails, "; ")
		}
		text += "."
		if why != "" {
			text += " " + why
		}
		return text
	case api.LabStateNone, "":
		// A merged change from the history has no lab state of this bench.
		return ""
	}
	return short + ": " + string(ch.LabState) + "."
}

var (
	reFailLine = regexp.MustCompile(`(?im)^\s*[-*]\s*FAIL:\s*(.+)$`)
	reLabRunID = regexp.MustCompile(`/?(run-\d+)$`)
)

// labFailures returns the failed checks of the newest lab run, a sentence
// that explains them (from the report) and the report path.
func (s *Svc) labFailures(ctx context.Context, p *progress, ch *api.Change) (fails []string, why, report string) {
	run := ch.LabRun
	for _, r := range ch.LabRuns {
		if run == "" || r.Run == run {
			report = r.Report
			run = r.Run
		}
	}
	// A run of this bench has its failures.
	if runs, err := s.o.Tools.LabRuns(ctx, ch.Id); err == nil {
		for _, r := range runs {
			if (r.Id == run || run == "") && len(r.Failures) > 0 {
				return r.Failures, "", report
			}
		}
	}
	if report == "" {
		return nil, "", ""
	}
	_ = p.do(ctx, "read_file", "read "+shortPath(report), func() (string, error) {
		fc, err := s.o.Tools.ReadFile(ctx, ch.Id, report)
		if err != nil || !fc.Exists {
			if fc, err = s.o.Tools.ReadFile(ctx, "main", report); err != nil {
				return "", err
			}
		}
		for _, m := range reFailLine.FindAllStringSubmatch(fc.Content, -1) {
			fails = append(fails, strings.TrimSpace(strings.TrimPrefix(m[1], "bgp: ")))
		}
		// The explanation is the last paragraph of the report.
		blocks := parseMD(fc.Content)
		for i := len(blocks) - 1; i >= 0; i-- {
			if blocks[i].kind == "p" {
				why = firstSentences(blocks[i].text, 2, 260)
				break
			}
			if blocks[i].kind != "code" {
				break
			}
		}
		return plural(len(fails), "check failed", "checks failed"), nil
	})
	return fails, why, report
}

func stateWords(st api.ChangeState) string {
	switch st {
	case api.ChangeStateLab:
		return "in lab"
	case api.ChangeStateReview:
		return "in review"
	case "":
		return "open"
	}
	return string(st)
}

// ---------------------------------------------------------------- open changes and history

var (
	reOpenChanges = regexp.MustCompile(`(?i)\b(open|pending|active|current|in review|in flight)\b.*\bchanges\b|\bwhich changes\b|\bwhat changes are\b|\bwhat('s| is) (open|pending|in review|going on|in flight)\b|\bstatus of (the |all )?changes\b|\blist (the |all )?changes\b|\bwhat (can|could) (i|we) merge\b`)
	reRecent      = regexp.MustCompile(`(?i)\b(recent(ly)?|latest|last (week|month|few|merges?|changes?)|lately)\b|\bwhat (was|got|has been|have been) merged\b|\bmerge history\b`)
)

func (s *Svc) openChanges(ctx context.Context, p *progress, _ Request, q string, a *Answer) bool {
	if !reOpenChanges.MatchString(q) {
		return false
	}
	list := s.openList(ctx, p)
	if list == "" {
		a.Text = "No change is open right now."
		return true
	}
	a.Text = list + "\n\nAsk about one of them, for example: \"What are the risks of " + firstID(list) + "?\""
	return true
}

var reListID = regexp.MustCompile(`\*\*(c-[a-z0-9]{6})\*\*`)

func firstID(list string) string {
	if m := reListID.FindStringSubmatch(list); m != nil {
		return m[1]
	}
	return "c-p4x8ma"
}

// openList is a Markdown list of the open changes with a lead sentence.
func (s *Svc) openList(ctx context.Context, p *progress) string {
	var open []api.ChangeSummary
	_ = p.do(ctx, "change", "list open changes", func() (string, error) {
		all, err := s.o.Tools.Changes(ctx)
		for _, c := range all {
			switch c.State {
			case api.ChangeStateMerged, api.ChangeStateDeployed, api.ChangeStateVerified, api.ChangeStateClosed:
				continue
			}
			open = append(open, c)
		}
		return plural(len(open), "open", "open"), err
	})
	if len(open) == 0 {
		return ""
	}
	approved, failed := 0, 0
	var b strings.Builder
	for _, c := range open {
		line := fmt.Sprintf("\n- **%s** %s · %s · %s", c.Id, c.Title, c.Author, stateWords(c.State))
		if c.State == api.ChangeStateApproved {
			approved++
		}
		if c.LabState == api.LabStateFail {
			failed++
			line += " · lab failed"
		}
		if len(c.Overlaps) > 0 {
			line += " · overlaps " + strings.Join(c.Overlaps, ", ")
		}
		b.WriteString(line)
	}
	lead := fmt.Sprintf("%s open.", plural(len(open), "change is", "changes are"))
	var parts []string
	if approved > 0 {
		parts = append(parts, fmt.Sprintf("%d approved", approved))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d with a failed lab", failed))
	}
	if len(parts) > 0 {
		lead += " " + upperFirst(joinAnd(parts)) + "."
	}
	return lead + b.String()
}

func (s *Svc) recentHistory(ctx context.Context, p *progress, _ Request, q string, a *Answer) bool {
	if !reRecent.MatchString(q) || len(advisor.EntityNames(q)) > 0 || isPlatformVersionQ(q) {
		return false
	}
	var entries []api.HistoryEntry
	if err := p.do(ctx, "history", "history main", func() (string, error) {
		var err error
		entries, err = s.o.Tools.History(ctx, api.ListHistoryParams{Limit: 6})
		return plural(len(entries), "merge", "merges"), err
	}); err != nil || len(entries) == 0 {
		return false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The last %s on main:", plural(min(len(entries), 5), "merge", "merges"))
	for i, h := range entries {
		if i >= 5 {
			break
		}
		id := h.Change
		if id == "" {
			id = shortSHA(h.Sha)
		}
		fmt.Fprintf(&b, "\n- **%s** %s · %s · %s", id, h.Title, h.Author, h.Date.Format("2006-01-02"))
		if h.BreakGlass != "" {
			b.WriteString(" · break-glass")
		}
		if h.Change != "" {
			addSource(a, llm.Source{Label: "trailer " + h.Change, Ref: h.Change, Kind: "trailer"})
		}
	}
	a.Text = b.String()
	return true
}

// ---------------------------------------------------------------- entities

var (
	reSwitchName = regexp.MustCompile(`\b(data-[a-z]|mgmt|spine-[a-z0-9]+)\b`)
	reRackName   = regexp.MustCompile(`(?i)\b[a-z]{1,3}\d[a-z0-9]{0,3}-\d{1,2}[a-z]\b`)
	rePolicyRef  = regexp.MustCompile(`\brack-[0-9a-z]+\b`)
	reWorkItem   = regexp.MustCompile(`\bw-\d{3}\b`)
)

// entityCandidates returns names in a question with the kinds to try, in
// order of appearance.
func entityCandidates(q string) ([]string, map[string][]string) {
	kinds := map[string][]string{}
	var names []string
	add := func(name string, k ...string) {
		if _, ok := kinds[name]; !ok {
			names = append(names, name)
		}
		kinds[name] = append(kinds[name], k...)
	}
	for _, n := range advisor.EntityNames(q) {
		switch {
		case strings.HasPrefix(n, "jb-"):
			add(n, "Jbod")
		default:
			add(n, "Node", "Jbod", "Switch")
		}
	}
	for _, n := range reWorkItem.FindAllString(q, -1) {
		add(n, "WorkItem")
	}
	for _, n := range rePolicyRef.FindAllString(q, -1) {
		add(n, "CiliumBGPPeeringPolicy")
	}
	for _, n := range reRackName.FindAllString(q, -1) {
		if !rePolicyRef.MatchString(n) {
			add(strings.ToUpper(n), "Rack")
		}
	}
	for _, n := range reSwitchName.FindAllString(q, -1) {
		add(n, "Switch")
	}
	return names, kinds
}

// reAboutName is a short question about one plain name: "tell me about
// defiant", "what is pegasus?".
var reAboutName = regexp.MustCompile(`(?i)^\s*(tell me about|what is|what's|describe|show me|show|info on|about)\s+(the\s+)?(cluster\s+)?([a-z][a-z0-9-]{2,})(\s+cluster)?\s*[?.!]?\s*$`)

func (s *Svc) entityQuestion(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	if reHowTo.MatchString(q) {
		return false
	}
	names, kinds := entityCandidates(q)
	// A plain name (a cluster): only in a short question about it.
	if len(names) == 0 {
		m := reAboutName.FindStringSubmatch(q)
		if m == nil || commonWord[strings.ToLower(m[4])] {
			return false
		}
		w := strings.ToLower(m[4])
		names, kinds[w] = []string{w}, []string{"Cluster", "HelmRelease", "Kustomization"}
	}
	// The selected change first (a node can be new there), then main.
	refs := []string{""}
	if req.ChangeID != "" {
		refs = []string{req.ChangeID, ""}
	}
	for i, name := range names {
		if i >= 3 {
			break
		}
		var e *api.Entity
		for _, ref := range refs {
			for _, k := range kinds[name] {
				if got, err := s.o.Tools.Entity(ctx, k, name, ref); err == nil && got != nil && got.Name != "" {
					e = got
					break
				}
			}
			if e != nil {
				break
			}
		}
		if e == nil {
			continue
		}
		where := ""
		if req.ChangeID != "" && e.Change.Id == req.ChangeID && e.Change.Status == api.FileChangeAdded {
			where = req.ChangeID
		}
		sum := fieldCount(e)
		if where != "" {
			sum = e.Kind + " · new in " + where
		}
		p.quick(ctx, "entity", "entity "+e.Kind+"/"+e.Name, sum)
		s.renderEntity(e, q, where, a)
		if e.Kind == "Switch" {
			s.switchPorts(ctx, p, e, a)
		}
		return true
	}
	// A node name that is nowhere: say so, with the names that exist.
	if ns := advisor.EntityNames(q); len(ns) > 0 {
		return s.unknownEntity(ctx, p, req, ns[0], a)
	}
	return false
}

// unknownEntity answers a question about a name that is not in the
// inventory of the change or of main.
func (s *Svc) unknownEntity(ctx context.Context, p *progress, req Request, name string, a *Answer) bool {
	ref := req.ChangeID
	if ref == "" {
		ref = "main"
	}
	var have []string
	_ = p.do(ctx, "search", "find "+name+" in the inventory", func() (string, error) {
		tree, err := s.o.Tools.Tree(ctx, ref)
		if err != nil {
			return "", err
		}
		prefix := name[:strings.Index(name, "-")+1]
		var walk func([]api.FileNode)
		walk = func(ns []api.FileNode) {
			for _, n := range ns {
				walk(n.Children)
				base := strings.TrimSuffix(shortPath(n.Path), ".yaml")
				if n.Type == api.FileNodeTypeFile && strings.HasPrefix(n.Path, "control/inventory/") && strings.HasPrefix(base, prefix) {
					have = append(have, base)
				}
			}
		}
		walk(tree)
		return "not found", nil
	})
	where := "on main"
	if req.ChangeID != "" {
		where = "in your change **" + req.ChangeID + "** or on main"
	}
	a.Text = fmt.Sprintf("I find no **%s** %s.", name, where)
	if len(have) > 0 {
		sort.Strings(have)
		a.Text += fmt.Sprintf(" The inventory has %s.", nameRange(have))
	}
	a.Text += " To add it, ask me \"Add " + name + " to the inventory\" for the steps."
	return true
}

var commonWord = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`what which where show tell describe about status info details there their them they this that these those
		does have with from into your mine ours here when then than also just only some many much more most other again
		cluster clusters node nodes rack racks switch switches change changes policy policies file files work item items
		please could would should shall will can know need want look find give list explain summary summarize how why who
		running runs used uses using happen happened happens state repo main merge merged lab labs test tests`) {
		commonWord[w] = true
	}
}

func fieldCount(e *api.Entity) string {
	n := 0
	for _, b := range e.Backlinks.Entities {
		if b.Kind != "" {
			n++
		}
	}
	if n > 0 {
		return e.Kind + " · " + plural(n, "link", "links")
	}
	return e.Kind
}

var reWhich = regexp.MustCompile(`(?i)\b(which|list|what)\b`)

// renderEntity writes a short description of an entity.
func (s *Svc) renderEntity(e *api.Entity, q, newIn string, a *Answer) {
	var b strings.Builder
	lead := entityLead(e)
	b.WriteString(lead.text)
	if newIn != "" {
		fmt.Fprintf(&b, " It is new in your change **%s** and not on main yet.", newIn)
	}
	shown := 0
	for _, f := range e.Fields {
		if shown >= 8 {
			break
		}
		d := fieldDisplay(f)
		if d == "" || f.Label == "Labels" || f.Label == "Id" || (e.Kind == "WorkItem" && strings.HasSuffix(d, " items")) ||
			(f.Field == "spec.cluster" && e.Cluster != "") || lead.used[f.Field] {
			continue
		}
		owner := ""
		if f.Owner != "" {
			owner = " _(" + f.Owner + ")_"
		}
		fmt.Fprintf(&b, "\n- **%s:** %s%s", f.Label, d, owner)
		shown++
	}
	byKind := map[string][]string{}
	var order []string
	for _, l := range e.Backlinks.Entities {
		if l.Kind == "" {
			continue
		}
		if _, ok := byKind[l.Kind]; !ok {
			order = append(order, l.Kind)
		}
		byKind[l.Kind] = append(byKind[l.Kind], l.Name)
	}
	if len(order) > 0 {
		var parts []string
		for _, k := range order {
			names := nameRange(byKind[k])
			if reWhich.MatchString(q) && len(byKind[k]) <= 12 {
				names = strings.Join(byKind[k], ", ")
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", pluralKind(len(byKind[k]), k), names))
		}
		fmt.Fprintf(&b, "\n- **Linked:** %s", strings.Join(parts, ", "))
	}
	if cs := e.Backlinks.Changes; len(cs) > 0 {
		var parts []string
		for i, c := range cs {
			if i >= 2 {
				break
			}
			part := fmt.Sprintf("%s %s", c.Id, c.Title)
			if c.Author != "" && !c.Date.IsZero() {
				part += fmt.Sprintf(" (%s, %s)", c.Author, c.Date.Format("2006-01-02"))
			}
			parts = append(parts, part)
			if strings.HasPrefix(c.Id, "c-") {
				addSource(a, llm.Source{Label: "trailer " + c.Id, Ref: c.Id, Kind: "trailer"})
			}
		}
		fmt.Fprintf(&b, "\n- **Last changes:** %s", strings.Join(parts, "; "))
	}
	a.Text = b.String()
	addSource(a, llm.Source{Label: e.Name, Ref: e.Kind + "/" + e.Name, Kind: "entity"})
	if newIn != "" {
		addSource(a, llm.Source{Label: newIn, Ref: newIn, Kind: "change"})
	}
	for i, art := range e.Backlinks.Articles {
		if i >= 2 {
			break
		}
		addSource(a, llm.Source{Label: shortPath(art.Path), Ref: art.Path, Kind: "kb"})
	}
}

var rePortAsk = regexp.MustCompile(`(?i)\bports?\b|\bfree\b`)

// switchPorts adds the ports in use and the free ports of a switch, from
// the nodes that are cabled to it.
func (s *Svc) switchPorts(ctx context.Context, p *progress, sw *api.Entity, a *Answer) {
	total, uplinks := 0, []int{}
	for _, f := range sw.Fields {
		switch f.Field {
		case "spec.ports":
			if n, ok := f.Value.(float64); ok {
				total = int(n)
			}
		case "spec.uplinks", "spec.uplinkPorts":
			if l, ok := f.Value.([]any); ok {
				for _, v := range l {
					if n, ok := v.(float64); ok {
						uplinks = append(uplinks, int(n))
					}
				}
			}
		}
	}
	if total == 0 {
		return
	}
	var nodes []string
	for _, l := range sw.Backlinks.Entities {
		if l.Kind == "Node" {
			nodes = append(nodes, l.Name)
		}
	}
	used := map[int]string{}
	for _, u := range uplinks {
		used[u] = "uplink"
	}
	n := p.begin("entity", fmt.Sprintf("read %s on %s", plural(len(nodes), "node", "nodes"), sw.Name))
	for _, name := range nodes {
		e, err := s.o.Tools.Entity(ctx, "Node", name, "")
		if err != nil {
			continue
		}
		for _, f := range e.Fields {
			ports, _ := f.Value.([]any)
			if !strings.HasSuffix(f.Field, "ports") {
				continue
			}
			for _, pv := range ports {
				m, _ := pv.(map[string]any)
				peer, _ := m["peer"].(map[string]any)
				if peer == nil || peer["switch"] != sw.Name {
					continue
				}
				if port, ok := peer["port"].(float64); ok {
					used[int(port)] = name
				}
			}
		}
	}
	var free []int
	for i := 1; i <= total; i++ {
		if _, ok := used[i]; !ok {
			free = append(free, i)
		}
	}
	p.end(ctx, n, fmt.Sprintf("%d of %d ports used", len(used), total), false)
	a.Text += fmt.Sprintf("\n- **Ports in use:** %d of %d (%s, %s)", len(used), total,
		plural(len(used)-len(uplinks), "node port", "node ports"), plural(len(uplinks), "uplink", "uplinks"))
	a.Text += fmt.Sprintf("\n- **Free:** %s (%d)", portRanges(free), len(free))
}

// portRanges writes 1-8, 10, 12-20.
func portRanges(ns []int) string {
	if len(ns) == 0 {
		return "none"
	}
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] == ns[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d–%d", ns[i], ns[j]))
		} else {
			parts = append(parts, fmt.Sprint(ns[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func kindWords(k string) string {
	switch k {
	case "CiliumBGPPeeringPolicy":
		return "BGP peering policy"
	case "WorkItem":
		return "work item"
	case "Jbod":
		return "disk shelf (JBOD)"
	}
	return k
}

func pluralKind(n int, k string) string {
	w := kindWords(k)
	if n == 1 {
		return "1 " + w
	}
	if strings.HasSuffix(w, "y") && !strings.HasSuffix(w, "ey") {
		return fmt.Sprintf("%d %sies", n, w[:len(w)-1])
	}
	if strings.HasSuffix(w, "h") {
		return fmt.Sprintf("%d %ses", n, w)
	}
	return fmt.Sprintf("%d %ss", n, w)
}

// nameRange shortens a long list of names: cn-025 … cn-034.
func nameRange(names []string) string {
	if len(names) <= 4 {
		return strings.Join(names, ", ")
	}
	s := append([]string(nil), names...)
	sort.Strings(s)
	return s[0] + " … " + s[len(s)-1]
}

// fieldDisplay formats a field; ports become "p2 → data-a 37".
func fieldDisplay(f api.EntityField) string {
	if ports, ok := f.Value.([]any); ok && strings.HasSuffix(f.Field, "ports") {
		var out []string
		for _, pv := range ports {
			m, _ := pv.(map[string]any)
			peer, _ := m["peer"].(map[string]any)
			if m == nil || peer == nil {
				continue
			}
			out = append(out, fmt.Sprintf("%v → %v %v", m["name"], peer["switch"], peer["port"]))
		}
		if len(out) > 0 {
			return strings.Join(out, ", ")
		}
	}
	if l, ok := f.Value.([]any); ok && len(l) > 0 && len(l) <= 8 {
		var parts []string
		for _, v := range l {
			switch v.(type) {
			case string, float64, bool:
				parts = append(parts, fmt.Sprint(v))
			}
		}
		if len(parts) == len(l) {
			return strings.Join(parts, ", ")
		}
	}
	d := strings.TrimSpace(f.Display)
	if d == "" {
		if v, ok := f.Value.(string); ok {
			d = v
		}
	}
	return clip(d, 140)
}

// ---------------------------------------------------------------- retrieval

var reHowTo = regexp.MustCompile(`(?i)^\s*how (do|can|should|would|to)\b`)

var reHow = regexp.MustCompile(`(?i)^\s*how\b|\bsteps?\b|\bprocedure\b|\bwhat do i (do|need)\b|\brunbook\b`)

// retrieval answers with the best passage of the best hit, and lists the
// related hits as sources. A hit that shares no word with the question is
// no answer.
func (s *Svc) retrieval(ctx context.Context, p *progress, req Request, q string, a *Answer) {
	var hits []llm.Hit
	err := p.do(ctx, "search", "search "+quoteQ(q), func() (string, error) {
		var err error
		hits, err = s.o.Tools.Search(ctx, q, 5)
		return plural(len(hits), "hit", "hits"), err
	})
	nothing := "I found nothing about that in the project: not in the state repo, the KB, the merge history, the MR discussions or the lab reports.\n\nI answer best questions like these:\n" + examples(req, 3)
	if err != nil || len(hits) == 0 {
		a.Text = nothing
		return
	}
	ct := contentTerms(q)
	related := func(h llm.Hit) int { return overlap(ct, h.Title+" "+h.Ref+" "+h.Text) }
	top := pickHit(hits, q)
	match := related(top)
	if len(ct) > 0 && match == 0 {
		a.Text = nothing
		return
	}
	p.quick(ctx, "read_file", "read "+hitSource(top).Label, "")
	switch top.Kind {
	case "trailer":
		a.Text = trailerAnswer(top)
	case "mr":
		a.Text = mrAnswer(top, q)
	case "lab":
		if strings.HasPrefix(top.Text, "---") {
			a.Text = kbAnswer(top, q)
		} else {
			a.Text = labHitAnswer(top)
		}
	case "kb":
		a.Text = kbAnswer(top, q)
	default:
		a.Text = fileAnswer(top, q, ct)
	}
	if len(ct) >= 2 && match*2 < len(ct) {
		a.Text = "I found no direct answer. The closest record: " + lowerLead(a.Text)
	}
	addSource(a, hitSource(top))
	for _, h := range hits {
		if len(a.Sources) >= 4 {
			break
		}
		if related(h) > 0 {
			addSource(a, hitSource(h))
		}
	}
}

// contentTerms are the words of a question that carry its topic.
func contentTerms(q string) map[string]bool {
	out := map[string]bool{}
	for _, t := range llm.Tokenize(q) {
		if commonWord[t] || len(t) < 2 {
			continue
		}
		out[t] = true
	}
	return out
}

// lowerLead lower-cases the first letter of a text unless it is Markdown
// or a name.
func lowerLead(s string) string {
	if s == "" || !(s[0] >= 'A' && s[0] <= 'Z') {
		return s
	}
	return lowerFirst(s)
}

// pickHit chooses the hit to answer from among the first three: its rank,
// how well its title matches, and the kind that suits the question (a
// trailer says why, a runbook says how, a KB article explains).
func pickHit(hits []llm.Hit, q string) llm.Hit {
	terms := termSet(q)
	why, how := isWhy(q), reHow.MatchString(q)
	best, bestScore := hits[0], -1
	for i, h := range hits[:min(len(hits), 3)] {
		score := 3 - i + min(overlap(terms, h.Title), 3)
		switch {
		case h.Kind == "trailer" && why:
			score += 2
		case h.Kind == "kb" && how && strings.Contains(h.Ref, "runbook"):
			score += 4
		case h.Kind == "kb":
			score += 2
		}
		if score > bestScore {
			best, bestScore = h, score
		}
	}
	return best
}

// quoteQ shortens a question to its search words: 'add compute node'.
func quoteQ(q string) string {
	words := llm.Tokenize(q)
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		// cn-003 also yields cn and 003: keep the whole name only.
		part := false
		for _, o := range out {
			if strings.ContainsAny(o, "-./") && strings.Contains(o, w) {
				part = true
			}
		}
		if !seen[w] && !part && len(out) < 4 {
			seen[w] = true
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return "'" + clip(q, 30) + "'"
	}
	return "'" + strings.Join(out, " ") + "'"
}

var reTrailer = regexp.MustCompile(`(?m)^([A-Z][A-Za-z]*(?:-[A-Za-z]+)*):\s*(.+)$`)

func trailers(text string) map[string]string {
	out := map[string]string{}
	for _, m := range reTrailer.FindAllStringSubmatch(text, -1) {
		if _, ok := out[m[1]]; !ok {
			out[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return out
}

var reMergedBy = regexp.MustCompile(`merged (\d{4}-\d{2}-\d{2}) by ([^,\n]+)`)

func trailerAnswer(h llm.Hit) string {
	t := trailers(h.Text)
	title := h.Title
	if i := strings.Index(title, ": "); i >= 0 {
		title = title[i+2:]
	}
	id := t["Pk-Change"]
	if id == "" {
		id = h.Ref
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From the merge history: change **%s**, *%s*.", id, title)
	if in := t["Pk-Intent"]; in != "" {
		fmt.Fprintf(&b, " Its intent: %s", endSentence(lowerFirst(in)))
	}
	var parts []string
	if au := t["Pk-Author"]; au != "" {
		parts = append(parts, "author "+au)
	}
	if ap := t["Approved-by"]; ap != "" {
		parts = append(parts, "approved by "+joinAnd(splitList(ap)))
	}
	if m := reMergedBy.FindStringSubmatch(h.Text); m != nil {
		parts = append(parts, "merged "+m[1])
	}
	if len(parts) > 0 {
		b.WriteString(" " + upperFirst(strings.Join(parts, ", ")) + ".")
	}
	if lr := t["Pk-Lab-Run"]; lr != "" {
		f := strings.Fields(lr)
		if len(f) >= 2 {
			res := map[string]string{"pass": "passed", "fail": "failed"}[f[1]]
			if res == "" {
				res = f[1]
			}
			fmt.Fprintf(&b, " Lab %s %s.", f[0], res)
		}
	}
	if bg := t["Pk-Break-Glass"]; bg != "" {
		fmt.Fprintf(&b, " It records a break-glass use: %s.", bg)
	}
	return b.String()
}

var reUserLine = regexp.MustCompile(`^([a-z][a-z0-9_.-]{1,30}):\s+(.+)$`)

func mrAnswer(h llm.Hit, q string) string {
	lines := strings.Split(h.Text, "\n")
	terms := termSet(q)
	var intent string
	type comment struct {
		who, text string
		score     int
	}
	var comments []comment
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if i == 0 || l == "" || reTrailer.MatchString(l) && !reUserLine.MatchString(l) {
			continue
		}
		if m := reUserLine.FindStringSubmatch(l); m != nil {
			comments = append(comments, comment{m[1], m[2], overlap(terms, m[2])})
			continue
		}
		if intent == "" {
			intent = l
		}
	}
	title := strings.TrimPrefix(h.Title, "MR "+h.Ref+" ")
	var b strings.Builder
	fmt.Fprintf(&b, "From the discussion in **MR %s**, *%s*:", h.Ref, title)
	if intent != "" {
		b.WriteString(" " + endSentence(upperFirst(firstSentences(intent, 2, 240))))
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].score > comments[j].score })
	for i, c := range comments {
		if i >= 2 {
			break
		}
		fmt.Fprintf(&b, "\n\n> **%s:** %s", c.who, firstSentences(c.text, 2, 240))
	}
	return b.String()
}

func labHitAnswer(h llm.Hit) string {
	lines := strings.Split(strings.TrimSpace(h.Text), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "Lab **%s**", h.Ref)
	if len(lines) > 0 {
		if _, res, ok := strings.Cut(lines[0], ": "); ok {
			fmt.Fprintf(&b, " ended with **%s**.", res)
		} else {
			b.WriteString(".")
		}
	}
	n := 0
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "|") {
			continue
		}
		if n >= 4 {
			break
		}
		fmt.Fprintf(&b, "\n- %s", clip(l, 160))
		n++
	}
	return b.String()
}

func kbAnswer(h llm.Hit, q string) string {
	text := h.Text
	if strings.TrimSpace(text) == "" {
		text = strings.ReplaceAll(h.Snippet, " · ", "\n\n")
	}
	meta, body := frontMatter(text)
	title := meta["title"]
	if title == "" {
		title = h.Title
	}
	title = strings.Trim(title, `"'`)
	blocks := parseMD(body)
	var lead string
	switch meta["type"] {
	case "runbook":
		lead = fmt.Sprintf("The runbook **%s** covers this.", title)
	case "incident":
		lead = fmt.Sprintf("The incident report **%s** says:", title)
	case "lab-report":
		lead = fmt.Sprintf("The lab report **%s** says:", title)
	default:
		lead = fmt.Sprintf("The KB article **%s** says:", title)
	}
	// A "how" question to a runbook: its numbered steps.
	if meta["type"] == "runbook" && reHow.MatchString(q) {
		var steps []mdBlock
		var code string
		var section string
		for _, bl := range blocks {
			if bl.kind == "ol" && (section == "" || bl.section == section) {
				section = bl.section
				steps = append(steps, bl)
			}
			if bl.kind == "code" && code == "" && section != "" && bl.section == section && len(steps) > 0 {
				code = bl.text
			}
		}
		if len(steps) >= 2 {
			var b strings.Builder
			fmt.Fprintf(&b, "The runbook **%s** has %d steps:\n", title, len(steps))
			for i, st := range steps {
				fmt.Fprintf(&b, "\n%d. %s", i+1, strings.TrimSuffix(firstSentences(st.text, 1, 200), ":"))
			}
			if code != "" {
				fmt.Fprintf(&b, "\n\nStep 1 starts with:\n\n```\n%s\n```", code)
			}
			return b.String()
		}
	}
	best := bestBlocks(blocks, q, 3)
	var b strings.Builder
	b.WriteString(lead)
	inList := false
	for i, bl := range best {
		switch bl.kind {
		case "table":
			b.WriteString("\n\n" + bl.text)
			inList = false
		case "li", "ol":
			if !inList {
				b.WriteString("\n")
			}
			b.WriteString("\n- " + firstSentences(bl.text, 2, 260))
			inList = true
		case "code":
			b.WriteString("\n\n```\n" + bl.text + "\n```")
			inList = false
		default:
			if i == 0 && meta["type"] != "runbook" {
				b.WriteString(" " + firstSentences(bl.text, 3, 360))
			} else {
				b.WriteString("\n\n" + firstSentences(bl.text, 3, 360))
			}
			inList = false
		}
	}
	return b.String()
}

// fileAnswer says what a file defines in one or two sentences, from its
// fields; never the whole file. A summary that names none of the key terms
// of the question answers another question ("Which Kubernetes version?" is
// not "chart cilium 1.18.2"): then the answer only points at the file.
func fileAnswer(h llm.Hit, q string, terms map[string]bool) string {
	path := h.Path
	if path == "" {
		path = h.Ref
	}
	if sum, ok := fileSummary(path, h.Text, terms); ok && namesKeyTerm(sum, path, q) {
		return sum + fmt.Sprintf(" Source: `%s` on main.", path)
	}
	return fmt.Sprintf("The file `%s` on main matches best. Open it from the source below.", path)
}

// namesKeyTerm reports whether a summary names a key term of the question:
// a word that can name an app or a topic, not a question word and not the
// cluster of the file. A question without key terms takes any summary.
func namesKeyTerm(sum, path, q string) bool {
	cluster := clusterOfPath(path)
	if m := reTalconfig.FindStringSubmatch(path); m != nil {
		cluster = m[1]
	}
	key := appTerms(q, map[string]bool{cluster: true})
	return len(key) == 0 || overlap(key, sum) > 0
}

// ---------------------------------------------------------------- Markdown helpers

type mdBlock struct {
	kind    string // p, li, ol, table, code
	text    string
	section string
}

var (
	reFront   = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n?`)
	reOLItem  = regexp.MustCompile(`^\s*\d+[.)]\s+`)
	reULItem  = regexp.MustCompile(`^\s*[-*+]\s+`)
	reMDLink  = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	reSpaces  = regexp.MustCompile(`\s+`)
	reHeading = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
)

// frontMatter splits YAML front matter (simple key: value lines) from the
// body.
func frontMatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	m := reFront.FindStringSubmatchIndex(text)
	if m == nil {
		return meta, text
	}
	for _, l := range strings.Split(text[m[2]:m[3]], "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			meta[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return meta, text[m[1]:]
}

// parseMD splits Markdown into paragraphs, list items, tables and code
// blocks, each with the heading above it.
func parseMD(text string) []mdBlock {
	_, body := frontMatter(text)
	var out []mdBlock
	section := ""
	var cur *mdBlock
	flush := func() {
		if cur != nil {
			if cur.kind != "code" && cur.kind != "table" {
				cur.text = strings.TrimSpace(reSpaces.ReplaceAllString(reMDLink.ReplaceAllString(cur.text, "$1"), " "))
			}
			if strings.TrimSpace(cur.text) != "" {
				out = append(out, *cur)
			}
			cur = nil
		}
	}
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "```"):
			flush()
			var code []string
			indent := len(l) - len(strings.TrimLeft(l, " "))
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				cl := lines[i]
				if len(cl) >= indent && strings.TrimSpace(cl[:indent]) == "" {
					cl = cl[indent:]
				}
				code = append(code, cl)
			}
			out = append(out, mdBlock{kind: "code", text: strings.Join(code, "\n"), section: section})
		case reHeading.MatchString(t):
			flush()
			section = reHeading.FindStringSubmatch(t)[1]
		case t == "":
			flush()
		case strings.HasPrefix(t, "|"):
			if cur == nil || cur.kind != "table" {
				flush()
				cur = &mdBlock{kind: "table", section: section}
			}
			if cur.text != "" {
				cur.text += "\n"
			}
			cur.text += t
		case reOLItem.MatchString(l) && (cur == nil || cur.kind != "code"):
			flush()
			cur = &mdBlock{kind: "ol", text: reOLItem.ReplaceAllString(l, ""), section: section}
		case reULItem.MatchString(l):
			flush()
			cur = &mdBlock{kind: "li", text: reULItem.ReplaceAllString(l, ""), section: section}
		default:
			if cur == nil {
				cur = &mdBlock{kind: "p", section: section}
			}
			cur.text += " " + t
		}
	}
	flush()
	return out
}

func termSet(q string) map[string]bool {
	m := map[string]bool{}
	for _, t := range llm.Tokenize(q) {
		m[t] = true
	}
	return m
}

func overlap(terms map[string]bool, text string) int {
	n := 0
	seen := map[string]bool{}
	for _, t := range llm.Tokenize(text) {
		if terms[t] && !seen[t] {
			seen[t] = true
			n++
		}
	}
	return n
}

// bestBlocks returns up to n blocks that match the question best, in
// document order; without a match, the first paragraphs.
func bestBlocks(blocks []mdBlock, q string, n int) []mdBlock {
	terms := termSet(q)
	type scored struct {
		i     int
		score float64
	}
	var ss []scored
	for i, b := range blocks {
		if b.kind == "code" {
			continue
		}
		sc := float64(overlap(terms, b.text)) + 0.5*float64(overlap(terms, b.section))
		if b.kind == "table" && strings.Count(b.text, "\n") > 10 {
			sc -= 1
		}
		if sc > 0 {
			ss = append(ss, scored{i, sc})
		}
	}
	sort.SliceStable(ss, func(a, b int) bool { return ss[a].score > ss[b].score })
	var pick []int
	for _, s := range ss {
		if len(pick) >= n {
			break
		}
		pick = append(pick, s.i)
	}
	if len(pick) == 0 {
		for i, b := range blocks {
			if b.kind == "p" && len(pick) < 2 {
				pick = append(pick, i)
			}
		}
	}
	sort.Ints(pick)
	out := make([]mdBlock, 0, len(pick))
	for _, i := range pick {
		out = append(out, blocks[i])
	}
	return out
}

// firstSentences returns up to n whole sentences, at most max bytes; a
// longer first sentence ends at a word with "…".
func firstSentences(text string, n, max int) string {
	text = strings.TrimSpace(text)
	var out strings.Builder
	count := 0
	for _, s := range sentences(text) {
		if count >= n || (count > 0 && out.Len()+1+len(s) > max) {
			break
		}
		if count > 0 {
			out.WriteString(" ")
		}
		out.WriteString(s)
		count++
	}
	r := out.String()
	if len(r) > max {
		r = clip(r, max)
	}
	return r
}

// sentences splits text after ". ", "! " or "? " before a capital, a digit
// or Markdown.
func sentences(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text)-2; i++ {
		c := text[i]
		if (c == '.' || c == '!' || c == '?') && text[i+1] == ' ' {
			n := text[i+2]
			if (n >= 'A' && n <= 'Z') || (n >= '0' && n <= '9') || n == '`' || n == '*' || n == '"' {
				out = append(out, strings.TrimSpace(text[start:i+1]))
				start = i + 2
			}
		}
	}
	if rest := strings.TrimSpace(text[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// clip cuts text at a word boundary before max bytes and adds "…".
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndex(s[:max], " ")
	if cut < max/2 {
		cut = max
	}
	return strings.TrimRight(s[:cut], " ,;:") + "…"
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func endSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, ")") && strings.HasSuffix(strings.TrimSuffix(s, ")"), ".") {
		return s
	}
	return s + "."
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func getsWord(n int) string {
	if n == 1 {
		return "gets"
	}
	return "get"
}

// ---------------------------------------------------------------- people

var rePerson = regexp.MustCompile(`(?i)\b(?:who is|who's|what (?:does|did|is|has)|what works? on|tell me about)\s+([a-z][a-z0-9_-]{2,20})\b`)

// personQuestion answers "who is anna?" from the changes and merges of a
// person.
func (s *Svc) personQuestion(ctx context.Context, p *progress, _ Request, q string, a *Answer) bool {
	m := rePerson.FindStringSubmatch(q)
	if m == nil {
		return false
	}
	who := strings.ToLower(m[1])
	if commonWord[who] || strings.ContainsAny(who, "0123456789") || len(advisor.EntityNames(who)) > 0 {
		return false
	}
	var open []api.ChangeSummary
	var authored, approved []api.HistoryEntry
	if all, err := s.o.Tools.Changes(ctx); err == nil {
		for _, c := range all {
			if c.Author == who {
				switch c.State {
				case api.ChangeStateMerged, api.ChangeStateDeployed, api.ChangeStateVerified, api.ChangeStateClosed:
				default:
					open = append(open, c)
				}
			}
		}
	}
	if hist, err := s.o.Tools.History(ctx, api.ListHistoryParams{Limit: 50}); err == nil {
		for _, h := range hist {
			if h.Author == who {
				authored = append(authored, h)
			}
			for _, ap := range h.ApprovedBy {
				if ap == who {
					approved = append(approved, h)
				}
			}
		}
	}
	if len(open)+len(authored)+len(approved) == 0 {
		// Not a person of this project: no step.
		return false
	}
	p.quick(ctx, "history", "changes of "+who, plural(len(open)+len(authored), "change", "changes"))
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** works on this project:", who)
	if len(open) > 0 {
		var ids []string
		for _, c := range open {
			ids = append(ids, fmt.Sprintf("**%s** %s (%s)", c.Id, c.Title, stateWords(c.State)))
		}
		fmt.Fprintf(&b, "\n- **Open:** %s", strings.Join(ids, "; "))
	}
	if len(authored) > 0 {
		var ids []string
		for i, h := range authored {
			if i >= 3 {
				break
			}
			ids = append(ids, fmt.Sprintf("%s %s", h.Change, h.Title))
			addSource(a, llm.Source{Label: "trailer " + h.Change, Ref: h.Change, Kind: "trailer"})
		}
		if len(authored) == 1 {
			fmt.Fprintf(&b, "\n- **Merged:** %s", ids[0])
		} else {
			fmt.Fprintf(&b, "\n- **Merged:** %s, for example %s", plural(len(authored), "change", "changes"), strings.Join(ids, "; "))
		}
	}
	if len(approved) > 0 {
		fmt.Fprintf(&b, "\n- **Approved:** %s of others", plural(len(approved), "change", "changes"))
	}
	a.Text = b.String()
	return true
}
