package render

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigsyaml "sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// Manifests is a directory of os.pk.io manifests: the day-0 CLI input, a git
// checkout read without an API server.
type Manifests struct {
	Cluster *osv1alpha1.TalosCluster
	// Nodes are sorted by name.
	Nodes []osv1alpha1.TalosNode
	// ConfigMaps are the ones patches may reference.
	ConfigMaps []corev1.ConfigMap
}

// Node returns the named node, or nil.
func (m *Manifests) Node(name string) *osv1alpha1.TalosNode {
	for i := range m.Nodes {
		if m.Nodes[i].Name == name {
			return &m.Nodes[i]
		}
	}
	return nil
}

// ConfigMapReader resolves patch references against the ConfigMaps found in
// the directory, the same way the controller resolves them against the API.
func (m *Manifests) ConfigMapReader() ConfigMapReader {
	return func(ref osv1alpha1.ConfigMapKeyReference) (string, error) {
		for i := range m.ConfigMaps {
			cm := &m.ConfigMaps[i]
			if cm.Namespace != ref.Namespace || cm.Name != ref.Name {
				continue
			}
			if err := PatchConfigMapLabeled(cm.Labels, ref); err != nil {
				return "", err
			}
			v, ok := cm.Data[ref.Key]
			if !ok {
				return "", fmt.Errorf("%w: ConfigMap %s/%s has no key %q", ErrPatchUnresolvable, ref.Namespace, ref.Name, ref.Key)
			}
			return v, nil
		}
		return "", fmt.Errorf("%w: no ConfigMap %s/%s in the manifests", ErrPatchUnresolvable, ref.Namespace, ref.Name)
	}
}

// LoadDir reads every *.yaml in dir (multi-document aware) and returns the one
// TalosCluster, its TalosNodes and any ConfigMaps. Other kinds are skipped, so
// the directory the cluster is applied from can be the one rendered from.
func LoadDir(dir string) (*Manifests, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifests{}
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, doc := range strings.Split(string(data), "\n---") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			if err := m.add(e.Name(), []byte(doc)); err != nil {
				return nil, err
			}
		}
	}
	if m.Cluster == nil {
		return nil, fmt.Errorf("no TalosCluster found in %s", dir)
	}
	for i := range m.Nodes {
		if ref := m.Nodes[i].Spec.ClusterRef.Name; ref != m.Cluster.Name {
			return nil, fmt.Errorf("TalosNode %s is a member of %q, the directory holds %q", m.Nodes[i].Name, ref, m.Cluster.Name)
		}
	}
	slices.SortFunc(m.Nodes, func(a, b osv1alpha1.TalosNode) int { return strings.Compare(a.Name, b.Name) })
	return m, nil
}

func (m *Manifests) add(file string, doc []byte) error {
	var tm metav1.TypeMeta
	if err := sigsyaml.Unmarshal(doc, &tm); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	switch {
	case tm.APIVersion == osv1alpha1.GroupVersion.String() && tm.Kind == "TalosCluster":
		if m.Cluster != nil {
			return fmt.Errorf("%s: second TalosCluster (already have %s): one cluster per directory", file, m.Cluster.Name)
		}
		m.Cluster = &osv1alpha1.TalosCluster{}
		if err := sigsyaml.UnmarshalStrict(doc, m.Cluster); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	case tm.APIVersion == osv1alpha1.GroupVersion.String() && tm.Kind == "TalosNode":
		var node osv1alpha1.TalosNode
		if err := sigsyaml.UnmarshalStrict(doc, &node); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		m.Nodes = append(m.Nodes, node)
	case tm.APIVersion == "v1" && tm.Kind == "ConfigMap":
		var cm corev1.ConfigMap
		if err := sigsyaml.UnmarshalStrict(doc, &cm); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		m.ConfigMaps = append(m.ConfigMaps, cm)
	}
	return nil
}

// WriteFiles dumps a cluster and its nodes as one file per object, in the
// form LoadDir reads back. TypeMeta is stamped here (on copies): objects read
// from the API server arrive without it.
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
	c.TypeMeta = metav1.TypeMeta{APIVersion: osv1alpha1.GroupVersion.String(), Kind: "TalosCluster"}
	c.Status = osv1alpha1.TalosClusterStatus{}
	if err := write("taloscluster-"+c.Name+".yaml", &c); err != nil {
		return err
	}
	for i := range nodes {
		n := nodes[i]
		n.TypeMeta = metav1.TypeMeta{APIVersion: osv1alpha1.GroupVersion.String(), Kind: "TalosNode"}
		n.Status = osv1alpha1.TalosNodeStatus{}
		if err := write("talosnode-"+n.Name+".yaml", &n); err != nil {
			return err
		}
	}
	return nil
}
