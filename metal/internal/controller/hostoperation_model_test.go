package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"syscall"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
)

// The executor model test. It drives the real Reconcile over generated
// operations against the sim, and injects what real deployments do to it:
//
//   - the BMC drops a call before it acts, or acts and the answer is lost
//     (both as network errors, which the executor must treat as transient);
//   - the BMC refuses one mutating call (a verdict, which must fail the
//     operation);
//   - the API server answers a status write with a conflict, or applies it
//     and the answer is lost;
//   - the process dies at any BMC or API call, before or after the call
//     acted; a new reconciler picks up from what the API server holds, with
//     an empty media grant table;
//   - a person deletes the operation in the middle of a pass.
//
// After every pass it checks the safety properties:
//
//   - the recorded steps are a prefix of the actions, and only the last can
//     be Failed;
//   - a finished operation makes no BMC call and keeps its outcome;
//   - a finished or deleted operation holds no live media grant;
//   - after a deletion, at most the step in flight completes.
//
// At the end it checks the outcome: only transient faults end in Succeeded,
// a refusal ends in Failed, the TTL removes the operation only when it is
// zero (the test is far shorter than any other TTL), and the machine is what
// the actions, applied once each in order, make of it: one boot per power
// cycle, no re-armed boot override, the media and BIOS settings the steps
// asked for. A failure names the seed and the actions, so a case replays
// exactly. Firmware flashes are not drawn: each one polls the version for
// seconds, which would make the model too slow to run with the unit tests.

// crashed is the panic a chaos point raises to end a pass the way a process
// death would: nothing after it runs, and nothing in memory survives.
type crashed struct{}

// chaos decides, call by call, what goes wrong. One instance spans a case.
type chaos struct {
	r *rand.Rand
	// Probabilities per call.
	pDrop, pLost, pCrash, pConflict, pDelete float64
	// refuseAt is the ordinal of the mutating BMC call the machine refuses,
	// or -1. refused records that it happened.
	refuseAt int
	refused  bool
	// failLost: a status write that recorded Executed=False did not persist,
	// or the process died between the refusal and that write (the failed
	// step ejects the media it mounted first, which calls the BMC).
	failLost bool
	// verdictKept: a status write that recorded Executed=False persisted.
	verdictKept bool

	mutating int // mutating BMC calls attempted
	applied  int // mutating BMC calls that took effect
	calls    int // every BMC call
	// deleteOp deletes the operation from the API, once, mid-pass.
	deleteOp  func()
	deletedAt int // applied count when the delete happened; -1 before
}

func (c *chaos) roll(p float64) bool { return p > 0 && c.r.Float64() < p }

// crash ends the pass as a process death would.
func (c *chaos) crash() {
	if c.refused && !c.verdictKept {
		c.failLost = true
	}
	panic(crashed{})
}

var errRefused = errors.New("the machine refused the request")

// netErr is a connection reset: what a dropped request or a lost answer
// looks like to the client.
func netErr(op string) error {
	return &net.OpError{Op: op, Net: "tcp", Err: syscall.ECONNRESET}
}

// chaosBMC is the sim behind chaos points.
type chaosBMC struct {
	bmc.Client
	c *chaos
}

// do runs one BMC call through the chaos points.
func (b *chaosBMC) do(mutating bool, refusable bool, call func() error) error {
	c := b.c
	c.calls++
	if c.roll(c.pCrash) {
		c.crash()
	}
	if c.roll(c.pDrop) {
		return netErr("write")
	}
	if mutating {
		n := c.mutating
		c.mutating++
		if refusable && n == c.refuseAt {
			c.refused = true
			return errRefused
		}
	}
	if err := call(); err != nil {
		return err
	}
	if mutating {
		c.applied++
	}
	if c.roll(c.pCrash) {
		c.crash()
	}
	if c.roll(c.pLost) {
		return netErr("read")
	}
	return nil
}

func (b *chaosBMC) Power(ctx context.Context, id string) (bmc.PowerState, error) {
	var p bmc.PowerState
	err := b.do(false, false, func() (err error) { p, err = b.Client.Power(ctx, id); return err })
	return p, err
}

