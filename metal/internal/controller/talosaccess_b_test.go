package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// A look at a node carries os:reader; only the dry-run drift check and the
// executor's steps carry os:admin.
func TestObservationsMintTheReaderRole(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "talos", "render", "testdata", "secrets.yaml"))
	require.NoError(t, err)
	bundle, err := render.LoadBundle(data, "v1.14.6")
	require.NoError(t, err)
	cluster := &osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"}}

	roles := func(tcfg []byte) []string {
		t.Helper()
		require.NotNil(t, tcfg)
		r, err := render.TalosconfigRoles(tcfg)
		require.NoError(t, err)
		return r
	}
	reader, err := readerTalosconfig(bundle, cluster.Name, []string{"10.0.0.1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"os:reader"}, roles(reader), "check-in, etcd health and the leader lookup only read")
	assert.Equal(t, []string{"os:admin"}, roles(driftTalosconfig(cluster, bundle, "10.0.0.1")),
		"Talos serves a dry-run apply to admins only")
	assert.Nil(t, driftTalosconfig(cluster, nil, "10.0.0.1"))
}
