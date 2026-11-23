// Package overlays holds the third-party kinds: CRD and core schema subsets
// with UI hints and semantic diff hooks (HelmRelease, Flux Kustomization,
// CiliumBGPPeeringPolicy, Deployment, Service, Namespace, Flux sources and
// the talhelper config).
package overlays

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/dennisklein/platform-kit/internal/kinds"
	"github.com/dennisklein/platform-kit/internal/kinds/kindutil"
)

func schemaFunc(kind, version, raw string) func(string) (json.RawMessage, error) {
	if !json.Valid([]byte(raw)) {
		panic("overlays: invalid schema JSON for " + kind)
	}
	return func(v string) (json.RawMessage, error) {
		if v != "" && v != version {
			return nil, fmt.Errorf("%s: unknown version %q", kind, v)
		}
		return json.RawMessage(raw), nil
	}
}

func overlay(group, version, name, description, raw string, apply kinds.ApplyPath, describe func(old, new *kinds.Doc) []kinds.SemanticLine) kinds.Kind {
	return kinds.Kind{
		Name:        name,
		Group:       group,
		Version:     version,
		Versions:    []string{version},
		Description: description,
		Truth:       kinds.Intent,
		ApplyPath:   apply,
		Schema:      schemaFunc(name, version, raw),
		Describe:    describe,
	}
}

// All returns the overlay kinds.
func All() []kinds.Kind {
	return []kinds.Kind{
		overlay("helm.toolkit.fluxcd.io", "v2", "HelmRelease", "Flux HelmRelease (CRD schema subset)", helmReleaseSchema, kinds.ApplyFlux, describeHelmRelease),
		overlay("kustomize.toolkit.fluxcd.io", "v1", "Kustomization", "Flux Kustomization (CRD schema subset)", fluxKustomizationSchema, kinds.ApplyFlux, describeFluxKustomization),
		overlay("cilium.io", "v2alpha1", "CiliumBGPPeeringPolicy", "Cilium BGP peering policy (CRD schema subset)", bgpSchema, kinds.ApplyFlux, describeBGP),
		overlay("apps", "v1", "Deployment", "Kubernetes Deployment", deploymentSchema, kinds.ApplyFlux, describeDeployment),
		overlay("", "v1", "Service", "Kubernetes Service", serviceSchema, kinds.ApplyFlux, generic("Service", "service")),
		overlay("", "v1", "Namespace", "Kubernetes Namespace", namespaceSchema, kinds.ApplyFlux, generic("Namespace", "namespace")),
		overlay("source.toolkit.fluxcd.io", "v1", "HelmRepository", "Flux HelmRepository", helmRepositorySchema, kinds.ApplyFlux, generic("HelmRepository", "Helm repository")),
		overlay("source.toolkit.fluxcd.io", "v1", "GitRepository", "Flux GitRepository", gitRepositorySchema, kinds.ApplyFlux, generic("GitRepository", "Git source")),
		talosConfig(),
	}
}

func talosConfig() kinds.Kind {
	k := overlay("talhelper.siderolabs.dev", "v1", "TalosConfig", "talhelper config of a cluster; applied with talosctl after the merge", talosSchema, kinds.ApplyManual, describeTalos)
	k.Paths = []string{"control/talos/*/talconfig.yaml"}
	k.NewPath = func(name string) string { return "control/talos/" + name + "/talconfig.yaml" }
	k.Match = func(p string, doc *kinds.Doc) bool {
		return strings.HasPrefix(p, "control/talos/") && path.Base(p) == "talconfig.yaml"
	}
	return k
}

func generic(kind, what string) func(old, new *kinds.Doc) []kinds.SemanticLine {
	return kindutil.Describer(kind, what)
}

func sl(d *kinds.Doc, kind, subject, text string, op kinds.Op, field string) kinds.SemanticLine {
	line, _ := kindutil.Pos(d, field)
	return kinds.SemanticLine{Subject: subject, Text: text, Op: op, Path: d.Path, Line: line, Kind: kind}
}

// ---------------------------------------------------------------- BGP

// SelectedNodes returns the node names a BGP policy selects by hostname
// (matchExpressions In on kubernetes.io/hostname, or matchLabels).
func SelectedNodes(data map[string]any) []string {
	var out []string
	if ml, ok := kindutil.Get(data, "spec", "nodeSelector", "matchLabels").(map[string]any); ok {
		if h, ok := ml["kubernetes.io/hostname"].(string); ok {
			out = append(out, h)
		}
	}
	exprs, _ := kindutil.Get(data, "spec", "nodeSelector", "matchExpressions").([]any)
	for _, e := range exprs {
		m, _ := e.(map[string]any)
		if kindutil.Str(m, "key") == "kubernetes.io/hostname" && kindutil.Str(m, "operator") == "In" {
			out = append(out, kindutil.Strings(m, "values")...)
		}
	}
	return out
}

