package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// rbacDoc is every role and binding the operator ships, and the
// +kubebuilder:rbac markers the generated role comes from, by source file:
// the controller pages show the rows of their own file.
type rbacDoc struct {
	Roles    []rbacRole    `json:"roles"`
	Bindings []rbacBinding `json:"bindings"`
	Markers  []rbacMarkers `json:"markers"`
}

type rbacRole struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Source    string `json:"source"`
	// Generated is true for the manager's roles, which controller-gen writes
	// from the markers.
	Generated bool `json:"generated"`
	// Description is the comment above the manifest.
	Description string     `json:"description"`
	Rules       []rbacRule `json:"rules"`
}

type rbacRule struct {
	APIGroups       []string `json:"apiGroups"`
	Resources       []string `json:"resources"`
	Verbs           []string `json:"verbs"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
	Namespace       string   `json:"namespace,omitempty"`
}

type rbacBinding struct {
	Kind        string           `json:"kind"`
	Name        string           `json:"name"`
	Namespace   string           `json:"namespace"`
	Source      string           `json:"source"`
	Description string           `json:"description"`
	RoleRef     rbacv1.RoleRef   `json:"roleRef"`
	Subjects    []rbacv1.Subject `json:"subjects"`
}

type rbacMarkers struct {
	Source string     `json:"source"`
	Rules  []rbacRule `json:"rules"`
}

func readRBAC(cfg config) (rbacDoc, error) {
	doc := rbacDoc{Roles: []rbacRole{}, Bindings: []rbacBinding{}, Markers: []rbacMarkers{}}
	paths, err := filepath.Glob(filepath.Join(cfg.rbacDir, "*.yaml"))
	if err != nil {
		return doc, err
	}
	for _, p := range paths {
		if filepath.Base(p) == "kustomization.yaml" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return doc, err
		}
		for _, part := range splitDocuments(string(raw)) {
			if err := addManifest(&doc, cfg, relPath(cfg.root, p), part); err != nil {
				return doc, fmt.Errorf("%s: %w", p, err)
			}
		}
	}
	markers, err := readMarkers(cfg)
	if err != nil {
		return doc, err
	}
	doc.Markers = markers
	return doc, nil
}

func addManifest(doc *rbacDoc, cfg config, source, part string) error {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal([]byte(part), &head); err != nil {
		return err
	}
	comment := leadingComment(part)
	switch head.Kind {
	case "ClusterRole", "Role":
		var r rbacv1.ClusterRole
		if err := yaml.UnmarshalStrict([]byte(part), &r); err != nil {
			return err
		}
		role := rbacRole{
			Kind: head.Kind, Name: r.Name, Namespace: r.Namespace, Source: source,
			Generated: r.Name == cfg.generatedRole, Description: comment, Rules: []rbacRule{},
		}
		for _, rule := range r.Rules {
			role.Rules = append(role.Rules, rbacRule{
				APIGroups: nonNil(rule.APIGroups), Resources: nonNil(rule.Resources), Verbs: nonNil(rule.Verbs),
				ResourceNames: rule.ResourceNames, NonResourceURLs: rule.NonResourceURLs,
			})
		}
		doc.Roles = append(doc.Roles, role)
	case "ClusterRoleBinding", "RoleBinding":
		var b rbacv1.RoleBinding
		if err := yaml.UnmarshalStrict([]byte(part), &b); err != nil {
			return err
		}
		doc.Bindings = append(doc.Bindings, rbacBinding{
			Kind: head.Kind, Name: b.Name, Namespace: b.Namespace, Source: source,
			Description: comment, RoleRef: b.RoleRef, Subjects: nonNil(b.Subjects),
		})
	}
	return nil
}

// splitDocuments splits a YAML stream at its "---" lines and drops empty parts.
func splitDocuments(s string) []string {
	var out []string
	cur := []string{}
	flush := func() {
		if strings.TrimSpace(strings.Join(cur, "\n")) != "" {
			out = append(out, strings.Join(cur, "\n"))
		}
		cur = []string{}
	}
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimRight(line, " ") == "---" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// leadingComment is the "#" block at the top of a manifest, joined into
// paragraphs: it says what the role is for and who it is bound to.
func leadingComment(part string) string {
	var lines []string
	for line := range strings.SplitSeq(part, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			break
		}
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(t, "#"), " "))
	}
	return joinWrapped(lines)
}

var markerLine = regexp.MustCompile(`^\s*//\s*\+kubebuilder:rbac:(.+)$`)

// readMarkers collects the +kubebuilder:rbac markers in the non-test Go files
// under the scan directories, in file order.
func readMarkers(cfg config) ([]rbacMarkers, error) {
	var out []rbacMarkers
	for _, dir := range cfg.scanDirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !isSourceFile(path) {
				return nil
			}
			rules, err := markersIn(path)
			if err != nil {
				return err
			}
			if len(rules) > 0 {
				out = append(out, rbacMarkers{Source: relPath(cfg.root, path), Rules: rules})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b rbacMarkers) int { return strings.Compare(a.Source, b.Source) })
	return nonNil(out), nil
}

func markersIn(path string) ([]rbacRule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rules []rbacRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := markerLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		rule, err := parseMarker(m[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		rules = append(rules, rule)
	}
	return rules, sc.Err()
}

// parseMarker reads "groups=x,resources=a;b,verbs=get;list,namespace=n".
// Values separate their items with ";"; a quoted "" is the core group.
func parseMarker(s string) (rbacRule, error) {
	r := rbacRule{APIGroups: []string{}, Resources: []string{}, Verbs: []string{}}
	for kv := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return r, fmt.Errorf("rbac marker: %q has no value", kv)
		}
		var items []string
		for it := range strings.SplitSeq(v, ";") {
			items = append(items, strings.Trim(it, `"`))
		}
		switch k {
		case "groups":
			r.APIGroups = items
		case "resources":
			r.Resources = items
		case "verbs":
			r.Verbs = items
		case "resourceNames":
			r.ResourceNames = items
		case "urls":
			r.NonResourceURLs = items
		case "namespace":
			r.Namespace = v
		default:
			return r, fmt.Errorf("rbac marker: unknown key %q", k)
		}
	}
	return r, nil
}

func isSourceFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go") && !strings.HasPrefix(base, "zz_generated")
}
