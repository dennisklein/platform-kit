package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGuardLiftValue(t *testing.T) {
	assert.Equal(t, "alice: etcd is down already", GuardLiftValue("alice", " etcd is down already "))
	assert.Equal(t, "alice: once", GuardLiftValue("alice", "alice: once"), "a named value is not stamped twice")
	assert.Equal(t, "system:serviceaccount:ci:runner: nightly",
		GuardLiftValue("system:serviceaccount:ci:runner", "nightly"))
}

func TestParseGuardLift(t *testing.T) {
	for _, tc := range []struct {
		value, user, why string
		ok               bool
	}{
		{value: "alice: CVE fix", user: "alice", why: "CVE fix", ok: true},
		{value: "system:serviceaccount:ci:runner: nightly", user: "system:serviceaccount:ci:runner", why: "nightly", ok: true},
		{value: "alice:  ", why: "alice:", ok: false},
		{value: "disk swap", why: "disk swap", ok: false},
		{value: ": no user", why: ": no user", ok: false},
		{value: "", ok: false},
	} {
		user, why, ok := ParseGuardLift(tc.value)
		assert.Equal(t, tc.ok, ok, tc.value)
		assert.Equal(t, tc.user, user, tc.value)
		assert.Equal(t, tc.why, why, tc.value)
	}
}
