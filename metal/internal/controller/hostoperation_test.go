package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	pkscheme "github.com/dennisklein/platform-kit/metal/internal/scheme"
)

// recordingClient counts the mutating calls the executor issues, over the
// in-memory sim.
type recordingClient struct {
	bmc.Client
	overrides int
	powers    []bmc.PowerAction
	// powerErr, when set, is what every power read answers.
	powerErr error
	// ignoreGraceful takes GracefulShutdown and does nothing — an OS that
	// does not react to the power button.
	ignoreGraceful bool
	// inserts and ejects name the image URLs and slots the executor mounted
	// and emptied, in order.
	inserts []string
	ejects  []string
	// insertErr, when set, is what every InsertMedia answers.
	insertErr error
	// onInsert, when set, sees each InsertMedia with its credentials.
	onInsert func(imageURL string, creds *bmc.MediaCredentials)
}

func (c *recordingClient) InsertMedia(ctx context.Context, id, imageURL string, creds *bmc.MediaCredentials) error {
	c.inserts = append(c.inserts, imageURL)
	if c.onInsert != nil {
		c.onInsert(imageURL, creds)
	}
	if c.insertErr != nil {
		return c.insertErr
	}
	return c.Client.InsertMedia(ctx, id, imageURL, creds)
}

func (c *recordingClient) EjectMedia(ctx context.Context, id string) error {
	c.ejects = append(c.ejects, id)
	return c.Client.EjectMedia(ctx, id)
}

func (c *recordingClient) Power(ctx context.Context, id string) (bmc.PowerState, error) {
	if c.powerErr != nil {
		return "", c.powerErr
	}
	return c.Client.Power(ctx, id)
}

func (c *recordingClient) SetBootOverride(ctx context.Context, id string, o bmc.BootOverride) error {
	c.overrides++
	return c.Client.SetBootOverride(ctx, id, o)
}

func (c *recordingClient) SetPower(ctx context.Context, id string, a bmc.PowerAction) error {
	c.powers = append(c.powers, a)
	if a == bmc.ActionGracefulShutdown && c.ignoreGraceful {
		return nil
	}
	return c.Client.SetPower(ctx, id, a)
}

func (c *recordingClient) Close() error { return nil }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s, err := pkscheme.New()
	require.NoError(t, err)
	return s
}

// executorFor builds an executor over a fake API and the named sim fixture.
func executorFor(t *testing.T, fixture string, objs ...client.Object) (*HostOperationReconciler, *sim.State, *recordingClient) {
	t.Helper()
	f, err := sim.LoadFixture(fixture)
	require.NoError(t, err)
	st := sim.NewState(f)
	rec := &recordingClient{Client: st}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}).
		WithIndex(&metalv1alpha1.HostOperation{}, opHostIndex, func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.HostOperation).Spec.HostRef.Name}
		}).
		WithObjects(append(objs, host)...).Build()
	r := &HostOperationReconciler{
		Client:   cl,
		Recorder: events.NewFakeRecorder(100),
		Connect: func(context.Context, *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
			return rec, f.Systems[0].ID, "", "", nil
		},
		PowerSettle: 50 * time.Millisecond,
	}
	return r, st, rec
}

func preClearedOp(actions ...metalv1alpha1.HostOperationAction) *metalv1alpha1.HostOperation {
	return &metalv1alpha1.HostOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "pk-metal"},
		Spec: metalv1alpha1.HostOperationSpec{
			HostRef:    metalv1alpha1.Reference{Name: "host-a"},
			ProposedBy: "system:serviceaccount:pk-metal:pk-metal-manager",
			Actions:    actions,
			Approval:   metalv1alpha1.OperationApproval{Required: ptr.To(false)},
		},
	}
}

// runOp reconciles until the operation finishes (or the budget runs out) and
// returns its final state.
func runOp(t *testing.T, r *HostOperationReconciler) *metalv1alpha1.HostOperation {
	t.Helper()
	key := client.ObjectKey{Namespace: "pk-metal", Name: "op"}
	op := &metalv1alpha1.HostOperation{}
	for range 10 {
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		require.NoError(t, err)
		require.NoError(t, r.Get(t.Context(), key, op))
		if op.Finished() {
			return op
		}
	}
	t.Fatalf("operation never finished: %+v", op.Status)
	return nil
}

