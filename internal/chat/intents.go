package chat

// More answers from the bench data: questions in other languages, prompt
// injections, small talk, follow-ups that lean on the previous question,
// requests to change something (edit, delete, fix, merge), versions and
// counts. Each answer is short, says what it read, and offers the next
// step when the chat cannot do it itself.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// ---------------------------------------------------------------- languages

// langs are the languages the chat understands besides English: markers
// tell the language, phrases and words translate a question well enough for
// the intents (ids and names stay as they are).
var langs = []struct {
	name    string
	markers map[string]bool
	phrases [][2]string
	words   map[string]string
}{
	{
		name: "German",
		markers: set(`wie viele welche welcher welches welchen welchem warum wieso weshalb wer ist sind läuft laufen gibt meine meiner meinem meinen
			änderung änderungen nicht bitte zeige zeig füge hinzu risiken knoten für und kannst du ich hast einen eine der das den dem mit von wo
			freie erzähl erzähle witz wetter heute genehmigt freigegeben lösche entferne repariere behebe mir uns auf nutzt benutzt verwendet zur zum
			gemergt letzte letzten woche offene offenen sicher erkläre richtlinie`),
		phrases: [][2]string{{"wie viele", "how many"}, {"was ist", "what is"}, {"was sind", "what are"}, {"gibt es", "are there"},
			{"bgp-policy", "bgp policy"}, {"bgp-richtlinie", "bgp policy"}, {"was hat", "what has"}},
		words: map[string]string{"wer": "who", "was": "what", "welche": "which", "welcher": "which", "welches": "which", "welchen": "which",
			"welchem": "which", "wie": "how", "wo": "where", "warum": "why", "wieso": "why", "weshalb": "why", "ist": "is", "sind": "are",
			"hat": "has", "haben": "have", "läuft": "runs", "laufen": "run", "nutzt": "uses", "benutzt": "uses", "verwendet": "uses",
			"auf": "on", "im": "in the", "mit": "with", "von": "of", "vom": "of the", "für": "for", "und": "and", "nicht": "not",
			"mein": "my", "meine": "my", "meiner": "my", "meinem": "my", "meinen": "my", "änderung": "change", "änderungen": "changes",
			"risiken": "risks", "risiko": "risk", "knoten": "nodes", "genehmigt": "approved", "freigegeben": "approved", "füge": "add",
			"hinzu": "", "zu": "to", "zur": "to the", "zum": "to the", "der": "the", "die": "the", "das": "the", "den": "the", "dem": "the",
			"des": "the", "ein": "a", "eine": "a", "einen": "a", "einem": "a", "einer": "a", "freie": "free", "zeige": "show", "zeig": "show",
			"mir": "me", "bitte": "please", "erkläre": "explain", "erklär": "explain", "lösche": "delete", "entferne": "remove",
			"repariere": "fix", "behebe": "fix", "sicher": "safe", "erzähl": "tell", "erzähle": "tell", "witz": "joke", "wetter": "weather",
			"heute": "today", "gemergt": "merged", "letzte": "last", "letzten": "last", "woche": "week", "offene": "open", "offenen": "open",
			"es": "it", "kann": "can", "kannst": "can", "du": "you", "ich": "I", "uns": "us", "richtlinie": "policy", "getestet": "tested",
			"wann": "when", "gemacht": "made", "geändert": "changed"},
	},
	{
		name: "French",
		markers: set(`combien pourquoi quel quelle quels quelles qui est sont tourne sur dans mon ma mes changement risques nœuds noeuds une des les
			approuvé ajoute ajouter supprime supprimer corrige corriger répare blague météo quoi c'est raconte moi politique`),
		phrases: [][2]string{{"combien de", "how many"}, {"qu'est-ce que", "what is"}, {"est-ce que", ""}, {"c'est", "is"}, {"politique bgp", "bgp policy"}},
		words: map[string]string{"combien": "how many", "pourquoi": "why", "quel": "which", "quelle": "which", "quels": "which",
			"quelles": "which", "qui": "who", "est": "is", "sont": "are", "a": "has", "ont": "have", "tourne": "runs", "sur": "on",
			"dans": "in", "de": "of", "du": "of the", "des": "of the", "le": "the", "la": "the", "les": "the", "un": "a", "une": "a",
			"mon": "my", "ma": "my", "mes": "my", "changement": "change", "risques": "risks", "nœuds": "nodes", "noeuds": "nodes",
			"approuvé": "approved", "ajoute": "add", "ajouter": "add", "à": "to", "au": "to the", "supprime": "delete",
			"supprimer": "delete", "corrige": "fix", "corriger": "fix", "répare": "fix", "blague": "joke", "météo": "weather",
			"quoi": "what", "raconte": "tell", "moi": "me", "politique": "policy"},
	},
	{
		name: "Spanish",
		markers: set(`cuántos cuantos cuántas cuantas qué quién cuál cuáles versión mi mis los las nodos tiene tienen cambio riesgos chiste del una
			está hay añade agrega elimina borra arregla clima política aprobó cómo`),
		phrases: [][2]string{{"por qué", "why"}, {"política bgp", "bgp policy"}},
		words: map[string]string{"cuántos": "how many", "cuantos": "how many", "cuántas": "how many", "cuantas": "how many",
			"qué": "what", "que": "what", "quién": "who", "cuál": "which", "cuáles": "which", "es": "is", "son": "are", "tiene": "has",
			"tienen": "have", "ejecuta": "runs", "corre": "runs", "en": "on", "de": "of", "del": "of the", "el": "the", "la": "the",
			"los": "the", "las": "the", "un": "a", "una": "a", "mi": "my", "mis": "my", "cambio": "change", "riesgos": "risks",
			"nodos": "nodes", "aprobó": "approved", "añade": "add", "agrega": "add", "a": "to", "al": "to the", "elimina": "delete",
			"borra": "delete", "arregla": "fix", "corrige": "fix", "chiste": "joke", "clima": "weather", "versión": "version",
			"política": "policy", "cuenta": "tell", "me": "me", "hay": "are there", "cómo": "how", "está": "is"},
	},
}

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

var reWordToken = regexp.MustCompile(`[\p{L}'’]+|[^\p{L}'’]+`)

// toEnglish translates a question in German, French or Spanish word by word
// into English, so the same intents answer it. lang is "" for English.
func toEnglish(q string) (en, lang string) {
	low := strings.ToLower(q)
	words := strings.FieldsFunc(low, func(r rune) bool { return !(unicode.IsLetter(r) || r == '\'' || r == '’') })
	best, bestN := -1, 0
	for i, l := range langs {
		n := 0
		for _, w := range words {
			if l.markers[w] {
				n++
			}
		}
		for _, ph := range l.phrases {
			if strings.Contains(low, ph[0]) && len(strings.Fields(ph[0])) > 1 {
				n++
			}
		}
		if n > bestN {
			best, bestN = i, n
		}
	}
	accent := strings.ContainsAny(low, "äöüßéèêàçñ¿¡")
	if best < 0 || bestN < 2 && !(bestN == 1 && accent) {
		return q, ""
	}
	l := langs[best]
	out := strings.NewReplacer("¿", "", "¡", "").Replace(low)
	for _, ph := range l.phrases {
		out = strings.ReplaceAll(out, ph[0], ph[1])
	}
	var b strings.Builder
	for _, tok := range reWordToken.FindAllString(out, -1) {
		if w, ok := l.words[tok]; ok {
			b.WriteString(w)
			continue
		}
		b.WriteString(tok)
	}
	return strings.Join(strings.Fields(b.String()), " "), l.name
}