// Peer is a BGP neighbor of a virtual router.
type Peer struct {
	Address string
	ASN     int
	Index   string // field path
}

// Peers returns the BGP neighbors of a policy.
func Peers(data map[string]any) []Peer {
	var out []Peer
	vrs, _ := kindutil.Get(data, "spec", "virtualRouters").([]any)
	for i, vr := range vrs {
		m, _ := vr.(map[string]any)
		ns, _ := m["neighbors"].([]any)
		for j, n := range ns {
			nm, _ := n.(map[string]any)
			out = append(out, Peer{Address: kindutil.Str(nm, "peerAddress"), ASN: kindutil.Int(nm, "peerASN"),
				Index: fmt.Sprintf("spec.virtualRouters[%d].neighbors[%d]", i, j)})
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func describeBGP(old, new *kinds.Doc) []kinds.SemanticLine {
	const kind = "CiliumBGPPeeringPolicy"
	switch {
	case old == nil && new == nil:
		return nil
	case old == nil:
		return []kinds.SemanticLine{sl(new, kind, new.Name, fmt.Sprintf("new BGP peering policy for %s, %s",
			plural(len(SelectedNodes(new.Data)), "node", "nodes"), plural(len(Peers(new.Data)), "peer", "peers")), kinds.OpAdd, "")}
	case new == nil:
		return []kinds.SemanticLine{{Subject: old.Name, Text: "BGP peering policy removed", Op: kinds.OpDelete, Path: old.Path, Kind: kind}}
	}
	var out []kinds.SemanticLine
	a, b := setOf(SelectedNodes(old.Data)), setOf(SelectedNodes(new.Data))
	added, removed := diffSets(a, b)
	if len(added) > 0 {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("BGP policy selects %s", plural(len(added), "more node", "more nodes")), kinds.OpChange, "spec.nodeSelector"))
	}
	if len(removed) > 0 {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("BGP policy selects %s less (%s)", plural(len(removed), "node", "nodes"), strings.Join(removed, ", ")), kinds.OpChange, "spec.nodeSelector"))
	}
	op := map[string]Peer{}
	for _, p := range Peers(old.Data) {
		op[p.Address] = p
	}
	var newPeers []Peer
	for _, p := range Peers(new.Data) {
		o, ok := op[p.Address]
		delete(op, p.Address)
		if !ok {
			newPeers = append(newPeers, p)
			continue
		}
		if o.ASN != p.ASN {
			out = append(out, sl(new, kind, new.Name, fmt.Sprintf("BGP peer %s ASN %d to %d", p.Address, o.ASN, p.ASN), kinds.OpChange, p.Index))
		}
	}
	if len(newPeers) > 0 {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("%s", plural(len(newPeers), "new BGP peer", "new BGP peers")), kinds.OpChange, newPeers[0].Index))
	}
	if len(op) > 0 {
		var gone []string
		for a := range op {
			gone = append(gone, a)
		}
		sort.Strings(gone)
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("%s removed (%s)", plural(len(gone), "BGP peer", "BGP peers"), strings.Join(gone, ", ")), kinds.OpChange, "spec.virtualRouters"))
	}
	if len(out) == 0 {
		return generic(kind, "BGP peering policy")(old, new)
	}
	return out
}

func setOf(l []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range l {
		m[s] = true
	}
	return m
}

