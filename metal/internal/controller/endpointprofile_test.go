package controller

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

func epProfile(name string, layer int32, creds *metalv1alpha1.CredentialsPolicy) metalv1alpha1.EndpointProfile {
	return metalv1alpha1.EndpointProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       metalv1alpha1.EndpointProfileSpec{Layer: layer, Credentials: creds},
	}
}

func TestMergeEndpointProfiles(t *testing.T) {
	base := &metalv1alpha1.CredentialsPolicy{Username: "pk-metal"}
	other := &metalv1alpha1.CredentialsPolicy{Username: "metal-ops"}

	t.Run("empty", func(t *testing.T) {
		d, conflict := mergeEndpointProfiles(nil)
		assert.Empty(t, conflict)
		assert.Nil(t, d.credentials)
	})

	t.Run("later-layer-overrides", func(t *testing.T) {
		d, conflict := mergeEndpointProfiles([]metalv1alpha1.EndpointProfile{
			epProfile("base", 0, base),
			epProfile("override", 10, other),
		})
		require.Empty(t, conflict)
		assert.Equal(t, "metal-ops", d.credentials.Username)
	})

	t.Run("stanza-gaps-fall-through", func(t *testing.T) {
		d, conflict := mergeEndpointProfiles([]metalv1alpha1.EndpointProfile{
			epProfile("base", 0, base),
			epProfile("no-creds", 10, nil),
		})
		require.Empty(t, conflict)
		assert.Equal(t, "pk-metal", d.credentials.Username, "a profile without the stanza overrides nothing")
	})

	t.Run("same-layer-agreement", func(t *testing.T) {
		d, conflict := mergeEndpointProfiles([]metalv1alpha1.EndpointProfile{
			epProfile("a", 0, base),
			epProfile("b", 0, base.DeepCopy()),
		})
		require.Empty(t, conflict)
		assert.Equal(t, "pk-metal", d.credentials.Username)
	})

	t.Run("same-layer-conflict-holds-whole", func(t *testing.T) {
		d, conflict := mergeEndpointProfiles([]metalv1alpha1.EndpointProfile{
			epProfile("a", 0, base),
			epProfile("b", 0, other),
		})
		assert.Contains(t, conflict, "profiles a and b disagree on layer 0")
		assert.Nil(t, d.credentials, "a conflicted merge yields nothing")
	})

	t.Run("stanzas-merge-independently", func(t *testing.T) {
		withNTP := epProfile("time", 10, nil)
		withNTP.Spec.NTP = &metalv1alpha1.NTPPolicy{Servers: []string{"ntp1"}}
		d, conflict := mergeEndpointProfiles([]metalv1alpha1.EndpointProfile{
			epProfile("base", 0, base),
			withNTP,
		})
		require.Empty(t, conflict)
		assert.Equal(t, "pk-metal", d.credentials.Username)
		assert.Equal(t, []string{"ntp1"}, d.ntp.Servers)
	})

	t.Run("ntp-conflict-holds-whole", func(t *testing.T) {
		a, b := epProfile("a", 0, base), epProfile("b", 0, base.DeepCopy())
		a.Spec.NTP = &metalv1alpha1.NTPPolicy{Servers: []string{"ntp1"}}
		b.Spec.NTP = &metalv1alpha1.NTPPolicy{Servers: []string{"ntp2"}}
		d, conflict := mergeEndpointProfiles([]metalv1alpha1.EndpointProfile{a, b})
		assert.Contains(t, conflict, "ntp: profiles a and b disagree on layer 0")
		assert.Nil(t, d.credentials, "an agreed stanza is still held by the conflicted one")
	})
}

func TestRotationDue(t *testing.T) {
	maxAge := &metav1.Duration{Duration: time.Hour}
	stamped := func(at string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{rotatedAtAnnotation: at},
		}}
	}
	assert.True(t, rotationDue(stamped(time.Now().UTC().Add(-91*24*time.Hour).Format(time.RFC3339)), nil),
		"no cadence named: 90 days")
	assert.False(t, rotationDue(stamped(time.Now().UTC().Add(-89*24*time.Hour).Format(time.RFC3339)), nil),
		"no cadence named: 90 days")
	assert.False(t, rotationDue(stamped(time.Now().UTC().Format(time.RFC3339)), maxAge), "fresh stamp")
	assert.True(t, rotationDue(stamped(time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339)), maxAge), "outlived")
	assert.True(t, rotationDue(&corev1.Secret{}, maxAge), "missing stamp rotates — conservative")
	assert.True(t, rotationDue(stamped("not-a-time"), maxAge), "unparsable stamp rotates")
}

