package lab_test

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"

	osv1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim/fleet"
	"github.com/dennisklein/platform-kit/metal/internal/lab"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// The committed spec, the one lab:up and the lab-e2e workflow render.
var specPath = filepath.Join("..", "..", "..", "lab", "lab.yaml")

func loadSpec(t *testing.T, size string) *lab.Spec {
	t.Helper()
	s, err := lab.Load(specPath)
	require.NoError(t, err)
	if size != "" {
		require.NoError(t, s.Resize(size))
	}
	return s
}

func render(t *testing.T, size string) lab.Files {
	t.Helper()
	files, err := lab.Render(loadSpec(t, size), "lab/lab.yaml")
	require.NoError(t, err)
	return files
}

// yamlDocs decodes every document of a multi-document YAML file into plain
// maps and lists, which compare by content: key order, quoting, flow or block
// style and comments do not count.
func yamlDocs(t *testing.T, raw []byte) []any {
	t.Helper()
	var docs []any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		require.NoError(t, err)
		if doc != nil {
			docs = append(docs, doc)
		}
	}
}

// The default render is the lab the lab-e2e workflow has proven: one control
// plane on generic, two workers, node-3 on supermicro-x12, the same
// addresses, memory and MACs. The hand-written files it replaced (formerly
// lab/fleet.yaml and lab/manifests) are kept under testdata as the reference;
// they are frozen, a change of the default lab changes this test.
func TestDefaultRenderMatchesHandWrittenLab(t *testing.T) {
	files := render(t, "")
	reference := filepath.Join("testdata", "handwritten")
	for _, path := range []string{lab.FileFleet, lab.FileMachines, lab.FileTalos, lab.FileClaims} {
		t.Run(path, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(reference, path))
			require.NoError(t, err)
			assert.Equal(t, yamlDocs(t, want), yamlDocs(t, files[path]))
		})
	}
}

func TestGolden(t *testing.T) {
	for _, size := range []string{"", "3cp+2w", "1cp+0w"} {
		name := size
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			files := render(t, size)
			dir := filepath.Join("testdata", "golden", name)
			if *update {
				require.NoError(t, os.RemoveAll(dir))
				require.NoError(t, lab.Write(dir, files))
			}
			for path, got := range files {
				want, err := os.ReadFile(filepath.Join(dir, path))
				require.NoError(t, err, "run go test ./internal/lab -update to write the golden files")
				assert.Equal(t, string(want), string(got), path)
			}
		})
	}
}

// Every size renders files the consumers accept: the fleet loads, every
// personality names a fixture, every manifest decodes strictly into its API
// type, and the ledger has one usable entry per machine.
func TestRenderedFilesAreValid(t *testing.T) {
	for _, size := range []string{"", "1cp+0w", "3cp+2w", "5cp+10w"} {
		t.Run(size, func(t *testing.T) {
			s := loadSpec(t, size)
			files, err := lab.Render(s, "lab/lab.yaml")
			require.NoError(t, err)
			nodes := s.Plan()

			cfg, err := fleet.Parse(files[lab.FileFleet])
			require.NoError(t, err)
			require.Len(t, cfg.Machines, len(nodes))
			for _, m := range cfg.Machines {
				_, err := sim.LoadFixture(m.Personality)
				require.NoError(t, err, "machine %s", m.Name)
				assert.Equal(t, fleet.BackendQemu, m.Backend, "machine %s", m.Name)
			}

			kinds := map[string]int{}
			for _, path := range []string{lab.FileMachines, lab.FileTalos, lab.FileClaims} {
				for _, doc := range strings.Split(string(files[path]), "\n---\n") {
					var meta struct{ Kind string }
					require.NoError(t, sigsyaml.Unmarshal([]byte(doc), &meta))
					var obj any
					switch meta.Kind {
					case "Secret":
						obj = &corev1.Secret{}
					case "Asset":
						obj = &metalv1.Asset{}
					case "HostClaim":
						obj = &metalv1.HostClaim{}
					case "TalosCluster":
						obj = &osv1.TalosCluster{}
					case "TalosNode":
						obj = &osv1.TalosNode{}
					default:
						t.Fatalf("%s: unexpected kind %q", path, meta.Kind)
					}
					require.NoError(t, sigsyaml.UnmarshalStrict([]byte(doc), obj), "%s: %s", path, meta.Kind)
					kinds[meta.Kind]++
				}
			}
			assert.Equal(t, map[string]int{
				"Secret": 1, "Asset": len(nodes), "HostClaim": len(nodes),
				"TalosCluster": 1, "TalosNode": len(nodes),
			}, kinds)

			l, err := ledger.Parse(files[lab.FileLedger])
			require.NoError(t, err)
			assert.Empty(t, l.Problems())
			for _, n := range nodes {
				url, ok := l.Lookup(n.BMCMAC)
				require.True(t, ok, n.Name)
				assert.Equal(t, "https://"+n.Listen, url)
			}

			assert.Len(t, strings.Fields(string(files[lab.FileTaps])), len(nodes))
		})
	}
}

// Three control planes share the VIP, which the TalosCluster names as its
// endpoint; without workers the control planes take ordinary workloads.
func TestControlPlaneShapes(t *testing.T) {
	cluster := func(files lab.Files) osv1.TalosCluster {
		var c osv1.TalosCluster
		doc, _, _ := strings.Cut(string(files[lab.FileTalos]), "\n---\n")
		require.NoError(t, sigsyaml.UnmarshalStrict([]byte(doc), &c))
		return c
	}

	ha := cluster(render(t, "3cp+2w"))
	assert.Equal(t, "https://10.77.0.10:6443", ha.Spec.Endpoint)
	require.NotNil(t, ha.Spec.ControlPlane)
	assert.Equal(t, "10.77.0.10", ha.Spec.ControlPlane.VIP)
	assert.False(t, ha.Spec.ControlPlane.AllowScheduling)

	alone := cluster(render(t, "1cp+0w"))
	assert.True(t, alone.Spec.ControlPlane.AllowScheduling)

	var roles []string
	for _, n := range loadSpec(t, "3cp+2w").Plan() {
		roles = append(roles, n.Role)
	}
	assert.Equal(t, []string{"controlplane", "controlplane", "controlplane", "worker", "worker"}, roles)
}
