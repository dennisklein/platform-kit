package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/events"
	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// DefaultDeadline bounds a model-backed answer. After it the bench answers
// from its data, so a slow model never leaves the asker waiting.
const DefaultDeadline = 25 * time.Second

// Options of the chat service.
type Options struct {
	Tools  advisor.Tools
	LLM    llm.Client
	Log    llm.Log
	Events events.Publisher
	// Advisor runs the advisor recipes (risks, merge order, lab gaps,
	// explain diff) for questions about them. Default: a deterministic
	// engine on Tools; its suggestions come back in the chat answer.
	Advisor advisor.Runner
	// Path is the suggestion file (<data>/llm/suggestions.json); "" keeps
	// them in memory.
	Path string
	// MaxSteps of the tool loop (default 8).
	MaxSteps int
	// Timeout of one model call (default 90 s).
	Timeout time.Duration
	// Deadline of a whole model-backed answer (default 25 s).
	Deadline time.Duration
	Now      func() time.Time
}

// Svc implements Service.
type Svc struct {
	o     Options
	st    *store
	log   *slog.Logger
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)
}

var _ Service = (*Svc)(nil)

// New returns the chat service.
func New(o Options) (*Svc, error) {
	if o.LLM == nil {
		o.LLM = llm.NewMock(nil)
	}
	if o.MaxSteps <= 0 {
		o.MaxSteps = 8
	}
	if o.Timeout <= 0 {
		o.Timeout = 90 * time.Second
	}
	if o.Deadline <= 0 {
		o.Deadline = DefaultDeadline
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Advisor == nil && o.Tools != nil {
		o.Advisor = advisor.New(advisor.Options{Tools: o.Tools, LLM: llm.NewMock(nil), Store: keepSuggestions{}, Now: o.Now})
	}
	st, err := newStore(o.Path)
	if err != nil {
		return nil, err
	}
	return &Svc{o: o, st: st, log: slog.Default().With("component", "chat"), now: o.Now, sleep: sleepCtx}, nil
}

// keepSuggestions hands the suggestions of an advisor run back unchanged:
// the chat stores them with its answer.
type keepSuggestions struct{}

func (keepSuggestions) AddSuggestion(_ context.Context, s advisor.Suggestion) (*advisor.Suggestion, error) {
	return &s, nil
}

// RefusalText is the answer to a question about a secret value.
const RefusalText = "I cannot see it. Secret fields are removed before each call."

var (
	reSecretWord = regexp.MustCompile(`(?i)\b(token|password|passwd|secret|api[- ]?key|private[- ]key|credential|passphrase|sops key|age key)s?\b`)
	reAskValue   = regexp.MustCompile(`(?i)\b(what|which|show|tell|give|print|reveal|read|value|decrypt|display|dump|leak|copy)\b`)
)

// IsSecretQuestion reports a question that asks for a secret value.
func IsSecretQuestion(q string) bool {
	return reSecretWord.MatchString(q) && reAskValue.MatchString(q)
}

// Ask answers the last user message. With a request id, the steps and the
// text go live to the asker while the answer forms.
func (s *Svc) Ask(ctx context.Context, req Request) (*Answer, error) {
	q := ""
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		q = strings.TrimSpace(req.Messages[n-1].Content)
	}
	if q == "" {
		return nil, ErrNoQuestion
	}
	start := s.now()
	p := s.newProgress(ctx, req.RequestID)
	ans := &Answer{Model: s.o.LLM.Model(), Mock: s.o.LLM.Mock(), Sources: []llm.Source{}, Suggestions: []advisor.Suggestion{}}
	entry := llm.LogEntry{Surface: llm.SurfaceChat, ChangeID: req.ChangeID, Model: ans.Model, Mock: ans.Mock, Prompt: transcript(req)}

	switch {
	case IsSecretQuestion(q):
		p.pace = true
		ans.Text, ans.Refused = RefusalText, true
		s.secretContext(ctx, p, q, ans, &entry)
	case ans.Mock:
		p.pace = true
		ans.Fallback = true
		s.mockAnswer(ctx, p, req, q, ans)
	default:
		dctx, cancel := context.WithTimeout(ctx, s.o.Deadline)
		err := s.loop(dctx, p, req, ans, &entry)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				// The asker stopped the answer.
				return nil, ctx.Err()
			}
			reason := "the model did not answer (" + shortErr(err) + ")"
			if errors.Is(err, context.DeadlineExceeded) && errors.Is(dctx.Err(), context.DeadlineExceeded) {
				reason = fmt.Sprintf("the model took longer than %d s", int(s.o.Deadline.Seconds()))
			}
			s.log.Warn("chat: model failed, answering from data", "err", err)
			entry.Error = err.Error()
			if strings.HasPrefix(reason, "the model took") {
				entry.Error = "model too slow (" + s.o.Deadline.String() + "): " + err.Error()
			}
			// Drop what the failed loop collected; the answer comes from data.
			p.reset()
			n := p.begin("model", ans.Model)
			p.end(ctx, n, strings.TrimPrefix(reason, "the model "), true)
			p.pace = true
			ans.Text, ans.Sources, ans.Suggestions = "", []llm.Source{}, []advisor.Suggestion{}
			ans.Fallback, ans.FallbackReason = true, reason
			s.mockAnswer(ctx, p, req, q, ans)
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !p.streamed {
		p.typeOut(ctx, ans.Text)
	}
	p.flush()
	ans.Steps = p.snapshot()
	entry.Model = ans.Model
	entry.Answer = ans.Text
	entry.Sources = ans.Sources
	entry.DurationMs = max(s.now().Sub(start).Milliseconds(), 1)
	ans.LogID = s.logEntry(ctx, entry)
	// Suggestions are stored after the answer exists, with its log id.
	stored := []advisor.Suggestion{}
	for _, sg := range ans.Suggestions {
		sg.LogID, sg.By = ans.LogID, "chat"
		if sg.ChangeID == "" {
			sg.ChangeID = req.ChangeID
		}
		saved, err := s.AddSuggestion(ctx, sg)
		if err != nil {
			s.log.Warn("chat: store suggestion", "err", err)
			continue
		}
		stored = append(stored, *saved)
	}
	ans.Suggestions = stored
	return ans, nil
}