func TestPowerCycleIsOneObservedCycle(t *testing.T) {
	// The iBMC serves no LastResetTime: the only observable is the power state.
	r, st, rec := executorFor(t, "huawei-2288h-v5",
		preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}}))
	before := st.Resets("1")

	op := runOp(t, r)
	require.True(t, opSucceeded(op), "%+v", op.Status)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionForceOff, bmc.ActionOn}, rec.powers, "off, observed, on")
	assert.Equal(t, before+1, st.Resets("1"), "exactly one boot")
	require.NotNil(t, op.Status.OffObserved, "the off half is recorded")
}

func TestResumedPowerCycleDoesNotCycleAgain(t *testing.T) {
	// A pass died after the machine was seen off and turned back on, before the
	// step was recorded. The machine is on; the record says it was seen off.
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Status = resumedStatus(0)
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	before := st.Resets("1")

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Empty(t, rec.powers, "a cycle already half-observed and finished is never issued again")
	assert.Equal(t, before, st.Resets("1"))
}

func TestResumedOperationDoesNotRearmBootOverride(t *testing.T) {
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd", Mode: "UEFI"}},
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	// The override was set (step recorded), the cycle ran and the boot used the
	// override up; the pass died before the power step was recorded.
	op.Status = resumedStatus(1)
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	require.NoError(t, st.SetBootOverride(t.Context(), "1", bmc.BootOverride{Target: "Cd", Enabled: bmc.OverrideOnce}))
	require.NoError(t, st.SetPower(t.Context(), "1", bmc.ActionForceRestart)) // consumes the Once override
	systems, err := st.Systems(t.Context())
	require.NoError(t, err)
	require.Equal(t, bmc.OverrideDisabled, systems[0].BootOverride.Enabled, "precondition: override used up")

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Zero(t, rec.overrides, "an override a later step consumed is not re-armed")
	assert.Empty(t, rec.powers)
}

func TestBootOverrideStepChecksItsGoal(t *testing.T) {
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd"}},
	)
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	// The previous pass PATCHed and died before recording the step.
	require.NoError(t, st.SetBootOverride(t.Context(), "1", bmc.BootOverride{Target: "Cd", Enabled: bmc.OverrideOnce}))

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Zero(t, rec.overrides, "an override already set as wanted is not written again")
}

func TestApplyBiosStagesThePinnedAttributes(t *testing.T) {
	// The live BiosSettings says something else — the executor must not read it.
	settings := &metalv1alpha1.BiosSettings{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a"},
		Spec: metalv1alpha1.BiosSettingsSpec{Attributes: map[string]apiextensionsv1.JSON{
			"QuietBoot": jsonOf("Disabled"),
		}},
	}
	op := preClearedOp(metalv1alpha1.HostOperationAction{ApplyBios: &metalv1alpha1.ApplyBiosAction{
		SettingsRef: &metalv1alpha1.Reference{Name: "host-a"},
		Attributes:  map[string]apiextensionsv1.JSON{"QuietBoot": jsonOf("Enabled")},
	}})
	r, st, _ := executorFor(t, "huawei-2288h-v5", op, settings)

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	state, err := st.Bios(t.Context(), "1")
	require.NoError(t, err)
	assert.Equal(t, "Enabled", state.Pending["QuietBoot"], "the approved value is staged, not the live one")
}

func TestStaleTimeoutFailsThePowerStep(t *testing.T) {
	// A BMC whose reported power never moves: the step gives up, it does not
	// loop forever or claim success.
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}})
	r, st, _ := executorFor(t, "huawei-2288h-v5", op)
	st.SetFault(sim.FaultStalePower, true)
	r.PowerTimeout = time.Millisecond

	got := runOp(t, r)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metav1.ConditionFalse, ex.Status)
	assert.Contains(t, ex.Message, "gave up")
}

func TestGracefulPowerOffAsksTheOSFirst(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: 300}})
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionGracefulShutdown}, rec.powers, "the OS shut down: nothing forced")
	require.NotNil(t, got.Status.ShutdownRequested)
	power, err := st.Power(t.Context(), "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, power)
}

