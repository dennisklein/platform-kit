package envtest_test

import (
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
)

// suite is one running kube-apiserver and etcd with the operator's CRDs, and
// an admin client for it.
type suite struct {
	env    *envtest.Environment
	cfg    *rest.Config
	scheme *runtime.Scheme
	cl     client.Client
}

// startEnv starts a control plane with the operator's CRDs and any extra CRD
// directories, and stops it when the test ends. It skips the test when the
// control-plane binaries are not set up.
func startEnv(t *testing.T, extraCRDDirs ...string) *suite {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run via `mise run envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     append([]string{filepath.Join("..", "..", "config", "crd")}, extraCRDDirs...),
		ErrorIfCRDPathMissing: true,
	}
	// envtest is loopback-only; without this the apiserver enumerates routes via
	// netlink to pick an advertise address, which sandboxes may forbid.
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	s, err := pkscheme.New()
	require.NoError(t, err)
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	return &suite{env: env, cfg: cfg, scheme: s, cl: cl}
}

// userClient is a client acting as the named person. system:masters skips
// RBAC, which is not under test; the username stays distinct, which is what
// the admission policies' request.userInfo rules see.
func (s *suite) userClient(t *testing.T, name string, groups ...string) client.Client {
	t.Helper()
	u, err := s.env.AddUser(envtest.User{Name: name, Groups: append(groups, "system:masters")}, nil)
	require.NoError(t, err)
	c, err := client.New(u.Config(), client.Options{Scheme: s.scheme})
	require.NoError(t, err)
	return c
}

// newManager is a manager on the suite's control plane, without metrics.
// Several suites run the same controllers in this one process, and controller
// names are process-global (metrics uniqueness), so the name check is off.
func (s *suite) newManager(t *testing.T) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(s.cfg, ctrl.Options{
		Scheme:     s.scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	return mgr
}

// runManager starts mgr in the background until ctx ends.
func runManager(t *testing.T, ctx context.Context, mgr ctrl.Manager) {
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()
}

// writeLedger writes the ledger text to a file and loads it, the way the
// manager reads its ledger.
func writeLedger(t *testing.T, text string) *ledger.Ledger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.yaml")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	led, err := ledger.Load(path)
	require.NoError(t, err)
	return led
}

// seedBMCSecrets creates the bmc-ca Secret, which pins the given certificates
// (DER), and the ibmc-factory credentials, both in the default namespace.
func seedBMCSecrets(t *testing.T, ctx context.Context, cl client.Client, certs ...[]byte) {
	t.Helper()
	var caBundle []byte
	for _, der := range certs {
		caBundle = append(caBundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-ca", Namespace: "default"},
			Data: map[string][]byte{"ca.crt": caBundle}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ibmc-factory", Namespace: "default"},
			Data: map[string][]byte{"username": []byte("admin"), "password": []byte("dev")}},
	} {
		require.NoError(t, cl.Create(ctx, sec))
	}
}
