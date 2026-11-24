package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// The shipped monitoring config (config/prometheus) is checked against this
// package and the manager's manifests, so that a renamed series, port or
// label cannot leave an alert that never fires. promtool checks the PromQL
// itself (mise run metrics:rules).

const runbookBase = "https://dennisklein.github.io/platform-kit/operate/runbooks/"

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, out), path)
}

// readDocs reads every document of a multi-document YAML file.
func readDocs(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path))
	require.NoError(t, err)
	var out []map[string]any
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var m map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(doc), &m), path)
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

type promRule struct {
	Alert       string            `json:"alert"`
	Expr        string            `json:"expr"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func alertRules(t *testing.T) []promRule {
	t.Helper()
	var pr struct {
		Spec struct {
			Groups []struct {
				Rules []promRule `json:"rules"`
			} `json:"groups"`
		} `json:"spec"`
	}
	readYAML(t, "config/prometheus/rules.yaml", &pr)
	var out []promRule
	for _, g := range pr.Spec.Groups {
		out = append(out, g.Rules...)
	}
	require.NotEmpty(t, out)
	return out
}

// seriesNames is every pk_metal_* name this package can serve.
func seriesNames(t *testing.T) []string {
	t.Helper()
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		(&Collector{}).Describe(ch)
		rolloutNodesDone.Describe(ch)
		rolloutNodesTotal.Describe(ch)
		close(ch)
	}()
	var names []string
	re := regexp.MustCompile(`fqName: "([^"]+)"`)
	for d := range ch {
		m := re.FindStringSubmatch(d.String())
		require.NotNil(t, m, d.String())
		names = append(names, m[1])
	}
	return names
}

func TestAlertRulesNameServedSeriesAndRunbooks(t *testing.T) {
	served := seriesNames(t)
	docs := filepath.Join("..", "..", "..", "docs", "src", "content", "docs", "operate", "runbooks")
	_, err := os.Stat(docs)
	haveDocs := err == nil
	seen := map[string]bool{}
	for _, r := range alertRules(t) {
		require.NotEmpty(t, r.Alert)
		assert.False(t, seen[r.Alert], "%s is defined twice", r.Alert)
		seen[r.Alert] = true
		assert.Contains(t, []string{"info", "warning", "critical"}, r.Labels["severity"], r.Alert)
		assert.NotEmpty(t, r.Annotations["summary"], r.Alert)

		for _, name := range regexp.MustCompile(`pk_metal_[a-z_]+`).FindAllString(r.Expr, -1) {
			assert.True(t, slices.Contains(served, name), "%s reads %s, which the operator does not serve", r.Alert, name)
		}

		url := r.Annotations["runbook_url"]
		require.True(t, strings.HasPrefix(url, runbookBase) && strings.HasSuffix(url, "/"),
			"%s: runbook_url %q is not a runbook page", r.Alert, url)
		if haveDocs {
			id := strings.TrimSuffix(strings.TrimPrefix(url, runbookBase), "/")
			_, err := os.Stat(filepath.Join(docs, id+".mdx"))
			assert.NoError(t, err, "%s links runbook %s, which the docs site does not have", r.Alert, id)
		}
	}
	assert.True(t, seen["PkMetalMetricsAbsent"], "the dead man's switch must stay")
}

// The ServiceMonitor must find the metrics Service and its port, and speak
// the scheme the Deployment serves.
func TestServiceMonitorFindsTheManager(t *testing.T) {
	var sm struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			NamespaceSelector struct {
				MatchNames []string `json:"matchNames"`
			} `json:"namespaceSelector"`
			Endpoints []struct {
				Port   string `json:"port"`
				Scheme string `json:"scheme"`
				Path   string `json:"path"`
			} `json:"endpoints"`
		} `json:"spec"`
	}
	readYAML(t, "config/prometheus/servicemonitor.yaml", &sm)
	require.Len(t, sm.Spec.Endpoints, 1)
	ep := sm.Spec.Endpoints[0]

	var svc map[string]any
	for _, d := range readDocs(t, "config/manager/services.yaml") {
		if d["metadata"].(map[string]any)["name"] == "pk-metal-metrics" {
			svc = d
		}
	}
	require.NotNil(t, svc, "config/manager/services.yaml has no pk-metal-metrics Service")
	md := svc["metadata"].(map[string]any)
	assert.Contains(t, sm.Spec.NamespaceSelector.MatchNames, md["namespace"])
	labels := md["labels"].(map[string]any)
	for k, v := range sm.Spec.Selector.MatchLabels {
		assert.Equal(t, v, labels[k], "the Service lacks the label %s the ServiceMonitor selects on", k)
	}
	var ports []string
	for _, p := range svc["spec"].(map[string]any)["ports"].([]any) {
		ports = append(ports, p.(map[string]any)["name"].(string))
	}
	assert.Contains(t, ports, ep.Port)
	assert.Equal(t, "/metrics", ep.Path)

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "manager", "deployment.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "--metrics-secure=false", "the Deployment serves HTTPS")
	assert.Equal(t, "https", ep.Scheme)

	// The rule's job matcher is the Service name: that is the job the
	// Prometheus Operator gives the scraped endpoints.
	for _, r := range alertRules(t) {
		if r.Alert == "PkMetalMetricsAbsent" {
			assert.Contains(t, r.Expr, `job="`+md["name"].(string)+`"`)
			assert.Contains(t, r.Expr, `namespace="`+md["namespace"].(string)+`"`)
		}
	}
}

// The kustomization lists every manifest in the directory.
func TestPrometheusKustomizationIsComplete(t *testing.T) {
	var k struct {
		Resources []string `json:"resources"`
	}
	readYAML(t, "config/prometheus/kustomization.yaml", &k)
	entries, err := os.ReadDir(filepath.Join("..", "..", "config", "prometheus"))
	require.NoError(t, err)
	for _, e := range entries {
		if e.Name() != "kustomization.yaml" && strings.HasSuffix(e.Name(), ".yaml") {
			assert.Contains(t, k.Resources, e.Name())
		}
	}
}
