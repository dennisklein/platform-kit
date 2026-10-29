package policy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/identity"
	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// MetaGlob is ignored for rule matching (the change metadata file).
const MetaGlob = ".pk/changes/**"

// Principal kinds in writers and approvers lists.
const (
	PrincipalAgent = "agent"
	TeamPrefix     = "team:"
)

// DefaultEngine is the policy engine of the bench, the merge check and pk ci.
type DefaultEngine struct {
	// MainDir returns the directory with the target tree (main), for Current.
	MainDir func(ctx context.Context) (string, error)
}

var _ Engine = (*DefaultEngine)(nil)

// NewEngine returns the engine. mainDir may be nil when Current is not used.
func NewEngine(mainDir func(ctx context.Context) (string, error)) *DefaultEngine {
	return &DefaultEngine{MainDir: mainDir}
}

// Load parses and validates a policy file.
func (e *DefaultEngine) Load(data []byte) (*Policy, []kinds.Problem, error) {
	return Load(data)
}

// Load parses a policy file and returns schema and consistency problems. It
// returns an error only when the file cannot be used at all.
func Load(data []byte) (*Policy, []kinds.Problem, error) {
	problems := []kinds.Problem{}
	docs, serr := schema.ParseYAML(data)
	if serr != nil {
		problems = append(problems, kinds.Problem{Path: Path, Line: serr.Line, Col: serr.Col, Severity: kinds.SevError, Message: serr.Message, Source: kinds.SourceYAML})
		return nil, problems, fmt.Errorf("%s: line %d: %s", Path, serr.Line, serr.Message)
	}
	if len(docs) == 0 {
		return nil, problems, fmt.Errorf("%s is empty", Path)
	}
	doc := docs[0]
	raw, err := Schema()
	if err != nil {
		return nil, problems, err
	}
	v, err := schema.Compile(raw)
	if err != nil {
		return nil, problems, err
	}
	for _, se := range v.Validate(doc) {
		problems = append(problems, kinds.Problem{Path: Path, Line: se.Line, Col: se.Col, Severity: kinds.SevError, Message: se.Message,
			Source: kinds.SourceSchema, Field: se.Field, Code: se.Keyword})
	}
	var p Policy
	tmp := kinds.Doc{Data: asMap(schema.ToValue(doc))}
	if err := tmp.Decode(&p); err != nil {
		return nil, problems, fmt.Errorf("%s: %w", Path, err)
	}
	problems = append(problems, p.check(doc)...)
	return &p, problems, nil
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// check finds inconsistencies the schema cannot see.
func (p *Policy) check(n *yaml.Node) []kinds.Problem {
	var out []kinds.Problem
	pos := func(field string) (int, int) {
		if n == nil {
			return 0, 0
		}
		loc, err := schema.ParseFieldPath(field)
		if err != nil {
			return 0, 0
		}
		v, _, _ := schema.Lookup(n, loc)
		if v == nil {
			return 0, 0
		}
		return v.Line, v.Column
	}
	add := func(field, format string, args ...any) {
		l, c := pos(field)
		out = append(out, kinds.Problem{Path: Path, Line: l, Col: c, Severity: kinds.SevError, Message: fmt.Sprintf(format, args...), Source: kinds.SourceKind, Field: field})
	}
	ids := map[string]string{}
	roles := map[string]bool{}
	teams := map[string]bool{}
	for i, pe := range p.People {
		if prev, ok := ids[pe.ID]; ok {
			add(fmt.Sprintf("people[%d].id", i), "id %s is also used by a %s", pe.ID, prev)
		}
		ids[pe.ID] = "person"
		for _, r := range pe.Roles {
			roles[r] = true
		}
		for _, t := range pe.Teams {
			teams[t] = true
		}
	}
	for i, a := range p.Agents {
		if prev, ok := ids[a.ID]; ok {
			add(fmt.Sprintf("agents[%d].id", i), "id %s is also used by a %s", a.ID, prev)
		}
		ids[a.ID] = "agent"
		if p.Person(a.ActsFor) == nil {
			add(fmt.Sprintf("agents[%d].actsFor", i), "agent %s acts for %s, who is not a person in the policy", a.ID, a.ActsFor)
		}
	}
	for i, b := range p.Bots {
		if prev, ok := ids[b.ID]; ok {
			add(fmt.Sprintf("bots[%d].id", i), "id %s is also used by a %s", b.ID, prev)
		}
		ids[b.ID] = "bot"
	}
	known := func(pr string) bool {
		switch {
		case pr == PrincipalAgent:
			return true
		case strings.HasPrefix(pr, TeamPrefix):
			return teams[strings.TrimPrefix(pr, TeamPrefix)]
		}
		return roles[pr] || ids[pr] != ""
	}
	names := map[string]bool{}
	for i, r := range p.Rules {
		if names[r.Name] {
			add(fmt.Sprintf("rules[%d].name", i), "rule name %s is used twice", r.Name)
		}
		names[r.Name] = true
		for j, g := range r.Paths {
			if !doublestar.ValidatePattern(g) {
				add(fmt.Sprintf("rules[%d].paths[%d]", i, j), "%s is not a valid path glob", g)
			}
		}
		for j, w := range r.Writers {
			if !known(w) {
				add(fmt.Sprintf("rules[%d].writers[%d]", i, j), "writer %s is no role, team, person, agent or bot of the policy", w)
			}
		}
		for j, a := range r.Approvers {
			if !known(a) {
				add(fmt.Sprintf("rules[%d].approvers[%d]", i, j), "approver %s is no role, team or person of the policy", a)
			}
			if a == PrincipalAgent || ids[a] == "agent" || ids[a] == "bot" {
				add(fmt.Sprintf("rules[%d].approvers[%d]", i, j), "agents and bots never approve")
			}
		}
		if r.Approvals > 0 && len(p.eligible([]Rule{r}, "")) < r.Approvals {
			l, c := pos(fmt.Sprintf("rules[%d].approvals", i))
			out = append(out, kinds.Problem{Path: Path, Line: l, Col: c, Severity: kinds.SevWarning, Source: kinds.SourceKind, Field: fmt.Sprintf("rules[%d].approvals", i),
				Message: fmt.Sprintf("rule %s needs %d approvals, but only %d people can approve", r.Name, r.Approvals, len(p.eligible([]Rule{r}, "")))})
		}
	}
	return out
}

// Person returns a person by id.
func (p *Policy) Person(id string) *Person {
	for i := range p.People {
		if p.People[i].ID == id {
			return &p.People[i]
		}
	}
	return nil
}

// Current reads the policy of the target tree.
func (e *DefaultEngine) Current(ctx context.Context) (*Policy, error) {
	if e.MainDir == nil {
		return nil, errors.New("policy: no main tree")
	}
	dir, err := e.MainDir(ctx)
	if err != nil {
		return nil, err
	}
	return LoadFile(filepath.Join(dir, Path))
}

// LoadFile reads a policy file; problems are ignored when the policy is usable.
func LoadFile(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, _, err := Load(b)
	return p, err
}

// Identify returns the identity of a person, agent or bot id.
func (e *DefaultEngine) Identify(p *Policy, user string) (identity.Identity, bool) {
	return p.Identify(user)
}

// Identify returns the identity of a person, agent or bot id.
func (p *Policy) Identify(user string) (identity.Identity, bool) {
	if pe := p.Person(user); pe != nil {
		return identity.Identity{User: pe.ID, Kind: identity.Human, Name: pe.Name, Email: pe.Email}, true
	}
	for _, a := range p.Agents {
		if a.ID == user {
			return identity.Identity{User: a.ID, Kind: identity.Agent, ActsFor: a.ActsFor, Email: a.ID + "@pk.local"}, true
		}
	}
	for _, b := range p.Bots {
		if b.ID == user {
			return identity.Identity{User: b.ID, Kind: identity.Bot, Email: b.ID + "@pk.local"}, true
		}
	}
	return identity.Identity{}, false
}

// MatchRule returns the first rule whose globs match the path, or nil.
// Paths below .pk/changes/ match no rule.
func (p *Policy) MatchRule(path string) *Rule {
	if IsMeta(path) {
		return nil
	}
	for i := range p.Rules {
		for _, g := range p.Rules[i].Paths {
			if ok, _ := doublestar.Match(g, path); ok {
				return &p.Rules[i]
			}
		}
	}
	return nil
}

// IsMeta reports whether a path is change metadata (ignored by rules).
func IsMeta(path string) bool {
	ok, _ := doublestar.Match(MetaGlob, path)
	return ok
}

// holds reports whether a person (or the person behind an agent) is one of
// the principals.
func (p *Policy) holds(pe *Person, principals []string) bool {
	if pe == nil {
		return false
	}
	for _, pr := range principals {
		switch {
		case pr == pe.ID:
			return true
		case strings.HasPrefix(pr, TeamPrefix):
			if slices.Contains(pe.Teams, strings.TrimPrefix(pr, TeamPrefix)) {
				return true
			}
		case slices.Contains(pe.Roles, pr):
			return true
		}
	}
	return false
}

// mayWrite checks one identity against a rule.
func (p *Policy) mayWrite(who identity.Identity, r *Rule) (bool, string) {
	switch who.Kind {
	case identity.Agent:
		if !slices.Contains(r.Writers, PrincipalAgent) && !slices.Contains(r.Writers, who.User) {
			return false, fmt.Sprintf("agents may not write (rule %s)", r.Name)
		}
		pe := p.Person(who.ActsFor)
		if pe == nil {
			return false, fmt.Sprintf("%s acts for %s, who is not in the policy", who.User, who.ActsFor)
		}
		if !p.holds(pe, r.Writers) {
			return false, fmt.Sprintf("%s acts for %s, who may not write (rule %s: %s)", who.User, pe.ID, r.Name, strings.Join(r.Writers, ", "))
		}
		return true, ""
	case identity.Bot:
		if slices.Contains(r.Writers, who.User) {
			return true, ""
		}
		return false, fmt.Sprintf("%s may not write (rule %s: %s)", who.User, r.Name, strings.Join(r.Writers, ", "))
	default:
		pe := p.Person(who.User)
		if pe == nil {
			return false, fmt.Sprintf("%s is not a person in the policy", who.User)
		}
		if p.holds(pe, r.Writers) {
			return true, ""
		}
		return false, fmt.Sprintf("%s may not write (rule %s: %s)", who.User, r.Name, strings.Join(r.Writers, ", "))
	}
}

// CanWrite reports the write rights of who for paths.
func (e *DefaultEngine) CanWrite(p *Policy, who identity.Identity, paths []string) []PathRight {
	out := []PathRight{}
	for _, path := range paths {
		r := p.MatchRule(path)
		if r == nil {
			if IsMeta(path) {
				continue
			}
			out = append(out, PathRight{Path: path, Who: who.User, OK: false, Message: "no rule matches " + path})
			continue
		}
		ok, msg := p.mayWrite(who, r)
		out = append(out, PathRight{Path: path, Rule: r.Name, Who: who.User, OK: ok, Message: msg})
	}
	return out
}

// eligible returns the people who are approvers of all rules, without the
// excluded person.
func (p *Policy) eligible(rules []Rule, exclude ...string) []string {
	var out []string
	for i := range p.People {
		pe := &p.People[i]
		if slices.Contains(exclude, pe.ID) {
			continue
		}
		ok := len(rules) > 0
		for _, r := range rules {
			if !p.holds(pe, r.Approvers) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, pe.ID)
		}
	}
	return out
}

