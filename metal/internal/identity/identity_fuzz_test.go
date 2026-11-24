package identity_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/identity"
)

// FuzzMAC checks that MAC has one output form and that every accepted
// spelling of that form normalises back to it.
func FuzzMAC(f *testing.F) {
	for _, seed := range []string{
		"0c:c4:7a:12:34:56", "0C-C4-7A-12-34-56", "0cc4.7a12.3456", " 0c:c4:7a:12:34:56 ",
		"00:00:5e:00:53:01:02:03", "0c:c4:7a:12:34", "", "zz:zz:zz:zz:zz:zz",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, ok := identity.MAC(s)
		assert.Equal(t, identity.SameMAC(s, out), identity.SameMAC(out, s), "SameMAC is not symmetric")
		if !ok {
			return
		}
		again, ok2 := identity.MAC(out)
		assert.True(t, ok2)
		assert.Equal(t, out, again, "MAC is not idempotent")
		assert.Len(t, out, 17)
		assert.Equal(t, strings.ToLower(out), out)

		hex := strings.ReplaceAll(out, ":", "")
		for _, form := range []string{
			strings.ToUpper(out),
			strings.ReplaceAll(out, ":", "-"),
			strings.ToUpper(strings.ReplaceAll(out, ":", "-")),
			hex[0:4] + "." + hex[4:8] + "." + hex[8:12],
		} {
			got, ok := identity.MAC(form)
			assert.True(t, ok, "form %q", form)
			assert.Equal(t, out, got, "form %q", form)
			assert.True(t, identity.SameMAC(form, s), "form %q", form)
		}
	})
}

// FuzzHostName checks that every Host name is a valid object name and that
// Name is idempotent on its own output.
func FuzzHostName(f *testing.F) {
	for _, seed := range []string{
		"SIMHW2288V5000391", "AB_12.C/", "", "---", "Ünïcødé-42",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, serial string) {
		name, ok := identity.Name(metalv1alpha1.HostIdentity{Serial: serial})
		if !ok {
			assert.Empty(t, name)
			return
		}
		assert.LessOrEqual(t, len(name), validation.DNS1123SubdomainMaxLength)
		assert.Empty(t, validation.IsDNS1123Subdomain(name), "name %q", name)
		again, ok := identity.Name(metalv1alpha1.HostIdentity{Serial: name})
		require.True(t, ok)
		assert.Equal(t, name, again, "Name is not idempotent")
	})
}