func TestGracefulPowerOffForcesAfterTheWindow(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: 300}})
	r, st, rec := executorFor(t, "huawei-2288h-v5", op)
	rec.ignoreGraceful = true
	key := client.ObjectKeyFromObject(op)

	// The first pass asks once and waits; the machine stays on.
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	assert.False(t, got.Finished())
	assert.Equal(t, []bmc.PowerAction{bmc.ActionGracefulShutdown}, rec.powers, "asked once, not on every pass")
	require.NotNil(t, got.Status.ShutdownRequested)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Contains(t, ex.Message, "forcing the machine off in")

	// The window passes with the machine still on: it is forced off.
	got.Status.ShutdownRequested = &metav1.MicroTime{Time: time.Now().Add(-301 * time.Second)}
	require.NoError(t, r.Status().Update(t.Context(), got))
	got = runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Equal(t, []bmc.PowerAction{bmc.ActionGracefulShutdown, bmc.ActionForceOff}, rec.powers)
	power, err := st.Power(t.Context(), "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, power)
}

// conflictOnce answers the first status write that match selects with a
// conflict, as the API server does when another writer moved the object.
type conflictOnce struct {
	client.Client
	match func(*metalv1alpha1.HostOperation) bool
	hit   bool
}

func (c *conflictOnce) Status() client.SubResourceWriter {
	return &conflictOnceStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type conflictOnceStatus struct {
	client.SubResourceWriter
	c *conflictOnce
}

func (s *conflictOnceStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if op, ok := obj.(*metalv1alpha1.HostOperation); ok && !s.c.hit && s.c.match(op) {
		s.c.hit = true
		return apierrors.NewConflict(metalv1alpha1.GroupVersion.WithResource("hostoperations").GroupResource(),
			op.Name, fmt.Errorf("the object has been modified"))
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// A status write that fails inside a step is not the machine's verdict. The
// operation used to end Failed when the record of the off half of a power
// cycle, or of a graceful shutdown request, did not land; with the machine
// off. Now the pass ends with the error and the next one finishes the step.
func TestStepRecordConflictIsNotAFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		action metalv1alpha1.HostOperationAction
		match  func(*metalv1alpha1.HostOperation) bool
		boots  int
	}{
		"power cycle, off half": {
			action: metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
			match:  func(op *metalv1alpha1.HostOperation) bool { return op.Status.OffObserved != nil },
			boots:  1,
		},
		"graceful power-off": {
			action: metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: 300}},
			match:  func(op *metalv1alpha1.HostOperation) bool { return op.Status.ShutdownRequested != nil },
		},
	} {
		t.Run(name, func(t *testing.T) {
			op := preClearedOp(tc.action)
			r, st, _ := executorFor(t, "huawei-2288h-v5", op)
			before := st.Resets("1")
			api := &conflictOnce{Client: r.Client, match: tc.match}
			r.Client = api

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)})
			require.True(t, api.hit, "precondition: the step's record met the conflict")
			assert.True(t, apierrors.IsConflict(err), "the pass ends with the write's error: %v", err)
			got := &metalv1alpha1.HostOperation{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), got))
			assert.False(t, got.Finished(), "%+v", got.Status.Conditions)

			got = runOp(t, r)
			require.True(t, opSucceeded(got), "%+v", got.Status)
			assert.Equal(t, before+tc.boots, st.Resets("1"))
		})
	}
}

func TestMaintenanceHoldsAnOperationNotStarted(t *testing.T) {
	op := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	r, _, rec := executorFor(t, "huawei-2288h-v5", op)
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Annotations = map[string]string{metalv1alpha1.AnnotationMaintenance: "replacing DIMM B2"}
	require.NoError(t, r.Update(t.Context(), host))
	key := client.ObjectKeyFromObject(op)

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	assert.Empty(t, rec.powers, "nothing runs on a host in maintenance")
	assert.Nil(t, got.Status.Baseline)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metalv1alpha1.ReasonHostInMaintenance, ex.Reason)
	assert.Contains(t, ex.Message, "replacing DIMM B2")

	// Given back, it runs.
	host.Annotations = nil
	require.NoError(t, r.Update(t.Context(), host))
	got = runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
}