// LabCoversHead reports whether a lab run on an older sha still counts for
// the head: the commits after the tested sha only touch the change metadata
// (.pk/changes/**) and the change's lab reports (kb/labs/<id>-run-*.md).
// paths are the files changed between the tested sha and the head.
func LabCoversHead(changeID string, paths []string) bool {
	for _, p := range paths {
		if IsMeta(p) {
			continue
		}
		if ok, _ := doublestar.Match("kb/labs/"+changeID+"-run-*.md", p); ok {
			continue
		}
		return false
	}
	return true
}

// Evaluate computes the gates of a change.
func (e *DefaultEngine) Evaluate(p *Policy, in GateInput) Gates {
	return p.Evaluate(in)
}

// Evaluate computes the gates of a change.
func (p *Policy) Evaluate(in GateInput) Gates {
	var g Gates
	g.PathRights.Writers = []string{}
	g.PathRights.Details = []PathRight{}
	g.Approvals.By = []string{}
	g.Approvals.Eligible = []string{}
	g.Approvals.Ignored = []Approval{}
	g.MatchedRules = []MatchedRule{}
	g.Blockers = []string{}
	g.Risk.Reasons = []RiskReason{}

	// Matched rules, in policy order.
	paths := []string{}
	for _, path := range in.Paths {
		if !IsMeta(path) {
			paths = append(paths, path)
		}
	}
	byRule := map[string][]string{}
	var rules []Rule
	for _, path := range paths {
		r := p.MatchRule(path)
		if r == nil {
			continue
		}
		if _, ok := byRule[r.Name]; !ok {
			rules = append(rules, *r)
		}
		byRule[r.Name] = append(byRule[r.Name], path)
	}
	sort.SliceStable(rules, func(i, j int) bool { return p.ruleIndex(rules[i].Name) < p.ruleIndex(rules[j].Name) })

	need, labGate := 0, false
	for _, r := range rules {
		g.MatchedRules = append(g.MatchedRules, MatchedRule{Name: r.Name, Paths: byRule[r.Name], Approvals: r.Approvals, LabGate: r.LabGate,
			Risk: r.Risk, Approvers: append([]string{}, r.Approvers...)})
		need = max(need, r.Approvals)
		labGate = labGate || r.LabGate
	}

	// Risk: rule risks of changed paths and risk rules over the impact.
	g.Risk = p.Rate(rules, in.Objects)
	if g.Risk.Level == RiskHigh {
		if need < 2 {
			need = 2
			g.Risk.Reasons = append(g.Risk.Reasons, RiskReason{Reason: "risk high: at least 2 approvals and a lab run", Source: "minimum"})
		}
		labGate = true
	}
	if in.Revert && p.FastPath != nil {
		g.FastPath = true
		need = p.FastPath.Approvals
		labGate = p.FastPath.LabGate
	}

	// Path rights for every writer.
	writers := in.Writers
	if len(writers) == 0 && !in.Author.IsZero() {
		writers = []identity.Identity{in.Author}
	}
	g.PathRights.OK = true
	seenW := map[string]bool{}
	for _, w := range writers {
		if seenW[w.User] {
			continue
		}
		seenW[w.User] = true
		g.PathRights.Writers = append(g.PathRights.Writers, w.User)
		for _, pr := range (&DefaultEngine{}).CanWrite(p, w, paths) {
			g.PathRights.Details = append(g.PathRights.Details, pr)
			if !pr.OK {
				g.PathRights.OK = false
			}
		}
	}
	if !g.PathRights.OK {
		for _, d := range g.PathRights.Details {
			if !d.OK {
				g.Blockers = append(g.Blockers, "path rights: "+d.Message)
				break
			}
		}
	}

	// Approvals: people who are approvers of all matched rules; not the
	// author, not a person who wrote commits, never agents or bots.
	exclude := []string{}
	if !in.Author.IsZero() {
		exclude = append(exclude, in.Author.Principal())
	}
	for _, w := range writers {
		if w.Kind != identity.Bot {
			exclude = append(exclude, w.Principal())
		}
	}
	g.Approvals.Eligible = p.eligible(rules, exclude...)
	g.Approvals.Need = need
	counted := map[string]bool{}
	fresh := map[string]bool{}
	for _, rv := range in.Reviews {
		if !rv.Stale {
			fresh[rv.User] = true
		}
	}
	for _, rv := range in.Reviews {
		a := Approval{User: rv.User, SHA: rv.SHA}
		id, known := p.Identify(rv.User)
		switch {
		case rv.Stale && fresh[rv.User]:
			continue // an approval of the head by the same person follows
		case rv.Stale:
			a.Reason = "approved an older head"
		case !known:
			a.Reason = rv.User + " is not in the policy"
		case id.Kind == identity.Agent:
			a.Reason = "agents never approve"
		case id.Kind == identity.Bot:
			a.Reason = "bots never approve"
		case slices.Contains(exclude, rv.User):
			a.Reason = "the author or a writer of the change"
		case !slices.Contains(g.Approvals.Eligible, rv.User):
			a.Reason = "not an approver of all matched rules"
		case counted[rv.User]:
			continue
		default:
			a.Counts = true
		}
		if a.Counts {
			counted[rv.User] = true
			g.Approvals.By = append(g.Approvals.By, rv.User)
		} else {
			g.Approvals.Ignored = append(g.Approvals.Ignored, a)
		}
	}
	g.Approvals.Have = len(g.Approvals.By)
	if g.Approvals.Have < need {
		g.Blockers = append(g.Blockers, fmt.Sprintf("approvals %d of %d", g.Approvals.Have, need))
	}

	// Lab gate.
	g.LabGate.Required = labGate
	g.LabGate.Run, g.LabGate.SHA = in.Lab.Run, in.Lab.SHA
	switch {
	case in.Lab.Status == "running":
		g.LabGate.Status = "running"
		g.LabGate.Detail = in.Lab.Run + " is running"
	case in.Lab.Status == "pass" && in.Lab.CoversHead:
		g.LabGate.Status = "met"
		g.LabGate.Detail = in.Lab.Run + " pass on the head"
	case in.Lab.Status == "pass":
		g.LabGate.Status = "needed"
		g.LabGate.Detail = in.Lab.Run + " tested an older head; run the lab again"
	case in.Lab.Status == "fail" && in.Lab.CoversHead:
		g.LabGate.Status = "failed"
		g.LabGate.Detail = in.Lab.Run + " failed"
	case in.Lab.Run != "" && !in.Lab.CoversHead:
		g.LabGate.Status = "needed"
		g.LabGate.Detail = in.Lab.Run + " tested an older head; run the lab again"
	default:
		g.LabGate.Status = "needed"
		g.LabGate.Detail = "no lab run on the head"
	}
	if !labGate && g.LabGate.Status != "met" && g.LabGate.Status != "running" {
		g.LabGate.Status = "na"
		g.LabGate.Detail = "the policy needs no lab run for these paths"
	}
	if labGate && g.LabGate.Status != "met" {
		g.Blockers = append(g.Blockers, "lab gate: "+g.LabGate.Detail)
	}

	// Rebased.
	g.Rebased.Behind = in.Behind
	g.Rebased.Target = in.Target
	if g.Rebased.Target == "" {
		g.Rebased.Target = "main"
	}
	g.Rebased.OK = in.Behind == 0
	if !g.Rebased.OK {
		g.Blockers = append(g.Blockers, fmt.Sprintf("behind %s by %d, rebase first", g.Rebased.Target, in.Behind))
	}
	if len(paths) == 0 {
		g.Blockers = append(g.Blockers, "no changed files")
	}
	g.Mergeable = len(g.Blockers) == 0
	return g
}

