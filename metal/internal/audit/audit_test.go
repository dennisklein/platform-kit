package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	op = Object{Kind: KindHostOperation, Namespace: "pk-metal", Name: "reboot-node-1", UID: "u-1"}
	ro = Object{Kind: KindTalosRollout, Name: "fleet-talosupgrade-1a2b3c4d", UID: "u-2"}
	at = time.Date(2026, 3, 4, 5, 6, 7, 800, time.UTC)
)

func newTest() (*Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return New(&b, "system:serviceaccount:pk-metal:pk-metal-manager").WithClock(func() time.Time { return at }), &b
}

// TestSchema pins every event's line, field for field. A change here is a
// change of the trail's schema, which sites parse: it needs a note in the
// documentation (docs: reference/audit-trail), not only a new expectation.
func TestSchema(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(l *Logger)
		want  string
	}{
		{
			name:  "proposed",
			write: func(l *Logger) { l.Proposed(op, "alice", "host-1", "talosReboot, waitNodeReady", true) },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"proposed","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1","actor":"alice","proposedBy":"alice",` +
				`"target":"host-1","plan":"talosReboot, waitNodeReady","approvalRequired":true}`,
		},
		{
			name:  "proposed, no approval needed",
			write: func(l *Logger) { l.Proposed(op, "op", "host-1", "powerOff", false) },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"proposed","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1","actor":"op","proposedBy":"op",` +
				`"target":"host-1","plan":"powerOff","approvalRequired":false}`,
		},
		{
			name:  "approved by another person",
			write: func(l *Logger) { l.Approved(op, "alice", "bob", "") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"approved","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1","actor":"bob","proposedBy":"alice",` +
				`"approvedBy":"bob","selfApproved":false}`,
		},
		{
			name:  "self-approved rollout",
			write: func(l *Logger) { l.Approved(ro, "alice", "alice", "sha256:abc") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"approved","kind":"TalosRollout",` +
				`"namespace":"","name":"fleet-talosupgrade-1a2b3c4d","uid":"u-2","actor":"alice","proposedBy":"alice",` +
				`"approvedBy":"alice","selfApproved":true,"planHash":"sha256:abc"}`,
		},
		{
			name: "guard lifted on the operation, at the start",
			write: func(l *Logger) {
				l.GuardLifted(op, "metal.pk.io/ignore-window", "alice: CVE fix", "")
			},
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"guard.lifted","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1","actor":"alice",` +
				`"annotation":"metal.pk.io/ignore-window","value":"alice: CVE fix","named":true}`,
		},
		{
			name: "guard lifted on another object, naming nobody",
			write: func(l *Logger) {
				l.GuardLifted(Object{Kind: KindHostClaim, Namespace: "default", Name: "node-2", UID: "u-3"},
					"metal.pk.io/ack-etcd-risk", "disk swap", "pk-metal/release-node-2")
			},
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"guard.lifted","kind":"HostClaim",` +
				`"namespace":"default","name":"node-2","uid":"u-3","actor":"",` +
				`"annotation":"metal.pk.io/ack-etcd-risk","value":"disk swap","named":false,` +
				`"operation":"pk-metal/release-node-2"}`,
		},
		{
			name: "guard lifted at a step",
			write: func(l *Logger) {
				l.GuardLiftedAtStep(op, "metal.pk.io/override-cluster-guard", "bob: single control plane", 2, "talosReboot")
			},
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"guard.lifted","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1","actor":"bob",` +
				`"annotation":"metal.pk.io/override-cluster-guard","value":"bob: single control plane","named":true,` +
				`"step":2,"action":"talosReboot"}`,
		},
		{
			name:  "step started",
			write: func(l *Logger) { l.StepStarted(op, 0, "cordon") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"step.started","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","step":0,"action":"cordon"}`,
		},
		{
			name:  "step finished",
			write: func(l *Logger) { l.StepFinished(op, 1, "drain", "Succeeded") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"step.finished","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","step":1,"action":"drain","result":"Succeeded"}`,
		},
		{
			name:  "step failed",
			write: func(l *Logger) { l.StepFailed(op, 2, "talosReboot", "StepFailed", "node did not come back") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"step.failed","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","step":2,"action":"talosReboot",` +
				`"result":"Failed","reason":"StepFailed","message":"node did not come back"}`,
		},
		{
			name:  "finished",
			write: func(l *Logger) { l.Finished(op, "Succeeded", "StepsSucceeded", 95*time.Second+400*time.Millisecond) },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"finished","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","reason":"StepsSucceeded",` +
				`"phase":"Succeeded","durationSeconds":95}`,
		},
		{
			name:  "finished without a start",
			write: func(l *Logger) { l.Finished(op, "Aborted", "Aborted", 0) },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"finished","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","reason":"Aborted",` +
				`"phase":"Aborted","durationSeconds":0}`,
		},
		{
			name:  "deleted",
			write: func(l *Logger) { l.Deleted(op, DeleteRetentionExpired, "finished 720h0m0s ago") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"deleted","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","reason":"RetentionExpired",` +
				`"message":"finished 720h0m0s ago"}`,
		},
		{
			name:  "media fetched",
			write: func(l *Logger) { l.MediaFetched(op, "node-1-config.iso", "192.0.2.10") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"media.fetched","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","grant":"node-1-config.iso","source":"192.0.2.10"}`,
		},
		{
			name:  "media refused",
			write: func(l *Logger) { l.MediaRefused(op, "node-1-config.iso", "198.51.100.7", "wrong credentials") },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"media.refused","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","reason":"wrong credentials",` +
				`"grant":"node-1-config.iso","source":"198.51.100.7"}`,
		},
		{
			name:  "media grant ended, never fetched",
			write: func(l *Logger) { l.MediaGrantEnded(op, "node-1-config.iso", "", "expired", 0, 0, 0) },
			want: `{"ts":"2026-03-04T05:06:07.0000008Z","logger":"audit","event":"media.grantEnded","kind":"HostOperation",` +
				`"namespace":"pk-metal","name":"reboot-node-1","uid":"u-1",` +
				`"actor":"system:serviceaccount:pk-metal:pk-metal-manager","reason":"expired",` +
				`"grant":"node-1-config.iso","requests":0,"bytes":0,"refused":0}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, b := newTest()
			tc.write(l)
			assert.Equal(t, tc.want+"\n", b.String())
		})
	}
}