func TestCheckInTimeoutSaysWhereToLook(t *testing.T) {
	node := &osv1alpha1.TalosNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Spec:       osv1alpha1.TalosNodeSpec{Address: "192.0.2.12"},
		Status:     osv1alpha1.TalosNodeStatus{Stage: "installing"},
	}
	op := preClearedOp(metalv1alpha1.HostOperationAction{AwaitCheckIn: &metalv1alpha1.AwaitCheckInAction{
		NodeRef: metalv1alpha1.Reference{Name: "node-2"}, TimeoutSeconds: 60,
	}})
	// The wait began two minutes ago.
	op.Status = resumedStatus(0)
	op.Status.Baseline.CapturedAt = &metav1.Time{Time: time.Now().Add(-2 * time.Minute)}
	r, _, _ := executorFor(t, "huawei-2288h-v5", op, node)
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "host-a"}, host))
	host.Labels = map[string]string{labelAsset: "node-2"}
	require.NoError(t, r.Update(t.Context(), host))

	got := runOp(t, r)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metav1.ConditionFalse, ex.Status)
	for _, want := range []string{
		"gave up after 1m0s",
		"last stage seen: installing",
		"serial-over-LAN",
		".lab/state/node-2/console.log",
		"talosctl -n 192.0.2.12 --insecure get addresses",
	} {
		assert.Contains(t, ex.Message, want)
	}
}

// resumedStatus is an executing operation whose first n steps are recorded,
// whose baseline saw the machine on, and whose power cycle was seen off.
func resumedStatus(n int) metalv1alpha1.HostOperationStatus {
	now := metav1.Now()
	s := metalv1alpha1.HostOperationStatus{
		Baseline:    &metalv1alpha1.OperationBaseline{PowerState: "On", CapturedAt: &now},
		OffObserved: &metav1.MicroTime{Time: now.Time},
		Conditions: []metav1.Condition{
			{Type: metalv1alpha1.ConditionApproved, Status: metav1.ConditionTrue, Reason: metalv1alpha1.ReasonApprovalNotRequired, LastTransitionTime: now},
			{Type: metalv1alpha1.ConditionExecuted, Status: metav1.ConditionUnknown, Reason: metalv1alpha1.ReasonExecuting, LastTransitionTime: now},
		},
	}
	for range n {
		s.Steps = append(s.Steps, metalv1alpha1.OperationStep{Action: "setBootOverride", Phase: "Succeeded", FinishedAt: &now})
	}
	return s
}

func TestResumedOperationRemountsAForgottenGrant(t *testing.T) {
	ctx := t.Context()
	iso, err := asset.BuildConfigISO([]byte("version: v1alpha1\n"))
	require.NoError(t, err)
	digest := godigest.FromBytes(iso).String()
	artifact := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config-artifact", Namespace: "pk-metal"},
		Data:       map[string][]byte{artifactKey: iso},
	}
	config := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node"}},
		Status: osv1alpha1.BootAssetStatus{
			Digest:            digest,
			ArtifactSecretRef: &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: artifact.Name},
			Conditions: []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Built", LastTransitionTime: metav1.Now()}},
		},
	}
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
			MediaID: "VirtualMedia1", BootAssetRef: &metalv1alpha1.Reference{Name: config.Name}, Digest: digest,
		}},
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	// The medium was mounted and the step recorded; then the process that
	// held the grant went away, and the new one's table is empty.
	op.Status = resumedStatus(1)
	op.Status.Steps[0].Action = "insertMedia"
	op.Status.OffObserved = nil
	r, st, _ := executorFor(t, "supermicro-x12", op, config, artifact)
	r.Media = startMedia(t)

	creds, err := asset.MintCredentials()
	require.NoError(t, err)
	g, err := r.Media.Grants.Issue(grantHandle(client.ObjectKeyFromObject(op)), "node-config.iso", iso, time.Hour, creds)
	require.NoError(t, err)
	require.NoError(t, st.InsertMedia(ctx, "VirtualMedia1", r.Media.URL(g),
		&bmc.MediaCredentials{Username: creds.Username, Password: creds.Password}))
	r.Media.Grants.Revoke(g.Token)

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Equal(t, iso, st.BootImage("VirtualMedia1"), "the machine booted the config, not a dead URL")
}

