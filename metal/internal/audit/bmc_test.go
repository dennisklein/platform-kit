package audit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBMCEvents pins the fields of the lines about the operator's own
// changes to a BMC. Each line names the endpoint and the operator, and the
// fields of its event; no line carries a password or key material.
func TestBMCEvents(t *testing.T) {
	ep := Object{Kind: KindManagementEndpoint, Name: "bmc-1", UID: "u-3"}
	for _, tc := range []struct {
		name  string
		write func(l *Logger)
		want  map[string]any
	}{
		{"account set", func(l *Logger) { l.BMCAccountSet(ep, "pk-metal", "Rotated") },
			map[string]any{"event": "bmc.account.set", "account": "pk-metal", "reason": "Rotated"}},
		{"account disabled", func(l *Logger) { l.BMCAccountDisabled(ep, "pk-old", "Replaced") },
			map[string]any{"event": "bmc.account.disabled", "account": "pk-old", "reason": "Replaced"}},
		{"identity pushed", func(l *Logger) { l.BMCIdentityPushed(ep, "0a1b", "bmc-1.metal.internal") },
			map[string]any{"event": "bmc.identity.pushed", "key": "0a1b", "message": "bmc-1.metal.internal"}},
		{"restarted", func(l *Logger) { l.BMCRestarted(ep, "GracefulRestart", "IdentityPushed") },
			map[string]any{"event": "bmc.restarted", "action": "GracefulRestart", "reason": "IdentityPushed"}},
		{"key pinned", func(l *Logger) { l.BMCKeyPinned(ep, "sha256:00", "FirstUse") },
			map[string]any{"event": "bmc.tls.pinned", "key": "sha256:00", "reason": "FirstUse"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, b := newTest()
			tc.write(l)
			var line map[string]any
			require.NoError(t, json.Unmarshal(b.Bytes(), &line))
			assert.Equal(t, "ManagementEndpoint", line["kind"])
			assert.Equal(t, "bmc-1", line["name"])
			assert.Equal(t, "u-3", line["uid"])
			assert.Equal(t, l.Operator(), line["actor"])
			for k, v := range tc.want {
				assert.Equal(t, v, line[k], k)
			}
			for _, k := range []string{"password", "token"} {
				assert.NotContains(t, line, k)
			}
		})
	}
}
