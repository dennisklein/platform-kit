package chat

// Facts from the repo files: the lead sentence about an entity, why a new
// node exists, counts of the inventory, and one or two sentences about what
// a YAML file defines (never the whole file).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/advisor"
	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// ---------------------------------------------------------------- entities

type lead struct {
	text string
	used map[string]bool
}

// entityLead is the first sentence about an entity: for a node its role,
// cluster and place ("cn-036 is a worker Node of cluster defiant, in rack
// GC5A-6Z at U8.").
func entityLead(e *api.Entity) lead {
	l := lead{used: map[string]bool{}}
	if e.Kind == "Node" {
		role, _ := kindutil.Get(e.Data, "spec", "role").(string)
		rack, _ := kindutil.Get(e.Data, "spec", "location", "rack").(string)
		unit := kindutil.Get(e.Data, "spec", "location", "unit")
		var b strings.Builder
		fmt.Fprintf(&b, "**%s** is a", e.Name)
		if role != "" {
			if strings.ContainsAny(role[:1], "aeiou") {
				b.WriteString("n")
			}
			b.WriteString(" " + strings.ReplaceAll(role, "-", " "))
			l.used["spec.role"] = true
		}
		b.WriteString(" Node")
		if e.Cluster != "" {
			fmt.Fprintf(&b, " of cluster %s", e.Cluster)
		}
		if rack != "" {
			fmt.Fprintf(&b, ", in rack %s", rack)
			if unit != nil {
				fmt.Fprintf(&b, " at U%v", unit)
			}
			l.used["spec.location"] = true
		}
		b.WriteString(".")
		l.text = b.String()
		return l
	}
	l.text = fmt.Sprintf("**%s** is a %s", e.Name, kindWords(e.Kind))
	if e.Cluster != "" && e.Kind != "Cluster" {
		l.text += " in cluster " + e.Cluster
	}
	l.text += "."
	return l
}

// whyNew answers "why cn-037?" for a node without history that is new in
// the selected change: the intent of the change says why.
func (s *Svc) whyNew(ctx context.Context, p *progress, req Request, names []string, a *Answer) bool {
	if req.ChangeID == "" {
		return false
	}
	for _, name := range names {
		var e *api.Entity
		for _, k := range []string{"Node", "Jbod", "Switch"} {
			if got, err := s.o.Tools.Entity(ctx, k, name, req.ChangeID); err == nil && got != nil && got.Name != "" {
				e = got
				break
			}
		}
		if e == nil || e.Change.Id != req.ChangeID || e.Change.Status != api.FileChangeAdded {
			continue
		}
		p.quick(ctx, "entity", "entity "+e.Kind+"/"+e.Name, "new in "+req.ChangeID)
		ch, err := s.o.Tools.Change(ctx, req.ChangeID)
		if err != nil {
			return false
		}
		p.quick(ctx, "change", "change "+req.ChangeID, stateWords(ch.State))
		text := fmt.Sprintf("**%s** has no history on main yet: your change **%s** (*%s*) adds it", name, ch.Id, ch.Title)
		if ch.Intent != "" {
			text += ", for this intent: " + endSentence(lowerFirst(ch.Intent))
		} else {
			text += "."
		}
		a.Text = text
		addSource(a, llm.Source{Label: ch.Id, Ref: ch.Id, Kind: "change"})
		addSource(a, llm.Source{Label: e.Name, Ref: e.Kind + "/" + e.Name, Kind: "entity"})
		return true
	}
	return false
}

// ---------------------------------------------------------------- counts

var (
	reCountQ = regexp.MustCompile(`(?i)\bhow many\s+(?:[\w-]+\s+){0,2}?(nodes?|servers?|machines?|hosts?|workers?|switch(?:es)?|racks?|clusters?|jbods?)\b|\bnumber of (?:[\w-]+\s+){0,2}?(nodes|switches|racks|clusters|servers|workers)\b`)
	// roleWords name a node role in a count question.
	roleWords = []struct {
		re         *regexp.Regexp
		role, word string
	}{
		{regexp.MustCompile(`(?i)\bworkers?\b`), "worker", "worker"},
		{regexp.MustCompile(`(?i)\bcontrol[- ]?planes?\b|\bcontrol\b|\bmasters?\b`), "control", "control plane"},
		{regexp.MustCompile(`(?i)\bstorage\b`), "storage", "storage"},
	}
)

