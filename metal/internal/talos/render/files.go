package render

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigsyaml "sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// LoadDir reads every *.yaml in dir (multi-doc aware) and returns the one
// TalosCluster and its TalosNodes, nodes sorted by name — the day-0 CLI input:
// a git checkout of CR manifests, no API server involved.
func LoadDir(dir string) (*osv1alpha1.TalosCluster, []osv1alpha1.TalosNode, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var cluster *osv1alpha1.TalosCluster
	var nodes []osv1alpha1.TalosNode
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		for _, doc := range strings.Split(string(data), "\n---") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var tm metav1.TypeMeta
			if err := sigsyaml.Unmarshal([]byte(doc), &tm); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			switch {
			case tm.APIVersion == osv1alpha1.GroupVersion.String() && tm.Kind == "TalosCluster":
				if cluster != nil {
					return nil, nil, fmt.Errorf("%s: second TalosCluster (already have %s) — one cluster per directory", e.Name(), cluster.Name)
				}
				cluster = &osv1alpha1.TalosCluster{}
				if err := sigsyaml.UnmarshalStrict([]byte(doc), cluster); err != nil {
					return nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
				}
			case tm.APIVersion == osv1alpha1.GroupVersion.String() && tm.Kind == "TalosNode":
				var node osv1alpha1.TalosNode
				if err := sigsyaml.UnmarshalStrict([]byte(doc), &node); err != nil {
					return nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
				}
				nodes = append(nodes, node)
			default:
				return nil, nil, fmt.Errorf("%s: unexpected document %s/%s — the render directory holds TalosCluster and TalosNode only", e.Name(), tm.APIVersion, tm.Kind)
			}
		}
	}
	if cluster == nil {
		return nil, nil, fmt.Errorf("no TalosCluster found in %s", dir)
	}
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("no TalosNode found in %s", dir)
	}
	slices.SortFunc(nodes, func(a, b osv1alpha1.TalosNode) int { return strings.Compare(a.Name, b.Name) })
	return cluster, nodes, nil
}

// WriteFiles dumps the CR set as one file per object — Convert's output and, at
// cutover, the manifests that replace talconfig.yaml in git. TypeMeta is
// stamped here (on copies): apiserver-fetched objects arrive without it, and a
// document LoadDir cannot re-read must never be written.
func WriteFiles(dir string, cluster *osv1alpha1.TalosCluster, nodes []osv1alpha1.TalosNode) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	write := func(name string, obj any) error {
		data, err := sigsyaml.Marshal(obj)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	c := *cluster
	c.TypeMeta = clusterTypeMeta()
	if err := write("taloscluster-"+c.Name+".yaml", &c); err != nil {
		return err
	}
	for i := range nodes {
		n := nodes[i]
		n.TypeMeta = nodeTypeMeta()
		if err := write("talosnode-"+n.Name+".yaml", &n); err != nil {
			return err
		}
	}
	return nil
}

func clusterTypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: osv1alpha1.GroupVersion.String(), Kind: "TalosCluster"}
}

func nodeTypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: osv1alpha1.GroupVersion.String(), Kind: "TalosNode"}
}