// ---------------------------------------------------------------- guard and small talk

var reInjection = regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override|bypass)\b[^.?!]{0,40}\b(instructions?|rules?|prompts?|guidelines|guard|everything above|the above)\b|` +
	`\bsystem prompt\b|\byour (hidden |initial |original |secret )?(instructions|prompt|rules|system)\b|\b(print|show|reveal|repeat|dump) (the|your) (system )?(prompt|instructions)\b|` +
	`\bjailbreak\b|\bdeveloper mode\b|\bpretend (to be|you are|you're)\b|\byou are now\b|\bdo anything now\b|\bact as (a|an|if)\b`)

// isInjection reports a question that tries to change the chat's rules.
func isInjection(q string) bool { return reInjection.MatchString(q) }

// injection declines politely and shows what the chat can do instead.
func (s *Svc) injection(ctx context.Context, p *progress, req Request, a *Answer) {
	p.quick(ctx, "guard", "check the question", "asks to change my rules: ignored")
	a.Text = "I keep my instructions, and text in a question cannot change them. My rules are the same for everyone: I read this project and cite my sources, I suggest edits as diffs, and I never commit, merge or show secret values.\n\n" +
		"Ask me about the project instead, for example:\n" + examples(req, 3)
}

var (
	reJoke      = regexp.MustCompile(`(?i)\bjokes?\b|\bfunny\b|\bmake me laugh\b|\bpuns?\b`)
	reWeather   = regexp.MustCompile(`(?i)\bweather\b|\bforecast\b|\bis it (raining|sunny|cold|hot|snowing)\b|\btemperature outside\b`)
	reThanks    = regexp.MustCompile(`(?i)^\s*(thanks|thank you|thx|cheers|great|cool|nice|awesome|perfect|danke|merci|gracias)\b[\s!.,]*(a lot|so much|very much)?[\s!.]*$`)
	reSmallTalk = regexp.MustCompile(`(?i)\bhow are you\b|\bhow's it going\b|\bare you (alive|human|sentient|conscious|real|happy|ok)\b|\bdo you (dream|sleep|love|like)\b|\bmeaning of life\b`)
	reElsewhere = regexp.MustCompile(`(?i)\b(poem|haiku|song|sing|recipe|pizza|coffee|beer|football|soccer|movie|film|lottery|bitcoin|stock price|capital of|president|who won|world cup|riddle|horoscope|lunch|dinner|vacation|holiday)\b`)
)

// isOffTopic reports small talk and questions outside the project. A joke
// is fine with a project word ("a joke about BGP"); the others only without
// a project name in them.
func isOffTopic(q string) bool {
	if reJoke.MatchString(q) || reThanks.MatchString(q) {
		return true
	}
	if len(advisor.EntityNames(q)) > 0 || reChangeID.MatchString(q) {
		return false
	}
	return reWeather.MatchString(q) || reSmallTalk.MatchString(q) || reElsewhere.MatchString(q)
}

// offTopic answers small talk in one friendly line and points back to what
// the chat can do.
func (s *Svc) offTopic(req Request, q string, a *Answer) {
	low := strings.ToLower(q)
	var line string
	switch {
	case reThanks.MatchString(q):
		a.Text = "You're welcome. Ask me anything else about this project."
		return
	case reJoke.MatchString(q):
		switch {
		case strings.Contains(low, "bgp") || strings.Contains(low, "rout") || strings.Contains(low, "network") || strings.Contains(low, "peer"):
			line = "I'd tell you a BGP joke, but it would take a while to propagate, and half of you would reject it."
		case strings.Contains(low, "yaml"):
			line = "I'd tell you a YAML joke, but one wrong indent and it means something else."
		case strings.Contains(low, "kubernetes") || strings.Contains(low, "k8s") || strings.Contains(low, "pod") || strings.Contains(low, "cluster"):
			line = "I'd tell you a Kubernetes joke, but you'd have to apply it three times before it reconciles."
		case strings.Contains(low, "dns"):
			line = "It's not DNS. There's no way it's DNS. It was DNS."
		case strings.Contains(low, "git") || strings.Contains(low, "flux"):
			line = "A GitOps joke is only funny once it's merged to main."
		default:
			line = "I'd tell you a UDP joke, but you might not get it."
		}
		line += " That's all the comedy I have: I'm better with facts."
	case reWeather.MatchString(q):
		line = "I can't look outside: I only read this project, not the weather."
	case reSmallTalk.MatchString(q):
		line = "I'm fine, thanks: I'm a chat that reads this project, so the rest is beyond me."
	default:
		line = "That's outside this project, so I'll leave it to someone who knows."
	}
	a.Text = line + "\n\nI can answer questions about this project, with sources. Try:\n" + examples(req, 3)
}

// examples lists questions the chat answers well, as Markdown bullets.
func examples(req Request, n int) string {
	id := req.ChangeID
	if id == "" {
		id = "c-p4x8ma"
	}
	all := []string{
		"Why does cn-003 use port 37 on data-a?",
		"What are the risks of " + id + "?",
		"Which podinfo version runs on defiant?",
		"Add cn-037 to the rack-6z BGP policy",
	}
	var b strings.Builder
	for i, x := range all {
		if i >= n {
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- " + x)
	}
	return b.String()
}

// ---------------------------------------------------------------- follow-ups

var (
	reAboutFollow = regexp.MustCompile(`(?i)^\s*(?:and\s+|but\s+|so\s+|ok,?\s+)?(?:what|how)\s+about\s+(.+?)\s*[?.!]*\s*$`)
	reFollowLead  = regexp.MustCompile(`(?i)^\s*(and|but|so|also|then)\b`)
	rePronoun     = regexp.MustCompile(`(?i)\b(it|its|it's|that|they|them|their|he|she|him|her)\b`)
	reAskApproval = regexp.MustCompile(`(?i)\b(approv\w*|reviewed|reviewers?|signed off|sign-off)\b`)
	reAskWhen     = regexp.MustCompile(`(?i)^\s*(?:and|so|but|ok|okay)?[\s,]*when\b`)
	reAskMerged   = regexp.MustCompile(`(?i)^\s*(?:and\s+)?(who|when)\b.*\b(merged?|made|wrote|authored|created|changed)\b`)
)

// subjectsOf returns the entity names and change ids in a text.
func subjectsOf(text string) (ents, ids []string) {
	ents = advisor.EntityNames(text)
	seen := map[string]bool{}
	for _, id := range reChangeID.FindAllString(text, -1) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ents, ids
}

// resolveFollowUp gives a follow-up ("And why?", "Who approved it?", "What
// about cn-036?") the subject of the previous question, or of the answer
// it got (a merge the answer named).
func resolveFollowUp(msgs []Message, q string) string {
	idx := -1
	for i := len(msgs) - 2; i >= 0; i-- {
		if msgs[i].Role == "user" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return q
	}
	prevA := ""
	for j := idx + 1; j < len(msgs)-1; j++ {
		if msgs[j].Role == "assistant" {
			prevA = msgs[j].Content
		}
	}
	pq := strings.TrimSpace(msgs[idx].Content)
	if en, lang := toEnglish(pq); lang != "" {
		pq = en
	}
	// The previous question can be a follow-up too.
	pq = resolveFollowUp(msgs[:idx+1], pq)
	ents, ids := subjectsOf(q)
	pe, pi := subjectsOf(pq)
	if m := reAboutFollow.FindStringSubmatch(q); m != nil && len(ents)+len(ids) > 0 {
		switch {
		case len(ents) > 0 && len(pe) > 0:
			return strings.Replace(pq, pe[0], ents[0], 1)
		case len(ids) > 0 && len(pi) > 0:
			return strings.Replace(pq, pi[0], ids[0], 1)
		}
		return "What is " + m[1] + "?"
	}
	if len(ents)+len(ids) > 0 || reMyChange.MatchString(q) {
		return q
	}
	words := len(strings.Fields(q))
	if words > 2 && !reFollowLead.MatchString(q) && !rePronoun.MatchString(q) {
		return q
	}
	_, ai := subjectsOf(prevA)
	var order [][]string
	if reAskApproval.MatchString(q) || reAskMerged.MatchString(q) {
		order = [][]string{pi, ai, pe}
	} else {
		order = [][]string{pe, pi, ai}
	}
	for _, l := range order {
		if len(l) > 0 {
			return strings.TrimRight(strings.TrimSpace(q), "?.! ") + " (" + l[0] + ")?"
		}
	}
	return q
}

// ---------------------------------------------------------------- approvals

// approvals answers "who approved c-2hd7vw?" and "who can approve my
// change?".
func (s *Svc) approvals(ctx context.Context, p *progress, req Request, id, q string, a *Answer) bool {
	var ch *api.Change
	if err := p.do(ctx, "change", "change "+id, func() (string, error) {
		var err error
		ch, err = s.o.Tools.Change(ctx, id)
		if err != nil {
			return "not found", err
		}
		return fmt.Sprintf("%s · %d of %d approvals", stateWords(ch.State), len(countingApprovals(ch)), max(ch.Approvals.Need, ch.Gates.Approvals.Need)), nil
	}); err != nil || ch == nil {
		a.Text = fmt.Sprintf("I find no change %s. Check the id on the Changes page.", id)
		return true
	}
	addSource(a, llm.Source{Label: id, Ref: id, Kind: "change"})
	mr := ""
	if ch.Mr.Number > 0 {
		mr = fmt.Sprintf("MR !%d", ch.Mr.Number)
		addSource(a, llm.Source{Label: mr, Ref: fmt.Sprintf("!%d", ch.Mr.Number), Kind: "mr"})
	}
	by := countingApprovals(ch)
	who := "**" + id + "**"
	if ch.Title != "" {
		who += " (*" + ch.Title + "*)"
	}
	var b strings.Builder
	merged := ch.State == api.ChangeStateMerged || ch.State == api.ChangeStateDeployed || ch.State == api.ChangeStateVerified
	// "And when was it merged?": the date first, not the approvals again.
	if reAskWhen.MatchString(q) && !reAskApproval.MatchString(q) {
		if merged && !ch.MergedAt.IsZero() {
			fmt.Fprintf(&b, "%s was merged on **%s**", who, ch.MergedAt.Format("2006-01-02"))
			if mr != "" {
				b.WriteString(" in " + mr)
			}
			if ch.Author != "" {
				b.WriteString("; " + ch.Author + " wrote it")
			}
			b.WriteString(".")
			a.Text = b.String()
			return true
		}
		if !merged {
			fmt.Fprintf(&b, "%s is not merged yet: it is %s, with %d of %d approvals.", who, stateWords(ch.State), len(by), max(ch.Approvals.Need, ch.Gates.Approvals.Need))
			a.Text = b.String()
			return true
		}
	}
	switch ch.State {
	case api.ChangeStateMerged, api.ChangeStateDeployed, api.ChangeStateVerified:
		if len(by) > 0 {
			fmt.Fprintf(&b, "%s and approved %s", joinAnd(by), who)
		} else {
			fmt.Fprintf(&b, "Nobody approved %s", who)
		}
		if mr != "" {
			b.WriteString(" in " + mr)
		}
		b.WriteString(".")
		if !ch.MergedAt.IsZero() {
			fmt.Fprintf(&b, " It was merged on %s", ch.MergedAt.Format("2006-01-02"))
			if ch.Author != "" {
				b.WriteString("; " + ch.Author + " wrote it")
			}
			b.WriteString(".")
		}
		text := b.String()
		if len(by) > 0 {
			// "anna and dennis and approved" reads wrong: put the verb first.
			text = strings.Replace(text, joinAnd(by)+" and approved", joinAnd(by)+" approved", 1)
		}
		a.Text = text
	default:
		need := max(ch.Approvals.Need, ch.Gates.Approvals.Need)
		fmt.Fprintf(&b, "%s has %d of %d approvals", who, len(by), need)
		if len(by) > 0 {
			fmt.Fprintf(&b, " (%s)", joinAnd(by))
		}
		b.WriteString(".")
		var can []string
		for _, u := range ch.Gates.Approvals.Eligible {
			if u != ch.Author {
				can = append(can, u)
			}
		}
		if len(can) > 0 && len(by) < need {
			fmt.Fprintf(&b, " %s can approve it", joinOr(can))
			if mr != "" {
				b.WriteString(" in " + mr)
			} else {
				b.WriteString(" once it is pushed and has a merge request")
			}
			b.WriteString(".")
		}
		b.WriteString(" I cannot approve: approvals come from people.")
		a.Text = b.String()
	}
	return true
}

func countingApprovals(ch *api.Change) []string {
	var by []string
	for _, ap := range ch.ApprovalsBy {
		if ap.User != "" && ap.Counts {
			by = append(by, ap.User)
		}
	}
	return by
}

func joinOr(words []string) string {
	if len(words) < 2 {
		return joinAnd(words)
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// ---------------------------------------------------------------- actions people take

var reActionReq = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+(?:please\s+)?)?(merge|approve|commit|push|deploy|revert|rebase|close|run the lab|run a lab|run its lab|start the lab|mark (?:it |this |my change )?ready)\b`)

// actionRequest answers "merge my change", "can you run the lab?": the
// chat cannot, so it says who can and what to click.
func (s *Svc) actionRequest(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	m := reActionReq.FindStringSubmatch(q)
	if m == nil {
		return false
	}
	verb := strings.ToLower(m[1])
	id, _ := changeRef(req, q)
	what := verb
	if strings.HasPrefix(verb, "mark") {
		what = "mark it ready"
	}
	if strings.Contains(verb, "lab") {
		what = "run the lab"
	}
	if id == "" {
		a.Text = fmt.Sprintf("I cannot %s: I only read and suggest edits; people decide. Open the change first (pick it in the top bar), then ask me where it stands.", what)
		return true
	}
	var next string
	switch {
	case verb == "merge":
		next = fmt.Sprintf("When the gates are green, merge it on the change page, or run `pk changes merge-change %s`.", id)
	case verb == "approve":
		next = fmt.Sprintf("Approvals come from reviewers in the merge request. If you may approve, run `pk changes approve-change %s`.", id)
	case verb == "commit":
		next = "In **Files**, click **Commit**, write the message (or click **Suggest message**) and commit."
	case verb == "push":
		next = fmt.Sprintf("On the change page, click **Push, open draft MR**, or run `pk changes push-change %s`.", id)
	case verb == "deploy":
		next = "Flux deploys main on its own after the merge: there is nothing to click."
	case verb == "revert":
		next = "Open a new change that undoes it: **Changes → New**."
	case verb == "rebase":
		next = fmt.Sprintf("Run `pk changes rebase-change %s`.", id)
	case verb == "close":
		next = fmt.Sprintf("Run `pk changes close-change %s`.", id)
	case strings.Contains(verb, "lab"):
		next = fmt.Sprintf("On the change page, click **Run lab**, or run `pk labs run %s --wait`.", id)
	default:
		next = "On the change page, click **Mark ready for review**."
	}
	if !s.changeBrief(ctx, p, req, id, q, a) {
		return false
	}
	a.Text = fmt.Sprintf("I cannot %s: I only read and suggest edits; people decide. %s\n\nWhere %s stands:\n\n", what, next, id) + a.Text
	return true
}

// ---------------------------------------------------------------- write requests

var (
	reRemoveReq = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+(?:please\s+)?)?(delete|remove|drop|decommission|get rid of|take out)\b`)
	reFixReq    = regexp.MustCompile(`(?i)\b(fix|repair|correct|solve|resolve|sort out)\b`)
	reBGPWord   = regexp.MustCompile(`(?i)\b(bgp|polic(y|ies)|peer(s|ing)?|sessions?)\b`)
	reWriteReq  = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+(?:please\s+)?)?(add|create|make|set|change|update|rename|move|bump|upgrade|downgrade|edit|modify|increase|decrease|scale|replace|write|configure|enable|disable|install|put)\b`)
	reToValue   = regexp.MustCompile(`(?i)\bto\s+(?:unit\s+|u|port\s+|version\s+|rack\s+)?([0-9][0-9a-z.:-]*|[a-z][a-z0-9-]*)\s*[?.!]*\s*$`)
)

// fieldWords maps words of a request to the field of a Node.
var fieldWords = []struct {
	re    *regexp.Regexp
	field string
}{
	{regexp.MustCompile(`(?i)\bunit\b|\bU\d+\b|\bslot\b`), "spec.location.unit"},
	{regexp.MustCompile(`(?i)\brack\b`), "spec.location.rack"},
	{regexp.MustCompile(`(?i)\brole\b|\bworker\b|\bcontrol[- ]plane\b`), "spec.role"},
	{regexp.MustCompile(`(?i)\bserial\b`), "spec.serial"},
	{regexp.MustCompile(`(?i)\bmodel\b`), "spec.model"},
	{regexp.MustCompile(`(?i)\bcluster\b`), "spec.cluster"},
	{regexp.MustCompile(`(?i)\bmac\b|\bbmc\b`), "spec.bmc.mac"},
	{regexp.MustCompile(`(?i)\bports?\b|\bcable\b`), "spec.ports"},
}

// fixRequest answers "fix the BGP policy for my change": a node of the
// change that the BGP policy of its rack misses gets a suggested edit;
// else the risks advisor says what needs fixing.
func (s *Svc) fixRequest(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	id, _ := changeRef(req, q)
	if id == "" {
		a.Text = "Which change should I look at? Open it (pick it in the top bar) or name it, for example \"Fix the BGP policy for c-p4x8ma\"."
		return true
	}
	gaps := s.bgpGaps(ctx, p, id)
	if len(gaps) > 0 {
		g := gaps[0]
		names := g.Names()
		lead := fmt.Sprintf("Your change adds %s to rack %s, but the BGP policy **%s** does not select %s, so %s would get no BGP session. ",
			joinAnd(names), g.Missing[0].Rack, g.Policy.Name, themIt(len(names)), joinAnd(names))
		if id != req.ChangeID {
			lead = strings.Replace(lead, "Your change adds", "**"+id+"** adds", 1)
		}
		r := req
		r.ChangeID = id
		if !s.suggestPolicyAdd(ctx, p, r, names, g.Policy.Name, a) {
			return true
		}
		a.Text = lead + a.Text
		return true
	}
	if reBGPWord.MatchString(q) {
		p.quick(ctx, "advise", "check BGP policies of "+id, "no node missing")
	}
	if s.o.Advisor == nil {
		a.Text = "I find no node of " + id + " missing from a BGP policy."
		return true
	}
	s.advise(ctx, p, req, id, advisor.Risks, a)
	lead := "I suggest fixes as diffs when the data shows one. "
	if reBGPWord.MatchString(q) {
		lead = "Every node of " + id + " is in the BGP policy of its rack, so there is nothing to add there. "
	}
	if len(a.Suggestions) == 0 {
		lead += "Here is what needs attention:\n\n"
	} else {
		lead += "The advisor has a fix below:\n\n"
	}
	a.Text = lead + a.Text
	return true
}

func themIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// bgpGaps returns the BGP policies of a change that miss nodes the change
// adds or edits in their rack.
func (s *Svc) bgpGaps(ctx context.Context, p *progress, id string) []advisor.BGPGap {
	var gaps []advisor.BGPGap
	_ = p.do(ctx, "advise", "check BGP policies of "+id, func() (string, error) {
		ch, err := s.o.Tools.Change(ctx, id)
		if err != nil {
			return "", err
		}
		var nodes []advisor.NodeInfo
		for _, f := range ch.ChangedFiles {
			if !strings.HasPrefix(f.Path, "control/inventory/") || f.Status == api.FileChangeDeleted {
				continue
			}
			fc, err := s.o.Tools.ReadFile(ctx, id, f.Path)
			if err != nil || !fc.Exists {
				continue
			}
			nodes = append(nodes, advisor.ParseNodes(f.Path, fc.Content)...)
		}
		if len(nodes) == 0 {
			return "no new nodes", nil
		}
		pols, err := s.findPolicies(ctx, id, "")
		if err != nil {
			return "", err
		}
		gaps = advisor.FindBGPGaps(nodes, pols)
		if len(gaps) == 0 {
			return plural(len(nodes), "node", "nodes") + " covered", nil
		}
		return joinAnd(gaps[0].Names()) + " missing in " + gaps[0].Policy.Name, nil
	})
	return gaps
}

// removeRequest answers "delete cn-034 from the inventory" and "remove
// cn-034 from the rack-6z BGP policy".
func (s *Svc) removeRequest(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	names := advisor.EntityNames(q)
	if len(names) == 0 {
		return false
	}
	id := req.ChangeID
	if m := reChangeID.FindString(q); m != "" {
		id = m
	}
	if reBGPWord.MatchString(q) && !strings.Contains(strings.ToLower(q), "inventory") {
		if id == "" {
			a.Text = "Open a change first. I make edits only as suggestions in a change."
			return true
		}
		want := policyNameIn(q)
		r := req
		r.ChangeID = id
		s.suggestPolicyRemove(ctx, p, r, names, want, a)
		return true
	}
	name := names[0]
	ref := id
	var e *api.Entity
	_ = p.do(ctx, "entity", "entity "+name, func() (string, error) {
		for _, k := range []string{"Node", "Jbod", "Switch"} {
			if ref != "" {
				if got, err := s.o.Tools.Entity(ctx, k, name, ref); err == nil && got != nil && got.Name != "" {
					e = got
					return fieldCount(e), nil
				}
			}
			if got, err := s.o.Tools.Entity(ctx, k, name, ""); err == nil && got != nil && got.Name != "" {
				e = got
				return fieldCount(e), nil
			}
		}
		return "not found", nil
	})
	if e == nil {
		a.Text = fmt.Sprintf("I find no **%s** in the inventory, so there is nothing to delete.", name)
		return true
	}
	addSource(a, llm.Source{Label: e.Name, Ref: e.Kind + "/" + e.Name, Kind: "entity"})
	path := e.Path
	if path == "" {
		path = "control/inventory/" + name + ".yaml"
	}
	// Who points at it: those files must change too.
	var policies []string
	var others []string
	for _, l := range e.Backlinks.Entities {
		switch {
		case l.Kind == "CiliumBGPPeeringPolicy":
			policies = append(policies, l.Name)
		case l.Kind != "" && l.Kind != "Cluster" && l.Kind != "Rack" && l.Kind != "Switch":
			others = append(others, l.Kind+" "+l.Name)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "I don't delete files: I suggest edits, and you decide.")
	if id == "" {
		fmt.Fprintf(&b, " Open a change first, then:\n\n1. Delete `%s`: `pk files delete-file <change id> --path %s`.", path, path)
	} else {
		fmt.Fprintf(&b, " To remove **%s** in **%s**:\n\n1. Delete `%s`: run `pk files delete-file %s --path %s`.", name, id, path, id, path)
	}
	step := 2
	var sugg bool
	if len(policies) > 0 {
		fmt.Fprintf(&b, "\n%d. Take it out of the BGP policy **%s**, which still selects it.", step, joinAnd(policies))
		step++
		if id != "" {
			r := req
			r.ChangeID = id
			d := &Answer{Sources: []llm.Source{}}
			if s.suggestPolicyRemove(ctx, p, r, []string{name}, policies[0], d) && len(d.Suggestions) > 0 {
				a.Suggestions = append(a.Suggestions, d.Suggestions...)
				for _, src := range d.Sources {
					addSource(a, src)
				}
				b.WriteString(" I suggest that edit below.")
				sugg = true
			}
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(&b, "\n%d. Check what else points at it: %s.", step, strings.Join(others, ", "))
		step++
	}
	fmt.Fprintf(&b, "\n%d. Commit, then run the lab.", step)
	if !sugg && len(policies) == 0 && len(others) == 0 {
		b.WriteString("\n\nNothing else in the repo points at it.")
	}
	a.Text = b.String()
	return true
}

// writeRequest answers a request to change something the chat has no edit
// for: it names the file and field to change and the exact next step.
func (s *Svc) writeRequest(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	m := reWriteReq.FindStringSubmatch(q)
	if m == nil {
		return false
	}
	verb := strings.ToLower(m[1])
	id := req.ChangeID
	if c := reChangeID.FindString(q); c != "" {
		id = c
	}
	inChange := "in your change **" + id + "**"
	if id == "" {
		inChange = "in a change (open one first: **Changes → New**)"
	}
	idArg := id
	if idArg == "" {
		idArg = "<change id>"
	}
	names := advisor.EntityNames(q)
	if len(names) > 0 {
		name := names[0]
		var e *api.Entity
		for _, k := range []string{"Node", "Jbod", "Switch"} {
			if id != "" {
				if got, err := s.o.Tools.Entity(ctx, k, name, id); err == nil && got != nil && got.Name != "" {
					e = got
					break
				}
			}
			if got, err := s.o.Tools.Entity(ctx, k, name, ""); err == nil && got != nil && got.Name != "" {
				e = got
				break
			}
		}
		if e == nil {
			p.quick(ctx, "entity", "entity "+name, "not in the inventory")
			if verb == "add" || verb == "create" || verb == "make" || verb == "put" {
				a.Text = fmt.Sprintf("**%s** is not in the inventory yet. Create it %s: **Files → New file**, kind **Node**, name %s, or run `pk files new-file %s --kind Node --name %s`. "+
					"Then fill in the rack, unit and ports, and ask me to add it to the BGP policy of its rack.", name, inChange, name, idArg, name)
				s.addRunbook(ctx, a)
				return true
			}
			a.Text = fmt.Sprintf("I find no **%s** in the inventory. Check the name, for example on the Assets page.", name)
			return true
		}
		p.quick(ctx, "entity", "entity "+e.Kind+"/"+e.Name, fieldCount(e))
		addSource(a, llm.Source{Label: e.Name, Ref: e.Kind + "/" + e.Name, Kind: "entity"})
		path := e.Path
		if path == "" {
			path = "control/inventory/" + name + ".yaml"
		}
		field := ""
		for _, fw := range fieldWords {
			if fw.re.MatchString(q) {
				field = fw.field
				break
			}
		}
		if field == "" || field == "spec.ports" {
			a.Text = fmt.Sprintf("I can't make that edit myself: I suggest edits as diffs for BGP policies only. "+
				"To change **%s**, open `%s` %s: the editor shows each field as a form. Save, commit, and run the lab. Ask me about the risks before you merge.", name, path, inChange)
			if field == "spec.ports" {
				a.Text += " The switch ports in use are on the switch page, or ask me \"Which ports are free on data-a?\"."
			}
			return true
		}
		value := "<value>"
		if v := reToValue.FindStringSubmatch(q); v != nil {
			value = v[1]
		}
		cur := ""
		for _, f := range e.Fields {
			if f.Field == field {
				cur = fieldDisplay(f)
			}
		}
		a.Text = fmt.Sprintf("I can't make that edit myself: I suggest edits as diffs for BGP policies only. Set `%s` of **%s** %s", field, name, inChange)
		if cur != "" {
			a.Text += fmt.Sprintf(" (now %s)", cur)
		}
		a.Text += fmt.Sprintf(":\n\n```\npk edit %s --set %s=%s\n```\n\nOr open `%s` in the editor and change the field in the form. Then commit and run the lab.", name, field, value, path)
		return true
	}
	// Talos or Kubernetes: the talconfig of the cluster.
	if reTalosWord.MatchString(q) || reK8sWord.MatchString(q) {
		if tree, err := s.o.Tools.Tree(ctx, "main"); err == nil {
			rest := appTerms(q, clusterNames(tree))
			for w := range platformWords {
				delete(rest, w)
			}
			if len(rest) == 0 && s.talconfigEdit(ctx, p, q, inChange, a) {
				return true
			}
		}
	}
	// No entity: find the file that holds the thing ("bump podinfo to 6.9").
	terms := appTerms(q, nil)
	if len(terms) > 0 {
		wls := s.findWorkloads(ctx, p, "main", terms)
		if len(wls) > 0 {
			w := wls[0]
			cur := w.version()
			a.Text = fmt.Sprintf("I can't make that edit myself: I suggest edits as diffs for BGP policies only. **%s** on %s is set in `%s`", w.name, w.cluster, w.path)
			if cur != "" {
				a.Text += fmt.Sprintf(" (now %s)", cur)
			}
			field := "spec.template.spec.containers[0].image"
			if w.kind == "HelmRelease" {
				field = "spec.chart.spec.version"
			}
			a.Text += fmt.Sprintf(". Change `%s` there %s, then commit and run the lab.", field, inChange)
			addSource(a, llm.Source{Label: shortPath(w.path), Ref: w.path, Kind: "file"})
			if pend := s.pendingChanges(ctx, p, req, w.path, a); pend != "" {
				a.Text += " " + pend
			}
			return true
		}
	}
	a.Text = fmt.Sprintf("I can't make that edit myself: I suggest edits as diffs for BGP policies only, for example \"Add cn-037 to the rack-6z BGP policy\". "+
		"For other edits, open the file %s in **Files**, change it, and ask me about the risks before you merge.", inChange)
	return true
}

// addRunbook adds the add-node runbook as a source when it exists.
func (s *Svc) addRunbook(ctx context.Context, a *Answer) {
	const rb = "kb/runbooks/add-node.md"
	if fc, err := s.o.Tools.ReadFile(ctx, "main", rb); err == nil && fc.Exists {
		addSource(a, llm.Source{Label: shortPath(rb), Ref: rb, Kind: "kb"})
		a.Text += " The runbook **add-node.md** has all steps."
	}
}

// policyNameIn returns the policy name in "the rack-6z BGP policy", or "".
func policyNameIn(q string) string {
	if m := rePolicyName.FindStringSubmatch(q); m != nil && !strings.EqualFold(m[1], "bgp") && !strings.EqualFold(m[1], "the") && !strings.EqualFold(m[1], "peering") {
		return m[1]
	}
	if m := rePolicyRef.FindString(q); m != "" {
		return m
	}
	return ""
}

// findPolicies returns the BGP policies of a change (want: a policy or
// rack name, or "" for all).
func (s *Svc) findPolicies(ctx context.Context, ref, want string) ([]advisor.BGPPolicy, error) {
	nodes, err := s.o.Tools.Tree(ctx, ref)
	if err != nil {
		return nil, err
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
			fc, err := s.o.Tools.ReadFile(ctx, ref, n.Path)
			if err != nil {
				continue
			}
			for _, pol := range advisor.ParseBGP(n.Path, fc.Content) {
				if want == "" || strings.EqualFold(pol.Name, want) || strings.EqualFold(pol.Rack, want) {
					found = append(found, pol)
				}
			}
		}
	}
	walk(nodes)
	return found, nil
}

// suggestPolicyRemove suggests taking nodes out of a BGP policy.
func (s *Svc) suggestPolicyRemove(ctx context.Context, p *progress, req Request, names []string, want string, a *Answer) bool {
	var found []advisor.BGPPolicy
	if err := p.do(ctx, "search", "find BGP policies in "+req.ChangeID, func() (string, error) {
		var err error
		found, err = s.findPolicies(ctx, req.ChangeID, want)
		var ns []string
		for _, f := range found {
			ns = append(ns, f.Name)
		}
		if len(ns) == 0 {
			return "none", err
		}
		return joinAnd(ns), err
	}); err != nil || len(found) == 0 {
		a.Text = "I find no BGP policy " + want + " in " + req.ChangeID + "."
		return false
	}
	var pol *advisor.BGPPolicy
	for i := range found {
		for _, n := range names {
			if found[i].Selects(n) {
				pol = &found[i]
			}
		}
	}
	if pol == nil {
		a.Text = fmt.Sprintf("%s %s not in the BGP policy %s, so there is nothing to remove.", joinAnd(names), isAre(len(names)), found[0].Name)
		return false
	}
	fc, err := s.o.Tools.ReadFile(ctx, req.ChangeID, pol.Path)
	if err != nil {
		a.Text = "I cannot read " + pol.Path + ": " + err.Error()
		return false
	}
	addSource(a, llm.Source{Label: shortPath(pol.Path), Ref: pol.Path, Kind: "file"})
	after, removed, err := removeHostnames(fc.Content, pol.Name, names)
	if err != nil || len(removed) == 0 {
		a.Text = "I cannot edit the policy: the hostname list has a form I don't edit. Remove the line by hand in " + shortPath(pol.Path) + "."
		return false
	}
	n := p.begin("suggest_edit", "suggest "+shortPath(pol.Path))
	p.end(ctx, n, "-"+plural(len(removed), "line", "lines"), false)
	a.Text = fmt.Sprintf("I suggest removing %s from %s: %s would lose %s BGP session. Review the diff below, then accept or reject it.",
		joinAnd(removed), pol.Name, joinAnd(removed), map[bool]string{true: "its", false: "their"}[len(removed) == 1])
	a.Suggestions = append(a.Suggestions, advisor.Suggestion{
		ChangeID: req.ChangeID, Path: pol.Path, Before: fc.Content, After: after,
		Title: fmt.Sprintf("Remove %s from the BGP policy %s", joinAnd(removed), pol.Name),
	})
	return true
}

// removeHostnames removes node names from the kubernetes.io/hostname In
// list of a BGP policy (block style), keeping the rest of the text.
func removeHostnames(content, policy string, names []string) (string, []string, error) {
	dec := yaml.NewDecoder(strings.NewReader(content))
	lines := strings.SplitAfter(content, "\n")
	for {
		var root yaml.Node
		if err := dec.Decode(&root); err != nil {
			return "", nil, fmt.Errorf("no BGP policy %s in the file", policy)
		}
		doc := &root
		if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
			doc = doc.Content[0]
		}
		if k := yget(doc, "kind"); k == nil || k.Value != "CiliumBGPPeeringPolicy" {
			continue
		}
		if md := yget(doc, "metadata"); policy != "" && (md == nil || yget(md, "name") == nil || yget(md, "name").Value != policy) {
			continue
		}
		var values *yaml.Node
		for _, e := range seq(yget(yget(yget(doc, "spec"), "nodeSelector"), "matchExpressions")) {
			if k := yget(e, "key"); k != nil && k.Value == "kubernetes.io/hostname" {
				if op := yget(e, "operator"); op != nil && op.Value == "In" {
					values = yget(e, "values")
				}
			}
		}
		if values == nil || values.Kind != yaml.SequenceNode || values.Style&yaml.FlowStyle != 0 {
			return "", nil, fmt.Errorf("no block hostname list in %s", policy)
		}
		drop := map[int]bool{}
		var removed []string
		for _, v := range values.Content {
			for _, n := range names {
				if v.Value == n && v.Line-1 < len(lines) && strings.TrimSpace(strings.TrimSpace(lines[v.Line-1])[1:]) == n {
					drop[v.Line-1] = true
					removed = append(removed, n)
				}
			}
		}
		var out strings.Builder
		for i, l := range lines {
			if !drop[i] {
				out.WriteString(l)
			}
		}
		return out.String(), removed, nil
	}
}

func yget(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func seq(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

// ---------------------------------------------------------------- versions

var (
	reVersionQ  = regexp.MustCompile(`(?i)\bversions?\b|\bwhich (image|chart|tag|release)\b|\bimage tags?\b`)
	reReplicasQ = regexp.MustCompile(`(?i)\breplicas?\b|\bhow many (pods|instances|copies)\b`)
)

// workload is a Deployment, StatefulSet, DaemonSet or HelmRelease.
type workload struct {
	path, cluster, kind, name string
	image, chart, chartVer    string
	replicas                  int
	interval                  string
}

// label is the short line of a workload for a step: podinfo 6.8.0, Ceph
// v19.2.3.
func (w workload) label() string {
	if w.kind == "CephCluster" {
		return "Ceph " + w.version()
	}
	return w.name + " " + w.version()
}

func (w workload) version() string {
	if w.kind == "HelmRelease" {
		return w.chartVer
	}
	if i := strings.LastIndex(w.image, ":"); i >= 0 && !strings.Contains(w.image[i:], "/") {
		return w.image[i+1:]
	}
	return ""
}

// genericWords are question words that name no app.
var genericWords = set(`version versions run runs running deployed deploy image images chart charts tag tags release releases helm app apps
	application applications cluster clusters which what does is the on in at of currently current now there today bump upgrade update set
	change to please can you could would should add make move new latest`)

// appTerms returns the words of a question that can name an app (podinfo,
// cilium), without cluster names.
func appTerms(q string, clusters map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, t := range llm.Tokenize(q) {
		if genericWords[t] || commonWord[t] || clusters[t] || len(t) < 3 || strings.ContainsAny(t, "0123456789") {
			continue
		}
		out[t] = true
	}
	return out
}

// clusterNames lists the clusters of a ref (clusters/<name>/).
func clusterNames(tree []api.FileNode) map[string]bool {
	out := map[string]bool{}
	var walk func([]api.FileNode)
	walk = func(ns []api.FileNode) {
		for _, n := range ns {
			parts := strings.Split(n.Path, "/")
			if len(parts) >= 2 && parts[0] == "clusters" && parts[1] != "" {
				out[parts[1]] = true
			}
			walk(n.Children)
		}
	}
	walk(tree)
	return out
}

// findWorkloads reads the workloads of a ref whose path or name has one of
// the terms.
func (s *Svc) findWorkloads(ctx context.Context, p *progress, ref string, terms map[string]bool) []workload {
	var out []workload
	tree, treeErr := s.o.Tools.Tree(ctx, ref)
	// The step names the terms that match a path; a stray word of the
	// question ("slow", "please") does not show up in it.
	label := matchedTerms(tree, terms)
	_ = p.do(ctx, "search", "find "+strings.Join(label, ", ")+" in clusters/", func() (string, error) {
		if treeErr != nil {
			return "", treeErr
		}
		var walk func([]api.FileNode)
		walk = func(ns []api.FileNode) {
			for _, n := range ns {
				walk(n.Children)
				if n.Type != api.FileNodeTypeFile || !strings.HasPrefix(n.Path, "clusters/") || !strings.HasSuffix(n.Path, ".yaml") {
					continue
				}
				switch n.Kind {
				case "", "Deployment", "StatefulSet", "DaemonSet", "HelmRelease", "CephCluster":
				default:
					continue
				}
				hit := false
				for t := range terms {
					if strings.Contains(strings.ToLower(n.Path), t) {
						hit = true
					}
				}
				if !hit {
					continue
				}
				fc, err := s.o.Tools.ReadFile(ctx, ref, n.Path)
				if err != nil {
					continue
				}
				out = append(out, workloadsIn(n.Path, fc.Content)...)
			}
		}
		walk(tree)
		sort.SliceStable(out, func(i, j int) bool { return out[i].cluster < out[j].cluster })
		if len(out) == 0 {
			return "none", nil
		}
		kinds := map[string]int{}
		for _, w := range out {
			kinds[w.kind]++
		}
		var parts []string
		for k, n := range kinds {
			parts = append(parts, pluralKind(n, k))
		}
		sort.Strings(parts)
		return strings.Join(parts, ", "), nil
	})
	return out
}

// matchedTerms lists the terms that occur in a path under clusters/, or all
// terms when none does.
func matchedTerms(tree []api.FileNode, terms map[string]bool) []string {
	hit := map[string]bool{}
	var walk func([]api.FileNode)
	walk = func(ns []api.FileNode) {
		for _, n := range ns {
			walk(n.Children)
			if !strings.HasPrefix(n.Path, "clusters/") {
				continue
			}
			for t := range terms {
				if strings.Contains(strings.ToLower(n.Path), t) {
					hit[t] = true
				}
			}
		}
	}
	walk(tree)
	if len(hit) == 0 {
		hit = terms
	}
	out := make([]string, 0, len(hit))
	for t := range hit {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// workloadsIn parses the workloads of a file.
func workloadsIn(path, content string) []workload {
	var out []workload
	for _, d := range repo.ParseFile(path, []byte(content)).Docs {
		w := workload{path: path, cluster: clusterOfPath(path), kind: d.Kind, name: d.Name}
		switch d.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
			switch r := kindutil.Get(d.Data, "spec", "replicas").(type) {
			case int:
				w.replicas = r
			case int64:
				w.replicas = int(r)
			case uint64:
				w.replicas = int(r)
			case float64:
				w.replicas = int(r)
			}
			if cs, ok := kindutil.Get(d.Data, "spec", "template", "spec", "containers").([]any); ok && len(cs) > 0 {
				if c, ok := cs[0].(map[string]any); ok {
					w.image, _ = c["image"].(string)
				}
			}
		case "HelmRelease":
			w.chart, _ = kindutil.Get(d.Data, "spec", "chart", "spec", "chart").(string)
			w.chartVer = fmt.Sprint(orEmpty(kindutil.Get(d.Data, "spec", "chart", "spec", "version")))
			w.interval, _ = kindutil.Get(d.Data, "spec", "interval").(string)
		case "CephCluster":
			w.image, _ = kindutil.Get(d.Data, "spec", "cephVersion", "image").(string)
		default:
			continue
		}
		out = append(out, w)
	}
	return out
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func clusterOfPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 2 && parts[0] == "clusters" {
		return parts[1]
	}
	return ""
}

// versionQuestion answers "which podinfo version runs on defiant?" from
// the desired state on main, and names open changes that change it.
func (s *Svc) versionQuestion(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	platform, replicas := isPlatformVersionQ(q), reReplicasQ.MatchString(q)
	if !reVersionQ.MatchString(q) && !platform && !replicas {
		return false
	}
	tree, err := s.o.Tools.Tree(ctx, "main")
	if err != nil {
		return false
	}
	clusters := clusterNames(tree)
	terms := appTerms(q, clusters)
	for w := range platformWords {
		delete(terms, w)
	}
	for _, w := range []string{"replica", "replicas", "pods", "instances", "copies"} {
		delete(terms, w)
	}
	var wls []workload
	if len(terms) > 0 {
		wls = s.findWorkloads(ctx, p, "main", terms)
	}
	if len(wls) == 0 {
		// No app: Talos and Kubernetes come from the talconfig of a
		// cluster; Flux is not pinned in the repo.
		switch {
		case platform:
			return s.platformVersion(ctx, p, req, q, tree, a)
		case terms["flux"] || terms["fluxcd"]:
			return s.fluxVersion(ctx, p, tree, a)
		}
		return false
	}
	var want string
	for _, t := range llm.Tokenize(q) {
		if clusters[t] {
			want = t
		}
	}
	if want != "" {
		var keep []workload
		for _, w := range wls {
			if w.cluster == want {
				keep = append(keep, w)
			}
		}
		if len(keep) == 0 && len(wls) > 0 {
			var where []string
			for _, w := range wls {
				where = append(where, w.cluster)
			}
			// A CephCluster is named after its cluster: say "Ceph", not
			// "defiant does not run pegasus".
			what := wls[0].name
			if wls[0].kind == "CephCluster" {
				what = "Ceph"
			}
			a.Text = fmt.Sprintf("**%s** does not run %s. It runs on %s", want, what, joinAnd(uniq(where)))
			if v := wls[0].version(); v != "" && len(uniq(where)) == 1 {
				a.Text += fmt.Sprintf(" (%s **%s**)", what, v)
			}
			a.Text += "."
			for _, w := range wls {
				addSource(a, llm.Source{Label: shortPath(w.path), Ref: w.path, Kind: "file"})
			}
			return true
		}
		wls = keep
	}
	if len(wls) == 0 {
		return false
	}
	describe := func(w workload) string {
		if w.kind == "HelmRelease" {
			d := fmt.Sprintf("the **%s** chart **%s**", w.chart, w.chartVer)
			if w.interval != "" {
				d += fmt.Sprintf(" (HelmRelease, Flux checks it every %s)", w.interval)
			}
			return d
		}
		if w.kind == "CephCluster" {
			return fmt.Sprintf("Ceph **%s** (CephCluster **%s**, image `%s`)", w.version(), w.name, w.image)
		}
		d := fmt.Sprintf("**%s %s**", w.name, w.version())
		var extra []string
		if w.image != "" {
			extra = append(extra, "image `"+w.image+"`")
		}
		if w.replicas > 0 {
			extra = append(extra, plural(w.replicas, "replica", "replicas"))
		}
		if len(extra) > 0 {
			d += " (" + strings.Join(extra, ", ") + ")"
		}
		return d
	}
	var b strings.Builder
	if len(wls) == 1 {
		w := wls[0]
		n := p.begin("read_file", "read "+shortPath(w.path))
		p.end(ctx, n, w.label(), false)
		if replicas && w.replicas > 0 {
			fmt.Fprintf(&b, "**%s** on %s runs **%s** (%s, image `%s`). That is the desired state on main, in `%s`; Flux applies it.",
				w.name, w.cluster, plural(w.replicas, "replica", "replicas"), w.name+" "+w.version(), w.image, w.path)
		} else {
			fmt.Fprintf(&b, "**%s** runs %s. That is the desired state on main, in `%s`; Flux applies it.", w.cluster, describe(w), w.path)
		}
	} else {
		n := p.begin("read_file", fmt.Sprintf("read %s", plural(len(wls), "file", "files")))
		p.end(ctx, n, joinAnd(uniq(versions(wls))), false)
		vs := uniq(versions(wls))
		if len(vs) == 1 {
			fmt.Fprintf(&b, "%s runs **%s** in all %s clusters (%s), from the desired state on main.", wls[0].name, vs[0], fmt.Sprint(len(wls)), joinAnd(clustersOf(wls)))
		} else {
			fmt.Fprintf(&b, "%s runs in %s, from the desired state on main:", wls[0].name, plural(len(wls), "cluster", "clusters"))
			for _, w := range wls {
				fmt.Fprintf(&b, "\n- **%s:** %s", w.cluster, describe(w))
			}
		}
	}
	for i, w := range wls {
		if i < 3 {
			addSource(a, llm.Source{Label: shortPath(w.path), Ref: w.path, Kind: "file"})
		}
	}
	if pend := s.pendingChanges(ctx, p, req, wls[0].path, a); pend != "" {
		b.WriteString("\n\n" + pend)
	}
	a.Text = b.String()
	return true
}

// pendingChanges names the open changes that change a file, with the new
// version when the file is a workload.
func (s *Svc) pendingChanges(ctx context.Context, p *progress, req Request, path string, a *Answer) string {
	var lines, ids []string
	_ = p.do(ctx, "change", "check open changes", func() (string, error) {
		all, err := s.o.Tools.Changes(ctx)
		if err != nil {
			return "", err
		}
		n := 0
		for _, c := range all {
			switch c.State {
			case api.ChangeStateMerged, api.ChangeStateDeployed, api.ChangeStateVerified, api.ChangeStateClosed:
				continue
			}
			if n >= 8 {
				break
			}
			n++
			ch, err := s.o.Tools.Change(ctx, c.Id)
			if err != nil {
				continue
			}
			touches := false
			for _, f := range ch.ChangedFiles {
				if f.Path == path {
					touches = true
				}
			}
			if !touches {
				continue
			}
			who := "**" + c.Id + "**"
			if c.Id == req.ChangeID {
				who = "Your change " + who
			} else {
				who = "Open change " + who
			}
			line := fmt.Sprintf("%s (*%s*, %s, %s) changes it", who, c.Title, c.Author, stateWords(c.State))
			if fc, err := s.o.Tools.ReadFile(ctx, c.Id, path); err == nil && fc.Exists {
				if ws := workloadsIn(path, fc.Content); len(ws) > 0 && ws[0].version() != "" {
					line += " to **" + ws[0].version() + "**"
				}
			}
			lines = append(lines, line+".")
			ids = append(ids, c.Id)
			addSource(a, llm.Source{Label: c.Id, Ref: c.Id, Kind: "change"})
		}
		if len(lines) == 0 {
			return "none changes it", nil
		}
		if len(ids) == 1 {
			return ids[0] + " changes it", nil
		}
		return joinAnd(ids) + " change it", nil
	})
	return strings.Join(lines, " ")
}

func versions(ws []workload) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.version())
	}
	return out
}

func clustersOf(ws []workload) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.cluster)
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
