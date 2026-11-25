package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/audit"
)

const testOperator = "system:serviceaccount:pk-metal:pk-metal-manager"

// trail reads back the lines an audit.Logger wrote.
func trail(t *testing.T, b *bytes.Buffer) []audit.Line {
	t.Helper()
	var out []audit.Line
	for _, raw := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if raw == "" {
			continue
		}
		var l audit.Line
		require.NoError(t, json.Unmarshal([]byte(raw), &l), raw)
		out = append(out, l)
	}
	return out
}

func eventNames(lines []audit.Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l.Event)
		if l.Annotation != "" {
			out[i] += " " + l.Annotation
		}
		if l.Step != nil {
			out[i] += " " + l.Action
		}
	}
	return out
}

// An operation a person proposed, approved and lifted the window for gets
// one line per decision, in order, however often it is reconciled, and a
// status write the API server refuses writes no line.
func TestOperationAuditTrail(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Spec.ProposedBy = "alice"
	op.Spec.Approval = metalv1alpha1.OperationApproval{Required: ptr.To(true)}
	op.UID = "op-uid"
	r, _, _ := executorFor(t, "huawei-2288h-v5", op)
	var b bytes.Buffer
	r.Audit = audit.New(&b, testOperator)
	ctx := t.Context()
	key := client.ObjectKeyFromObject(op)
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
	host.Annotations = map[string]string{metalv1alpha1.AnnotationAllowSelfDisruption: "true"}
	require.NoError(t, r.Update(ctx, host))

	for range 3 {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}
	require.Equal(t, []string{"proposed"}, eventNames(trail(t, &b)), "waiting for approval is one decision, not one per pass")

	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(ctx, key, got))
	got.Spec.Approval.ApprovedBy = "alice"
	got.Annotations = map[string]string{metalv1alpha1.AnnotationIgnoreWindow: "alice: CVE fix"}
	require.NoError(t, r.Update(ctx, got))

	// The first write of the approval meets a conflict: that pass writes
	// no line, the next writes it once.
	api := &conflictOnce{Client: r.Client, match: func(o *metalv1alpha1.HostOperation) bool {
		return meta.IsStatusConditionTrue(o.Status.Conditions, metalv1alpha1.ConditionApproved)
	}}
	r.Client = api
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.True(t, apierrors.IsConflict(err), "precondition: the approval's write met the conflict: %v", err)
	require.Len(t, trail(t, &b), 1, "a refused write writes no line")

	done := runOp(t, r)
	require.True(t, opSucceeded(done), "%+v", done.Status)
	for range 3 {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
	}

	lines := trail(t, &b)
	assert.Equal(t, []string{
		"proposed",
		"approved",
		"guard.lifted metal.pk.io/ignore-window",
		"guard.lifted metal.pk.io/allow-self-disruption",
		"step.started powerCycle",
		"step.finished powerCycle",
		"finished",
	}, eventNames(lines))

	byEvent := map[audit.Event]audit.Line{}
	for _, l := range lines {
		if _, seen := byEvent[l.Event]; !seen {
			byEvent[l.Event] = l
		}
		if l.Kind == audit.KindHostOperation {
			assert.Equal(t, "op-uid", l.UID)
		}
	}
	p := byEvent[audit.EventProposed]
	assert.Equal(t, "alice", p.Actor)
	assert.Equal(t, "host-a", p.Target)
	assert.Equal(t, "powerCycle", p.Plan)
	assert.Equal(t, ptr.To(true), p.ApprovalRequired)
	a := byEvent[audit.EventApproved]
	assert.Equal(t, "alice", a.ApprovedBy)
	assert.Equal(t, ptr.To(true), a.SelfApproved, "the proposer approved")
	g := byEvent[audit.EventGuardLifted]
	assert.Equal(t, "alice", g.Actor, "the value names who lifted the guard")
	assert.Equal(t, "alice: CVE fix", g.Value)
	assert.Equal(t, ptr.To(true), g.Named)
	assert.Equal(t, audit.KindHost, lines[3].Kind)
	assert.Equal(t, "pk-metal/op", lines[3].Operation)
	assert.Equal(t, ptr.To(false), lines[3].Named, "allow-self-disruption takes any value")
	f := byEvent[audit.EventFinished]
	assert.Equal(t, "Succeeded", f.Phase)
	assert.Equal(t, testOperator, f.Actor)
	require.NotNil(t, f.DurationSeconds)
}

