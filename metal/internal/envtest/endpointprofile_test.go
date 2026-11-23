// The endpoint-convergence suite: EndpointProfiles select endpoints over
// their discovered-fact labels and run the manager itself. The credentials
// lane runs the operator's own account — adopted through the factory
// session, rotated on cadence (the old password provably dead), repaired
// through the factory break-glass after an out-of-band change, held whole on
// a same-layer conflict; the factory account survives everything, by
// decision. The ntp lane converges both vendors' time sources. The identity
// lane issues a cert-manager-signed certificate per endpoint, pushes it over
// each vendor's dialect (csr-native / key-upload), proves it with a
// pinned-CA handshake against what the listener actually serves, and — the
// load-bearing guard — never re-pushes a certificate the manager answered
// success to but does not serve.
package envtest_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
)

const (
	labelVLAN     = "metal.pk.io/vlan"
	managedByName = "pk-metal"
)

func TestEndpointConvergence(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset — run via `mise run envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
			// The CertificateRequest stand-in — real clusters run
			// cert-manager's own CRD, this suite plays the issuer itself.
			filepath.Join("testdata"),
		},
		ErrorIfCRDPathMissing: true,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1")
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	// Enforcement-enabled sims: the factory account seeded, everything else
	// requires whatever credential the operator runs. The listeners serve
	// whatever identity the sim currently holds — the hot swap at manager
	// reset is the identity lane's observable.
	huaweiURL, huaweiState, huaweiSeed := authSimServer(t, "huawei-2288h-v5")
	smcURL, smcState, smcSeed := authSimServer(t, "supermicro-x12")

	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.yaml")
	require.NoError(t, os.WriteFile(ledgerPath, []byte(`
"02:00:48:0b:74:60": `+huaweiURL+`
"02:00:5a:3a:cc:18": `+smcURL+`
`), 0o600))
	led, err := ledger.Load(ledgerPath)
	require.NoError(t, err)

	s, err := pkscheme.New()
	require.NoError(t, err)
	cl, err := client.New(cfg, client.Options{Scheme: s})
	require.NoError(t, err)
	ctx := t.Context()

	// The suite's CA plays cert-manager's issuer; the pinned bmc-ca bundle
	// holds the sims' factory identities AND this CA — exactly the live
	// posture, where the pinned bundle must cover both sides of an identity
	// roll.
	caCert, caKey := testCA(t)
	go issuerLoop(t, cl, caCert, caKey)

	var caBundle []byte
	for _, der := range [][]byte{huaweiSeed.Raw, smcSeed.Raw, caCert.Raw} {
		caBundle = append(caBundle,
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: "bmc-ca", Namespace: "default"},
			Data: map[string][]byte{"ca.crt": caBundle}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ibmc-factory", Namespace: "default"},
			Data: map[string][]byte{"username": []byte("admin"), "password": []byte("dev")}},
	} {
		require.NoError(t, cl.Create(ctx, sec))
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     s,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	require.NoError(t, controller.SetupAll(mgr, controller.Options{
		CredentialsNamespace: "default",
		Ledger:               led,
		TLS:                  &metalv1alpha1.EndpointTLS{CASecretRef: &corev1.LocalObjectReference{Name: "bmc-ca"}},
		PollInterval:         pollInterval,
	}, controller.LaneAsset, controller.LaneEndpoint, controller.LaneEndpointConvergence))
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager: %v", err)
		}
	}()

	for _, a := range []*metalv1alpha1.Asset{
		{ObjectMeta: metav1.ObjectMeta{Name: "ci-huawei"},
			Spec: metalv1alpha1.AssetSpec{CID: "CI-1", Serial: "SIMHW2288V5000391",
				BMCMac: "02:00:48:0b:74:60", VLAN: 1034,
				CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ci-smc"},
			Spec: metalv1alpha1.AssetSpec{CID: "CI-2", Serial: "SIMX12B000001",
				BMCMac: "02:00:5a:3a:cc:18", VLAN: 1034,
				CredentialsRef: corev1.LocalObjectReference{Name: "ibmc-factory"}}},
	} {
		require.NoError(t, cl.Create(ctx, a))
	}

	endpoint := func(name string) metalv1alpha1.ManagementEndpoint {
		ep := metalv1alpha1.ManagementEndpoint{}
		_ = cl.Get(ctx, client.ObjectKey{Name: name}, &ep)
		return ep
	}
	waitEndpoint := func(name string, ok func(metalv1alpha1.ManagementEndpoint) bool) metalv1alpha1.ManagementEndpoint {
		var last metalv1alpha1.ManagementEndpoint
		if !assert.Eventually(t, func() bool {
			last = endpoint(name)
			return ok(last)
		}, waitFor, tick) {
			t.Fatalf("endpoint %s: condition never met, last %+v / %+v", name, last.Labels, last.Status)
		}
		return last
	}
	configured := func(ep metalv1alpha1.ManagementEndpoint) *metav1.Condition {
		return findCondition(ep.Status.Conditions, metalv1alpha1.ConditionConfigured)
	}
	configuredReason := func(reason string) func(metalv1alpha1.ManagementEndpoint) bool {
		return func(ep metalv1alpha1.ManagementEndpoint) bool {
			c := configured(ep)
			return c != nil && c.Reason == reason
		}
	}
	managedSecret := func(epName string) (*corev1.Secret, bool) {
		sec := &corev1.Secret{}
		err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "endpoint-creds-" + epName}, sec)
		return sec, err == nil
	}
	states := map[string]*sim.State{"ci-huawei": huaweiState, "ci-smc": smcState}

	t.Run("discovered-labels", func(t *testing.T) {
		for name, model := range map[string]string{"ci-huawei": "2288H-V5", "ci-smc": "X12DPi-N6"} {
			ep := waitEndpoint(name, func(ep metalv1alpha1.ManagementEndpoint) bool {
				return ep.Labels[labelVLAN] == "1034"
			})
			assert.Equal(t, model, ep.Labels["metal.pk.io/model"], name)
			assert.NotEmpty(t, ep.Labels["metal.pk.io/vendor"], name)
		}
	})

	t.Run("adoption", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "managed-bmcs"},
			Spec: metalv1alpha1.EndpointProfileSpec{
				Match: metav1.LabelSelector{MatchLabels: map[string]string{labelVLAN: "1034"}},
				Credentials: &metalv1alpha1.CredentialsPolicy{
					Username: managedByName,
				},
			},
		}))
		for name, st := range states {
			ep := waitEndpoint(name, configuredReason(metalv1alpha1.ReasonInSync))
			assert.Equal(t, metav1.ConditionTrue, configured(ep).Status)
			assert.Equal(t, "ibmc-factory", ep.Spec.CredentialsRef.Name,
				"the factory ref is never moved — it is the break-glass")

			sec, ok := managedSecret(name)
			require.True(t, ok, "managed secret for %s", name)
			assert.Equal(t, managedByName, string(sec.Data["username"]))
			assert.NotEmpty(t, sec.Annotations["metal.pk.io/rotated-at"])

			assert.True(t, st.Authenticate(managedByName, string(sec.Data["password"])),
				"%s: the managed account carries the secret's password", name)
			assert.True(t, st.Authenticate("admin", "dev"),
				"%s: the factory account survives adoption untouched", name)
		}
	})

	t.Run("ntp", func(t *testing.T) {
		// The X12 fixture ships drifted (site server doubled into both
		// slots), the iBMC ships disabled — both must land on the profile.
		profile := &metalv1alpha1.EndpointProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "managed-bmcs"}, profile))
		want := []string{"ntp1.metal.internal", "ntp2.metal.internal"}
		profile.Spec.NTP = &metalv1alpha1.NTPPolicy{Servers: want}
		require.NoError(t, cl.Update(ctx, profile))

		for name, st := range states {
			require.Eventually(t, func() bool {
				cfg, err := st.NTP(ctx)
				return err == nil && cfg.Enabled && slices.Equal(cfg.Servers, want)
			}, waitFor, tick, "%s: NTP must converge to the profile", name)
			waitEndpoint(name, configuredReason(metalv1alpha1.ReasonInSync))
		}
	})

	t.Run("rotation-kills-the-old-credential", func(t *testing.T) {
		sec, ok := managedSecret("ci-smc")
		require.True(t, ok)
		before := string(sec.Data["password"])

		profile := &metalv1alpha1.EndpointProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "managed-bmcs"}, profile))
		profile.Spec.Credentials.MaxAge = &metav1.Duration{Duration: 500 * time.Millisecond}
		require.NoError(t, cl.Update(ctx, profile))

		require.Eventually(t, func() bool {
			sec, ok := managedSecret("ci-smc")
			if !ok || string(sec.Data["password"]) == before {
				return false
			}
			// Converged: the account answers to the new secret, not the old one.
			return smcState.Authenticate(managedByName, string(sec.Data["password"])) &&
				!smcState.Authenticate(managedByName, before)
		}, waitFor, tick, "the rotation must land on the BMC and kill the old password")

		// Stop the cadence before the next subtest, then wait for a converged
		// steady state (a rotation may still be mid-flight).
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "managed-bmcs"}, profile))
		profile.Spec.Credentials.MaxAge = nil
		require.NoError(t, cl.Update(ctx, profile))
		require.Eventually(t, func() bool {
			sec, ok := managedSecret("ci-smc")
			return ok && smcState.Authenticate(managedByName, string(sec.Data["password"]))
		}, waitFor, tick)

		ep := endpoint("ci-smc")
		v := findCondition(ep.Status.Conditions, metalv1alpha1.ConditionVerified)
		require.NotNil(t, v)
		assert.Equal(t, metav1.ConditionTrue, v.Status, "the endpoint stays verified through the rotation")
	})

	t.Run("break-glass-repair", func(t *testing.T) {
		// Someone changes the managed password behind the operator's back. The
		// managed secret no longer works — the repair path goes through the
		// factory account and converges the machine back to the secret.
		require.NoError(t, huaweiState.SetAccountPassword(managedByName, "scrambled-out-of-band"))
		require.Eventually(t, func() bool {
			sec, ok := managedSecret("ci-huawei")
			return ok && huaweiState.Authenticate(managedByName, string(sec.Data["password"]))
		}, waitFor, tick, "the repair must converge the account back to the managed secret")
		waitEndpoint("ci-huawei", configuredReason(metalv1alpha1.ReasonInSync))
	})

	t.Run("same-layer-conflict-holds", func(t *testing.T) {
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "other-account"},
			Spec: metalv1alpha1.EndpointProfileSpec{
				Match: metav1.LabelSelector{MatchLabels: map[string]string{labelVLAN: "1034"}},
				Credentials: &metalv1alpha1.CredentialsPolicy{
					Username: "metal-ops",
				},
			},
		}))
		ep := waitEndpoint("ci-smc", configuredReason(metalv1alpha1.ReasonProfileConflict))
		assert.Equal(t, metav1.ConditionFalse, configured(ep).Status)
		assert.Contains(t, configured(ep).Message, "managed-bmcs")
		_, ok := managedSecret("ci-smc")
		assert.True(t, ok, "a conflict holds — it never unwinds prior adoption")
		if pw, ok := smcState.AccountPassword("metal-ops"); ok {
			t.Fatalf("a conflicted merge must not touch the BMC, found account metal-ops (%q)", pw)
		}

		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "other-account"},
		}))
		waitEndpoint("ci-smc", configuredReason(metalv1alpha1.ReasonInSync))
	})

	crsFor := func(epName string) []string {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequestList",
		})
		require.NoError(t, cl.List(ctx, list, client.InNamespace("default")))
		var names []string
		for _, cr := range list.Items {
			if strings.HasPrefix(cr.GetName(), epName+"-identity-") {
				names = append(names, cr.GetName())
			}
		}
		return names
	}

	t.Run("identity", func(t *testing.T) {
		profile := &metalv1alpha1.EndpointProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "managed-bmcs"}, profile))
		profile.Spec.TLS = &metalv1alpha1.IdentityPolicy{
			IssuerRef: metalv1alpha1.IssuerRef{Name: "pk-test-ca"},
			Subject:   &metalv1alpha1.CertificateSubject{Country: "DE", Organization: "Example"},
		}
		require.NoError(t, cl.Update(ctx, profile))

		urls := map[string]string{"ci-huawei": huaweiURL, "ci-smc": smcURL}
		for name, st := range states {
			cn := "bmc-" + name + ".metal.internal"
			require.Eventually(t, func() bool {
				leaf := st.ServedLeaf()
				return leaf != nil && leaf.Subject.CommonName == cn
			}, waitFor, tick, "%s: the issued identity must end up served", name)

			leaf := st.ServedLeaf()
			require.NoError(t, leaf.CheckSignatureFrom(caCert), "%s: signed by the issuer", name)
			ep := waitEndpoint(name, configuredReason(metalv1alpha1.ReasonInSync))
			require.NotNil(t, ep.Status.IdentityPushed, "%s: the push memory survives InSync", name)

			// The material never outlives the push: key and CSR are gone.
			sec := &corev1.Secret{}
			err := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "endpoint-identity-" + name}, sec)
			assert.True(t, err != nil, "%s: the in-flight identity secret must be withdrawn", name)

			// The PoC's stage-4 proof, in-process: a handshake pinned to the
			// issuing CA alone, dialing the identity's name.
			pool := x509.NewCertPool()
			pool.AddCert(caCert)
			conn, err := tls.Dial("tcp", strings.TrimPrefix(urls[name], "https://"),
				&tls.Config{RootCAs: pool, ServerName: cn, MinVersion: tls.VersionTLS12})
			require.NoError(t, err, "%s: pinned-CA handshake against the served identity", name)
			require.NoError(t, conn.Close())
		}
	})

	t.Run("identity-stuck-never-repushes", func(t *testing.T) {
		// The lying oracle: the import answers success, the listener keeps
		// serving the old identity across the manager reset. A narrower
		// layered profile moves only the huawei endpoint to a new name, so
		// the re-issue is isolated.
		huaweiState.SetFault(sim.FaultCertStuck, true)
		require.NoError(t, cl.Create(ctx, &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "huawei-identity"},
			Spec: metalv1alpha1.EndpointProfileSpec{
				Match: metav1.LabelSelector{MatchLabels: map[string]string{
					"metal.pk.io/model": "2288H-V5",
				}},
				Layer: 10,
				TLS: &metalv1alpha1.IdentityPolicy{
					IssuerRef: metalv1alpha1.IssuerRef{Name: "pk-test-ca"},
					Domain:    "mgmt.metal.internal",
					Subject:   &metalv1alpha1.CertificateSubject{Country: "DE"},
				},
			},
		}))
		// IdentityPushFailed is reached by more than one path: the hold this
		// subtest is about, and a bare push error. A re-push of the same CSR
		// is one of those errors — the manager has already consumed it — and
		// it surfaces under the same reason, so waiting on the reason alone
		// can return one poll before the hold latches. Wait for the diagnosis
		// being asserted, not for a reason that merely precedes it.
		waitEndpoint("ci-huawei", func(ep metalv1alpha1.ManagementEndpoint) bool {
			c := configured(ep)
			return c != nil && c.Reason == metalv1alpha1.ReasonIdentityPushFailed &&
				strings.Contains(c.Message, "not being served")
		})

		// The hold is a hold: no second CertificateRequest, no second push,
		// over several poll rounds.
		crs := crsFor("ci-huawei")
		require.Len(t, crs, 1)
		served := huaweiState.ServedLeaf().Subject.CommonName
		time.Sleep(5 * pollInterval)
		assert.Equal(t, crs, crsFor("ci-huawei"), "an unchanged desired state never re-issues")
		assert.Equal(t, served, huaweiState.ServedLeaf().Subject.CommonName)

		// Unstick: the fault clears and the desired state returns to the
		// identity the listener still serves.
		huaweiState.SetFault(sim.FaultCertStuck, false)
		require.NoError(t, cl.Delete(ctx, &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "huawei-identity"},
		}))
		waitEndpoint("ci-huawei", configuredReason(metalv1alpha1.ReasonInSync))
	})

	t.Run("unmatched-is-unmanaged-not-unrotated", func(t *testing.T) {
		profile := &metalv1alpha1.EndpointProfile{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "managed-bmcs"}, profile))
		profile.Spec.Match = metav1.LabelSelector{MatchLabels: map[string]string{labelVLAN: "9999"}}
		require.NoError(t, cl.Update(ctx, profile))

		for name, st := range states {
			ep := waitEndpoint(name, func(ep metalv1alpha1.ManagementEndpoint) bool {
				return configured(ep) == nil
			})
			sec, ok := managedSecret(name)
			require.True(t, ok, "%s: the managed secret outlives the profile", name)
			assert.True(t, st.Authenticate(managedByName, string(sec.Data["password"])),
				"%s: the managed account keeps working", name)
			assert.Nil(t, ep.Status.IdentityPushed, "%s: the push memory leaves with the desired state", name)
			cn := "bmc-" + name + ".metal.internal"
			assert.Equal(t, cn, st.ServedLeaf().Subject.CommonName,
				"%s: the pushed identity stays — unmanaged, not un-pushed", name)
		}
	})
}

// authSimServer serves a fixture with the factory account seeded (Basic-auth
// enforcement on — the posture every adopted BMC has) over a listener whose
// certificate follows the sim's identity state. Returns the base URL, the
// state, and the seeded factory identity.
func authSimServer(t *testing.T, fixture string) (string, *sim.State, *x509.Certificate) {
	t.Helper()
	f, err := sim.LoadFixture(fixture)
	require.NoError(t, err)
	f.Accounts = []sim.FixtureAccount{
		{ID: "2", Username: "admin", Password: "dev", Role: "Administrator", Enabled: true},
	}
	st := sim.NewState(f)
	seed, err := asset.EphemeralCert()
	require.NoError(t, err)
	st.SetServedCertificate(seed)
	seedLeaf, err := x509.ParseCertificate(seed.Certificate[0])
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: sim.NewServer(st)}
	go func() {
		_ = srv.Serve(tls.NewListener(ln, &tls.Config{
			GetCertificate: st.TLSCertificate,
			MinVersion:     tls.VersionTLS12,
		}))
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String(), st, seedLeaf
}

// testCA mints the suite's issuing CA.
func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pk-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

// issuerLoop plays cert-manager: every unsigned CertificateRequest gets its
// CSR honored verbatim (names, IPs, requested duration), signed by the test
// CA, the chain and Ready condition stamped on status.
func issuerLoop(t *testing.T, cl client.Client, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) {
	ctx := t.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequestList",
		})
		if err := cl.List(ctx, list); err != nil {
			continue
		}
		for i := range list.Items {
			cr := &list.Items[i]
			if cert, _, _ := unstructured.NestedString(cr.Object, "status", "certificate"); cert != "" {
				continue
			}
			b64, _, _ := unstructured.NestedString(cr.Object, "spec", "request")
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				continue
			}
			block, _ := pem.Decode(raw)
			if block == nil {
				continue
			}
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				continue
			}
			duration := 2160 * time.Hour
			if ds, _, _ := unstructured.NestedString(cr.Object, "spec", "duration"); ds != "" {
				if d, err := time.ParseDuration(ds); err == nil {
					duration = d
				}
			}
			leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
				SerialNumber: big.NewInt(time.Now().UnixNano()),
				Subject:      csr.Subject,
				DNSNames:     csr.DNSNames,
				IPAddresses:  csr.IPAddresses,
				NotBefore:    time.Now().Add(-time.Hour),
				NotAfter:     time.Now().Add(duration),
				KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
				ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}, caCert, csr.PublicKey, caKey)
			if err != nil {
				continue
			}
			chain := append(
				pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
				pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})...)
			_ = unstructured.SetNestedField(cr.Object, base64.StdEncoding.EncodeToString(chain),
				"status", "certificate")
			_ = unstructured.SetNestedSlice(cr.Object, []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "Issued",
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			}}, "status", "conditions")
			if err := cl.Status().Update(ctx, cr); err != nil {
				t.Logf("issuer: %v", err)
			}
		}
	}
}