// A granted medium that a later recorded step replaced in the same slot is not
// mounted again after a restart. It used to be: the re-mount put the config
// back over the image the later step mounted, and the machine booted from it.
func TestRemountLeavesAReplacedMediumAlone(t *testing.T) {
	ctx := t.Context()
	iso, err := asset.BuildConfigISO([]byte("version: v1alpha1\n"))
	require.NoError(t, err)
	digest := godigest.FromBytes(iso).String()
	artifact := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config-artifact", Namespace: "pk-metal"},
		Data:       map[string][]byte{artifactKey: iso},
	}
	config := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config"},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node"}},
		Status: osv1alpha1.BootAssetStatus{
			Digest:            digest,
			ArtifactSecretRef: &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: artifact.Name},
			Conditions: []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Built", LastTransitionTime: metav1.Now()}},
		},
	}
	installer := []byte("installer image")
	images := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(installer)
	}))
	t.Cleanup(images.Close)
	imageURL := images.URL + "/installer.iso"

	op := preClearedOp(
		metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
			MediaID: "VirtualMedia1", BootAssetRef: &metalv1alpha1.Reference{Name: config.Name}, Digest: digest,
		}},
		metalv1alpha1.HostOperationAction{InsertMedia: &metalv1alpha1.InsertMediaAction{
			MediaID: "VirtualMedia1", ImageURL: imageURL, Digest: godigest.FromBytes(installer).String(),
		}},
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	// Both mounts are recorded, the slot holds the image; then the process
	// went away, and the new one holds no grant for the config.
	op.Status = resumedStatus(2)
	op.Status.Steps[0].Action, op.Status.Steps[1].Action = "insertMedia", "insertMedia"
	op.Status.OffObserved = nil
	r, st, _ := executorFor(t, "supermicro-x12", op, config, artifact)
	withImages(t, r, images)
	downloaded(t, r, imageURL, godigest.FromBytes(installer).String())
	require.NoError(t, st.InsertMedia(ctx, "VirtualMedia1", imageURL, nil))

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Equal(t, installer, st.BootImage("VirtualMedia1"), "the machine booted the image the last step mounted")
}

func TestUnreachableMachineHoldsTheStep(t *testing.T) {
	for name, breakIt := range map[string]func(r *HostOperationReconciler, rec *recordingClient){
		"no reachable path": func(r *HostOperationReconciler, _ *recordingClient) {
			r.Connect = func(context.Context, *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
				return nil, "", metalv1alpha1.ReasonNoReachablePath, "host host-a has no reachable management path", nil
			}
		},
		"deadline": func(_ *HostOperationReconciler, rec *recordingClient) {
			rec.powerErr = fmt.Errorf("get power: %w", context.DeadlineExceeded)
		},
		"connection refused": func(_ *HostOperationReconciler, rec *recordingClient) {
			rec.powerErr = &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			op := preClearedOp(metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}})
			// The baseline is on record: the failure meets the step itself.
			op.Status = resumedStatus(0)
			r, _, rec := executorFor(t, "huawei-2288h-v5", op)
			connect := r.Connect
			breakIt(r, rec)

			key := client.ObjectKeyFromObject(op)
			res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			got := &metalv1alpha1.HostOperation{}
			require.NoError(t, r.Get(t.Context(), key, got))
			require.False(t, got.Finished(), "a pass that could not reach the machine is no verdict: %+v", got.Status)
			assert.Positive(t, res.RequeueAfter)
			ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
			require.NotNil(t, ex)
			assert.Contains(t, ex.Message, "BMC unavailable")

			r.Connect, rec.powerErr = connect, nil
			require.True(t, opSucceeded(runOp(t, r)))
		})
	}
}