// A failed step names its reason and message, and the operation's end
// follows it.
func TestOperationAuditFailedStep(t *testing.T) {
	var b bytes.Buffer
	r := &HostOperationReconciler{Audit: audit.New(&b, testOperator)}
	before := preClearedOp(
		metalv1alpha1.HostOperationAction{Cordon: &metalv1alpha1.CordonAction{NodeRef: metalv1alpha1.Reference{Name: "n"}}},
		metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: metalv1alpha1.Reference{Name: "n"}}},
	)
	before.Annotations = map[string]string{metalv1alpha1.AnnotationOverrideClusterGuard: "bob: etcd is down already"}
	now := metav1.Now()
	before.Status = metalv1alpha1.HostOperationStatus{
		Baseline: &metalv1alpha1.OperationBaseline{CapturedAt: ptr.To(metav1.NewTime(now.Add(-time.Minute)))},
		Conditions: []metav1.Condition{
			{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonApprovalNotRequired},
			{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting},
		},
	}
	mid := before.DeepCopy()
	mid.Status.Steps = []metalv1alpha1.OperationStep{{Action: "cordon", Phase: "Succeeded"}}
	r.auditOperation(t.Context(), before, mid, nil)

	after := mid.DeepCopy()
	after.Status.Steps = append(after.Status.Steps, metalv1alpha1.OperationStep{Action: "talosReboot", Phase: "Failed", Message: "node did not come back"})
	meta.SetStatusCondition(&after.Status.Conditions, metav1.Condition{Type: metalv1alpha1.ConditionExecuted,
		Status: metav1.ConditionFalse, Reason: metalv1alpha1.ReasonStepFailed, LastTransitionTime: now})
	r.auditOperation(t.Context(), mid, after, nil)

	lines := trail(t, &b)
	assert.Equal(t, []string{
		"step.finished cordon",
		"step.started talosReboot",
		"guard.lifted metal.pk.io/override-cluster-guard talosReboot",
		"step.failed talosReboot",
		"finished",
	}, eventNames(lines), "the reboot is a step the guard decides again")
	require.Len(t, lines, 5)
	assert.Equal(t, ptr.To(1), lines[2].Step)
	assert.Equal(t, "bob", lines[2].Actor)
	assert.Equal(t, metalv1alpha1.ReasonStepFailed, lines[3].Reason)
	assert.Equal(t, "node did not come back", lines[3].Message)
	assert.Equal(t, "Failed", lines[4].Phase)
	assert.Equal(t, ptr.To(60.0), lines[4].DurationSeconds)
}

func rolloutFixture() *osv1alpha1.TalosRollout {
	return &osv1alpha1.TalosRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-talosupgrade-1", UID: "ro-uid"},
		Spec: osv1alpha1.TalosRolloutSpec{
			ClusterRef: osv1alpha1.Reference{Name: "fleet"}, Kind: osv1alpha1.RolloutTalosUpgrade,
			Target:     &osv1alpha1.RolloutTarget{TalosVersion: "v1.12.1"},
			ProposedBy: testOperator,
		},
	}
}

