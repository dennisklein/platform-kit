package chat

// Platform versions: the Talos and Kubernetes version of a cluster come from
// its talhelper config (control/talos/<cluster>/talconfig.yaml); Flux is not
// pinned in the state repo, and the answer says so instead of guessing.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/kinds/repo"
	"github.com/dennisklein/platform-kit/internal/llm"
)

var (
	reTalosWord   = regexp.MustCompile(`(?i)\btalos\b`)
	reK8sWord     = regexp.MustCompile(`(?i)\b(kubernetes|k8s|kube|kubelet)\b`)
	reLatestQ     = regexp.MustCompile(`(?i)\b(latest|newest|up[- ]to[- ]date|outdated|old)\b`)
	reTalconfig   = regexp.MustCompile(`^control/talos/([^/]+)/talconfig\.ya?ml$`)
	rePlatformAsk = regexp.MustCompile(`(?i)\b(which|what)\s+(talos|kubernetes|k8s)\b|\bversions?\b|\breleases?\b`)
)

// platformWords are the words of a question that name Talos or Kubernetes,
// not an app.
var platformWords = set(`talos kubernetes k8s kube kubelet`)

// isPlatformVersionQ reports a question about the Talos or Kubernetes
// version of the clusters ("Which Kubernetes version runs on defiant?",
// "Is defiant on the latest Talos?").
func isPlatformVersionQ(q string) bool {
	if !reTalosWord.MatchString(q) && !reK8sWord.MatchString(q) {
		return false
	}
	return rePlatformAsk.MatchString(q) || reLatestQ.MatchString(q)
}

// talconf is what a talhelper config pins for one cluster.
type talconf struct {
	path, cluster        string
	talos, kubernetes    string
	controlPlane, worker []string
}