func (p *Policy) ruleIndex(name string) int {
	for i, r := range p.Rules {
		if r.Name == name {
			return i
		}
	}
	return len(p.Rules)
}

// Rate rates a change: the rule risks of the matched rules and the risk
// rules over the changed objects. The highest level wins.
func (p *Policy) Rate(rules []Rule, objects []kinds.ObjectRef) Risk {
	risk := Risk{Level: RiskLow, Reasons: []RiskReason{}}
	raise := func(l RiskLevel) {
		if l.Rank() > risk.Level.Rank() {
			risk.Level = l
		}
	}
	seen := map[string]bool{}
	for _, rr := range p.RiskRules {
		for _, o := range objects {
			if !rr.Match.Matches(o) {
				continue
			}
			raise(rr.Risk)
			if !seen[rr.Reason] {
				seen[rr.Reason] = true
				risk.Reasons = append(risk.Reasons, RiskReason{Reason: rr.Reason, Source: "riskRule", Object: o.Key()})
			}
			break
		}
	}
	for _, r := range rules {
		raise(r.Risk)
		if r.Risk.Rank() >= RiskMedium.Rank() {
			risk.Reasons = append(risk.Reasons, RiskReason{Reason: fmt.Sprintf("path rule %s: %s", r.Name, r.Risk), Source: "rule", Rule: r.Name})
		}
	}
	// Highest first.
	sort.SliceStable(risk.Reasons, func(i, j int) bool { return reasonRank(p, risk.Reasons[i]) > reasonRank(p, risk.Reasons[j]) })
	return risk
}