func TestRolloutAuditTrail(t *testing.T) {
	var b bytes.Buffer
	l := audit.New(&b, testOperator)
	empty := rolloutFixture()
	planned := empty.DeepCopy()
	planned.Status.Plan = []osv1alpha1.RolloutStep{
		{Kind: osv1alpha1.RolloutStepNode, Node: "cp-1"}, {Kind: osv1alpha1.RolloutStepNode, Node: "w-1"}}
	planned.Status.PlanHash = "sha256:abc"
	planned.Status.Steps = []osv1alpha1.RolloutStepStatus{{State: osv1alpha1.RolloutNodePending}, {State: osv1alpha1.RolloutNodePending}}
	planned.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionApproved, Status: metav1.ConditionFalse,
		Reason: osv1alpha1.ReasonAwaitingApproval}}
	auditRollout(l, empty, planned)
	auditRollout(l, planned, planned.DeepCopy())

	approved := planned.DeepCopy()
	approved.Spec.Approval = osv1alpha1.RolloutApproval{ApprovedBy: "bob", PlanHash: "sha256:abc"}
	meta.SetStatusCondition(&approved.Status.Conditions, metav1.Condition{Type: osv1alpha1.ConditionApproved,
		Status: metav1.ConditionTrue, Reason: osv1alpha1.ReasonPlanApproved})
	started := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	approved.Status.Steps[0] = osv1alpha1.RolloutStepStatus{State: osv1alpha1.RolloutNodeRunning, StartedAt: &started}
	auditRollout(l, planned, approved)

	ended := approved.DeepCopy()
	ended.Status.Steps[0].State = osv1alpha1.RolloutNodeFailed
	ended.Status.Steps[0].Message = "operation failed"
	ended.Status.Steps[1].State = osv1alpha1.RolloutNodeSkipped
	meta.SetStatusCondition(&ended.Status.Conditions, metav1.Condition{Type: osv1alpha1.ConditionComplete,
		Status: metav1.ConditionFalse, Reason: osv1alpha1.ReasonRolloutFailed})
	auditRollout(l, approved, ended)
	auditRollout(l, ended, ended.DeepCopy())

	lines := trail(t, &b)
	assert.Equal(t, []string{"proposed", "approved", "step.started cp-1", "step.failed cp-1", "finished"}, eventNames(lines))
	assert.Equal(t, "TalosUpgrade to v1.12.1, 2 steps: cp-1, w-1", lines[0].Plan)
	assert.Equal(t, "fleet", lines[0].Target)
	assert.Equal(t, ptr.To(false), lines[1].SelfApproved)
	assert.Equal(t, "sha256:abc", lines[1].PlanHash)
	assert.Equal(t, "Failed", lines[4].Phase)
	assert.Equal(t, osv1alpha1.ReasonRolloutFailed, lines[4].Reason)
}

func TestParseRetention(t *testing.T) {
	for v, want := range map[string]retention{
		"Forever": {forever: true},
		"30d":     {keep: 30 * 24 * time.Hour},
		"106751d": {keep: 106751 * 24 * time.Hour},
		"720h":    {keep: 720 * time.Hour},
		" 90m ":   {keep: 90 * time.Minute},
	} {
		assert.Equal(t, want, parseRetention(v), v)
	}
	// Zero would delete each record as it finishes; a number of days past
	// what a duration holds would wrap around and delete at once.
	for _, v := range []string{"forever", "-1h", "a month", "1.5d", "", "0", "0s", "0d", "-3d",
		"106752d", "999999d", "99999999999999999999d"} {
		got := parseRetention(v)
		assert.True(t, got.forever, "%q keeps the records", v)
		assert.Contains(t, got.problem, "is not Forever", v)
	}
}

func settings(retention *string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: siteSettingsConfigMap},
		Data: map[string]string{"selfApproval": "Allowed"}}
	if retention != nil {
		cm.Data[retentionKey] = *retention
	}
	return cm
}

// finishedOp is an operation that succeeded ago, with a TTL of ttl.
func finishedOp(ago time.Duration, ttl int32) *metalv1alpha1.HostOperation {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Spec.TTLSecondsAfterFinished = ptr.To(ttl)
	op.Status.Conditions = []metav1.Condition{{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionTrue,
		Reason: metalv1alpha1.ReasonStepsSucceeded, LastTransitionTime: metav1.NewTime(time.Now().Add(-ago))}}
	return op
}