func (s *Svc) logEntry(ctx context.Context, e llm.LogEntry) string {
	if s.o.Log == nil {
		return ""
	}
	if id, ok := identity.From(ctx); ok {
		e.User = id.User
	}
	// Redactions already counts values removed from retrieved context.
	var n int
	e.Prompt, n = redactor.Redact(e.Prompt)
	e.Redactions += n
	id, err := s.o.Log.Append(ctx, e)
	if err != nil {
		s.log.Warn("llm log append", "err", err)
		return ""
	}
	return id
}

var redactor = llm.NewRedactor()

func transcript(req Request) string {
	var b strings.Builder
	if req.ChangeID != "" {
		fmt.Fprintf(&b, "[change %s", req.ChangeID)
		if req.Path != "" {
			fmt.Fprintf(&b, ", file %s", req.Path)
		}
		b.WriteString("]\n")
	}
	msgs := req.Messages
	if len(msgs) > 12 {
		msgs = msgs[len(msgs)-12:]
	}
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	return strings.TrimSpace(b.String())
}

func addSource(a *Answer, src llm.Source) {
	for _, s := range a.Sources {
		if s.Kind == src.Kind && s.Label == src.Label {
			return
		}
	}
	a.Sources = append(a.Sources, src)
}

// ---------------------------------------------------------------- mock intents

var (
	reAddToPolicy = regexp.MustCompile(`(?i)\b(add|select|peer|include|put)\b.*\bpolic(y|ies)\b`)
	rePolicyName  = regexp.MustCompile(`(?i)\b(?:the\s+)?([a-z0-9][a-z0-9-]*[a-z0-9])\s+(?:bgp\s+)?(?:peering\s+)?polic(?:y|ies)\b`)
	reWhy         = regexp.MustCompile(`(?i)^\s*(?:(?:so|and|but|ok|okay|then|hi|hey)\b[\s,]*)*(why|when|who|how come)\b`)
	reNumber      = regexp.MustCompile(`\b\d{1,3}\b`)
)