func reasonRank(p *Policy, r RiskReason) int {
	switch r.Source {
	case "riskRule":
		for _, rr := range p.RiskRules {
			if rr.Reason == r.Reason {
				return rr.Risk.Rank()*10 + 1
			}
		}
	case "rule":
		for _, ru := range p.Rules {
			if ru.Name == r.Rule {
				return ru.Risk.Rank() * 10
			}
		}
	}
	return 0
}

// Matches reports whether an object matches (empty fields match all; values
// are globs).
func (m ObjectMatch) Matches(o kinds.ObjectRef) bool {
	pairs := [][2]string{{m.Cluster, o.Cluster}, {m.APIVersion, o.APIVersion}, {m.Kind, o.Kind}, {m.Namespace, o.Namespace}, {m.Name, o.Name}}
	for _, p := range pairs {
		if p[0] == "" {
			continue
		}
		if ok, _ := doublestar.Match(p[0], p[1]); !ok {
			return false
		}
	}
	return true
}

// ForgeRules generates the branch protection and CODEOWNERS.
func (e *DefaultEngine) ForgeRules(p *Policy) ForgeRules {
	return p.ForgeRules()
}

// ForgeRules generates the forge settings: main takes no direct pushes,
// needs the pk/policy check (which enforces the per-path approvals), blocks
// outdated branches, and only platform admins merge.
func (p *Policy) ForgeRules() ForgeRules {
	bp := BranchProtection{
		Branch:             "main",
		RequiredChecks:     []string{"pk/policy"},
		MergeWhitelist:     []string{},
		ApprovalsWhitelist: []string{},
		RequiredApprovals:  1,
		BlockOnOutdated:    true,
		EnablePush:         false,
	}
	approvers := map[string]bool{}
	for _, pe := range p.People {
		if slices.Contains(pe.Roles, "platform-admin") {
			bp.MergeWhitelist = append(bp.MergeWhitelist, pe.ID)
		}
		for _, r := range p.Rules {
			if p.holds(&pe, r.Approvers) {
				approvers[pe.ID] = true
			}
		}
	}
	for _, pe := range p.People {
		if approvers[pe.ID] {
			bp.ApprovalsWhitelist = append(bp.ApprovalsWhitelist, pe.ID)
		}
	}
	return ForgeRules{BranchProtection: bp, CodeOwners: p.CodeOwners()}
}