// The operator deletes a finished operation only after the site's
// retention, whatever its own TTL says, and writes the line first.
func TestOperationRetention(t *testing.T) {
	for name, tc := range map[string]struct {
		retention *string
		ago       time.Duration
		ttl       int32
		gone      bool
	}{
		"Forever keeps it":                                 {retention: ptr.To("Forever"), ago: 400 * 24 * time.Hour, gone: false},
		"a TTL shorter than the retention waits":           {retention: ptr.To("7d"), ago: 2 * 24 * time.Hour, ttl: 60, gone: false},
		"past the retention it goes":                       {retention: ptr.To("7d"), ago: 8 * 24 * time.Hour, ttl: 60, gone: true},
		"a TTL longer than the retention decides":          {retention: ptr.To("1h"), ago: 2 * time.Hour, ttl: 86400, gone: false},
		"the default keeps it for 30 days":                 {ago: 29 * 24 * time.Hour, gone: false},
		"the default lets it go after 30 days":             {ago: 31 * 24 * time.Hour, gone: true},
		"a mistyped setting keeps it":                      {retention: ptr.To("a month"), ago: 400 * 24 * time.Hour, gone: false},
		"a short one leaves it to the operation's own TTL": {retention: ptr.To("1s"), ago: 2 * time.Minute, ttl: 60, gone: true},
		"zero is refused and keeps it":                     {retention: ptr.To("0s"), ago: 2 * time.Minute, ttl: 60, gone: false},
		"an overflowing number of days keeps it":           {retention: ptr.To("106752d"), ago: 2 * time.Minute, ttl: 60, gone: false},
	} {
		t.Run(name, func(t *testing.T) {
			op := finishedOp(tc.ago, tc.ttl)
			r, _, _ := executorFor(t, "huawei-2288h-v5", op, settings(tc.retention))
			r.CredentialsNamespace = "pk-metal"
			var b bytes.Buffer
			r.Audit = audit.New(&b, testOperator)
			res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)})
			require.NoError(t, err)
			err = r.Get(t.Context(), client.ObjectKeyFromObject(op), &metalv1alpha1.HostOperation{})
			lines := trail(t, &b)
			if !tc.gone {
				require.NoError(t, err, "the record stays")
				assert.Empty(t, lines)
				assert.Positive(t, res.RequeueAfter, "a changed setting is read again")
				assert.LessOrEqual(t, res.RequeueAfter, settingsRecheck)
				return
			}
			assert.True(t, apierrors.IsNotFound(err), "the record goes: %v", err)
			require.Len(t, lines, 1)
			assert.Equal(t, audit.EventDeleted, lines[0].Event)
			assert.Equal(t, string(audit.DeleteRetentionExpired), lines[0].Reason)
			assert.Equal(t, testOperator, lines[0].Actor)
		})
	}
}

// A succeeded rollout goes after the site's retention, its operations
// with it, each with its line first; a failed one stays.
func TestRolloutRetention(t *testing.T) {
	for name, tc := range map[string]struct {
		retention *string
		status    metav1.ConditionStatus
		gone      bool
	}{
		"Forever keeps a succeeded rollout": {retention: ptr.To("Forever"), status: metav1.ConditionTrue},
		"a duration lets it go":             {retention: ptr.To("24h"), status: metav1.ConditionTrue, gone: true},
		"a failed rollout stays":            {retention: ptr.To("24h"), status: metav1.ConditionFalse},
	} {
		t.Run(name, func(t *testing.T) {
			ro := rolloutFixture()
			ro.Status.Conditions = []metav1.Condition{{Type: osv1alpha1.ConditionComplete, Status: tc.status,
				Reason: osv1alpha1.ReasonRolloutSucceeded, LastTransitionTime: metav1.NewTime(time.Now().Add(-48 * time.Hour))}}
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
			op.Labels = map[string]string{osv1alpha1.LabelRollout: ro.Name}
			op.OwnerReferences = []metav1.OwnerReference{{APIVersion: osv1alpha1.GroupVersion.String(), Kind: "TalosRollout",
				Name: ro.Name, UID: ro.UID, Controller: ptr.To(true)}}
			cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithStatusSubresource(&osv1alpha1.TalosRollout{}).
				WithObjects(ro, op, settings(tc.retention)).Build()
			var b bytes.Buffer
			r := &TalosRolloutReconciler{Client: cl, Namespace: "pk-metal", OperationsNamespace: "pk-metal",
				Audit: audit.New(&b, testOperator)}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ro)})
			require.NoError(t, err)
			err = cl.Get(t.Context(), client.ObjectKeyFromObject(ro), &osv1alpha1.TalosRollout{})
			lines := trail(t, &b)
			if !tc.gone {
				require.NoError(t, err)
				assert.Empty(t, lines)
				return
			}
			assert.True(t, apierrors.IsNotFound(err), "%v", err)
			require.Len(t, lines, 2)
			assert.Equal(t, audit.KindHostOperation, lines[0].Kind)
			assert.Equal(t, string(audit.DeleteOwnerDeleted), lines[0].Reason)
			assert.Equal(t, audit.KindTalosRollout, lines[1].Kind)
			assert.Equal(t, string(audit.DeleteRetentionExpired), lines[1].Reason)
		})
	}
}