// mockAnswer answers from the bench data: known intents first (greeting,
// policy edit, why, a change, open changes, history, an entity), else the
// best passage of the index.
func (s *Svc) mockAnswer(ctx context.Context, p *progress, req Request, q string, a *Answer) {
	if isGreeting(q) {
		s.greet(req, a)
		return
	}
	if reAddToPolicy.MatchString(q) && len(advisor.EntityNames(q)) > 0 {
		s.policyEdit(ctx, p, req, q, a)
		return
	}
	if names := advisor.EntityNames(q); reWhy.MatchString(q) && len(names) > 0 {
		if s.whyFromHistory(ctx, p, q, names, a) {
			return
		}
	}
	for _, intent := range []func(context.Context, *progress, Request, string, *Answer) bool{
		s.changeQuestion, s.openChanges, s.recentHistory, s.entityQuestion, s.personQuestion,
	} {
		if intent(ctx, p, req, q, a) {
			return
		}
	}
	s.retrieval(ctx, p, q, a)
}

// whyFromHistory answers "why does cn-003 ..." from merge trailers and the
// semantic diff of the merges that touched the entity.
func (s *Svc) whyFromHistory(ctx context.Context, p *progress, q string, names []string, a *Answer) bool {
	terms := map[string]bool{}
	for _, t := range llm.Tokenize(q) {
		terms[t] = true
	}
	type cand struct {
		entry api.HistoryEntry
		lines []api.SemanticLine
		score int
	}
	var best *cand
	for _, name := range names {
		var entries []api.HistoryEntry
		if err := p.do(ctx, "history", "history "+name, func() (string, error) {
			var err error
			entries, err = s.o.Tools.History(ctx, api.ListHistoryParams{Entity: name, Limit: 20})
			return plural(len(entries), "merge", "merges"), err
		}); err != nil || len(entries) == 0 {
			continue
		}
		n := p.begin("history_entry", fmt.Sprintf("read %s", plural(min(len(entries), 10), "merge diff", "merge diffs")))
		var nameBest *cand
		for i, h := range entries {
			if i >= 10 {
				break
			}
			c := cand{entry: h}
			if d, err := s.o.Tools.HistoryEntry(ctx, h.Sha); err == nil {
				for _, sl := range d.Semantic {
					if strings.HasPrefix(sl.Subject, name) {
						c.lines = append(c.lines, sl)
					}
				}
			}
			text := h.Title + " " + h.Intent
			for _, l := range c.lines {
				text += " " + l.Subject + " " + l.Text
			}
			for _, t := range llm.Tokenize(text) {
				if terms[t] {
					c.score++
				}
			}
			// Numbers (ports, units) in the question weigh more.
			for _, n := range reNumber.FindAllString(q, -1) {
				if strings.Contains(text, " "+n) {
					c.score += 3
				}
			}
			if best == nil || c.score > best.score {
				cc := c
				best = &cc
			}
			if nameBest == nil || c.score > nameBest.score {
				cc := c
				nameBest = &cc
			}
		}
		sum := "no match"
		if nameBest != nil && nameBest.score > 0 {
			sum = "match " + shortSHA(nameBest.entry.Sha)
			if nameBest.entry.Change != "" {
				sum = "match " + nameBest.entry.Change
			}
		}
		p.end(ctx, n, sum, false)
	}
	if best == nil || best.score == 0 {
		return false
	}
	h := best.entry
	change := h.Change
	if change == "" {
		change = shortSHA(h.Sha)
	}
	var what string
	if len(best.lines) > 0 {
		var parts []string
		for _, l := range best.lines {
			parts = append(parts, describeMove(l))
		}
		what = strings.Join(parts, "; ")
	} else {
		what = lowerFirst(h.Title)
	}
	text := fmt.Sprintf("%s %s", change, what)
	if h.Intent != "" {
		text += ", to " + lowerFirst(strings.TrimSuffix(h.Intent, "."))
	}
	text += "."
	if len(h.ApprovedBy) > 0 {
		text += fmt.Sprintf(" Approved by %s.", joinAnd(h.ApprovedBy))
	}
	if !h.Date.IsZero() {
		text += fmt.Sprintf(" Merged %s", h.Date.Format("2006-01-02"))
		if h.Author != "" {
			text += ", author " + h.Author
		}
		text += "."
	}
	a.Text = text
	addSource(a, llm.Source{Label: "trailer " + change, Ref: change, Kind: "trailer"})
	if h.Mr != "" {
		addSource(a, llm.Source{Label: "MR " + h.Mr, Ref: h.Mr, Kind: "mr"})
	}
	addSource(a, llm.Source{Label: "history " + shortSHA(h.Sha), Ref: h.Sha, Kind: "history"})
	return true
}