func TestPassStopsBeforeItsBudgetRunsOut(t *testing.T) {
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{SetBootOverride: &metalv1alpha1.SetBootOverrideAction{Target: "Cd"}},
		metalv1alpha1.HostOperationAction{PowerOff: &metalv1alpha1.PowerOffAction{}},
	)
	r, _, _ := executorFor(t, "huawei-2288h-v5", op)
	// Less is left after the first step than a settle window plus the margin.
	r.PassTimeout = 2 * time.Second

	key := client.ObjectKeyFromObject(op)
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	got := &metalv1alpha1.HostOperation{}
	require.NoError(t, r.Get(t.Context(), key, got))
	require.Len(t, got.Status.Steps, 1, "one step, then the pass yields")
	assert.False(t, got.Finished())
	assert.Positive(t, res.RequeueAfter)
	require.True(t, opSucceeded(runOp(t, r)), "the next pass continues")
}

// startMedia runs a media service on a loopback port for the test's lifetime.
func startMedia(t *testing.T) *asset.Service {
	t.Helper()
	cert, err := asset.EphemeralCert()
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	media, err := asset.NewService(addr, "https://"+addr, cert)
	require.NoError(t, err)
	go func() { _ = media.Start(t.Context()) }()
	_, err = media.Addr(t.Context())
	require.NoError(t, err)
	return media
}

// A boot that touched the machine and then failed is retried: pk-metal retry
// deletes it, and the claim makes a new one. A claim deleted before the new
// boot starts still powers the machine off before it lets the host go. It
// used to hand the machine back as it was, with the failed boot's medium in
// the slot: no record said that a boot had run.
func TestRetriedBootKeepsItsRecord(t *testing.T) {
	ctx := t.Context()
	claim := testClaim("node-b")
	claim.Finalizers = []string{releaseFinalizer}
	images := isoServer(t)
	failing := preClearedOp(
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		insertURL(images.URL+"/installer.iso"),
	)
	failing.Labels = claimLabels(claim, purposeProvision)
	r, _, rec := executorFor(t, "supermicro-x12", failing, claim)
	withImages(t, r, images)
	downloaded(t, r, images.URL+"/installer.iso", isoDigest)
	rec.insertErr = fmt.Errorf("the BMC refuses the image")
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
	host.Spec.ClaimRef = &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID}
	require.NoError(t, r.Update(ctx, host))

	failed := runOp(t, r)
	require.False(t, opSucceeded(failed), "precondition: the machine refused a step")

	// pk-metal retry: the failed boot goes, and a new one waits to start.
	require.NoError(t, r.Delete(ctx, failed))
	retried := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	retried.Name, retried.Labels = "boot-2", claimLabels(claim, purposeProvision)
	require.NoError(t, r.Create(ctx, retried))

	// The claim is deleted before the new boot starts.
	require.NoError(t, r.Delete(ctx, claim))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(claim), claim))
	cr := &HostClaimReconciler{Client: r.Client, OperationsNamespace: "pk-metal", Proposer: "operator"}
	_, err := cr.finishRelease(ctx, claim)
	require.NoError(t, err)

	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
	assert.NotNil(t, host.Spec.ClaimRef, "the machine a boot touched stays bound until a release powered it off")
	assert.True(t, powerOffDecided(host, claim), "releasing the host powers it off")
}