func diffSets(a, b map[string]bool) (added, removed []string) {
	for k := range b {
		if !a[k] {
			added = append(added, k)
		}
	}
	for k := range a {
		if !b[k] {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return
}

// ---------------------------------------------------------------- apps

// imageParts splits an image into repository and tag.
func imageParts(img string) (repo, tag string) {
	at := strings.LastIndex(img, "@")
	if at > 0 {
		return img[:at], img[at+1:]
	}
	slash := strings.LastIndex(img, "/")
	colon := strings.LastIndex(img, ":")
	if colon > slash {
		return img[:colon], img[colon+1:]
	}
	return img, "latest"
}

func containers(data map[string]any) map[string]string {
	out := map[string]string{}
	cs, _ := kindutil.Get(data, "spec", "template", "spec", "containers").([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		out[kindutil.Str(m, "name")] = kindutil.Str(m, "image")
	}
	return out
}

func describeDeployment(old, new *kinds.Doc) []kinds.SemanticLine {
	const kind = "Deployment"
	if old == nil || new == nil {
		return generic(kind, "deployment")(old, new)
	}
	var out []kinds.SemanticLine
	oc, nc := containers(old.Data), containers(new.Data)
	names := make([]string, 0, len(nc))
	for n := range nc {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		oi, ok := oc[n]
		ni := nc[n]
		if !ok || oi == ni {
			continue
		}
		subject := new.Name
		if len(nc) > 1 {
			subject = new.Name + " " + n
		}
		or, ot := imageParts(oi)
		nr, nt := imageParts(ni)
		text := fmt.Sprintf("image %s to %s", ot, nt)
		if or != nr {
			text = fmt.Sprintf("image %s to %s", oi, ni)
		}
		out = append(out, sl(new, kind, subject, text, kinds.OpChange, fmt.Sprintf("spec.template.spec.containers[%d].image", i)))
	}
	if a, b := kindutil.Int(old.Data, "spec", "replicas"), kindutil.Int(new.Data, "spec", "replicas"); a != b {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("replicas %d to %d", a, b), kinds.OpChange, "spec.replicas"))
	}
	if len(out) == 0 {
		return generic(kind, "deployment")(old, new)
	}
	return out
}

// ---------------------------------------------------------------- Flux

func describeHelmRelease(old, new *kinds.Doc) []kinds.SemanticLine {
	const kind = "HelmRelease"
	if old == nil || new == nil {
		return generic(kind, "Helm release")(old, new)
	}
	var out []kinds.SemanticLine
	if a, b := kindutil.Str(old.Data, "spec", "chart", "spec", "version"), kindutil.Str(new.Data, "spec", "chart", "spec", "version"); a != b {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("chart %s to %s", a, b), kinds.OpChange, "spec.chart.spec.version"))
	}
	if a, b := kindutil.Str(old.Data, "spec", "interval"), kindutil.Str(new.Data, "spec", "interval"); a != b {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("interval %s to %s", a, b), kinds.OpChange, "spec.interval"))
	}
	ov, _ := kindutil.Get(old.Data, "spec", "values").(map[string]any)
	nv, _ := kindutil.Get(new.Data, "spec", "values").(map[string]any)
	for _, t := range kindutil.FieldLines(ov, nv, 4) {
		out = append(out, sl(new, kind, new.Name, "values "+t, kinds.OpChange, "spec.values"))
	}
	if len(out) == 0 {
		// Other spec fields; the sops metadata changes with every encryption.
		oc, nc := copyWithout(old.Data, "sops"), copyWithout(new.Data, "sops")
		for _, t := range kindutil.FieldLines(oc, nc, 4) {
			out = append(out, sl(new, kind, new.Name, t, kinds.OpChange, ""))
		}
	}
	return out
}

func copyWithout(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func describeFluxKustomization(old, new *kinds.Doc) []kinds.SemanticLine {
	const kind = "Kustomization"
	if old == nil || new == nil {
		return generic(kind, "Flux Kustomization")(old, new)
	}
	var out []kinds.SemanticLine
	if a, b := kindutil.Str(old.Data, "spec", "path"), kindutil.Str(new.Data, "spec", "path"); a != b {
		out = append(out, sl(new, kind, new.Name, fmt.Sprintf("path %s to %s", a, b), kinds.OpChange, "spec.path"))
	}
	if len(out) == 0 {
		return generic(kind, "Flux Kustomization")(old, new)
	}
	return out
}

func describeTalos(old, new *kinds.Doc) []kinds.SemanticLine {
	const kind = "TalosConfig"
	if old == nil || new == nil {
		return generic(kind, "talconfig")(old, new)
	}
	subject := kindutil.Str(new.Data, "clusterName") + " talconfig"
	var out []kinds.SemanticLine
	for _, f := range []string{"talosVersion", "kubernetesVersion"} {
		if a, b := kindutil.Str(old.Data, f), kindutil.Str(new.Data, f); a != b {
			out = append(out, sl(new, kind, subject, fmt.Sprintf("%s %s to %s", strings.TrimSuffix(f, "Version"), a, b), kinds.OpChange, f))
		}
	}
	for _, t := range kindutil.FieldLines(copyWithout(copyWithout(old.Data, "talosVersion"), "kubernetesVersion"),
		copyWithout(copyWithout(new.Data, "talosVersion"), "kubernetesVersion"), 4) {
		out = append(out, sl(new, kind, subject, t, kinds.OpChange, ""))
	}
	return out
}
