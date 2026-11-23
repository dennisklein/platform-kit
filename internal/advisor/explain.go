package advisor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/lab"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// ---------------------------------------------------------------- explain diff

// explainDiff says per cluster, in plain words, what Flux will do after the
// merge, and what to check in the Rendered tab.
func (e *Engine) explainDiff(w *workspace) ([]finding, error) {
	imp, err := w.impact()
	if err != nil {
		return nil, err
	}
	var fs []finding
	var quiet []string
	var changed []api.ObjectChange
	for _, c := range imp.Clusters {
		var objs []api.ObjectChange
		for _, k := range c.Kustomizations {
			objs = append(objs, k.Objects...)
		}
		if len(objs) == 0 {
			quiet = append(quiet, c.Cluster)
			continue
		}
		where := c.Cluster
		var tags []string
		if c.Role != "" {
			tags = append(tags, c.Role+" cluster")
		}
		if c.Ring != "" {
			tags = append(tags, c.Ring+" ring")
		}
		if len(tags) > 0 {
			where += " (" + strings.Join(tags, ", ") + ")"
		}
		count := fmt.Sprintf("It goes from %d to %d objects.", c.ObjectsBase, c.ObjectsHead)
		if c.ObjectsBase == c.ObjectsHead {
			count = fmt.Sprintf("The object count stays at %d.", c.ObjectsHead)
		}
		text := fmt.Sprintf("On %s: %s %s", where, strings.Join(explainObjects(objs), " "), count)
		fs = append(fs, finding{Point: Point{Title: c.Cluster, Text: text, Severity: SevInfo, Sources: objectSources(w.id, objs)}})
		for _, o := range objs {
			if o.Op == api.OpChange {
				changed = append(changed, o)
			}
		}
	}
	if chk := checkRendered(changed); chk != "" {
		fs = append(fs, finding{Point: Point{Title: "What to check in the Rendered tab", Text: chk, Severity: SevInfo,
			Sources: objectSources(w.id, changed)}})
	}
	if len(fs) > 0 && len(quiet) > 0 {
		verb := "does"
		if len(quiet) > 1 {
			verb = "do"
		}
		fs = append(fs, finding{Point: Point{Title: "Other clusters", Severity: SevOK,
			Text: fmt.Sprintf("%s %s not change.", joinAnd(quiet), verb), Sources: []llm.Source{srcImpact(w.id)}}})
	}
	for _, st := range imp.OpenSteps {
		fs = append(fs, finding{Point: Point{Title: "After merge", Text: st.Text, Severity: SevWarn, Sources: []llm.Source{srcFile(st.Path, 0)}}})
	}
	if len(fs) == 0 {
		fs = append(fs, finding{Point: Point{Title: "No rendered change", Severity: SevOK,
			Text: "The change does not change any rendered object (for example only docs or work items).", Sources: []llm.Source{srcImpact(w.id)}}})
	}
	return fs, nil
}

// objectSources: the impact and the source files of the objects (at most 4).
func objectSources(id string, objs []api.ObjectChange) []llm.Source {
	srcs := []llm.Source{srcImpact(id)}
	seen := map[string]bool{}
	for _, o := range objs {
		if o.Source.Path != "" && !seen[o.Source.Path] && len(srcs) < 5 {
			seen[o.Source.Path] = true
			srcs = append(srcs, srcFile(o.Source.Path, o.Source.Line))
		}
	}
	return srcs
}

