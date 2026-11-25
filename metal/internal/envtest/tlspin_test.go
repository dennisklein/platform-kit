// The TLS key pin: with no CA and no opt-out, an endpoint trusts the key
// its BMC presents at the first contact, over a real TLS listener, and from
// then on sends nothing to a BMC that presents another key until a person
// approves that key with the annotation, which the operator then removes.
package envtest_test

import (
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
)

func TestTLSKeyPin(t *testing.T) {
	st := startEnv(t)
	url, simState, seed := authSimServer(t, "supermicro-x12")
	led := writeLedger(t, `"02:00:5a:3a:cc:18": `+url+"\n")
	cl := st.cl
	ctx := t.Context()
	seedBMCSecrets(t, ctx, cl)

	trail := &syncBuffer{}
	mgr := st.newManager(t)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		Ledger:               led,
		PollInterval:         pollInterval,
		Audit:                audit.New(trail, "op"),
	}, controller.LaneAsset, controller.LaneEndpoint))
	runManager(t, ctx, mgr)

	require.NoError(t, cl.Create(ctx, &metalv1alpha1.Asset{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-smc"},
		Spec: metalv1alpha1.AssetSpec{CID: "CI-2", Serial: "SIMX12B000001",
			BMCMac: "02:00:5a:3a:cc:18", VLAN: 100,
			CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"}},
	}))
	endpoint := func() metalv1alpha1.ManagementEndpoint {
		ep := metalv1alpha1.ManagementEndpoint{}
		_ = cl.Get(ctx, client.ObjectKey{Name: "ci-smc"}, &ep)
		return ep
	}
	reachable := func(ep metalv1alpha1.ManagementEndpoint) *metav1.Condition {
		return meta.FindStatusCondition(ep.Status.Conditions, metalv1alpha1.ConditionReachable)
	}
	wait := func(ok func(metalv1alpha1.ManagementEndpoint) bool) metalv1alpha1.ManagementEndpoint {
		var last metalv1alpha1.ManagementEndpoint
		if !assert.Eventually(t, func() bool { last = endpoint(); return ok(last) }, waitFor, tick) {
			t.Fatalf("condition never met, last %+v", last.Status)
		}
		return last
	}

	first := gofishclient.PublicKeyPin(seed)
	ep := wait(func(ep metalv1alpha1.ManagementEndpoint) bool {
		c := reachable(ep)
		return c != nil && c.Status == metav1.ConditionTrue && ep.Status.Phase == metalv1alpha1.EndpointReady
	})
	assert.Nil(t, ep.Spec.TLS, "no CA and no opt-out: the key pin")
	assert.Equal(t, &metalv1alpha1.TLSPin{PublicKey: first, Source: metalv1alpha1.TLSPinFirstUse}, ep.Status.TLSPin)

	// The BMC presents another key: the endpoint holds, and no login
	// reaches the BMC any more.
	swapped, err := asset.EphemeralCert()
	require.NoError(t, err)
	simState.SetServedCertificate(swapped)
	swappedLeaf, err := x509.ParseCertificate(swapped.Certificate[0])
	require.NoError(t, err)
	other := gofishclient.PublicKeyPin(swappedLeaf)
	ep = wait(func(ep metalv1alpha1.ManagementEndpoint) bool {
		c := reachable(ep)
		return c != nil && c.Reason == metalv1alpha1.ReasonTLSKeyChanged
	})
	assert.Equal(t, metalv1alpha1.EndpointUnreachable, ep.Status.Phase)
	assert.Equal(t, first, ep.Status.TLSPin.PublicKey)
	assert.Contains(t, reachable(ep).Message, other)

	// A person approves the presented key: the pin moves, the endpoint is
	// reachable again, and the used-up approval goes.
	ep.Annotations = map[string]string{metalv1alpha1.AnnotationApproveTLSKey: other}
	require.NoError(t, cl.Update(ctx, &ep))
	ep = wait(func(ep metalv1alpha1.ManagementEndpoint) bool {
		_, approval := ep.Annotations[metalv1alpha1.AnnotationApproveTLSKey]
		c := reachable(ep)
		return !approval && c != nil && c.Status == metav1.ConditionTrue
	})
	assert.Equal(t, &metalv1alpha1.TLSPin{PublicKey: other, Source: metalv1alpha1.TLSPinApproved}, ep.Status.TLSPin)

	// The trail has each pin once, with how it came to be trusted.
	var pins []string
	for _, line := range strings.Split(strings.TrimSpace(trail.String()), "\n") {
		var l struct{ Event, Key, Reason string }
		require.NoError(t, json.Unmarshal([]byte(line), &l))
		if l.Event == "bmc.tls.pinned" {
			pins = append(pins, l.Reason+" "+l.Key)
		}
	}
	assert.Equal(t, []string{"FirstUse " + first, "Approved " + other}, pins)
}
