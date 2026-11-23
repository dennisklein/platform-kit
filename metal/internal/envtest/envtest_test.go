// The API-scaffold gate: the generated CRDs install into a real apiserver, the types round-trip
// through it, and the schema actually rejects what the markers claim it rejects.
// Needs the envtest control-plane binaries — `make -C metal envtest` provides them and
// runs this; a bare `go test ./...` skips.
package envtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/phase"
)

func TestAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset — run via `make -C metal envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	// envtest is loopback-only; without this the apiserver enumerates routes via
	// netlink to pick an advertise address, which sandboxes may forbid.
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	s := runtime.NewScheme()
	require.NoError(t, metalv1alpha1.AddToScheme(s))
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	ctx := t.Context()

	t.Run("asset round-trip and validation", func(t *testing.T) {
		asset := &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-00397"},
			Spec: metalv1alpha1.AssetSpec{
				CID:            "CI-00397",
				Serial:         "SIMHW2288V5000397",
				BMCMac:         "02:00:48:0b:74:83",
				VLAN:           1034,
				CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"},
			},
		}
		require.NoError(t, cl.Create(ctx, asset))
		got := &metalv1alpha1.Asset{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ci-00397"}, got))
		assert.Equal(t, asset.Spec, got.Spec)

		missingCID := &metalv1alpha1.Asset{
			ObjectMeta: metav1.ObjectMeta{Name: "no-cid"},
			Spec: metalv1alpha1.AssetSpec{
				Serial:         "X",
				CredentialsRef: corev1.LocalObjectReference{Name: "c"},
			},
		}
		assert.Error(t, cl.Create(ctx, missingCID), "cid MinLength must reject")

		badMAC := asset.DeepCopy()
		badMAC.ObjectMeta = metav1.ObjectMeta{Name: "bad-mac"}
		badMAC.Spec.BMCMac = "48-DC-2D-0B-74-83"
		assert.Error(t, cl.Create(ctx, badMAC), "bmcMac pattern must reject non-canonical form")
	})

	t.Run("endpoint status subresource and phase enum", func(t *testing.T) {
		ep := &metalv1alpha1.ManagementEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "ci-00397"},
			Spec: metalv1alpha1.ManagementEndpointSpec{
				AssetRef: metalv1alpha1.Reference{Name: "ci-00397"},
				Address:  "https://[fd6b:5a2e:1c4d:1034::14]",
			},
		}
		require.NoError(t, cl.Create(ctx, ep))

		plainHTTP := &metalv1alpha1.ManagementEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "plain-http"},
			Spec: metalv1alpha1.ManagementEndpointSpec{
				AssetRef: metalv1alpha1.Reference{Name: "ci-00397"},
				Address:  "http://[fd6b:5a2e:1c4d:1034::14]",
			},
		}
		assert.Error(t, cl.Create(ctx, plainHTTP), "address pattern must reject plain http")

		ep.Status.Conditions = []metav1.Condition{{
			Type: metalv1alpha1.ConditionReachable, Status: metav1.ConditionTrue,
			Reason: "SessionUp", LastTransitionTime: metav1.Now(),
		}}
		ep.Status.Vendor = "Huawei"
		phase.StampEndpoint(ep)
		require.NoError(t, cl.Status().Update(ctx, ep))

		got := &metalv1alpha1.ManagementEndpoint{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ci-00397"}, got))
		assert.Equal(t, metalv1alpha1.EndpointInventorying, got.Status.Phase)
		assert.Equal(t, "Huawei", got.Status.Vendor)

		got.Status.Phase = "Bogus"
		assert.Error(t, cl.Status().Update(ctx, got), "phase enum must reject unknown values")
	})

	t.Run("host status shape", func(t *testing.T) {
		h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "ref-2288h"}}
		require.NoError(t, cl.Create(ctx, h))

		h.Status = metalv1alpha1.HostStatus{
			Identity: &metalv1alpha1.HostIdentity{
				UUID:   "2D0B7483-48DC-B772-EB11-5D279EA3DCD3",
				Serial: "SIMHW2288V5000397",
				Vendor: "Huawei",
			},
			ManagedBy: []metalv1alpha1.ManagementPath{{
				EndpointRef: metalv1alpha1.Reference{Name: "ci-00397"},
				SystemID:    "1",
				Reachable:   true,
			}},
			Conditions: []metav1.Condition{{
				Type: metalv1alpha1.ConditionInspected, Status: metav1.ConditionTrue,
				Reason: "InventoryGathered", LastTransitionTime: metav1.Now(),
			}},
		}
		phase.StampHost(h, phase.HostFacts{})
		require.NoError(t, cl.Status().Update(ctx, h))

		got := &metalv1alpha1.Host{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "ref-2288h"}, got))
		assert.Equal(t, metalv1alpha1.HostInspecting, got.Status.Phase)
		require.Len(t, got.Status.ManagedBy, 1)
		assert.Equal(t, "ci-00397", got.Status.ManagedBy[0].EndpointRef.Name)
		require.NotNil(t, got.Status.Identity)
		assert.Equal(t, "SIMHW2288V5000397", got.Status.Identity.Serial)
	})
}