// explainObjects returns one sentence per kind and operation, in the order
// the objects come: "Flux creates the Machines cn-035 and cn-036; the
// metal operator installs them with Talos."
func explainObjects(objs []api.ObjectChange) []string {
	type group struct {
		kind string
		op   api.Op
		objs []api.ObjectChange
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, o := range objs {
		k := o.Kind + "\x00" + string(o.Op)
		g, ok := byKey[k]
		if !ok {
			g = &group{kind: o.Kind, op: o.Op}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.objs = append(g.objs, o)
	}
	var out []string
	for _, g := range groups {
		var names []string
		for _, o := range g.objs {
			names = append(names, o.Name)
		}
		switch {
		case g.kind == "Machine" && g.op == api.OpAdd:
			it := "it"
			if len(names) > 1 {
				it = "them"
			}
			out = append(out, fmt.Sprintf("Flux creates the %s %s; the metal operator installs %s with Talos.", kindNoun(g.kind, len(names)), joinAnd(names), it))
		case g.kind == "Machine" && g.op == api.OpDelete:
			out = append(out, fmt.Sprintf("Flux deletes the %s %s; the metal operator releases the hardware.", kindNoun(g.kind, len(names)), joinAnd(names)))
		case g.kind == "CiliumBGPPeeringPolicy" && g.op == api.OpChange:
			for _, o := range g.objs {
				out = append(out, explainBGP(o))
			}
		case g.kind == "HelmRelease" && g.op == api.OpChange:
			for _, o := range g.objs {
				plus, _ := diffItems(o.Diff)
				if v := versionOf(plus); v != "" {
					out = append(out, fmt.Sprintf("Flux upgrades the HelmRelease %s to %s.", o.Name, v))
				} else {
					out = append(out, fmt.Sprintf("Flux changes the values of the HelmRelease %s.", o.Name))
				}
			}
		default:
			verb := map[api.Op]string{api.OpAdd: "creates", api.OpChange: "changes", api.OpDelete: "deletes"}[g.op]
			if verb == "" {
				verb = "changes"
			}
			out = append(out, fmt.Sprintf("Flux %s the %s %s.", verb, kindNoun(g.kind, len(names)), joinAnd(names)))
		}
	}
	return out
}

func kindNoun(kind string, n int) string {
	if n == 1 {
		return kind
	}
	switch {
	case strings.HasSuffix(kind, "Policy"):
		return strings.TrimSuffix(kind, "y") + "ies"
	case strings.HasSuffix(kind, "s"):
		return kind + "es"
	}
	return kind + "s"
}

var reHostname = regexp.MustCompile(`^[a-z][a-z0-9-]*\d$`)

// explainBGP: which nodes a BGP policy selects or drops, or which peers it adds.
func explainBGP(o api.ObjectChange) string {
	plus, minus := diffItems(o.Diff)
	var addNodes, dropNodes, peers []string
	for _, p := range plus {
		switch {
		case reHostname.MatchString(p):
			addNodes = append(addNodes, p)
		case strings.HasPrefix(p, "peerAddress:"):
			peers = append(peers, strings.TrimSpace(strings.TrimPrefix(p, "peerAddress:")))
		}
	}
	for _, m := range minus {
		if reHostname.MatchString(m) {
			dropNodes = append(dropNodes, m)
		}
	}
	var parts []string
	if len(addNodes) > 0 {
		gets := "it gets a BGP session"
		if len(addNodes) > 1 {
			gets = "they get BGP sessions"
		}
		parts = append(parts, fmt.Sprintf("The BGP policy %s also selects %s, so %s.", o.Name, joinAnd(addNodes), gets))
	}
	if len(dropNodes) > 0 {
		parts = append(parts, fmt.Sprintf("The BGP policy %s no longer selects %s.", o.Name, joinAnd(dropNodes)))
	}
	if len(peers) > 0 {
		parts = append(parts, fmt.Sprintf("The BGP policy %s also peers with %s.", o.Name, joinAnd(peers)))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Flux changes the BGP policy %s.", o.Name)
	}
	return strings.Join(parts, " ")
}

// diffItems returns the added and removed lines of a unified diff, trimmed
// and without a list dash: "+      - cn-035" → "cn-035".
func diffItems(u string) (plus, minus []string) {
	for _, l := range strings.Split(u, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			plus = append(plus, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l[1:]), "- ")))
		case strings.HasPrefix(l, "-"):
			minus = append(minus, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l[1:]), "- ")))
		}
	}
	return plus, minus
}