// countQuestion answers "how many nodes does defiant have?" from the
// inventory of the selected change (or main).
func (s *Svc) countQuestion(ctx context.Context, p *progress, req Request, q string, a *Answer) bool {
	m := reCountQ.FindStringSubmatch(q)
	if m == nil {
		return false
	}
	what := strings.ToLower(m[1] + m[2])
	ref := req.ChangeID
	if ref == "" {
		ref = "main"
	}
	var tree []api.FileNode
	if err := p.do(ctx, "search", "list the inventory of "+ref, func() (string, error) {
		var err error
		tree, err = s.o.Tools.Tree(ctx, ref)
		if err != nil && ref != "main" {
			// A change that is gone: count main.
			ref = "main"
			tree, err = s.o.Tools.Tree(ctx, ref)
		}
		return plural(countFiles(tree), "file", "files"), err
	}); err != nil {
		return false
	}
	clusters := clusterNames(tree)
	if strings.HasPrefix(what, "cluster") {
		names := make([]string, 0, len(clusters))
		for c := range clusters {
			names = append(names, c)
		}
		sort.Strings(names)
		a.Text = fmt.Sprintf("The state repo has %s: %s.", plural(len(names), "cluster", "clusters"), joinAnd(names))
		for _, c := range names {
			addSource(a, llm.Source{Label: c, Ref: "Cluster/" + c, Kind: "entity"})
		}
		return true
	}
	var inv []string
	var walk func([]api.FileNode)
	walk = func(ns []api.FileNode) {
		for _, n := range ns {
			walk(n.Children)
			if n.Type == api.FileNodeTypeFile && strings.HasPrefix(n.Path, "control/inventory/") && strings.HasSuffix(n.Path, ".yaml") {
				inv = append(inv, n.Path)
			}
		}
	}
	walk(tree)
	if !strings.HasPrefix(what, "node") && !strings.HasPrefix(what, "server") && !strings.HasPrefix(what, "machine") &&
		!strings.HasPrefix(what, "host") && !strings.HasPrefix(what, "worker") {
		kind, dir := "Switch", "control/inventory/switches/"
		switch {
		case strings.HasPrefix(what, "rack"):
			kind, dir = "Rack", "control/inventory/racks/"
		case strings.HasPrefix(what, "jbod"):
			kind, dir = "Jbod", "control/inventory/jb-"
		}
		var names []string
		for _, path := range inv {
			if strings.HasPrefix(path, dir) {
				names = append(names, strings.TrimSuffix(shortPath(path), ".yaml"))
			}
		}
		sort.Strings(names)
		p.quick(ctx, "read_file", "read the inventory", pluralKind(len(names), kind))
		a.Text = fmt.Sprintf("The inventory has %s", pluralKind(len(names), kind))
		if len(names) > 0 {
			a.Text += ": " + strings.Join(names, ", ")
		}
		a.Text += "."
		return true
	}
	// Nodes, per cluster and role.
	var nodes []advisor.NodeInfo
	n := p.begin("read_file", "read the Node files")
	for _, path := range inv {
		if strings.Contains(strings.TrimPrefix(path, "control/inventory/"), "/") {
			continue
		}
		fc, err := s.o.Tools.ReadFile(ctx, ref, path)
		if err != nil || !fc.Exists {
			continue
		}
		nodes = append(nodes, advisor.ParseNodes(path, fc.Content)...)
	}
	p.end(ctx, n, plural(len(nodes), "node", "nodes"), false)
	if len(nodes) == 0 {
		return false
	}
	want := ""
	for _, t := range llm.Tokenize(q) {
		if clusters[t] {
			want = t
		}
	}
	rack := ""
	if r := reRackName.FindString(q); r != "" && !rePolicyRef.MatchString(r) {
		rack = strings.ToUpper(r)
	}
	role, roleWord := "", ""
	for _, rw := range roleWords {
		if rw.re.MatchString(q) {
			role, roleWord = rw.role, rw.word
			break
		}
	}
	var sel []advisor.NodeInfo
	for _, nd := range nodes {
		if (want == "" || nd.Cluster == want) && (rack == "" || strings.EqualFold(nd.Rack, rack)) && (role == "" || nd.Role == role) {
			sel = append(sel, nd)
		}
	}
	// New in the change: not on main.
	newIn := map[string]bool{}
	if req.ChangeID != "" {
		if ch, err := s.o.Tools.Change(ctx, req.ChangeID); err == nil {
			for _, f := range ch.ChangedFiles {
				if f.Status == api.FileChangeAdded {
					newIn[strings.TrimSuffix(shortPath(f.Path), ".yaml")] = true
				}
			}
		}
	}
	byRole := map[string][]string{}
	var added []string
	for _, nd := range sel {
		role := nd.Role
		if role == "" {
			role = "other"
		}
		byRole[role] = append(byRole[role], nd.Name)
		if newIn[nd.Name] {
			added = append(added, nd.Name)
		}
	}
	roles := make([]string, 0, len(byRole))
	for r := range byRole {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	var parts []string
	for _, r := range roles {
		names := byRole[r]
		sort.Strings(names)
		word := strings.ReplaceAll(r, "-", " ")
		if len(names) != 1 {
			word += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", len(names), word, nameRange(names)))
	}
	subject := "The inventory"
	switch {
	case want != "" && rack != "":
		subject = fmt.Sprintf("**%s** in rack %s", want, rack)
	case want != "":
		subject = "**" + want + "**"
		addSource(a, llm.Source{Label: want, Ref: "Cluster/" + want, Kind: "entity"})
	case rack != "":
		subject = "Rack **" + rack + "**"
		addSource(a, llm.Source{Label: rack, Ref: "Rack/" + rack, Kind: "entity"})
	}
	text := fmt.Sprintf("%s has **%s**", subject, plural(len(sel), "node", "nodes"))
	switch {
	case role != "":
		var names []string
		for _, nd := range sel {
			names = append(names, nd.Name)
		}
		sort.Strings(names)
		text = fmt.Sprintf("%s has **%s**", subject, plural(len(sel), roleWord+" node", roleWord+" nodes"))
		if len(names) > 0 && len(names) <= 4 {
			text += " (" + joinAnd(names) + ")"
		} else if len(names) > 4 {
			text += " (" + nameRange(names) + ")"
		}
	case len(roles) == 1 && len(sel) > 1:
		names := byRole[roles[0]]
		text += fmt.Sprintf(", all %ss (%s)", strings.ReplaceAll(roles[0], "-", " "), nameRange(names))
	case len(parts) > 0:
		text += ": " + joinAnd(parts)
	}
	text += "."
	if len(added) > 0 {
		sort.Strings(added)
		text += fmt.Sprintf(" That counts your change **%s**, which adds %s; main has %d.", req.ChangeID, joinAnd(added), len(sel)-len(added))
		addSource(a, llm.Source{Label: req.ChangeID, Ref: req.ChangeID, Kind: "change"})
	}
	if want == "" && rack == "" {
		per := map[string]int{}
		for _, nd := range sel {
			per[nd.Cluster]++
		}
		var cs []string
		for c := range per {
			if c != "" {
				cs = append(cs, fmt.Sprintf("%s %d", c, per[c]))
			}
		}
		sort.Strings(cs)
		if len(cs) > 1 {
			text += " Per cluster: " + strings.Join(cs, ", ") + "."
		}
	}
	a.Text = text
	return true
}

// ---------------------------------------------------------------- files

// fileSummary says in one or two sentences what a YAML file defines, from
// its fields: an image or chart version, a node's place and ports, a Flux
// path and interval. ok is false when the file has no document it knows.
func fileSummary(path, content string, terms map[string]bool) (string, bool) {
	if reTalconfig.MatchString(path) {
		return talconfigSummary(path, content)
	}
	docs := repo.ParseFile(path, []byte(content)).Docs
	if len(docs) == 0 {
		return "", false
	}
	// The document that matches the question best.
	best, bestScore := docs[0], -1
	for _, d := range docs {
		score := 0
		for t := range terms {
			if strings.Contains(strings.ToLower(d.Name+" "+d.Kind), t) {
				score += 2
			}
		}
		if score > bestScore {
			best, bestScore = d, score
		}
	}
	d := best
	on := ""
	if c := clusterOfPath(path); c != "" {
		on = " on " + c
	}
	get := func(p ...string) string {
		v := kindutil.Get(d.Data, p...)
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	switch d.Kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		for _, x := range workloadsIn(path, content) {
			if x.name == d.Name {
				s := fmt.Sprintf("**%s** is a %s%s", d.Name, d.Kind, on)
				if x.image != "" {
					s += fmt.Sprintf(" that runs `%s`", x.image)
				}
				if x.replicas > 0 {
					s += " with " + plural(x.replicas, "replica", "replicas")
				}
				return s + ".", true
			}
		}
	case "HelmRelease":
		s := fmt.Sprintf("**%s** is a HelmRelease%s: chart %s **%s**", d.Name, on, get("spec", "chart", "spec", "chart"), get("spec", "chart", "spec", "version"))
		if src := get("spec", "chart", "spec", "sourceRef", "name"); src != "" {
			s += " from the repository " + src
		}
		if iv := get("spec", "interval"); iv != "" {
			s += ", checked every " + iv
		}
		return s + ".", true
	case "Kustomization":
		if p := get("spec", "path"); p != "" {
			s := fmt.Sprintf("The Flux Kustomization **%s**%s applies `%s`", d.Name, on, p)
			if iv := get("spec", "interval"); iv != "" {
				s += " every " + iv
			}
			if deps, ok := kindutil.Get(d.Data, "spec", "dependsOn").([]any); ok && len(deps) > 0 {
				var ns []string
				for _, dep := range deps {
					if m, ok := dep.(map[string]any); ok {
						ns = append(ns, fmt.Sprint(m["name"]))
					}
				}
				s += ", after " + joinAnd(ns)
			}
			return s + ".", true
		}
		if rs, ok := kindutil.Get(d.Data, "resources").([]any); ok {
			var ns []string
			for _, r := range rs {
				ns = append(ns, "`"+fmt.Sprint(r)+"`")
			}
			return fmt.Sprintf("`%s` lists the files Flux applies%s: %s.", shortPath(path), on, strings.Join(ns, ", ")), true
		}
	case "Node":
		for _, nd := range advisor.ParseNodes(path, content) {
			var ports []string
			for _, pt := range nd.Ports {
				ports = append(ports, fmt.Sprintf("%s → %s %d", pt.Name, pt.Peer.Switch, pt.Peer.Port))
			}
			s := fmt.Sprintf("**%s** is a %s Node of %s in rack %s at U%d", nd.Name, nd.Role, nd.Cluster, nd.Rack, nd.Unit)
			if len(ports) > 0 {
				s += "; ports " + strings.Join(ports, ", ")
			}
			return s + ".", true
		}
	case "CiliumBGPPeeringPolicy":
		for _, pol := range advisor.ParseBGP(path, content) {
			s := fmt.Sprintf("The BGP policy **%s**%s selects %s", pol.Name, on, plural(len(pol.Hostnames), "node", "nodes"))
			if len(pol.Hostnames) > 0 {
				s += " (" + nameRange(pol.Hostnames) + ")"
			}
			if len(pol.Peers) > 0 {
				s += " and peers with " + joinAnd(pol.Peers)
			}
			return s + ".", true
		}
	case "Namespace":
		s := fmt.Sprintf("**%s** is a Namespace%s", d.Name, on)
		if team := get("metadata", "labels", "pk.io/team"); team != "" {
			s += ", owned by the team " + team
		}
		return s + ".", true
	case "Service":
		s := fmt.Sprintf("**%s** is a Service%s", d.Name, on)
		if ps, ok := kindutil.Get(d.Data, "spec", "ports").([]any); ok && len(ps) > 0 {
			var out []string
			for _, pv := range ps {
				if m, ok := pv.(map[string]any); ok {
					out = append(out, fmt.Sprintf("%v → %v", m["port"], orEmpty(m["targetPort"])))
				}
			}
			s += " on port " + strings.Join(out, ", ")
		}
		return s + ".", true
	case "GitRepository":
		return fmt.Sprintf("The Flux source **%s**%s pulls branch %s of `%s` every %s.", d.Name, on, get("spec", "ref", "branch"), get("spec", "url"), get("spec", "interval")), true
	}
	if d.Kind == "" {
		return "", false
	}
	// Any other kind: its name and up to three plain spec fields.
	s := fmt.Sprintf("`%s` defines the %s **%s**%s", shortPath(path), kindWords(d.Kind), d.Name, on)
	if spec, ok := d.Data["spec"].(map[string]any); ok {
		keys := make([]string, 0, len(spec))
		for k, v := range spec {
			switch v.(type) {
			case string, int, float64, bool:
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var parts []string
		for i, k := range keys {
			if i >= 3 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %v", k, spec[k]))
		}
		if len(parts) > 0 {
			s += ": " + strings.Join(parts, ", ")
		}
	}
	return s + ".", true
}

// countFiles counts the files of a tree.
func countFiles(ns []api.FileNode) int {
	n := 0
	for _, f := range ns {
		if f.Type == api.FileNodeTypeFile {
			n++
		}
		n += countFiles(f.Children)
	}
	return n
}