// CodeOwners renders .gitea/CODEOWNERS. Gitea reads Go regular expressions
// and the last matching line wins, so the rules are written in reverse
// order (the policy's first match wins).
func (p *Policy) CodeOwners() string {
	var b strings.Builder
	b.WriteString("# Generated by platform-kit from .pk/policy.yaml. Do not edit.\n")
	b.WriteString("# The pk/policy check enforces the approval count per rule.\n")
	for i := len(p.Rules) - 1; i >= 0; i-- {
		r := p.Rules[i]
		owners := p.eligible([]Rule{r})
		if len(owners) == 0 {
			continue
		}
		at := make([]string, len(owners))
		for j, o := range owners {
			at[j] = "@" + o
		}
		fmt.Fprintf(&b, "\n# rule %s: %d approval(s)%s, risk %s\n", r.Name, r.Approvals, labText(r.LabGate), r.Risk)
		for _, g := range r.Paths {
			fmt.Fprintf(&b, "%s %s\n", GlobRegexp(g), strings.Join(at, " "))
		}
	}
	return b.String()
}

func labText(on bool) string {
	if on {
		return ", lab run"
	}
	return ""
}

// GlobRegexp converts a doublestar glob into an anchored Go regexp.
func GlobRegexp(g string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			if i+2 < len(g) && g[i+2] == '/' {
				b.WriteString("(.*/)?")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case strings.ContainsRune(`.+()|[]{}^$\`, rune(c)):
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return b.String()
}

// CheckDoc returns the consistency problems of a parsed policy document
// (without schema problems; the repo validation adds those).
func CheckDoc(n *yaml.Node) []kinds.Problem {
	var p Policy
	tmp := kinds.Doc{Data: asMap(schema.ToValue(n))}
	if err := tmp.Decode(&p); err != nil {
		return nil
	}
	return p.check(n)
}