var reFromTo = regexp.MustCompile(`^(.+?) to (.+)$`)

// describeMove turns "cn-003 p2: data-b 37 to data-a 37" into "moved cn-003
// p2 from data-b 37 to data-a 37".
func describeMove(l api.SemanticLine) string {
	if m := reFromTo.FindStringSubmatch(l.Text); m != nil && len(advisor.PortsInText(l.Text)) > 0 {
		return fmt.Sprintf("moved %s from %s to %s", l.Subject, m[1], m[2])
	}
	return fmt.Sprintf("changed %s: %s", l.Subject, l.Text)
}

// policyEdit answers "add cn-037 to the rack-6z BGP policy" with a
// suggested edit in the open change.
func (s *Svc) policyEdit(ctx context.Context, p *progress, req Request, q string, a *Answer) {
	names := advisor.EntityNames(q)
	if req.ChangeID == "" {
		a.Text = "Open a change first. I make edits only as suggestions in a change."
		return
	}
	want := ""
	if m := rePolicyName.FindStringSubmatch(q); m != nil && !strings.EqualFold(m[1], "bgp") && !strings.EqualFold(m[1], "the") {
		want = m[1]
	}
	nodes, err := s.o.Tools.Tree(ctx, req.ChangeID)
	if err != nil {
		a.Text = "I cannot read the files of " + req.ChangeID + ": " + err.Error()
		return
	}
	step := p.begin("search", "find BGP policies in "+req.ChangeID)
	var found []advisor.BGPPolicy
	var walk func([]api.FileNode)
	walk = func(ns []api.FileNode) {
		for _, n := range ns {
			walk(n.Children)
			if n.Type != api.FileNodeTypeFile || !strings.HasPrefix(n.Path, "clusters/") || !(strings.HasSuffix(n.Path, ".yaml") || strings.HasSuffix(n.Path, ".yml")) {
				continue
			}
			if n.Kind != "CiliumBGPPeeringPolicy" && !(n.Kind == "" && strings.Contains(n.Path, "/cilium/")) {
				continue
			}
			fc, err := s.o.Tools.ReadFile(ctx, req.ChangeID, n.Path)
			if err != nil {
				continue
			}
			for _, p := range advisor.ParseBGP(n.Path, fc.Content) {
				if want == "" || strings.EqualFold(p.Name, want) || strings.EqualFold(p.Rack, want) {
					found = append(found, p)
				}
			}
		}
	}
	walk(nodes)
	{
		var ns []string
		for _, f := range found {
			ns = append(ns, f.Name)
		}
		sum := "none"
		if len(ns) > 0 {
			sum = joinAnd(ns)
		}
		p.end(ctx, step, sum, len(found) == 0)
	}
	if len(found) == 0 {
		if want != "" {
			a.Text = fmt.Sprintf("I find no BGP policy %s in %s.", want, req.ChangeID)
		} else {
			a.Text = "Which policy? Name it, for example \"the rack-6z BGP policy\"."
		}
		return
	}
	if len(found) > 1 {
		var ns []string
		for _, p := range found {
			ns = append(ns, p.Name)
		}
		a.Text = fmt.Sprintf("More than one BGP policy matches: %s. Name one.", joinAnd(ns))
		return
	}
	pol := found[0]
	var fc *api.FileContent
	if err := p.do(ctx, "read_file", "read "+shortPath(pol.Path), func() (string, error) {
		var err error
		fc, err = s.o.Tools.ReadFile(ctx, req.ChangeID, pol.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d nodes selected", len(pol.Hostnames)), nil
	}); err != nil {
		a.Text = "I cannot read " + pol.Path + ": " + err.Error()
		return
	}
	addSource(a, llm.Source{Label: shortPath(pol.Path), Ref: pol.Path, Kind: "file"})
	after, err := advisor.AddHostnames(fc.Content, pol.Name, names)
	if err != nil {
		a.Text = "I cannot edit the policy: " + err.Error()
		return
	}
	var already []string
	for _, n := range names {
		if pol.Selects(n) {
			already = append(already, n)
		}
	}
	if after == fc.Content {
		a.Text = fmt.Sprintf("%s %s in %s already.", joinAnd(already), isAre(len(already)), pol.Name)
		return
	}
	var add []string
	for _, n := range names {
		if !pol.Selects(n) {
			add = append(add, n)
		}
	}
	// Names that are not in the inventory of the change.
	var unknown []string
	step = p.begin("read_file", "check inventory "+joinAnd(add))
	for _, n := range add {
		if fc, err := s.o.Tools.ReadFile(ctx, req.ChangeID, "control/inventory/"+n+".yaml"); err != nil || !fc.Exists {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		p.end(ctx, step, joinAnd(unknown)+" missing", false)
	} else {
		p.end(ctx, step, "in the inventory", false)
	}
	added := len(add)
	n := p.begin("suggest_edit", "suggest "+shortPath(pol.Path))
	p.end(ctx, n, "+"+plural(added, "line", "lines"), false)
	a.Text = fmt.Sprintf("I suggest %s in %s: it adds %s to %s, so %s %s a BGP session. Review the diff below, then accept or reject it.",
		plural(added, "line", "lines"), shortPath(pol.Path), joinAnd(add), pol.Name, joinAnd(add), getsWord(added))
	if len(unknown) > 0 {
		a.Text += fmt.Sprintf(" Note: %s %s not in the inventory of this change yet.", joinAnd(unknown), isAre(len(unknown)))
	}
	a.Suggestions = append(a.Suggestions, advisor.Suggestion{
		ChangeID: req.ChangeID, Path: pol.Path, Before: fc.Content, After: after,
		Title: fmt.Sprintf("Add %s to the BGP policy %s", joinAnd(add), pol.Name),
	})
}

// secretContext retrieves the context a model call for a secret question
// would carry (as every call does) and logs it as sent: the redaction
// filter has removed the secret values, so the log shows "[redacted]" where
// the value was, and the number of removed values. The answer stays the
// refusal; no model sees the question's target.
func (s *Svc) secretContext(ctx context.Context, p *progress, q string, a *Answer, e *llm.LogEntry) {
	if s.o.Tools == nil {
		return
	}
	var hits []llm.Hit
	n := p.begin("search", "search "+quoteQ(q))
	hits, err := s.o.Tools.Search(ctx, q, 4)
	removed := 0
	for _, h := range hits {
		removed += h.Redactions
	}
	sum := plural(len(hits), "hit", "hits")
	if removed > 0 {
		sum = plural(removed, "secret value", "secret values") + " removed"
	}
	p.end(ctx, n, sum, err != nil)
	if err != nil || len(hits) == 0 {
		return
	}
	var b strings.Builder
	n = 0
	for _, h := range hits {
		// Only context the filter changed: it is what the question is about.
		if h.Redactions == 0 || n >= 2 {
			continue
		}
		n++
		text := h.Text
		if len(text) > 2000 {
			text = text[:2000] + "\n…"
		}
		fmt.Fprintf(&b, "\n--- context: %s (%d value%s redacted) ---\n%s\n", h.Ref, h.Redactions, map[bool]string{true: "", false: "s"}[h.Redactions == 1], strings.TrimRight(text, "\n"))
		e.Redactions += h.Redactions
		addSource(a, hitSource(h))
	}
	if n > 0 {
		e.Prompt += b.String()
	}
}

func hitSource(h llm.Hit) llm.Source {
	switch h.Kind {
	case "trailer":
		return llm.Source{Label: "trailer " + h.Ref, Ref: h.Ref, Kind: "trailer"}
	case "mr":
		return llm.Source{Label: "MR " + h.Ref, Ref: h.Ref, Kind: "mr"}
	case "lab":
		return llm.Source{Label: shortPath(h.Ref), Ref: h.Ref, Kind: "lab"}
	case "kb":
		return llm.Source{Label: shortPath(h.Ref), Ref: h.Ref, Kind: "kb"}
	}
	return llm.Source{Label: shortPath(h.Ref), Ref: h.Ref, Kind: "file"}
}

func shortPath(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func lowerFirst(s string) string {
	if s == "" || (len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z') {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func joinAnd(words []string) string {
	w := append([]string(nil), words...)
	switch len(w) {
	case 0:
		return ""
	case 1:
		return w[0]
	}
	return strings.Join(w[:len(w)-1], ", ") + " and " + w[len(w)-1]
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