// A boot records its start on the host as the API server holds it. A cache
// that has not yet seen the bind shows the host as free; the record must be
// written all the same, or a retried boot loses it again.
func TestBootRecordsItsStartPastAStaleCache(t *testing.T) {
	ctx := t.Context()
	claim := testClaim("node-b")
	boot := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	boot.Labels = claimLabels(claim, purposeProvision)
	r, _, _ := executorFor(t, "supermicro-x12", boot, claim)
	host := &metalv1alpha1.Host{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
	free := host.DeepCopy()
	host.Spec.ClaimRef = &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID}
	require.NoError(t, r.Update(ctx, host))
	r.APIReader = r.Client
	r.Client = &staleHost{Client: r.Client, host: free}

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	require.NoError(t, r.APIReader.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
	assert.True(t, powerOffDecided(host, claim), "the boot's start is on the host")
}

// staleHost answers reads of the host from a cache that has not seen its
// latest write; everything else goes to the API server.
type staleHost struct {
	client.Client
	host *metalv1alpha1.Host
}

func (c *staleHost) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if h, ok := obj.(*metalv1alpha1.Host); ok && key.Name == c.host.Name {
		c.host.DeepCopyInto(h)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// A boot does not start once its claim is being deleted, or once the claim no
// longer holds the host. It used to: a claim pass that read the claim from a
// cache that had not yet seen the deletion made a new boot, which could start
// after the release handed the machine back and mount its media there.
func TestBootOfAReleasedClaimDoesNotStart(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, r *HostOperationReconciler, claim *metalv1alpha1.HostClaim)
	}{
		{"the claim is being deleted", func(t *testing.T, r *HostOperationReconciler, claim *metalv1alpha1.HostClaim) {
			require.NoError(t, r.Delete(ctx, claim))
		}},
		{"the claim is gone", func(t *testing.T, r *HostOperationReconciler, claim *metalv1alpha1.HostClaim) {
			claim.Finalizers = nil
			require.NoError(t, r.Update(ctx, claim))
			require.NoError(t, r.Delete(ctx, claim))
		}},
		{"the host was unbound", func(t *testing.T, r *HostOperationReconciler, _ *metalv1alpha1.HostClaim) {
			host := &metalv1alpha1.Host{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
			host.Spec.ClaimRef = nil
			require.NoError(t, r.Update(ctx, host))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := testClaim("node-b")
			claim.Finalizers = []string{releaseFinalizer}
			boot := preClearedOp(metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
			boot.Labels = claimLabels(claim, purposeProvision)
			r, _, rec := executorFor(t, "supermicro-x12", boot, claim)
			host := &metalv1alpha1.Host{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Name: "host-a"}, host))
			host.Spec.ClaimRef = &metalv1alpha1.ClaimReference{Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID}
			require.NoError(t, r.Update(ctx, host))
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(claim), claim))
			tc.setup(t, r, claim)

			got := runOp(t, r)
			ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
			require.NotNil(t, ex)
			assert.Equal(t, metav1.ConditionFalse, ex.Status, "the boot ends")
			assert.Equal(t, metalv1alpha1.ReasonHostNotClaimed, ex.Reason, ex.Message)
			assert.Nil(t, got.Status.Baseline, "it never started")
			assert.Empty(t, rec.powers, "the machine is not touched")
		})
	}
}

// An operation that names an action the executor does not run yet fails
// before it starts: no baseline, no call to the BMC, even for the BMC
// actions in front of it, and the step message says why.
func TestEveryActionIsImplemented(t *testing.T) {
	for _, key := range metalv1alpha1.ActionKeys() {
		assert.True(t, implementedActions[key], "the executor does not run %s", key)
	}
}

func TestUnimplementedActionFailsBeforeAnythingRuns(t *testing.T) {
	op := preClearedOp(
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
		metalv1alpha1.HostOperationAction{TalosReboot: &metalv1alpha1.TalosRebootAction{NodeRef: metalv1alpha1.Reference{Name: "node-a"}}},
	)
	r, _, rec := executorFor(t, "huawei-2288h-v5", op)
	// Every action the API defines today runs; the refusal stays for the
	// next one the API gains before its executor does.
	delete(implementedActions, "talosReboot")
	t.Cleanup(func() { implementedActions["talosReboot"] = true })

	got := runOp(t, r)
	ex := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionExecuted)
	require.NotNil(t, ex)
	assert.Equal(t, metav1.ConditionFalse, ex.Status)
	assert.Equal(t, metalv1alpha1.ReasonStepFailed, ex.Reason)
	assert.Contains(t, ex.Message, "step 2/2 talosReboot")
	require.Len(t, got.Status.Steps, 1)
	assert.Equal(t, "talosReboot", got.Status.Steps[0].Action)
	assert.Equal(t, "Failed", got.Status.Steps[0].Phase)
	assert.Contains(t, got.Status.Steps[0].Message, "not implemented by this operator version yet")
	assert.Nil(t, got.Status.Baseline, "the operation never started")
	assert.Empty(t, rec.powers, "nothing was done to the machine")
	assert.Equal(t, metalv1alpha1.OperationFailed, got.Status.Phase)
}