func TestRandomPassword(t *testing.T) {
	a, err := randomPassword()
	require.NoError(t, err)
	b, err := randomPassword()
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	for range 200 {
		pw, err := randomPassword()
		require.NoError(t, err)
		require.Len(t, pw, 16)
		var lower, upper, digit, special bool
		for _, r := range pw {
			switch {
			case r >= 'a' && r <= 'z':
				lower = true
			case r >= 'A' && r <= 'Z':
				upper = true
			case r >= '0' && r <= '9':
				digit = true
			case strings.ContainsRune(passwordSpecials, r):
				special = true
			default:
				t.Fatalf("%q: character %q is outside the safe set", pw, r)
			}
		}
		assert.True(t, lower && upper && digit && special, "%q: every class the strictest vendor rule asks for", pw)
	}
}

func TestDesiredIdentity(t *testing.T) {
	ep := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-1"},
		Spec:       metalv1alpha1.ManagementEndpointSpec{Address: "https://[2001:db8:0:100::14]:443"},
	}
	d := desiredIdentity(ep, &metalv1alpha1.IdentityPolicy{})
	assert.Equal(t, "bmc-ci-1.metal.internal", d.CommonName, "domain defaults")
	assert.Equal(t, []string{"bmc-ci-1.metal.internal"}, d.DNSNames)
	assert.Equal(t, []string{"2001:db8:0:100::14"}, d.IPAddresses, "an IP-literal address rides as SAN")

	ep.Spec.Address = "https://bmc-ci-1.metal.internal"
	d = desiredIdentity(ep, &metalv1alpha1.IdentityPolicy{Domain: "mgmt.metal.internal"})
	assert.Equal(t, "bmc-ci-1.mgmt.metal.internal", d.CommonName)
	assert.Empty(t, d.IPAddresses, "a hostname address adds no IP SAN")
}

func TestIdentityDrift(t *testing.T) {
	leaf := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "bmc-ci-1.metal.internal"},
		DNSNames:    []string{"bmc-ci-1.metal.internal"},
		IPAddresses: []net.IP{net.ParseIP("2001:db8:0:100::14")},
		NotAfter:    time.Now().Add(60 * 24 * time.Hour),
	}
	desired := identityDesired{
		CommonName:  "bmc-ci-1.metal.internal",
		DNSNames:    []string{"bmc-ci-1.metal.internal"},
		IPAddresses: []string{"2001:db8:0:100::14"},
	}
	assert.Empty(t, identityDrift(leaf, desired, 30*24*time.Hour), "in sync")
	assert.Contains(t, identityDrift(leaf, desired, 90*24*time.Hour), "renewal window",
		"lifetime inside renewBefore is drift — the renewal trigger")

	other := desired
	other.CommonName, other.DNSNames = "bmc-ci-2.metal.internal", []string{"bmc-ci-2.metal.internal"}
	assert.Contains(t, identityDrift(leaf, other, 0), "served CN")

	noIP := leaf
	noIP.IPAddresses = nil
	assert.Contains(t, identityDrift(noIP, desired, 0), "IP SAN")
}

func TestStampEndpointLabels(t *testing.T) {
	ep := &metalv1alpha1.ManagementEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ci-1",
			Labels: map[string]string{"team": "hpc"},
		},
		Status: metalv1alpha1.ManagementEndpointStatus{
			Vendor: "Huawei",
			Model:  "2288H V5",
			Asset:  &metalv1alpha1.AssetSnapshot{VLAN: 100},
		},
	}
	require.True(t, stampEndpointLabels(ep))
	assert.Equal(t, "Huawei", ep.Labels[labelVendor])
	assert.Equal(t, "2288H-V5", ep.Labels[labelModel], "discovered facts sanitize into label values")
	assert.Equal(t, "100", ep.Labels[labelVLAN])
	assert.Equal(t, "hpc", ep.Labels["team"], "human labels survive")

	assert.False(t, stampEndpointLabels(ep), "stamping is idempotent")

	// A fact withdrawn (endpoint re-materialized against an asset without a
	// vlan) clears exactly the managed key.
	ep.Status.Asset = nil
	require.True(t, stampEndpointLabels(ep))
	_, present := ep.Labels[labelVLAN]
	assert.False(t, present)
	assert.Equal(t, "hpc", ep.Labels["team"])
}