// Every place the operator withdraws a proposal writes the line before the
// delete: here, a boot proposal a newer plan replaces.
func TestWithdrawWritesTheLineFirst(t *testing.T) {
	stale := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	stale.Name = "boot-old"
	stale.Spec.Approval.Required = ptr.To(true)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(stale).Build()
	var b bytes.Buffer
	order := &deleteOrder{Client: cl, b: &b}
	r := &HostClaimReconciler{Client: order, Audit: audit.New(&b, testOperator)}
	require.NoError(t, r.withdraw(t.Context(), []metalv1alpha1.HostOperation{*stale}, "boot-new"))
	require.True(t, order.deleted)
	assert.Equal(t, 1, order.linesBefore, "the line is in the trail when the delete is sent")
	lines := trail(t, &b)
	require.Len(t, lines, 1)
	assert.Equal(t, string(audit.DeleteWithdrawn), lines[0].Reason)
}

// deleteOrder counts the lines in the trail at the moment of a delete.
type deleteOrder struct {
	client.Client
	b           *bytes.Buffer
	deleted     bool
	linesBefore int
}

func (d *deleteOrder) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	d.deleted = true
	d.linesBefore = strings.Count(d.b.String(), "\n")
	return d.Client.Delete(ctx, obj, opts...)
}

// TestMediaFetchesReachTheTrail: a BMC's fetch of a granted medium, a
// refused copy of it and the grant's end are lines of the trail, under the
// operation that minted the grant, and none names the token.
func TestMediaFetchesReachTheTrail(t *testing.T) {
	var b bytes.Buffer
	table := asset.NewGrants()
	table.Record = mediaAuditor(audit.New(&b, testOperator))
	key := client.ObjectKey{Namespace: "pk-metal", Name: "provision-host-1"}
	g, err := table.Issue(grantHandle(key), "node-1-config.iso", []byte("config"), time.Hour, nil)
	require.NoError(t, err)
	srv := &asset.Server{Grants: table}
	for _, from := range []string{"192.0.2.10:1000", "198.51.100.7:2000"} {
		req := httptest.NewRequest(http.MethodGet, "https://media"+g.Path(), nil)
		req.RemoteAddr = from
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}
	r := &HostOperationReconciler{Media: &asset.Service{Grants: table}}
	r.revokeGrants(key)

	lines := trail(t, &b)
	require.Len(t, lines, 3)
	for i, ev := range []audit.Event{audit.EventMediaFetched, audit.EventMediaRefused, audit.EventMediaGrantEnded} {
		assert.Equal(t, ev, lines[i].Event)
		assert.Equal(t, audit.KindHostOperation, lines[i].Kind)
		assert.Equal(t, "pk-metal", lines[i].Namespace)
		assert.Equal(t, "provision-host-1", lines[i].Name)
		assert.Equal(t, "node-1-config.iso", lines[i].Grant)
	}
	assert.Equal(t, "192.0.2.10", lines[0].Source)
	assert.Equal(t, "198.51.100.7", lines[1].Source)
	require.NotNil(t, lines[2].Bytes)
	assert.Equal(t, int64(len("config")), *lines[2].Bytes)
	assert.Equal(t, "revoked", lines[2].Reason)
	assert.NotContains(t, b.String(), g.Token)
}
