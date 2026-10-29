package chat

// General questions about a technology of the platform ("Explain BGP in one
// sentence", "How does Flux work?"): one plain line in general, then where
// the project uses it, from the search index.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/llm"
)

// glossary holds one general line per technology, keyed by the word that
// names it.
var glossary = map[string]string{
	"bgp":        "**BGP** (Border Gateway Protocol) is how routers tell each other which networks they can reach, so traffic finds its way between them.",
	"flux":       "**Flux** is a GitOps tool: it watches a Git repository and applies what is on its main branch to the cluster, so the repository is the desired state.",
	"gitops":     "**GitOps** keeps the desired state of a system in Git: a change is a merge, and a tool such as Flux applies it.",
	"talos":      "**Talos** is a minimal Linux for Kubernetes nodes: no shell and no SSH, it is configured and managed through an API from one config file.",
	"cilium":     "**Cilium** is a Kubernetes network plugin (CNI) built on eBPF: it connects the pods and also does network policy, load balancing and BGP.",
	"kubernetes": "**Kubernetes** runs containers on a group of machines and keeps them in the state you declare.",
	"ceph":       "**Ceph** is distributed storage: it spreads data and its copies over many disks (OSDs), so one failed disk loses nothing.",
	"rook":       "**Rook** runs Ceph inside Kubernetes and manages it with resources such as a CephCluster.",
	"helm":       "**Helm** packages Kubernetes apps as charts with values; Flux installs a chart with a HelmRelease.",
	"hubble":     "**Hubble** is the observability layer of Cilium: it shows the network flows between pods.",
	"bmc":        "A **BMC** (baseboard management controller) is a small computer on a server board that powers it on and off and gives a remote console, also when the server is down.",
	"sops":       "**SOPS** encrypts the secret values in a file, so the file can live in Git; Flux decrypts it when it applies the file.",
}

var (
	reGlossaryAsk = regexp.MustCompile(`(?i)^\s*(?:(?:can|could) you\s+|please\s+)*(explain|define|describe|what is|what's|what are|what does|how does|how do(?:es)?|tell me about)\b`)
	// glossaryFiller are words of such a question that name no topic of
	// their own ("in one sentence", "in simple words").
	glossaryFiller = set(`explain define describe sentence sentences one two short simple simply briefly brief plain words word terms
		works work working mean means meaning used use here exactly actually really quick quickly k8s`)
)

func kindRank(kind string) int {
	switch kind {
	case "file":
		return 0
	case "kb":
		return 1
	}
	return 2
}

// glossaryQuestion answers a general question about one technology of the
// platform with one plain line, then names where the project uses it.
func (s *Svc) glossaryQuestion(ctx context.Context, p *progress, _ Request, q string, a *Answer) bool {
	if !reGlossaryAsk.MatchString(q) || reChangeID.MatchString(q) {
		return false
	}
	topic := ""
	for t := range contentTerms(q) {
		switch {
		case glossaryFiller[t]:
		case glossary[t] != "" && topic == "":
			topic = t
		default:
			// Another topic word: the question is about the project
			// ("the Ceph OSD layout on pegasus"), not the term.
			return false
		}
	}
	if topic == "" {
		return false
	}
	var where []llm.Hit
	_ = p.do(ctx, "search", "search '"+topic+"'", func() (string, error) {
		hits, err := s.o.Tools.Search(ctx, topic, 8)
		// Hits named after the term first (bgp-rack-6z.yaml), then files
		// and KB articles: they say how the project uses it. A hit that
		// only mentions the term counts when nothing is named after it.
		named := func(h llm.Hit) bool { return strings.Contains(strings.ToLower(h.Title+" "+h.Ref), topic) }
		sort.SliceStable(hits, func(i, j int) bool {
			if named(hits[i]) != named(hits[j]) {
				return named(hits[i])
			}
			return kindRank(hits[i].Kind) < kindRank(hits[j].Kind)
		})
		for _, h := range hits {
			if len(where) >= 3 || len(where) > 0 && !named(h) {
				break
			}
			if named(h) || strings.Contains(strings.ToLower(h.Text), topic) {
				where = append(where, h)
			}
		}
		return plural(len(where), "place in the project", "places in the project"), err
	})
	text := glossary[topic]
	if len(where) > 0 {
		labels := map[string]int{}
		for _, h := range where {
			labels[hitSource(h).Label]++
		}
		var names []string
		for _, h := range where {
			src := hitSource(h)
			name := src.Label
			if labels[name] > 1 {
				// Three flux.yaml files: name them by path.
				name = src.Ref
			}
			names = append(names, "`"+name+"`")
			addSource(a, src)
		}
		text += fmt.Sprintf("\n\nIn this project it shows up in %s. Ask me about %s for the details.", joinAnd(names), map[bool]string{true: "it", false: "one of them"}[len(names) == 1])
	}
	a.Text = text
	return true
}