func (b *chaosBMC) Systems(ctx context.Context) ([]bmc.System, error) {
	var s []bmc.System
	err := b.do(false, false, func() (err error) { s, err = b.Client.Systems(ctx); return err })
	return s, err
}

func (b *chaosBMC) Media(ctx context.Context) ([]bmc.VirtualMedia, error) {
	var m []bmc.VirtualMedia
	err := b.do(false, false, func() (err error) { m, err = b.Client.Media(ctx); return err })
	return m, err
}

func (b *chaosBMC) Bios(ctx context.Context, id string) (bmc.BiosState, error) {
	var s bmc.BiosState
	err := b.do(false, false, func() (err error) { s, err = b.Client.Bios(ctx, id); return err })
	return s, err
}

func (b *chaosBMC) SetPower(ctx context.Context, id string, a bmc.PowerAction) error {
	// A refused graceful shutdown is forced by design, so only the forcing
	// actions are refusable.
	return b.do(true, a != bmc.ActionGracefulShutdown, func() error { return b.Client.SetPower(ctx, id, a) })
}

func (b *chaosBMC) SetBootOverride(ctx context.Context, id string, o bmc.BootOverride) error {
	return b.do(true, true, func() error { return b.Client.SetBootOverride(ctx, id, o) })
}

func (b *chaosBMC) InsertMedia(ctx context.Context, slot, url string, creds *bmc.MediaCredentials) error {
	return b.do(true, true, func() error { return b.Client.InsertMedia(ctx, slot, url, creds) })
}

func (b *chaosBMC) EjectMedia(ctx context.Context, slot string) error {
	return b.do(true, true, func() error { return b.Client.EjectMedia(ctx, slot) })
}

func (b *chaosBMC) SetBiosAttributes(ctx context.Context, id string, attrs map[string]any) error {
	return b.do(true, true, func() error { return b.Client.SetBiosAttributes(ctx, id, attrs) })
}

func (b *chaosBMC) Close() error { return nil }

// chaosAPI is the fake API server behind chaos points on the executor's
// status writes and reads.
type chaosAPI struct {
	client.Client
	c *chaos
}

func (a *chaosAPI) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*metalv1alpha1.HostOperation); ok && a.c.deletedAt < 0 && a.c.roll(a.c.pDelete) {
		a.c.deleteOp()
	}
	if a.c.roll(a.c.pCrash) {
		a.c.crash()
	}
	return a.Client.Get(ctx, key, obj, opts...)
}

func (a *chaosAPI) Status() client.SubResourceWriter {
	return &chaosStatus{SubResourceWriter: a.Client.Status(), c: a.c}
}

type chaosStatus struct {
	client.SubResourceWriter
	c *chaos
}

func (s *chaosStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	c := s.c
	failing := false
	if op, ok := obj.(*metalv1alpha1.HostOperation); ok {
		ex := meta.FindStatusCondition(op.Status.Conditions, metalv1alpha1.ConditionExecuted)
		failing = ex != nil && ex.Status == metav1.ConditionFalse
	}
	if c.roll(c.pCrash) {
		c.failLost = c.failLost || failing
		c.crash()
	}
	if c.roll(c.pConflict) {
		c.failLost = c.failLost || failing
		return apierrors.NewConflict(schema.GroupResource{Group: "metal.pk.io", Resource: "hostoperations"},
			obj.GetName(), errors.New("the object has been modified"))
	}
	if err := s.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
		return err
	}
	c.verdictKept = c.verdictKept || failing
	if c.roll(c.pCrash) {
		c.crash()
	}
	if c.roll(c.pConflict) {
		// Applied, and the answer lost on the way back.
		return apierrors.NewServerTimeout(schema.GroupResource{Group: "metal.pk.io", Resource: "hostoperations"}, "update", 1)
	}
	return nil
}

// modelWorld is the machine as the actions, applied once each in order, would
// leave it: the oracle a finished operation is compared with.
type modelWorld struct {
	power   bmc.PowerState
	boots   int
	target  string
	enabled string
	slots   map[string]string
	current map[string]any
	pending map[string]any
}