func versionOf(lines []string) string {
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "version:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// checkRendered says what to look at in the Rendered tab for changed objects.
func checkRendered(objs []api.ObjectChange) string {
	var parts []string
	for _, o := range objs {
		plus, minus := diffItems(o.Diff)
		if len(plus)+len(minus) == 0 {
			continue
		}
		var what []string
		if len(plus) > 0 && len(plus) <= 3 {
			what = append(what, "gains "+joinAnd(plus))
		} else if len(plus) > 3 {
			what = append(what, fmt.Sprintf("gains %d lines", len(plus)))
		}
		if len(minus) > 0 && len(minus) <= 3 {
			what = append(what, "loses "+joinAnd(minus))
		} else if len(minus) > 3 {
			what = append(what, fmt.Sprintf("loses %d lines", len(minus)))
		}
		parts = append(parts, fmt.Sprintf("%s/%s %s", o.Kind, o.Name, joinAnd(what)))
		if len(parts) == 3 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Check that " + strings.Join(parts, "; that ") + ", and that nothing else in it changes."
}

// ---------------------------------------------------------------- planned lab

// plannedLab says what the lab will test before the change has a run: the
// built-in scenario for its paths, and what that scenario leaves out.
func (e *Engine) plannedLab(w *workspace) []finding {
	var paths []string
	for _, f := range w.changedFiles() {
		paths = append(paths, f.Path)
	}
	if len(paths) == 0 {
		return nil
	}
	scenario := lab.DefaultScenario(paths)
	var names, newNodes []string
	nets := map[string]bool{}
	nodes := w.changeNodes()
	for _, n := range nodes {
		names = append(names, n.Name)
		if n.New {
			newNodes = append(newNodes, n.Name)
		}
		for _, p := range n.DataPorts() {
			if p.Peer.Switch != "" {
				nets[p.Peer.Switch] = true
			}
		}
	}
	var netList []string
	for n := range nets {
		netList = append(netList, n)
	}
	sort.Strings(netList)
	fault := ""
	if len(netList) > 0 {
		fault = netList[len(netList)-1]
	}
	steps, err := lab.ScenarioSteps(scenario, names, fault)
	if err != nil {
		return nil
	}
	var tested []string
	faulted := false
	for _, s := range steps {
		tested = append(tested, stepWords(s, faulted))
		if s.Fault != nil || (s.Action != nil && s.Action.BMC != nil) {
			faulted = true
		}
	}
	src := srcFile(".pk/emulation.yaml", 0)
	w.read(src)
	w.read(srcPolicy())
	text := fmt.Sprintf("The lab runs the %s scenario.", scenario)
	if scenario == lab.ScenarioMetal && len(newNodes) > 0 {
		text = fmt.Sprintf("The lab runs the metal scenario. It installs %s with Talos through the emulated BMC, joins %s, then tests: %s.",
			joinAnd(newNodes), map[bool]string{true: "them", false: "it"}[len(newNodes) > 1], strings.Join(tested, "; "))
	} else if len(tested) > 0 {
		text += " It tests: " + strings.Join(tested, "; ") + "."
	}
	out := []finding{{Point: Point{Title: "What the lab will test", Text: text, Severity: SevInfo, Sources: []llm.Source{src, srcPolicy()}}}}

	var left []string
	if scenario == lab.ScenarioMetal {
		if len(netList) >= 2 && fault != "" {
			left = append(left, fmt.Sprintf("both data links down (it takes only %s down)", fault))
		}
		for _, pol := range w.bgpPolicies() {
			if len(pol.Peers) == 1 && touchesRack(nodes, pol.Rack) {
				left = append(left, fmt.Sprintf("a BGP peer down (%s has one peer, %s: no failover)", pol.Name, pol.Peers[0]))
				break
			}
		}
		if len(newNodes) > 1 {
			left = append(left, fmt.Sprintf("a power cycle of %s (it power cycles %s only)", joinAnd(newNodes[:len(newNodes)-1]), newNodes[len(newNodes)-1]))
		}
		left = append(left, "real hardware (it emulates the machines, the BMCs and the switches)")
	}
	if len(left) > 0 {
		out = append(out, finding{Point: Point{Title: "What it leaves out", Severity: SevWarn,
			Text: capitalize(strings.Join(left, "; ")) + ".", Sources: []llm.Source{src}}})
	}
	return out
}

// stepWords says a scenario step in plain words: "nodes Ready and BGP up",
// and after a fault "still healthy (Flux, nodes Ready and BGP up)".
func stepWords(s lab.ScenarioStep, afterFault bool) string {
	if s.Check == nil {
		return lab.StepTitle(s)
	}
	var parts []string
	if s.Check.Flux != "" {
		parts = append(parts, "Flux ready")
	}
	if s.Check.Nodes != "" {
		parts = append(parts, "nodes Ready")
	}
	if s.Check.BGP != "" {
		parts = append(parts, "BGP up")
	}
	if len(parts) == 0 {
		return lab.StepTitle(s)
	}
	if afterFault {
		return "still healthy (" + joinAnd(parts) + ")"
	}
	return joinAnd(parts)
}

func touchesRack(nodes []NodeInfo, rack string) bool {
	for _, n := range nodes {
		if strings.EqualFold(n.Rack, rack) {
			return true
		}
	}
	return false
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