// TestCommonFields: every line of every event carries the fixed fields, and
// is one line of valid JSON.
func TestCommonFields(t *testing.T) {
	l, b := newTest()
	l.Proposed(op, "a", "h", "p", true)
	l.Approved(op, "a", "b", "")
	l.GuardLifted(op, "k", "a: v", "")
	l.GuardLiftedAtStep(op, "k", "a: v", 1, "x")
	l.StepStarted(op, 0, "x")
	l.StepFinished(op, 0, "x", "Succeeded")
	l.StepFailed(op, 0, "x", "r", "m")
	l.Finished(op, "Failed", "r", time.Second)
	l.Deleted(op, DeleteWithdrawn, "")
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	require.Len(t, lines, 9, "one line per call")
	for _, line := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		for _, k := range []string{"ts", "logger", "event", "kind", "namespace", "name", "uid", "actor"} {
			assert.Contains(t, m, k, line)
		}
		assert.Equal(t, LoggerName, m["logger"])
	}
}

func TestNilLoggerWritesNothing(t *testing.T) {
	var l *Logger
	assert.NotPanics(t, func() {
		l.Proposed(op, "a", "h", "p", true)
		l.Deleted(op, DeleteWithdrawn, "")
	})
	assert.Empty(t, l.Operator())
}

func TestMessageIsClipped(t *testing.T) {
	l, b := newTest()
	l.StepFailed(op, 0, "x", "r", strings.Repeat("é", maxMessage))
	var got Line
	require.NoError(t, json.Unmarshal(b.Bytes(), &got))
	assert.LessOrEqual(t, len(got.Message), maxMessage+len("…"))
	assert.True(t, strings.HasSuffix(got.Message, "…"))
	assert.NotContains(t, got.Message, "�", "the cut falls between two characters")
}

// TestMessageIsMasked: a step's error that echoes a grant URL with its
// credentials (the Huawei form) reaches the trail masked.
func TestMessageIsMasked(t *testing.T) {
	l, b := newTest()
	l.StepFailed(op, 1, "insertMedia", "StepFailed",
		"bmc: VmmControl Connect: 400: Image https://media:fedcba9876543210@10.0.0.1:30443/grants/0123456789abcdef/n.iso")
	var got Line
	require.NoError(t, json.Unmarshal(b.Bytes(), &got))
	assert.Equal(t, "bmc: VmmControl Connect: 400: Image https://***@10.0.0.1:30443/grants/***/n.iso", got.Message)
}
