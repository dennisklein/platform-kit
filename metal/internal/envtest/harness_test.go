package envtest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	reads  *readLog
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
	return &suite{env: env, cfg: cfg, scheme: s, cl: cl, reads: &readLog{seen: map[string]int{}}}
}

// userClient is a client acting as the named person. system:masters skips
// RBAC, so these clients test admission, not roles; the username stays
// distinct, which is what the admission policies' request.userInfo rules
// see. The committed roles are under test in rbac_test.go, through
// installManagerRBAC and approverClient.
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
		NewClient:  s.reads.newClient,
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

// barrierAnnotation carries the token that settle stamps on an object.
const barrierAnnotation = "test.metal.pk.io/barrier"

// readLog records which barrier tokens the managers' reconcilers have read,
// per object. A reconcile pass starts by reading its object, so a read that
// carries a token proves that a pass looked at the object as it was after
// the token was stamped.
type readLog struct {
	mu    sync.Mutex
	seen  map[string]int
	token int
}

// newClient is a ctrl.Options.NewClient that records the reads.
func (l *readLog) newClient(cfg *rest.Config, opts client.Options) (client.Client, error) {
	c, err := client.New(cfg, opts)
	if err != nil {
		return nil, err
	}
	return &recordingReader{Client: c, log: l}, nil
}

func readKey(obj client.Object, key types.NamespacedName, token string) string {
	return fmt.Sprintf("%T %s %s", obj, key, token)
}

func (l *readLog) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[key]
}

// recordingReader is the manager's client with its reads recorded.
type recordingReader struct {
	client.Client
	log *readLog
}

func (c *recordingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if err == nil {
		if token := obj.GetAnnotations()[barrierAnnotation]; token != "" {
			c.log.mu.Lock()
			c.log.seen[readKey(obj, key, token)]++
			c.log.mu.Unlock()
		}
	}
	return err
}

// settle replaces "sleep, then assert that nothing happened". A sleep
// passes without proof when the reconcilers are slow; settle fails instead.
// It stamps obj with a fresh token, rounds times, and each time waits until a
// reconciler has read obj with that token. A stamp is a metadata change: it
// wakes the reconcilers that watch obj, and those that requeue on a timer
// read it on their next pass. A reconciler works one pass at a time, so when
// it reads stamp n, its passes that read the stamps before have finished.
//
// settle reports whether anything but the stamps wrote obj while it ran,
// which is what a steady-state check asserts. Every stamp after the first is
// conditional on the resourceVersion of the one before, so no write between
// two stamps goes unseen. obj is left as the last stamp wrote it.
func (s *suite) settle(t *testing.T, ctx context.Context, obj client.Object, rounds int) (written bool) {
	t.Helper()
	for i := range rounds {
		s.reads.mu.Lock()
		s.reads.token++
		token := strconv.Itoa(s.reads.token)
		s.reads.mu.Unlock()
		stamp := func(precondition string) error {
			m := map[string]any{"annotations": map[string]string{barrierAnnotation: token}}
			if precondition != "" {
				m["resourceVersion"] = precondition
			}
			raw, err := json.Marshal(map[string]any{"metadata": m})
			require.NoError(t, err)
			return s.cl.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
		}
		precondition := ""
		if i > 0 {
			precondition = obj.GetResourceVersion()
		}
		err := stamp(precondition)
		if apierrors.IsConflict(err) {
			written = true
			err = stamp("")
		}
		require.NoError(t, err)
		key := readKey(obj, client.ObjectKeyFromObject(obj), token)
		require.Eventually(t, func() bool { return s.reads.count(key) > 0 }, waitFor, tick,
			"no reconciler read %s", key)
	}
	latest, ok := obj.DeepCopyObject().(client.Object)
	require.True(t, ok)
	require.NoError(t, s.cl.Get(ctx, client.ObjectKeyFromObject(obj), latest))
	return written || latest.GetResourceVersion() != obj.GetResourceVersion()
}

// syncBuffer is a bytes.Buffer that the manager's workers write to and the
// test reads from at the same time: the audit trail of a suite.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