func (w *modelWorld) apply(a metalv1alpha1.HostOperationAction) {
	switch {
	case a.PowerCycle != nil:
		// Off then on, or only on from off: one boot either way.
		w.power = bmc.PowerOn
		w.boots++
		if w.enabled == bmc.OverrideOnce {
			w.enabled = bmc.OverrideDisabled
		}
		for k, v := range w.pending {
			w.current[k] = v
		}
		w.pending = map[string]any{}
	case a.PowerOff != nil:
		w.power = bmc.PowerOff
	case a.SetBootOverride != nil:
		w.target, w.enabled = a.SetBootOverride.Target, bmc.OverrideOnce
		if a.SetBootOverride.Once != nil && !*a.SetBootOverride.Once {
			w.enabled = bmc.OverrideContinuous
		}
	case a.InsertMedia != nil && a.InsertMedia.BootAssetRef != nil:
		w.slots[a.InsertMedia.MediaID] = grantedMedium
	case a.InsertMedia != nil:
		w.slots[a.InsertMedia.MediaID] = granted + urlGrantFile(a.InsertMedia.Digest)
	case a.EjectMedia != nil:
		delete(w.slots, a.EjectMedia.MediaID)
	case a.ApplyBios != nil:
		for k, v := range decodeAttrs(a.ApplyBios.Attributes) {
			if p, ok := w.pending[k]; ok {
				if !jsonEqual(p, v) {
					w.pending[k] = v
				}
				continue
			}
			if !jsonEqual(w.current[k], v) {
				w.pending[k] = v
			}
		}
	}
}

// grantedMedium stands for a slot holding the config ISO under a grant: its
// URL carries a fresh token on every mount, so the oracle compares only that
// the slot holds the granted file. An image named by URL is served under a
// grant too, as granted plus its grant filename.
const grantedMedium = granted + modelConfig + ".iso"

const granted = "granted:"

// modelConfig is the secret-bearing BootAsset the model mounts.
const modelConfig = "node-config"

// genActions draws 1-6 actions the API admits: at most one power action.
func genActions(r *rand.Rand, image func(string) string, digest string) []metalv1alpha1.HostOperationAction {
	slots := []string{"1", "2"}
	var out []metalv1alpha1.HostOperationAction
	power := false
	for range 1 + r.IntN(6) {
		var a metalv1alpha1.HostOperationAction
		switch r.IntN(8) {
		case 0:
			if power {
				continue
			}
			power = true
			a.PowerCycle = &metalv1alpha1.PowerCycleAction{}
		case 1:
			if power {
				continue
			}
			power = true
			a.PowerOff = &metalv1alpha1.PowerOffAction{GracefulTimeoutSeconds: int32(r.IntN(2) * 300)}
		case 2:
			a.SetBootOverride = &metalv1alpha1.SetBootOverrideAction{
				Target: []string{"Cd", "Usb", "Pxe"}[r.IntN(3)], Once: ptr.To(r.IntN(3) != 0)}
		case 3:
			slot := slots[r.IntN(2)]
			url := image(fmt.Sprintf("img-%d", r.IntN(2)))
			a.InsertMedia = &metalv1alpha1.InsertMediaAction{MediaID: slot, ImageURL: url, Digest: imageDigest(url)}
		case 4:
			a.EjectMedia = &metalv1alpha1.EjectMediaAction{MediaID: slots[r.IntN(2)]}
		case 5:
			attrs := map[string]apiextensionsv1.JSON{}
			if r.IntN(2) == 0 {
				attrs["SriovGlobalEnable"] = jsonOf([]string{"Enabled", "Disabled"}[r.IntN(2)])
			}
			if r.IntN(2) == 0 {
				attrs["QuietBoot"] = jsonOf(r.IntN(2) == 0)
			}
			attrs["PxeRetryCount"] = jsonOf(r.IntN(5))
			a.ApplyBios = &metalv1alpha1.ApplyBiosAction{Attributes: attrs}
		case 7:
			a.InsertMedia = &metalv1alpha1.InsertMediaAction{MediaID: slots[r.IntN(2)],
				BootAssetRef: &metalv1alpha1.Reference{Name: modelConfig}, Digest: digest}
		case 6:
			a.AwaitCheckIn = &metalv1alpha1.AwaitCheckInAction{NodeRef: metalv1alpha1.Reference{Name: "node-a"}, TimeoutSeconds: 3600}
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		out = append(out, metalv1alpha1.HostOperationAction{EjectMedia: &metalv1alpha1.EjectMediaAction{MediaID: "1"}})
	}
	return out
}

func TestExecutorModel(t *testing.T) {
	// Each image is its own path: two images have two digests.
	images := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(req.URL.Path))
	}))
	t.Cleanup(images.Close)
	image := func(name string) string { return images.URL + "/" + name + ".iso" }
	// One media service for every case. A crash empties its grant table for
	// the operation, which is what a new process would start with.
	media := startMedia(t)

	cases := 300
	if testing.Short() {
		cases = 40
	}
	for seed := range uint64(cases) {
		if !t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) { runExecutorCase(t, seed, image, images.Client(), media) }) {
			return // one failing seed is enough to read
		}
	}
}

