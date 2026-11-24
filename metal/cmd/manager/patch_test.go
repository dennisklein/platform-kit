package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// patchOp is one JSON-patch operation of a kustomize patch file.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// applyAdd applies a JSON-patch add (RFC 6902) to doc: the only operation
// the committed overlays use. It is hand-rolled because the surface is this
// small; a JSON-patch library would be a dependency for one test.
func applyAdd(doc any, path string, value any) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var walk func(node any, parts []string) (any, error)
	walk = func(node any, parts []string) (any, error) {
		key := strings.NewReplacer("~1", "/", "~0", "~").Replace(parts[0])
		last := len(parts) == 1
		switch n := node.(type) {
		case map[string]any:
			if last {
				n[key] = value // add on an existing member replaces it, lists included
				return n, nil
			}
			child, ok := n[key]
			if !ok {
				return nil, fmt.Errorf("%s: no member %q", path, key)
			}
			v, err := walk(child, parts[1:])
			n[key] = v
			return n, err
		case []any:
			i := len(n)
			if key != "-" {
				var err error
				if i, err = strconv.Atoi(key); err != nil || i < 0 || i > len(n) {
					return nil, fmt.Errorf("%s: bad index %q", path, key)
				}
			}
			if last {
				return append(n[:i:i], append([]any{value}, n[i:]...)...), nil
			}
			if i == len(n) {
				return nil, fmt.Errorf("%s: index %q past the end", path, key)
			}
			v, err := walk(n[i], parts[1:])
			n[i] = v
			return n, err
		}
		return nil, fmt.Errorf("%s: %q is not inside an object or a list", path, key)
	}
	return walk(doc, parts)
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, into))
}

// The manager's Deployment as the lab overlay renders it, with the
// media-store component: every environment variable of the base must still
// be there. A JSON-patch add on .../env (not .../env/-) replaced the base's
// list, dropped NODE_NAME, and turned the manager's self-protection off
// without a word wherever the media store was used.
func TestOverlaysKeepTheBaseEnvironment(t *testing.T) {
	var doc any
	readYAML(t, "config/manager/deployment.yaml", &doc)
	var base appsv1.Deployment
	readYAML(t, "config/manager/deployment.yaml", &base)

	for _, patch := range []string{"config/lab/manager_args.yaml", "config/media-store/manager.yaml"} {
		var ops []patchOp
		readYAML(t, patch, &ops)
		for _, op := range ops {
			require.Equal(t, "add", op.Op, "%s: only add is applied here", patch)
			var err error
			doc, err = applyAdd(doc, op.Path, op.Value)
			require.NoError(t, err, patch)
		}
	}
	raw, err := yaml.Marshal(doc)
	require.NoError(t, err)
	var got appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(raw, &got))

	env := map[string]bool{}
	for _, e := range got.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = true
	}
	for _, e := range base.Spec.Template.Spec.Containers[0].Env {
		assert.True(t, env[e.Name], "the overlays drop %s from the base Deployment", e.Name)
	}
	assert.True(t, env["NODE_NAME"], "the self-protection needs the node the manager runs on")
	assert.True(t, env["POD_NAME"] && env["POD_NAMESPACE"], "the media store finds the manager's own image by them")
}

func TestApplyAdd(t *testing.T) {
	doc := map[string]any{"l": []any{"a"}}
	got, err := applyAdd(doc, "/l/-", "b")
	require.NoError(t, err)
	assert.Equal(t, []any{"a", "b"}, got.(map[string]any)["l"])
	got, err = applyAdd(got, "/l", []any{"c"})
	require.NoError(t, err)
	assert.Equal(t, []any{"c"}, got.(map[string]any)["l"], "an add on the list itself replaces it")
	_, err = applyAdd(got, "/missing/x", 1)
	assert.Error(t, err)
}
