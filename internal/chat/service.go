package chat

import (
	"context"
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

// Options of the chat service.
type Options struct {
	Tools  advisor.Tools
	LLM    llm.Client
	Log    llm.Log
	Events events.Publisher
	// Path is the suggestion file (<data>/llm/suggestions.json); "" keeps
	// them in memory.
	Path string
	// MaxSteps of the tool loop (default 8).
	MaxSteps int
	// Timeout of one model call (default 90 s).
	Timeout time.Duration
	Now     func() time.Time
}

// Svc implements Service.
type Svc struct {
	o   Options
	st  *store
	log *slog.Logger
	now func() time.Time
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
	if o.Now == nil {
		o.Now = time.Now
	}
	st, err := newStore(o.Path)
	if err != nil {
		return nil, err
	}
	return &Svc{o: o, st: st, log: slog.Default().With("component", "chat"), now: o.Now}, nil
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

// Ask answers the last user message.
func (s *Svc) Ask(ctx context.Context, req Request) (*Answer, error) {
	q := ""
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		q = strings.TrimSpace(req.Messages[n-1].Content)
	}
	if q == "" {
		return nil, ErrNoQuestion
	}
	start := s.now()
	ans := &Answer{Model: s.o.LLM.Model(), Mock: s.o.LLM.Mock(), Sources: []llm.Source{}, Suggestions: []advisor.Suggestion{}}
	entry := llm.LogEntry{Surface: llm.SurfaceChat, ChangeID: req.ChangeID, Model: ans.Model, Mock: ans.Mock, Prompt: transcript(req)}

	switch {
	case IsSecretQuestion(q):
		ans.Text, ans.Refused = RefusalText, true
		s.secretContext(ctx, q, ans, &entry)
	case ans.Mock:
		ans.Fallback = true
		s.mockAnswer(ctx, req, q, ans)
	default:
		if err := s.loop(ctx, req, ans, &entry); err != nil {
			s.log.Warn("chat: model failed, answering from data", "err", err)
			entry.Error = err.Error()
			// Drop what the failed loop collected; the answer comes from data.
			ans.Text, ans.Sources, ans.Suggestions = "", []llm.Source{}, []advisor.Suggestion{}
			ans.Fallback = true
			s.mockAnswer(ctx, req, q, ans)
		}
	}
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
	reWhy         = regexp.MustCompile(`(?i)^\s*(why|when|who|how come)\b`)
	reNumber      = regexp.MustCompile(`\b\d{1,3}\b`)
)

// mockAnswer answers known intents from data, else with retrieval snippets.
func (s *Svc) mockAnswer(ctx context.Context, req Request, q string, a *Answer) {
	if reAddToPolicy.MatchString(q) && len(advisor.EntityNames(q)) > 0 {
		s.policyEdit(ctx, req, q, a)
		return
	}
	if names := advisor.EntityNames(q); reWhy.MatchString(q) && len(names) > 0 {
		if s.whyFromHistory(ctx, q, names, a) {
			return
		}
	}
	s.retrieval(ctx, q, a)
}

// whyFromHistory answers "why does cn-003 ..." from merge trailers and the
// semantic diff of the merges that touched the entity.
func (s *Svc) whyFromHistory(ctx context.Context, q string, names []string, a *Answer) bool {
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
		entries, err := s.o.Tools.History(ctx, api.ListHistoryParams{Entity: name, Limit: 20})
		if err != nil {
			continue
		}
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
		}
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
func (s *Svc) policyEdit(ctx context.Context, req Request, q string, a *Answer) {
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
	fc, err := s.o.Tools.ReadFile(ctx, req.ChangeID, pol.Path)
	if err != nil {
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
	added := len(names) - len(already)
	a.Text = fmt.Sprintf("I suggest %s in %s. See the editor.", plural(added, "line", "lines"), shortPath(pol.Path))
	// Names that are not in the inventory of the change.
	var unknown []string
	for _, n := range names {
		if fc, err := s.o.Tools.ReadFile(ctx, req.ChangeID, "control/inventory/"+n+".yaml"); err != nil || !fc.Exists {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		a.Text += fmt.Sprintf(" Note: %s %s not in the inventory of this change yet.", joinAnd(unknown), isAre(len(unknown)))
	}
	var add []string
	for _, n := range names {
		if !pol.Selects(n) {
			add = append(add, n)
		}
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
func (s *Svc) secretContext(ctx context.Context, q string, a *Answer, e *llm.LogEntry) {
	if s.o.Tools == nil {
		return
	}
	hits, err := s.o.Tools.Search(ctx, q, 4)
	if err != nil || len(hits) == 0 {
		return
	}
	var b strings.Builder
	n := 0
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

// retrieval answers with the best snippets of the index.
func (s *Svc) retrieval(ctx context.Context, q string, a *Answer) {
	hits, err := s.o.Tools.Search(ctx, q, 4)
	if err != nil || len(hits) == 0 {
		a.Text = "I found nothing about that in the KB, the history, the MRs or the lab reports."
		return
	}
	var b strings.Builder
	b.WriteString("From the project:\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s: %s\n", h.Title, h.Snippet)
		addSource(a, hitSource(h))
	}
	a.Text = strings.TrimSpace(b.String())
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