func runExecutorCase(t *testing.T, seed uint64, image func(string) string, images *http.Client, media *asset.Service) {
	ctx := t.Context()
	// The media store is on disk: it outlives a crash of the process.
	mediaStore, err := asset.NewStore(t.TempDir())
	require.NoError(t, err)
	handle := grantHandle(types.NamespacedName{Namespace: "pk-metal", Name: "op"})
	media.Grants.RevokeAsset(handle)
	iso, err := asset.BuildConfigISO([]byte("version: v1alpha1\n"))
	require.NoError(t, err)
	digest := godigest.FromBytes(iso).String()
	artifact := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "node-config-artifact", Namespace: "pk-metal"},
		Data:       map[string][]byte{artifactKey: iso},
	}
	config := &osv1alpha1.BootAsset{
		ObjectMeta: metav1.ObjectMeta{Name: modelConfig},
		Spec:       osv1alpha1.BootAssetSpec{Strategy: osv1alpha1.StrategyConfigISO, NodeRef: &osv1alpha1.Reference{Name: "node-a"}},
		Status: osv1alpha1.BootAssetStatus{
			Digest:            digest,
			ArtifactSecretRef: &osv1alpha1.SecretReference{Namespace: "pk-metal", Name: artifact.Name},
			Conditions: []metav1.Condition{{Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Built", LastTransitionTime: metav1.Now()}},
		},
	}
	r := rand.New(rand.NewPCG(5, seed))
	c := &chaos{r: r, refuseAt: -1, deletedAt: -1}
	// A third of the cases are calm, so the oracle is reached without noise;
	// the rest draw their own fault rates.
	if r.IntN(3) != 0 {
		c.pDrop, c.pLost, c.pCrash = 0.05*r.Float64(), 0.05*r.Float64(), 0.03*r.Float64()
		c.pConflict = 0.1 * r.Float64()
	}
	if r.IntN(4) == 0 {
		c.refuseAt = r.IntN(6)
	}
	if r.IntN(6) == 0 {
		c.pDelete = 0.1
	}

	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	st := sim.NewState(f)
	if r.IntN(2) == 0 {
		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionOn))
	}
	baselinePower, err := st.Power(ctx, "1")
	require.NoError(t, err)
	resets0 := st.Resets("1")
	bios0, err := st.Bios(ctx, "1")
	require.NoError(t, err)

	actions := genActions(r, image, digest)
	op := preClearedOp(actions...)
	var ttl *int32
	switch r.IntN(3) {
	case 0:
		ttl = ptr.To(int32(0))
	case 1:
		ttl = ptr.To(int32(3600))
	}
	op.Spec.TTLSecondsAfterFinished = ttl
	// The site's retention comes from its own source, so that the draws
	// above and below stay what they were for every seed: the default
	// (no key), none beyond the TTL ("0s"), or Forever.
	keep := []string{"", "0s", RetentionForever}[rand.New(rand.NewPCG(7, seed)).IntN(3)]
	site := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: siteSettingsConfigMap},
		Data: map[string]string{}}
	if keep != "" {
		site.Data[retentionKey] = keep
	}
	node := &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	host := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-a"}}
	store := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&metalv1alpha1.HostOperation{}).
		WithIndex(&metalv1alpha1.HostOperation{}, opHostIndex, func(o client.Object) []string {
			return []string{o.(*metalv1alpha1.HostOperation).Spec.HostRef.Name}
		}).
		WithObjects(op, node, host, artifact, config, site).Build()
	key := client.ObjectKeyFromObject(op)
	c.deleteOp = func() {
		c.deletedAt = c.applied
		require.NoError(t, client.IgnoreNotFound(store.Delete(ctx, &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})))
	}
	rec := &chaosBMC{Client: st, c: c}
	api := &chaosAPI{Client: store, c: c}

	desc := func() string {
		return fmt.Sprintf("seed %d actions %s baseline %s refuseAt %d", seed, actionNames(actions), baselinePower, c.refuseAt)
	}

	var (
		finishedCalls = -1 // BMC calls when the operation was first seen finished
		finished      *metalv1alpha1.HostOperation
		goneAfter     = -1 // BMC calls when the operation was first seen gone
		settled       int  // passes seen finished
	)
	for pass := 0; pass < 400; pass++ {
		// The world: the node checks in at some point.
		if r.IntN(8) == 0 {
			n := &osv1alpha1.TalosNode{}
			require.NoError(t, store.Get(ctx, client.ObjectKeyFromObject(node), n))
			meta.SetStatusCondition(&n.Status.Conditions, metav1.Condition{
				Type: osv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"})
			require.NoError(t, store.Update(ctx, n))
		}

		// Each pass is a fresh reconciler: whatever the last one held in
		// memory is gone, as after a restart.
		rc := &HostOperationReconciler{
			Client:               api,
			CredentialsNamespace: "pk-metal",
			Recorder:             events.NewFakeRecorder(1000),
			Connect: func(context.Context, *metalv1alpha1.Host) (bmc.Client, string, string, string, error) {
				return rec, "1", "", "", nil
			},
			Media:          media,
			Store:          mediaStore,
			RegistryClient: images,
			PowerSettle:    10 * time.Millisecond,
			PassTimeout:    time.Duration(1+r.IntN(30)) * time.Second,
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					if _, ok := p.(crashed); !ok {
						panic(p)
					}
					media.Grants.RevokeAsset(handle)
				}
			}()
			_, _ = rc.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		}()

		cur := &metalv1alpha1.HostOperation{}
		if err := store.Get(ctx, key, cur); apierrors.IsNotFound(err) {
			_, live := media.Grants.Live(handle, modelConfig+".iso")
			require.False(t, live, "%s: a deleted operation's grant still answers", desc())
			if goneAfter < 0 {
				goneAfter = c.calls
			}
			if c.calls != goneAfter {
				t.Fatalf("%s: BMC calls after the operation was gone", desc())
			}
			if pass > 0 && finished == nil && c.deletedAt < 0 {
				t.Fatalf("%s: the operation vanished unfinished", desc())
			}
			if settled++; settled > 3 {
				break
			}
			continue
		} else {
			require.NoError(t, err)
		}

		// Recorded steps are a prefix of the actions; only the last can be Failed.
		require.LessOrEqual(t, len(cur.Status.Steps), len(actions), desc())
		for i, s := range cur.Status.Steps {
			require.Equal(t, actionName(actions[i]), s.Action, "%s: step %d", desc(), i)
			if s.Phase == "Failed" {
				require.Equal(t, len(cur.Status.Steps)-1, i, "%s: a failed step is the last record", desc())
			}
		}

		if cur.Finished() {
			_, live := media.Grants.Live(handle, modelConfig+".iso")
			require.False(t, live, "%s: a finished operation's grant still answers", desc())
			if finished == nil {
				finished, finishedCalls = cur.DeepCopy(), c.calls
			}
			// Terminal is absorbing: no BMC call, no change of outcome.
			require.Equal(t, finishedCalls, c.calls, "%s: a finished operation called the BMC", desc())
			assert.Equal(t, opSucceeded(finished), opSucceeded(cur), desc())
			if settled++; settled > 3 {
				break // a few passes past the end show it stays there
			}
		}
	}

	// A deleted operation: what was in flight may complete (at most one
	// step's two mutating calls), nothing after.
	if c.deletedAt >= 0 {
		assert.LessOrEqual(t, c.applied-c.deletedAt, 2, "%s: BMC writes after deletion", desc())
		return
	}
	if finished == nil {
		t.Fatalf("%s: the operation never finished", desc())
	}
	if ttl != nil && *ttl == 0 && keep == "0s" && opSucceeded(finished) {
		assert.GreaterOrEqual(t, goneAfter, 0, "%s: a zero TTL with no site retention removes the succeeded operation", desc())
	} else {
		assert.Less(t, goneAfter, 0, "%s (retention %q): no TTL, one or a retention not yet run out, or a failure keeps the operation",
			desc(), keep)
	}

	switch {
	case c.refused && !c.failLost:
		// A refusal is a verdict: the operation fails and nothing retries it.
		require.False(t, opSucceeded(finished), "%s: a refused call ended in success: %+v", desc(), finished.Status)
		return
	case c.refused:
		// The Failed record itself was lost: the step is entered again, and
		// may now succeed. Known: a terminal verdict is only as durable as
		// its write.
		return
	}
	require.True(t, opSucceeded(finished), "%s: only transient faults, yet: %+v", desc(), finished.Status.Conditions)

	// The oracle: every action applied once, in order.
	want := &modelWorld{power: baselinePower, enabled: bmc.OverrideDisabled, slots: map[string]string{},
		current: maps.Clone(bios0.Attributes), pending: map[string]any{}}
	maps.Copy(want.pending, bios0.Pending)
	for _, a := range actions {
		want.apply(a)
	}
	power, err := st.Power(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, want.power, power, "%s: power", desc())
	assert.Equal(t, want.boots, st.Resets("1")-resets0, "%s: boots", desc())
	systems, err := st.Systems(ctx)
	require.NoError(t, err)
	got := systems[0].BootOverride
	assert.Equal(t, want.enabled, got.Enabled, "%s: override enablement", desc())
	if want.enabled != bmc.OverrideDisabled {
		assert.Equal(t, want.target, got.Target, "%s: override target", desc())
	}
	slotsNow, err := st.Media(ctx)
	require.NoError(t, err)
	gotSlots := map[string]string{}
	for _, m := range slotsNow {
		if m.Inserted {
			gotSlots[m.ID] = m.ImageURL
			if strings.HasPrefix(m.ImageURL, media.BaseURL) {
				gotSlots[m.ID] = granted + path.Base(m.ImageURL)
			}
		}
	}
	assert.Equal(t, want.slots, gotSlots, "%s: media", desc())
	bios, err := st.Bios(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, want.current, bios.Attributes, "%s: bios current", desc())
	assert.Equal(t, len(want.pending), len(bios.Pending), "%s: bios pending %v", desc(), bios.Pending)
	for k, v := range want.pending {
		assert.True(t, jsonEqual(v, bios.Pending[k]), "%s: pending %s", desc(), k)
	}
}

// imageDigest is the digest of what the model's image server serves at url:
// its path.
func imageDigest(url string) string {
	return godigest.FromString(url[strings.Index(url[len("https://"):], "/")+len("https://"):]).String()
}

func actionNames(actions []metalv1alpha1.HostOperationAction) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = actionName(a)
		switch {
		case a.InsertMedia != nil && a.InsertMedia.BootAssetRef != nil:
			out[i] += "(" + a.InsertMedia.MediaID + ":" + a.InsertMedia.BootAssetRef.Name + ")"
		case a.InsertMedia != nil:
			out[i] += "(" + a.InsertMedia.MediaID + ":" + a.InsertMedia.ImageURL[strings.LastIndex(a.InsertMedia.ImageURL, "/")+1:] + ")"
		case a.EjectMedia != nil:
			out[i] += "(" + a.EjectMedia.MediaID + ")"
		}
	}
	return out
}