// parseTalconfig reads the versions and the node list of a talconfig.yaml.
func parseTalconfig(path, content string) (talconf, bool) {
	var doc struct {
		ClusterName       string `yaml:"clusterName"`
		TalosVersion      string `yaml:"talosVersion"`
		KubernetesVersion string `yaml:"kubernetesVersion"`
		Nodes             []struct {
			Hostname     string `yaml:"hostname"`
			ControlPlane bool   `yaml:"controlPlane"`
		} `yaml:"nodes"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil || (doc.TalosVersion == "" && doc.KubernetesVersion == "") {
		return talconf{}, false
	}
	tc := talconf{path: path, cluster: doc.ClusterName, talos: doc.TalosVersion, kubernetes: doc.KubernetesVersion}
	if tc.cluster == "" {
		if m := reTalconfig.FindStringSubmatch(path); m != nil {
			tc.cluster = m[1]
		}
	}
	for _, n := range doc.Nodes {
		if n.ControlPlane {
			tc.controlPlane = append(tc.controlPlane, n.Hostname)
		} else {
			tc.worker = append(tc.worker, n.Hostname)
		}
	}
	return tc, true
}

// talconfigSummary is the fileSummary of a talconfig.yaml.
func talconfigSummary(path, content string) (string, bool) {
	tc, ok := parseTalconfig(path, content)
	if !ok {
		return "", false
	}
	var parts []string
	if tc.talos != "" {
		parts = append(parts, "Talos **"+tc.talos+"**")
	}
	if tc.kubernetes != "" {
		parts = append(parts, "Kubernetes **"+tc.kubernetes+"**")
	}
	s := fmt.Sprintf("`%s` pins %s", shortPath(path), joinAnd(parts))
	if tc.cluster != "" {
		s += " for the cluster **" + tc.cluster + "**"
	}
	if n := len(tc.controlPlane); n > 0 {
		s += fmt.Sprintf(", with %s (%s)", plural(n, "control plane node", "control plane nodes"), joinAnd(tc.controlPlane))
	}
	return s + " (`talosVersion`, `kubernetesVersion`).", true
}

// talconfigs reads the talhelper configs of a ref.
func (s *Svc) talconfigs(ctx context.Context, p *progress, ref string, tree []api.FileNode) []talconf {
	var paths []string
	var walk func([]api.FileNode)
	walk = func(ns []api.FileNode) {
		for _, n := range ns {
			walk(n.Children)
			if n.Type == api.FileNodeTypeFile && reTalconfig.MatchString(n.Path) {
				paths = append(paths, n.Path)
			}
		}
	}
	walk(tree)
	sort.Strings(paths)
	label := "read talconfig.yaml"
	if len(paths) != 1 {
		label = fmt.Sprintf("read %s", plural(len(paths), "talconfig file", "talconfig files"))
	}
	var out []talconf
	_ = p.do(ctx, "read_file", label, func() (string, error) {
		for _, path := range paths {
			fc, err := s.o.Tools.ReadFile(ctx, ref, path)
			if err != nil || !fc.Exists {
				continue
			}
			if tc, ok := parseTalconfig(path, fc.Content); ok {
				out = append(out, tc)
			}
		}
		if len(out) == 0 {
			return "none", nil
		}
		var vs []string
		for _, tc := range out {
			vs = append(vs, versionPair(tc))
		}
		return joinAnd(uniq(vs)), nil
	})
	return out
}

func versionPair(tc talconf) string {
	return "Talos " + tc.talos + " · Kubernetes " + tc.kubernetes
}

// platformVersion answers a question about the Talos or Kubernetes version
// of one cluster or all clusters, from their talconfig.yaml on main.
func (s *Svc) platformVersion(ctx context.Context, p *progress, req Request, q string, tree []api.FileNode, a *Answer) bool {
	clusters := clusterNames(tree)
	confs := s.talconfigs(ctx, p, "main", tree)
	for _, tc := range confs {
		clusters[tc.cluster] = true
	}
	want := ""
	for _, t := range llm.Tokenize(q) {
		if clusters[t] {
			want = t
		}
	}
	k8s, talos := reK8sWord.MatchString(q), reTalosWord.MatchString(q)
	// "runs Kubernetes v1.33.4 on Talos v1.10.6" or "runs Talos v1.10.6,
	// with Kubernetes v1.33.4": the asked version first.
	runs := func(tc talconf) string {
		if talos && !k8s {
			return fmt.Sprintf("Talos **%s**, with Kubernetes **%s**", tc.talos, tc.kubernetes)
		}
		return fmt.Sprintf("Kubernetes **%s** on Talos **%s**", tc.kubernetes, tc.talos)
	}
	fields := "`kubernetesVersion`, `talosVersion`"
	if talos && !k8s {
		fields = "`talosVersion`, `kubernetesVersion`"
	}
	what := "Kubernetes"
	if talos && !k8s {
		what = "Talos"
	}
	var have []string
	for _, tc := range confs {
		have = append(have, tc.cluster)
	}
	var without []string
	for c := range clusters {
		found := false
		for _, h := range have {
			found = found || h == c
		}
		if !found {
			without = append(without, c)
		}
	}
	sort.Strings(without)
	var b strings.Builder
	var paths []string
	switch {
	case want != "":
		var tc *talconf
		for i := range confs {
			if confs[i].cluster == want {
				tc = &confs[i]
			}
		}
		if tc == nil {
			fmt.Fprintf(&b, "**%s** has no talconfig in the state repo (no `control/talos/%s/talconfig.yaml`), so the repo does not say which %s version it runs.", want, want, what)
			if len(confs) > 0 {
				if len(uniqPairs(confs)) == 1 {
					word, verb := "talconfig", "pins"
					if len(have) > 1 {
						word, verb = "talconfigs", "pin"
					}
					fmt.Fprintf(&b, " The %s of %s %s %s.", word, joinAnd(have), verb, runs(confs[0]))
				}
				for _, c := range confs {
					paths = append(paths, c.path)
				}
			}
			break
		}
		fmt.Fprintf(&b, "**%s** runs %s. That is the desired state on main, in `%s` (%s).", want, runs(*tc), tc.path, fields)
		paths = append(paths, tc.path)
	case len(confs) == 0:
		return false
	default:
		for _, c := range confs {
			paths = append(paths, c.path)
		}
		if len(uniqPairs(confs)) == 1 {
			switch len(confs) {
			case 1:
				fmt.Fprintf(&b, "**%s** runs %s, from `%s` on main.", confs[0].cluster, runs(confs[0]), confs[0].path)
			case 2:
				fmt.Fprintf(&b, "Both clusters with a talconfig, %s, run %s. That is the desired state on main, in `control/talos/<cluster>/talconfig.yaml` (%s).", joinAnd(have), runs(confs[0]), fields)
			default:
				fmt.Fprintf(&b, "All %d clusters with a talconfig (%s) run %s. That is the desired state on main, in `control/talos/<cluster>/talconfig.yaml` (%s).", len(confs), joinAnd(have), runs(confs[0]), fields)
			}
		} else {
			b.WriteString("The clusters run different versions, from their talconfig.yaml on main:")
			for _, c := range confs {
				fmt.Fprintf(&b, "\n- **%s:** %s", c.cluster, runs(c))
			}
		}
		switch len(without) {
		case 0:
		case 1:
			fmt.Fprintf(&b, " **%s** has no talconfig in the state repo, so its versions are not set here.", without[0])
		default:
			fmt.Fprintf(&b, " %s have no talconfig in the state repo, so their versions are not set here.", joinAnd(without))
		}
	}
	if reLatestQ.MatchString(q) {
		b.WriteString(" I can't check upstream releases: I only read this project.")
	}
	for i, path := range paths {
		if i < 3 {
			addSource(a, llm.Source{Label: shortPath(path), Ref: path, Kind: "file"})
		}
	}
	if want != "" && len(paths) == 1 {
		if pend := s.pendingChanges(ctx, p, req, paths[0], a); pend != "" {
			b.WriteString("\n\n" + pend)
		}
	}
	a.Text = b.String()
	return true
}

func uniqPairs(cs []talconf) []string {
	var vs []string
	for _, c := range cs {
		vs = append(vs, versionPair(c))
	}
	return uniq(vs)
}

// fluxVersion answers "which Flux version runs?": the state repo pins none,
// so the answer says what the repo has instead.
func (s *Svc) fluxVersion(ctx context.Context, p *progress, tree []api.FileNode, a *Answer) bool {
	var paths []string
	var walk func([]api.FileNode)
	walk = func(ns []api.FileNode) {
		for _, n := range ns {
			walk(n.Children)
			if n.Type == api.FileNodeTypeFile && strings.HasPrefix(n.Path, "clusters/") && shortPath(n.Path) == "flux.yaml" {
				paths = append(paths, n.Path)
			}
		}
	}
	walk(tree)
	sort.Strings(paths)
	if len(paths) == 0 {
		return false
	}
	apis := map[string]bool{}
	var clusters []string
	_ = p.do(ctx, "read_file", "read "+plural(len(paths), "flux.yaml", "flux.yaml files"), func() (string, error) {
		for _, path := range paths {
			fc, err := s.o.Tools.ReadFile(ctx, "main", path)
			if err != nil || !fc.Exists {
				continue
			}
			clusters = append(clusters, clusterOfPath(path))
			for _, d := range repo.ParseFile(path, []byte(fc.Content)).Docs {
				if strings.Contains(d.APIVersion, "fluxcd.io") {
					apis[d.APIVersion] = true
				}
			}
		}
		return "no Flux version pinned", nil
	})
	var list []string
	for v := range apis {
		list = append(list, "`"+v+"`")
	}
	sort.Strings(list)
	text := "The state repo does not pin a Flux version, so I can't say which Flux release runs."
	if len(clusters) > 0 {
		text += fmt.Sprintf(" What it has: the Flux objects of %s in `clusters/<cluster>/flux.yaml`", joinAnd(clusters))
		if len(list) > 0 {
			text += ", on the APIs " + joinAnd(list)
		}
		text += "."
	}
	a.Text = text
	for i, path := range paths {
		if i < 3 {
			addSource(a, llm.Source{Label: shortPath(path), Ref: path, Kind: "file"})
		}
	}
	return true
}

// talconfigEdit answers "upgrade Kubernetes on defiant to v1.34": the field
// to change in the cluster's talconfig.yaml.
func (s *Svc) talconfigEdit(ctx context.Context, p *progress, q, inChange string, a *Answer) bool {
	tree, err := s.o.Tools.Tree(ctx, "main")
	if err != nil {
		return false
	}
	confs := s.talconfigs(ctx, p, "main", tree)
	if len(confs) == 0 {
		return false
	}
	tc := confs[0]
	for _, t := range llm.Tokenize(q) {
		for _, c := range confs {
			if c.cluster == t {
				tc = c
			}
		}
	}
	field, what, cur := "kubernetesVersion", "Kubernetes", tc.kubernetes
	if reTalosWord.MatchString(q) && !reK8sWord.MatchString(q) {
		field, what, cur = "talosVersion", "Talos", tc.talos
	}
	a.Text = fmt.Sprintf("I can't make that edit myself: I suggest edits as diffs for BGP policies only. %s on **%s** is set in `%s` (`%s`, now %s). Change it there %s, then commit and run the lab.",
		what, tc.cluster, tc.path, field, cur, inChange)
	addSource(a, llm.Source{Label: shortPath(tc.path), Ref: tc.path, Kind: "file"})
	return true
}
